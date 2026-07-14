package workflowkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

var (
	// ErrInvalidEngineConfiguration marks an Engine without the durable ports
	// required to execute a frozen workflow safely.
	ErrInvalidEngineConfiguration = errors.New("workflowkit: invalid engine configuration")
	// ErrInvalidExecution marks malformed immutable execution input.
	ErrInvalidExecution = errors.New("workflowkit: invalid frozen execution")
	// ErrInvalidJobClaim marks a durable job claim that is not bound to its
	// immutable execution, stage, or lease identity.
	ErrInvalidJobClaim = errors.New("workflowkit: invalid durable job claim")
	// ErrExpiredJobClaim marks a claim whose lease has already expired. The
	// Engine deliberately does not try to complete such a claim: its backend
	// must fence or reconcile it first.
	ErrExpiredJobClaim = errors.New("workflowkit: durable job claim lease expired")
	// ErrInvalidStageResult marks an executor result that cannot be committed
	// against the frozen descriptor.
	ErrInvalidStageResult = errors.New("workflowkit: invalid stage execution result")
)

// SubjectBinding binds a generic workflow execution to one immutable subject
// revision. Domain adapters own the meaning and allocation of the opaque IDs;
// the kernel only requires their stable pairing with a digest.
type SubjectBinding struct {
	SubjectID  string        `json:"subject_id"`
	RevisionID string        `json:"revision_id"`
	Digest     SubjectDigest `json:"digest"`
}

// Validate verifies a complete immutable subject binding.
func (binding SubjectBinding) Validate() error {
	if err := validateRequired("subject id", binding.SubjectID, ErrInvalidExecution); err != nil {
		return err
	}
	if err := validateRequired("subject revision id", binding.RevisionID, ErrInvalidExecution); err != nil {
		return err
	}
	if err := binding.Digest.Validate(); err != nil {
		return fmt.Errorf("%w: subject digest: %v", ErrInvalidExecution, err)
	}
	return nil
}

// OpaqueExecutionBinding is a canonical domain-owned document frozen into an
// execution. The kernel validates its identity and bytes but never interprets
// its schema. This keeps provider, checkout, and secret vocabulary outside the
// reusable Engine while still making every job auditable and replay-safe.
type OpaqueExecutionBinding struct {
	Format      string      `json:"format"`
	Version     string      `json:"version"`
	Canonical   []byte      `json:"canonical"`
	Fingerprint Fingerprint `json:"fingerprint"`
}

// NewOpaqueExecutionBinding freezes canonical domain bytes under a typed
// format/version identity.
func NewOpaqueExecutionBinding(format, version string, canonical []byte) (OpaqueExecutionBinding, error) {
	binding := OpaqueExecutionBinding{
		Format:    strings.TrimSpace(format),
		Version:   strings.TrimSpace(version),
		Canonical: append([]byte(nil), canonical...),
	}
	if err := binding.recomputeFingerprint(); err != nil {
		return OpaqueExecutionBinding{}, err
	}
	return binding, nil
}

// Clone returns an independently owned binding value.
func (binding OpaqueExecutionBinding) Clone() OpaqueExecutionBinding {
	binding.Canonical = append([]byte(nil), binding.Canonical...)
	return binding
}

// Validate proves that canonical bytes still match the immutable binding
// fingerprint. It intentionally does not parse the domain document.
func (binding OpaqueExecutionBinding) Validate() error {
	if err := validateRequired("execution binding format", binding.Format, ErrInvalidExecution); err != nil {
		return err
	}
	if err := validateRequired("execution binding version", binding.Version, ErrInvalidExecution); err != nil {
		return err
	}
	if len(binding.Canonical) == 0 {
		return fmt.Errorf("%w: execution binding canonical bytes are required", ErrInvalidExecution)
	}
	if err := binding.Fingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: execution binding fingerprint: %v", ErrInvalidExecution, err)
	}
	expected, err := opaqueExecutionBindingFingerprint(binding.Format, binding.Version, binding.Canonical)
	if err != nil {
		return err
	}
	if binding.Fingerprint != expected {
		return fmt.Errorf("%w: execution binding fingerprint does not match canonical bytes", ErrInvalidExecution)
	}
	return nil
}

func (binding *OpaqueExecutionBinding) recomputeFingerprint() error {
	if err := validateRequired("execution binding format", binding.Format, ErrInvalidExecution); err != nil {
		return err
	}
	if err := validateRequired("execution binding version", binding.Version, ErrInvalidExecution); err != nil {
		return err
	}
	if len(binding.Canonical) == 0 {
		return fmt.Errorf("%w: execution binding canonical bytes are required", ErrInvalidExecution)
	}
	fingerprint, err := opaqueExecutionBindingFingerprint(binding.Format, binding.Version, binding.Canonical)
	if err != nil {
		return err
	}
	binding.Fingerprint = fingerprint
	return nil
}

func opaqueExecutionBindingFingerprint(format, version string, canonical []byte) (Fingerprint, error) {
	return FingerprintParts("workflowkit.opaque-execution-binding.v1", []FingerprintPart{
		{Name: "format", Value: []byte(format)},
		{Name: "version", Value: []byte(version)},
		{Name: "canonical", Value: canonical},
	})
}

// ExecutionPlan is the immutable, explicitly ordered set of batches a durable
// backend may dispatch. Independent stages in one batch are allowed to run in
// parallel; dependencies always belong to an earlier batch.
type ExecutionPlan struct {
	Batches     []ScheduleBatch `json:"batches"`
	Fingerprint Fingerprint     `json:"fingerprint"`
}

// Clone returns an independently owned execution plan.
func (plan ExecutionPlan) Clone() ExecutionPlan {
	batches := plan.Batches
	plan.Batches = make([]ScheduleBatch, len(batches))
	for index, batch := range batches {
		plan.Batches[index] = batch.Clone()
	}
	return plan
}

// CompileDependencyExecutionPlan freezes the default initial scheduling
// policy: every automatically dispatchable stage at the same DAG dependency
// level is placed in one batch. Operator-only stages remain in the frozen DAG
// as readiness contracts, but never become durable worker jobs.
func CompileDependencyExecutionPlan(workflow WorkflowDescriptor) (ExecutionPlan, error) {
	if err := workflow.Validate(); err != nil {
		return ExecutionPlan{}, fmt.Errorf("%w: workflow: %v", ErrInvalidExecution, err)
	}
	levels := make(map[StageKey]int, len(workflow.Stages))
	for _, key := range mustTopologicalStages(workflow) {
		stage, _ := workflow.Stage(key)
		level := 0
		for _, dependency := range stage.Dependencies {
			if dependencyLevel := levels[dependency] + 1; dependencyLevel > level {
				level = dependencyLevel
			}
		}
		levels[key] = level
	}
	maxLevel := 0
	for _, level := range levels {
		if level > maxLevel {
			maxLevel = level
		}
	}
	batches := make([]ScheduleBatch, 0, maxLevel+1)
	for level := 0; level <= maxLevel; level++ {
		batch := ScheduleBatch{ID: fmt.Sprintf("dependency-level-%03d", level+1)}
		for _, stage := range workflow.Stages {
			if levels[stage.Key] == level && stage.AutomaticallyDispatchable() {
				batch.NodeIDs = append(batch.NodeIDs, NodeID(stage.Key))
			}
		}
		if len(batch.NodeIDs) != 0 {
			batches = append(batches, batch)
		}
	}
	return FreezeExecutionPlan(workflow, batches)
}

// FreezeExecutionPlan validates a caller-supplied batch partition and returns
// a deep-copied, fingerprinted immutable plan. It is useful for a continuation
// whose planner has already selected a narrower frozen schedule.
func FreezeExecutionPlan(workflow WorkflowDescriptor, batches []ScheduleBatch) (ExecutionPlan, error) {
	plan := ExecutionPlan{Batches: make([]ScheduleBatch, len(batches))}
	for index, batch := range batches {
		plan.Batches[index] = batch.Clone()
	}
	if err := plan.validate(workflow); err != nil {
		return ExecutionPlan{}, err
	}
	fingerprint, err := fingerprintExecutionPlan(plan.Batches)
	if err != nil {
		return ExecutionPlan{}, err
	}
	plan.Fingerprint = fingerprint
	return plan, nil
}

// Validate verifies that a frozen execution plan remains a complete partition
// of every automatically dispatchable stage in the supplied frozen workflow.
// Operator-only stages must remain absent from the plan.
func (plan ExecutionPlan) Validate(workflow WorkflowDescriptor) error {
	if err := plan.validate(workflow); err != nil {
		return err
	}
	if err := plan.Fingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: execution plan fingerprint: %v", ErrInvalidExecution, err)
	}
	expected, err := fingerprintExecutionPlan(plan.Batches)
	if err != nil {
		return err
	}
	if expected != plan.Fingerprint {
		return fmt.Errorf("%w: execution plan fingerprint does not match batches", ErrInvalidExecution)
	}
	return nil
}

func (plan ExecutionPlan) validate(workflow WorkflowDescriptor) error {
	if err := workflow.Validate(); err != nil {
		return fmt.Errorf("%w: workflow: %v", ErrInvalidExecution, err)
	}
	automaticStages := 0
	for _, stage := range workflow.Stages {
		if stage.AutomaticallyDispatchable() {
			automaticStages++
		}
	}
	if len(plan.Batches) == 0 && automaticStages != 0 {
		return fmt.Errorf("%w: execution plan requires at least one batch", ErrInvalidExecution)
	}
	stageBatch := make(map[StageKey]int, len(workflow.Stages))
	batchIDs := make(map[string]struct{}, len(plan.Batches))
	for index, batch := range plan.Batches {
		if err := validateRequired("execution batch id", batch.ID, ErrInvalidExecution); err != nil {
			return err
		}
		if _, exists := batchIDs[batch.ID]; exists {
			return fmt.Errorf("%w: duplicate execution batch id %q", ErrInvalidExecution, batch.ID)
		}
		batchIDs[batch.ID] = struct{}{}
		if len(batch.NodeIDs) == 0 {
			return fmt.Errorf("%w: execution batch %q has no stages", ErrInvalidExecution, batch.ID)
		}
		for _, nodeID := range batch.NodeIDs {
			key := StageKey(nodeID)
			if _, exists := stageBatch[key]; exists {
				return fmt.Errorf("%w: stage %q appears in multiple execution batches", ErrInvalidExecution, key)
			}
			stage, found := workflow.Stage(key)
			if !found {
				return fmt.Errorf("%w: execution plan references unknown stage %q", ErrInvalidExecution, key)
			}
			if !stage.AutomaticallyDispatchable() {
				return fmt.Errorf("%w: execution plan schedules operator-only stage %q", ErrInvalidExecution, key)
			}
			stageBatch[key] = index
		}
	}
	if len(stageBatch) != automaticStages {
		return fmt.Errorf("%w: execution plan must include every automatically dispatchable workflow stage", ErrInvalidExecution)
	}
	for _, stage := range workflow.Stages {
		if !stage.AutomaticallyDispatchable() {
			continue
		}
		for _, dependency := range stage.Dependencies {
			if stageBatch[dependency] >= stageBatch[stage.Key] {
				return fmt.Errorf("%w: stage %q dependency %q is not in an earlier batch", ErrInvalidExecution, stage.Key, dependency)
			}
		}
	}
	return nil
}

func fingerprintExecutionPlan(batches []ScheduleBatch) (Fingerprint, error) {
	canonical := make([]ScheduleBatch, len(batches))
	for index, batch := range batches {
		canonical[index] = batch.Clone()
		sort.Slice(canonical[index].NodeIDs, func(left, right int) bool {
			return canonical[index].NodeIDs[left] < canonical[index].NodeIDs[right]
		})
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode execution plan: %v", ErrInvalidExecution, err)
	}
	return FingerprintBytes("workflowkit.execution-plan.v1", encoded)
}

func mustTopologicalStages(workflow WorkflowDescriptor) []StageKey {
	stages, err := workflow.TopologicalStages()
	if err != nil {
		panic(fmt.Sprintf("validated workflow has no topological ordering: %v", err))
	}
	return stages
}

// PrepareRequest contains all immutable inputs that must be persisted before
// a durable coordinator job is visible. The Engine never reads a mutable
// profile, workspace, or domain document after this boundary.
type PrepareRequest struct {
	ExecutionID        string                 `json:"execution_id"`
	IdempotencyKey     string                 `json:"idempotency_key"`
	Subject            SubjectBinding         `json:"subject"`
	Workflow           WorkflowDescriptor     `json:"workflow"`
	ProfileFingerprint Fingerprint            `json:"profile_fingerprint"`
	Binding            OpaqueExecutionBinding `json:"binding"`
	Plan               *ExecutionPlan         `json:"plan,omitempty"`
	Actor              string                 `json:"actor"`
	Reason             string                 `json:"reason"`
}

// Clone returns independently owned request input.
func (request PrepareRequest) Clone() PrepareRequest {
	request.Workflow = request.Workflow.Clone()
	request.Binding = request.Binding.Clone()
	if request.Plan != nil {
		plan := request.Plan.Clone()
		request.Plan = &plan
	}
	return request
}

func (request PrepareRequest) validate() error {
	if err := validateRequired("execution id", request.ExecutionID, ErrInvalidExecution); err != nil {
		return err
	}
	if err := validateRequired("execution idempotency key", request.IdempotencyKey, ErrInvalidExecution); err != nil {
		return err
	}
	if err := request.Subject.Validate(); err != nil {
		return err
	}
	if err := request.Workflow.Validate(); err != nil {
		return fmt.Errorf("%w: workflow: %v", ErrInvalidExecution, err)
	}
	if err := request.ProfileFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: profile fingerprint: %v", ErrInvalidExecution, err)
	}
	if err := request.Binding.Validate(); err != nil {
		return err
	}
	if request.Plan != nil {
		if err := request.Plan.Validate(request.Workflow); err != nil {
			return err
		}
	}
	if err := validateRequired("execution actor", request.Actor, ErrInvalidExecution); err != nil {
		return err
	}
	return validateRequired("execution reason", request.Reason, ErrInvalidExecution)
}

// FrozenExecution is the durable, immutable execution identity passed to all
// jobs. It contains only generic workflow data and opaque domain bytes.
type FrozenExecution struct {
	ID                    string                 `json:"id"`
	IdempotencyKey        string                 `json:"idempotency_key"`
	Subject               SubjectBinding         `json:"subject"`
	Workflow              WorkflowDescriptor     `json:"workflow"`
	DefinitionFingerprint Fingerprint            `json:"definition_fingerprint"`
	ProfileFingerprint    Fingerprint            `json:"profile_fingerprint"`
	Binding               OpaqueExecutionBinding `json:"binding"`
	Plan                  ExecutionPlan          `json:"plan"`
	Actor                 string                 `json:"actor"`
	Reason                string                 `json:"reason"`
	CreatedAt             time.Time              `json:"created_at"`
}

// Clone returns independently owned immutable execution data.
func (execution FrozenExecution) Clone() FrozenExecution {
	execution.Workflow = execution.Workflow.Clone()
	execution.Binding = execution.Binding.Clone()
	execution.Plan = execution.Plan.Clone()
	return execution
}

// Validate verifies that all execution data remains tied to the frozen
// workflow definition and domain binding.
func (execution FrozenExecution) Validate() error {
	if err := validateRequired("execution id", execution.ID, ErrInvalidExecution); err != nil {
		return err
	}
	if err := validateRequired("execution idempotency key", execution.IdempotencyKey, ErrInvalidExecution); err != nil {
		return err
	}
	if err := execution.Subject.Validate(); err != nil {
		return err
	}
	if err := execution.Workflow.Validate(); err != nil {
		return fmt.Errorf("%w: workflow: %v", ErrInvalidExecution, err)
	}
	definitionFingerprint, err := execution.Workflow.Fingerprint()
	if err != nil {
		return err
	}
	if execution.DefinitionFingerprint != definitionFingerprint {
		return fmt.Errorf("%w: definition fingerprint does not match workflow", ErrInvalidExecution)
	}
	if err := execution.ProfileFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: profile fingerprint: %v", ErrInvalidExecution, err)
	}
	if err := execution.Binding.Validate(); err != nil {
		return err
	}
	if err := execution.Plan.Validate(execution.Workflow); err != nil {
		return err
	}
	if err := validateRequired("execution actor", execution.Actor, ErrInvalidExecution); err != nil {
		return err
	}
	if err := validateRequired("execution reason", execution.Reason, ErrInvalidExecution); err != nil {
		return err
	}
	if execution.CreatedAt.IsZero() {
		return fmt.Errorf("%w: execution creation time is required", ErrInvalidExecution)
	}
	return nil
}

func freezeExecution(request PrepareRequest, now time.Time) (FrozenExecution, error) {
	if err := request.validate(); err != nil {
		return FrozenExecution{}, err
	}
	plan := ExecutionPlan{}
	var err error
	if request.Plan == nil {
		plan, err = CompileDependencyExecutionPlan(request.Workflow)
	} else {
		plan = request.Plan.Clone()
	}
	if err != nil {
		return FrozenExecution{}, err
	}
	definitionFingerprint, err := request.Workflow.Fingerprint()
	if err != nil {
		return FrozenExecution{}, err
	}
	execution := FrozenExecution{
		ID:                    request.ExecutionID,
		IdempotencyKey:        request.IdempotencyKey,
		Subject:               request.Subject,
		Workflow:              request.Workflow.Clone(),
		DefinitionFingerprint: definitionFingerprint,
		ProfileFingerprint:    request.ProfileFingerprint,
		Binding:               request.Binding.Clone(),
		Plan:                  plan,
		Actor:                 request.Actor,
		Reason:                request.Reason,
		CreatedAt:             now.UTC(),
	}
	if err := execution.Validate(); err != nil {
		return FrozenExecution{}, err
	}
	return execution, nil
}

// PreparedExecution is returned only after a backend atomically persists the
// frozen execution, its first coordinator job, and the matching audit/outbox
// facts.
type PreparedExecution struct {
	Execution        FrozenExecution `json:"execution"`
	CoordinatorJobID string          `json:"coordinator_job_id"`
}

func (prepared PreparedExecution) validateAgainst(execution FrozenExecution) error {
	if err := prepared.Execution.Validate(); err != nil {
		return err
	}
	if prepared.Execution.ID != execution.ID || prepared.Execution.IdempotencyKey != execution.IdempotencyKey ||
		prepared.Execution.DefinitionFingerprint != execution.DefinitionFingerprint ||
		prepared.Execution.ProfileFingerprint != execution.ProfileFingerprint ||
		prepared.Execution.Binding.Fingerprint != execution.Binding.Fingerprint ||
		prepared.Execution.Subject != execution.Subject || prepared.Execution.Plan.Fingerprint != execution.Plan.Fingerprint {
		return fmt.Errorf("%w: durable backend returned a different frozen execution", ErrInvalidExecution)
	}
	return validateRequired("coordinator job id", prepared.CoordinatorJobID, ErrInvalidExecution)
}

// JobKind distinguishes generic coordinator and concrete stage work. A
// backend owns durable claiming and lease fencing; the Engine owns immutable
// binding and executor validation once a claim has been supplied.
type JobKind string

const (
	JobCoordinator JobKind = "coordinator"
	JobStage       JobKind = "stage"
)

func (kind JobKind) valid() bool { return kind == JobCoordinator || kind == JobStage }

// StageControlSignal delivers at most one target-scoped durable control intent
// to an active executor. It cannot represent a UI or scheduler-root context.
type StageControlSignal struct {
	Action      ControlAction `json:"action"`
	GracePeriod time.Duration `json:"grace_period"`
}

func (signal StageControlSignal) validate() error {
	if !signal.Action.valid() {
		return fmt.Errorf("%w: unsupported stage control action %q", ErrInvalidJobClaim, signal.Action)
	}
	if signal.GracePeriod < 0 {
		return fmt.Errorf("%w: stage control grace period cannot be negative", ErrInvalidJobClaim)
	}
	return nil
}

// StageClaim is the immutable stage work attached to one leased job. Inputs
// are already resolved immutable artifact bindings; a backend must not reopen
// a mutable workspace or recompute them after the claim is visible.
type StageClaim struct {
	StageAttempt AttemptIdentity           `json:"stage_attempt"`
	Stage        StageDescriptor           `json:"stage"`
	Generation   int                       `json:"generation"`
	Inputs       []ArtifactBinding         `json:"inputs"`
	Control      <-chan StageControlSignal `json:"-"`
}

// Clone returns independently owned stage work. Control is intentionally a
// runtime-only channel and remains the same scoped signal source.
func (claim StageClaim) Clone() StageClaim {
	claim.Stage = claim.Stage.Clone()
	claim.Inputs = append([]ArtifactBinding(nil), claim.Inputs...)
	return claim
}

// JobClaim is the public, durable worker boundary. A job may be handled only
// while its fence is live; callers cannot substitute a stage descriptor or an
// execution binding after a job is claimed.
type JobClaim struct {
	JobID          string          `json:"job_id"`
	ClaimID        string          `json:"claim_id"`
	Kind           JobKind         `json:"kind"`
	Owner          string          `json:"owner"`
	FencingToken   uint64          `json:"fencing_token"`
	LeaseExpiresAt time.Time       `json:"lease_expires_at"`
	Execution      FrozenExecution `json:"execution"`
	Stage          *StageClaim     `json:"stage,omitempty"`
}

// Clone returns independently owned job claim data.
func (claim JobClaim) Clone() JobClaim {
	claim.Execution = claim.Execution.Clone()
	if claim.Stage != nil {
		stage := claim.Stage.Clone()
		claim.Stage = &stage
	}
	return claim
}

// Validate verifies claim-local integrity. The clock-sensitive lease check is
// intentionally separate so persisted claims can still be inspected after
// expiry.
func (claim JobClaim) Validate() error {
	if err := validateRequired("job id", claim.JobID, ErrInvalidJobClaim); err != nil {
		return err
	}
	if err := validateRequired("job claim id", claim.ClaimID, ErrInvalidJobClaim); err != nil {
		return err
	}
	if !claim.Kind.valid() {
		return fmt.Errorf("%w: unsupported job kind %q", ErrInvalidJobClaim, claim.Kind)
	}
	if err := validateRequired("job owner", claim.Owner, ErrInvalidJobClaim); err != nil {
		return err
	}
	if claim.FencingToken == 0 {
		return fmt.Errorf("%w: job fencing token is required", ErrInvalidJobClaim)
	}
	if claim.LeaseExpiresAt.IsZero() {
		return fmt.Errorf("%w: job lease expiry is required", ErrInvalidJobClaim)
	}
	if err := claim.Execution.Validate(); err != nil {
		return err
	}
	switch claim.Kind {
	case JobCoordinator:
		if claim.Stage != nil {
			return fmt.Errorf("%w: coordinator job cannot include stage work", ErrInvalidJobClaim)
		}
	case JobStage:
		if claim.Stage == nil {
			return fmt.Errorf("%w: stage job requires stage work", ErrInvalidJobClaim)
		}
		if err := claim.validateStage(); err != nil {
			return err
		}
	}
	return nil
}

func (claim JobClaim) validateStage() error {
	stageClaim := claim.Stage
	if stageClaim.Generation < 0 {
		return fmt.Errorf("%w: stage generation cannot be negative", ErrInvalidJobClaim)
	}
	if err := stageClaim.StageAttempt.validate(); err != nil {
		return err
	}
	if stageClaim.StageAttempt.Kind != AttemptStage {
		return fmt.Errorf("%w: stage claim attempt kind must be stage", ErrInvalidJobClaim)
	}
	if err := stageClaim.Stage.Validate(); err != nil {
		return err
	}
	frozen, found := claim.Execution.Workflow.Stage(stageClaim.Stage.Key)
	if !found {
		return fmt.Errorf("%w: stage %q is not in frozen workflow", ErrInvalidJobClaim, stageClaim.Stage.Key)
	}
	if !reflect.DeepEqual(frozen, stageClaim.Stage) {
		return fmt.Errorf("%w: stage %q does not match frozen descriptor", ErrInvalidJobClaim, stageClaim.Stage.Key)
	}
	return validateClaimInputs(stageClaim.Stage, stageClaim.Inputs)
}

func validateClaimInputs(stage StageDescriptor, bindings []ArtifactBinding) error {
	specifications := make(map[string]ArtifactSpec, len(stage.Inputs))
	for _, specification := range stage.Inputs {
		specifications[specification.Name] = specification
	}
	seen := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if err := binding.Validate(); err != nil {
			return err
		}
		specification, found := specifications[binding.Name]
		if !found {
			return fmt.Errorf("%w: stage %q received undeclared input %q", ErrInvalidJobClaim, stage.Key, binding.Name)
		}
		if specification.SchemaVersion != binding.SchemaVersion {
			return fmt.Errorf("%w: stage %q input %q schema %q, want %q", ErrInvalidJobClaim, stage.Key, binding.Name, binding.SchemaVersion, specification.SchemaVersion)
		}
		if _, duplicate := seen[binding.Name]; duplicate {
			return fmt.Errorf("%w: stage %q received duplicate input %q", ErrInvalidJobClaim, stage.Key, binding.Name)
		}
		seen[binding.Name] = struct{}{}
	}
	for _, specification := range stage.Inputs {
		if specification.Required {
			if _, found := seen[specification.Name]; !found {
				return fmt.Errorf("%w: stage %q omitted required input %q", ErrInvalidJobClaim, stage.Key, specification.Name)
			}
		}
	}
	return nil
}

// StageCheckpoint is a durable executor substep. The Engine supplies its
// execution/stage/attempt scope to the backend, preventing a plugin from
// checkpointing another stage by construction.
type StageCheckpoint struct {
	CheckpointID   string     `json:"checkpoint_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	TurnOrdinal    int        `json:"turn_ordinal"`
	Substep        string     `json:"substep"`
	Payload        []byte     `json:"payload,omitempty"`
	ArtifactID     ArtifactID `json:"artifact_id,omitempty"`
	Resumable      bool       `json:"resumable"`
	OccurredAt     time.Time  `json:"occurred_at"`
}

// Clone returns independently owned checkpoint payload bytes.
func (checkpoint StageCheckpoint) Clone() StageCheckpoint {
	checkpoint.Payload = append([]byte(nil), checkpoint.Payload...)
	return checkpoint
}

func (checkpoint StageCheckpoint) validate() error {
	if err := validateRequired("stage checkpoint id", checkpoint.CheckpointID, ErrInvalidStageResult); err != nil {
		return err
	}
	if err := validateRequired("stage checkpoint idempotency key", checkpoint.IdempotencyKey, ErrInvalidStageResult); err != nil {
		return err
	}
	if checkpoint.TurnOrdinal < 0 {
		return fmt.Errorf("%w: stage checkpoint turn ordinal cannot be negative", ErrInvalidStageResult)
	}
	if err := validateRequired("stage checkpoint substep", checkpoint.Substep, ErrInvalidStageResult); err != nil {
		return err
	}
	if checkpoint.OccurredAt.IsZero() {
		return fmt.Errorf("%w: stage checkpoint time is required", ErrInvalidStageResult)
	}
	return nil
}

// CheckpointReceipt is the backend's durable acknowledgement of a checkpoint.
type CheckpointReceipt struct {
	CheckpointID string `json:"checkpoint_id"`
}

// StageUsage is one known billable event. OperationKey is durable and unique
// within a backend's quota ledger, so worker replay cannot double-charge it.
type StageUsage struct {
	OperationKey string    `json:"operation_key"`
	Dimension    string    `json:"dimension"`
	Units        int64     `json:"units"`
	OccurredAt   time.Time `json:"occurred_at"`
}

func (usage StageUsage) validate() error {
	if err := validateRequired("stage usage operation key", usage.OperationKey, ErrInvalidStageResult); err != nil {
		return err
	}
	if err := validateRequired("stage usage dimension", usage.Dimension, ErrInvalidStageResult); err != nil {
		return err
	}
	if usage.Units <= 0 {
		return fmt.Errorf("%w: stage usage units must be positive", ErrInvalidStageResult)
	}
	if usage.OccurredAt.IsZero() {
		return fmt.Errorf("%w: stage usage time is required", ErrInvalidStageResult)
	}
	return nil
}

// StageArtifact holds immutable output bytes until the durable backend stores
// them and records full lineage. Returning declared diagnostic artifacts with
// an infrastructure error is valid and intentionally preserves failure
// evidence.
type StageArtifact struct {
	Name          string `json:"name"`
	SchemaVersion string `json:"schema_version"`
	Content       []byte `json:"content"`
	TurnOrdinal   int    `json:"turn_ordinal"`
}

// StageWaitKind identifies a nonterminal executor handoff. The kernel has one
// generic external-decision kind; a domain adapter owns the versioned opaque
// binding that explains what decision is required and how it resumes.
type StageWaitKind string

const (
	StageWaitExternalDecision StageWaitKind = "external_decision"
)

func (kind StageWaitKind) valid() bool { return kind == StageWaitExternalDecision }

// StageWait is an explicit durable handoff rather than a manufactured
// terminal outcome. The opaque binding is immutable and typed by format and
// version, so the Engine does not learn domain-specific gate vocabulary.
type StageWait struct {
	Kind            StageWaitKind          `json:"kind"`
	OperationKey    string                 `json:"operation_key"`
	DecisionBinding OpaqueExecutionBinding `json:"decision_binding"`
}

// Clone returns independently owned opaque wait data.
func (wait StageWait) Clone() StageWait {
	wait.DecisionBinding = wait.DecisionBinding.Clone()
	return wait
}

func (wait StageWait) validate() error {
	if !wait.Kind.valid() {
		return fmt.Errorf("%w: unsupported stage wait kind %q", ErrInvalidStageResult, wait.Kind)
	}
	if err := validateRequired("stage wait operation key", wait.OperationKey, ErrInvalidStageResult); err != nil {
		return err
	}
	return wait.DecisionBinding.Validate()
}

// Clone returns independently owned artifact content.
func (artifact StageArtifact) Clone() StageArtifact {
	artifact.Content = append([]byte(nil), artifact.Content...)
	return artifact
}

func (artifact StageArtifact) validate() error {
	if err := validateRequired("stage artifact name", artifact.Name, ErrInvalidStageResult); err != nil {
		return err
	}
	if err := validateRequired("stage artifact schema version", artifact.SchemaVersion, ErrInvalidStageResult); err != nil {
		return err
	}
	if artifact.TurnOrdinal < 0 {
		return fmt.Errorf("%w: stage artifact turn ordinal cannot be negative", ErrInvalidStageResult)
	}
	return nil
}

// StageExecutionResult is the terminal result returned by a public
// StageExecutor. A completed result must carry an allowed verdict and every
// required output. Failed results may retain declared diagnostic artifacts.
type StageExecutionResult struct {
	Outcome   Outcome         `json:"outcome"`
	Wait      *StageWait      `json:"wait,omitempty"`
	Artifacts []StageArtifact `json:"artifacts,omitempty"`
	ErrorText string          `json:"error_text,omitempty"`
}

// Clone returns independently owned output content.
func (result StageExecutionResult) Clone() StageExecutionResult {
	if result.Wait != nil {
		wait := result.Wait.Clone()
		result.Wait = &wait
	}
	artifacts := result.Artifacts
	result.Artifacts = make([]StageArtifact, len(artifacts))
	for index, artifact := range artifacts {
		result.Artifacts[index] = artifact.Clone()
	}
	return result
}

func (result StageExecutionResult) validate(stage StageDescriptor) error {
	if result.Wait != nil {
		if err := result.Wait.validate(); err != nil {
			return err
		}
		if !stage.Capabilities.Has(CapabilityApprove) {
			return fmt.Errorf("%w: stage %q cannot enter an external decision wait without %q capability", ErrInvalidStageResult, stage.Key, CapabilityApprove)
		}
		if result.Outcome.Status != "" || len(result.Artifacts) != 0 || strings.TrimSpace(result.ErrorText) != "" {
			return fmt.Errorf("%w: nonterminal stage wait cannot carry outcome, artifacts, or error text", ErrInvalidStageResult)
		}
		return nil
	}
	if err := result.Outcome.Validate(); err != nil {
		return fmt.Errorf("%w: outcome: %v", ErrInvalidStageResult, err)
	}
	if result.Outcome.Status == StatusCompleted && !stage.Verdicts.Allows(result.Outcome.Verdict) {
		return fmt.Errorf("%w: stage %q does not allow verdict %q", ErrInvalidStageResult, stage.Key, result.Outcome.Verdict)
	}
	outputs := make(map[string]ArtifactSpec, len(stage.Outputs))
	for _, output := range stage.Outputs {
		outputs[output.Name] = output
	}
	seen := make(map[string]struct{}, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		if err := artifact.validate(); err != nil {
			return err
		}
		specification, found := outputs[artifact.Name]
		if !found {
			return fmt.Errorf("%w: stage %q returned undeclared output %q", ErrInvalidStageResult, stage.Key, artifact.Name)
		}
		if artifact.SchemaVersion != specification.SchemaVersion {
			return fmt.Errorf("%w: stage %q output %q schema %q, want %q", ErrInvalidStageResult, stage.Key, artifact.Name, artifact.SchemaVersion, specification.SchemaVersion)
		}
		if _, duplicate := seen[artifact.Name]; duplicate {
			return fmt.Errorf("%w: stage %q returned duplicate output %q", ErrInvalidStageResult, stage.Key, artifact.Name)
		}
		seen[artifact.Name] = struct{}{}
	}
	if result.Outcome.Status == StatusCompleted {
		for _, output := range stage.Outputs {
			if output.Required {
				if _, found := seen[output.Name]; !found {
					return fmt.Errorf("%w: completed stage %q omitted required output %q", ErrInvalidStageResult, stage.Key, output.Name)
				}
			}
		}
	}
	return nil
}

// StageExecutionRequest is the generic executor contract. It exposes only
// frozen descriptors and verified input reads; persistence and quota
// callbacks are scoped to the claimed stage attempt.
type StageExecutionRequest struct {
	Execution  FrozenExecution
	Claim      JobClaim
	Stage      StageDescriptor
	Inputs     []ArtifactBinding
	ReadInput  func(context.Context, ArtifactBinding) ([]byte, error)
	Checkpoint func(context.Context, StageCheckpoint) (CheckpointReceipt, error)
	Charge     func(context.Context, StageUsage) error
	Control    <-chan StageControlSignal
}

func (request StageExecutionRequest) clone() StageExecutionRequest {
	request.Execution = request.Execution.Clone()
	request.Claim = request.Claim.Clone()
	request.Stage = request.Stage.Clone()
	request.Inputs = append([]ArtifactBinding(nil), request.Inputs...)
	return request
}

// StageExecutor is implemented by a controlled domain plugin. The Engine
// resolves it only by the exact plugin ID/version frozen in StageDescriptor.
type StageExecutor interface {
	ExecuteStage(context.Context, StageExecutionRequest) (StageExecutionResult, error)
}

// StageExecutorFunc adapts a function into a StageExecutor.
type StageExecutorFunc func(context.Context, StageExecutionRequest) (StageExecutionResult, error)

// ExecuteStage invokes the function with a defensive copy of frozen input.
func (function StageExecutorFunc) ExecuteStage(ctx context.Context, request StageExecutionRequest) (StageExecutionResult, error) {
	return function(ctx, request.clone())
}

// JobTerminalState is the semantic delivery result returned to a durable
// worker. Backend-specific queue states remain behind DurableBackend.
type JobTerminalState string

const (
	JobCompleted         JobTerminalState = "completed"
	JobRetryScheduled    JobTerminalState = "retry_scheduled"
	JobReconcileRequired JobTerminalState = "reconcile_required"
)

func (state JobTerminalState) valid() bool {
	return state == JobCompleted || state == JobRetryScheduled || state == JobReconcileRequired
}

// StageCompletion is committed atomically by a durable backend. The backend
// must publish artifacts, append attempt facts, settle quota/control state,
// and make any next coordinator job visible in the same semantic transaction.
type StageCompletion struct {
	Claim       JobClaim             `json:"claim"`
	Result      StageExecutionResult `json:"result"`
	CompletedAt time.Time            `json:"completed_at"`
}

// Clone returns independently owned completion data.
func (completion StageCompletion) Clone() StageCompletion {
	completion.Claim = completion.Claim.Clone()
	completion.Result = completion.Result.Clone()
	return completion
}

// StageWaitCommit is the durable nonterminal projection emitted by an
// executor. A backend must atomically mark the stage waiting, persist the
// domain wait binding, and create any domain-owned decision work without
// manufacturing a terminal outcome or scheduling successors.
type StageWaitCommit struct {
	Claim      JobClaim  `json:"claim"`
	Wait       StageWait `json:"wait"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Clone returns independently owned wait commit data.
func (commit StageWaitCommit) Clone() StageWaitCommit {
	commit.Claim = commit.Claim.Clone()
	commit.Wait = commit.Wait.Clone()
	return commit
}

// RecoveryScope selects durable recovery subjects without exposing a
// persistence implementation. An empty execution ID means the backend's
// complete local recovery scope.
type RecoveryScope struct {
	ExecutionID string    `json:"execution_id,omitempty"`
	ObservedAt  time.Time `json:"observed_at"`
}

func (scope RecoveryScope) validate() error {
	if scope.ObservedAt.IsZero() {
		return fmt.Errorf("%w: recovery observation time is required", ErrInvalidExecution)
	}
	return nil
}

// DurableBackend is the public semantic persistence port. It deliberately
// models complete durable operations rather than leaking a particular SQL
// schema, scheduler row, filesystem layout, or product-domain type.
type DurableBackend interface {
	PrepareExecution(context.Context, PrepareRequest, FrozenExecution) (PreparedExecution, error)
	AdvanceCoordinator(context.Context, JobClaim) (JobTerminalState, error)
	ReadStageInput(context.Context, JobClaim, ArtifactBinding) ([]byte, error)
	RecordStageCheckpoint(context.Context, JobClaim, StageCheckpoint) (CheckpointReceipt, error)
	RecordStageUsage(context.Context, JobClaim, StageUsage) error
	CommitStage(context.Context, StageCompletion) (JobTerminalState, error)
	CommitStageWait(context.Context, StageWaitCommit) (JobTerminalState, error)
	RejectStageClaim(context.Context, JobClaim, error) (JobTerminalState, error)
	ListRecoverySubjects(context.Context, RecoveryScope) ([]RecoverySubject, error)
	ApplyRecovery(context.Context, RecoveryScope, []RecoveryDecision) error
}

// FailureClassifier maps a plugin error to a generic infrastructure class.
// Domain adapters may supply a richer classifier; the default is conservative.
type FailureClassifier interface {
	ClassifyFailure(error) FailureClass
}

// FailureClassifierFunc adapts a function into FailureClassifier.
type FailureClassifierFunc func(error) FailureClass

// ClassifyFailure implements FailureClassifier.
func (function FailureClassifierFunc) ClassifyFailure(err error) FailureClass { return function(err) }

// EngineConfig supplies the only mutable collaborators of a public Engine.
type EngineConfig struct {
	Backend    DurableBackend
	Executors  PluginResolver[StageExecutor]
	Classifier FailureClassifier
	Now        func() time.Time
}

// Engine is the public, domain-neutral durable workflow executor. It freezes
// executions, validates claims, resolves exact plugin bindings, preserves
// failed evidence, and delegates atomic durable state changes through ports.
type Engine struct {
	backend    DurableBackend
	executors  PluginResolver[StageExecutor]
	classifier FailureClassifier
	now        func() time.Time
}

// NewEngine creates a reusable Engine with no product persistence, CLI, or UI
// dependency.
func NewEngine(config EngineConfig) (*Engine, error) {
	if config.Backend == nil || config.Executors == nil {
		return nil, fmt.Errorf("%w: durable backend and exact plugin resolver are required", ErrInvalidEngineConfiguration)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Classifier == nil {
		config.Classifier = FailureClassifierFunc(defaultFailureClass)
	}
	return &Engine{backend: config.Backend, executors: config.Executors, classifier: config.Classifier, now: config.Now}, nil
}

// Prepare freezes and asks the backend to atomically persist an execution and
// initial coordinator job. Idempotent backend replays must return the exact
// same frozen execution, never re-read mutable caller input.
func (engine *Engine) Prepare(ctx context.Context, request PrepareRequest) (PreparedExecution, error) {
	if err := engine.validate(); err != nil {
		return PreparedExecution{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request = request.Clone()
	execution, err := freezeExecution(request, engine.now())
	if err != nil {
		return PreparedExecution{}, err
	}
	prepared, err := engine.backend.PrepareExecution(ctx, request, execution.Clone())
	if err != nil {
		return PreparedExecution{}, err
	}
	if err := prepared.validateAgainst(execution); err != nil {
		return PreparedExecution{}, err
	}
	prepared.Execution = prepared.Execution.Clone()
	return prepared, nil
}

// HandleClaim executes one already-leased durable job. Coordinator scheduling
// is backend-owned; concrete stage execution is Engine-owned and always uses
// the exact frozen plugin binding.
func (engine *Engine) HandleClaim(ctx context.Context, claim JobClaim) (JobTerminalState, error) {
	if err := engine.validate(); err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	claim = claim.Clone()
	if err := claim.Validate(); err != nil {
		return "", err
	}
	if !claim.LeaseExpiresAt.After(engine.now()) {
		return "", ErrExpiredJobClaim
	}
	if claim.Kind == JobCoordinator {
		state, err := engine.backend.AdvanceCoordinator(ctx, claim)
		if err != nil {
			return "", err
		}
		if !state.valid() {
			return "", fmt.Errorf("%w: backend returned invalid coordinator terminal state %q", ErrInvalidJobClaim, state)
		}
		return state, nil
	}
	return engine.handleStageClaim(ctx, claim)
}

func (engine *Engine) handleStageClaim(ctx context.Context, claim JobClaim) (JobTerminalState, error) {
	stageClaim := claim.Stage.Clone()
	if !stageClaim.Stage.AutomaticallyDispatchable() {
		return engine.rejectStageClaim(ctx, claim, fmt.Errorf("%w: operator-only stage %q cannot execute from a generic worker claim", ErrInvalidJobClaim, stageClaim.Stage.Key))
	}
	executor, err := engine.executors.ResolvePlugin(stageClaim.Stage.Plugin)
	if err != nil {
		return engine.rejectStageClaim(ctx, claim, err)
	}
	request := StageExecutionRequest{
		Execution: claim.Execution.Clone(),
		Claim:     claim.Clone(),
		Stage:     stageClaim.Stage.Clone(),
		Inputs:    append([]ArtifactBinding(nil), stageClaim.Inputs...),
		Control:   stageClaim.Control,
	}
	request.ReadInput = func(callCtx context.Context, binding ArtifactBinding) ([]byte, error) {
		if err := inputBelongsToClaim(stageClaim.Inputs, binding); err != nil {
			return nil, err
		}
		return engine.backend.ReadStageInput(callCtx, claim, binding)
	}
	request.Checkpoint = func(callCtx context.Context, checkpoint StageCheckpoint) (CheckpointReceipt, error) {
		if err := checkpoint.validate(); err != nil {
			return CheckpointReceipt{}, err
		}
		return engine.backend.RecordStageCheckpoint(callCtx, claim, checkpoint.Clone())
	}
	request.Charge = func(callCtx context.Context, usage StageUsage) error {
		if err := usage.validate(); err != nil {
			return err
		}
		return engine.backend.RecordStageUsage(callCtx, claim, usage)
	}
	result, executionErr := executor.ExecuteStage(ctx, request.clone())
	result = result.Clone()
	if executionErr != nil {
		if result.Wait != nil {
			return engine.rejectStageClaim(ctx, claim, fmt.Errorf("%w: executor returned both a wait and an error: %v", ErrInvalidStageResult, executionErr))
		}
		result.Outcome = Outcome{Status: StatusInfraFailed, Failure: engine.classifyFailure(executionErr)}
		if strings.TrimSpace(result.ErrorText) == "" {
			result.ErrorText = executionErr.Error()
		}
	}
	if err := result.validate(stageClaim.Stage); err != nil {
		return engine.rejectStageClaim(ctx, claim, err)
	}
	if result.Wait != nil {
		state, err := engine.backend.CommitStageWait(ctx, StageWaitCommit{Claim: claim.Clone(), Wait: result.Wait.Clone(), OccurredAt: engine.now().UTC()})
		if err != nil {
			return "", err
		}
		if !state.valid() {
			return "", fmt.Errorf("%w: backend returned invalid stage-wait terminal state %q", ErrInvalidJobClaim, state)
		}
		return state, nil
	}
	completion := StageCompletion{Claim: claim.Clone(), Result: result, CompletedAt: engine.now().UTC()}
	state, err := engine.backend.CommitStage(ctx, completion)
	if err != nil {
		return "", err
	}
	if !state.valid() {
		return "", fmt.Errorf("%w: backend returned invalid stage terminal state %q", ErrInvalidJobClaim, state)
	}
	return state, nil
}

func (engine *Engine) rejectStageClaim(ctx context.Context, claim JobClaim, cause error) (JobTerminalState, error) {
	state, err := engine.backend.RejectStageClaim(ctx, claim, cause)
	if err != nil {
		return "", err
	}
	if !state.valid() {
		return "", fmt.Errorf("%w: backend returned invalid rejected-claim terminal state %q", ErrInvalidJobClaim, state)
	}
	return state, nil
}

// Reconcile applies only decisions derived from persisted recovery facts. It
// never reruns an unknown side effect: DecideRecovery returns reconcile for
// that case and the backend must preserve the resulting state for its adapter.
func (engine *Engine) Reconcile(ctx context.Context, scope RecoveryScope) ([]RecoveryDecision, error) {
	if err := engine.validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if scope.ObservedAt.IsZero() {
		scope.ObservedAt = engine.now().UTC()
	}
	if err := scope.validate(); err != nil {
		return nil, err
	}
	subjects, err := engine.backend.ListRecoverySubjects(ctx, scope)
	if err != nil {
		return nil, err
	}
	decisions := make([]RecoveryDecision, 0, len(subjects))
	for _, subject := range subjects {
		if subject.ObservedAt.IsZero() {
			subject.ObservedAt = scope.ObservedAt
		}
		decision, err := DecideRecovery(subject)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	if err := engine.backend.ApplyRecovery(ctx, scope, append([]RecoveryDecision(nil), decisions...)); err != nil {
		return nil, err
	}
	return decisions, nil
}

func (engine *Engine) validate() error {
	if engine == nil || engine.backend == nil || engine.executors == nil || engine.classifier == nil || engine.now == nil {
		return ErrInvalidEngineConfiguration
	}
	return nil
}

func (engine *Engine) classifyFailure(err error) FailureClass {
	class := engine.classifier.ClassifyFailure(err)
	if !class.valid() || class == FailureNone {
		return FailureUnknown
	}
	return class
}

func defaultFailureClass(err error) FailureClass {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.Is(err, context.Canceled):
		// The backend's durable control projection decides whether cancellation
		// was an acknowledged user action. A bare context cancellation is not
		// enough evidence to manufacture a user-canceled outcome.
		return FailureProcess
	default:
		return FailureUnknown
	}
}

func inputBelongsToClaim(inputs []ArtifactBinding, requested ArtifactBinding) error {
	for _, input := range inputs {
		if input == requested {
			return nil
		}
	}
	return fmt.Errorf("%w: requested input %q is not bound to this stage claim", ErrInvalidJobClaim, requested.Name)
}
