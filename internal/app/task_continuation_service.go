package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var (
	// ErrTaskContinuationNotFound is returned when an immutable plan or
	// execution record is absent from the control plane.
	ErrTaskContinuationNotFound = errors.New("task continuation: record not found")
	// ErrTaskContinuationTarget is returned when an intent cannot be expanded
	// into a deterministic no-content continuation target.
	ErrTaskContinuationTarget = errors.New("task continuation: target is not actionable")
)

const (
	continuationCommandFormat   = "harbor.continue-task-command.v1"
	continuationExecutionFormat = "harbor.continuation-execution.v2"
)

// TaskContinuationService is the only application boundary for no-content
// retry and recompute work. Content-changing commands intentionally do not
// enter this service until the ChangeProvider/candidate-revision transaction
// is available; callers cannot accidentally mutate a sealed revision here.
type TaskContinuationService struct {
	core     *lifecycleServiceCore
	observer continuationSubjectStateObserver
}

func newTaskContinuationService(core *lifecycleServiceCore) *TaskContinuationService {
	service := &TaskContinuationService{core: core}
	service.observer = storeContinuationStateObserver{dataStore: core.store, objects: core.objects}
	return service
}

// ContinueTaskCommand is the persisted user intent for a no-content
// continuation. CommandKey is a caller-issued idempotency key. Plan expiry is
// deliberately absent: it is derived only from the versioned profile frozen
// into the target Run manifest.
type ContinueTaskCommand struct {
	CommandKey                  string                                   `json:"command_key"`
	TaskID                      string                                   `json:"task_id"`
	RunID                       string                                   `json:"run_id"`
	Expected                    workflowkit.CheckpointRef                `json:"expected"`
	TargetStageGroups           []string                                 `json:"target_stage_groups,omitempty"`
	TargetNodeIDs               []workflowkit.NodeID                     `json:"target_node_ids,omitempty"`
	ForceSelected               bool                                     `json:"force_selected,omitempty"`
	ExternalEffectConfirmations []workflowkit.ExternalEffectConfirmation `json:"external_effect_confirmations,omitempty"`
	Actor                       string                                   `json:"actor"`
	Reason                      string                                   `json:"reason"`
	Change                      *TaskChangeRequest                       `json:"change,omitempty"`
}

// CurrentCheckpoint returns the complete optimistic identity a UI or CLI
// must attach to a continuation request. It includes the run sequence and
// epoch as well as the task/revision/workflow binding, so a delayed action is
// rejected instead of silently applying to newer work.
func (service *TaskContinuationService) CurrentCheckpoint(ctx context.Context, runID string) (workflowkit.CheckpointRef, error) {
	if service == nil || service.core == nil {
		return workflowkit.CheckpointRef{}, fmt.Errorf("task continuation service is not configured")
	}
	return currentContinuationCheckpoint(ctx, service.core, runID)
}

// PlanTaskContinuation validates and persists an immutable user command,
// observes only durable lineage facts, and freezes a full transition plan. A
// replayed CommandKey returns the original plan rather than interpreting the
// same user request against a later mutable run state.
func (service *TaskContinuationService) PlanTaskContinuation(ctx context.Context, command ContinueTaskCommand) (workflowkit.ContinuationPlan, error) {
	if service == nil || service.core == nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("task continuation service is not configured")
	}
	if command.Change != nil {
		if service.core.changes == nil {
			return workflowkit.ContinuationPlan{}, fmt.Errorf("change provider service is not configured")
		}
		result, err := service.core.changes.PlanTaskChange(ctx, command, *command.Change)
		if err != nil {
			return workflowkit.ContinuationPlan{}, err
		}
		if result.NoOp {
			return workflowkit.ContinuationPlan{}, fmt.Errorf("%w: candidate %s", ErrChangeNoOp, result.Candidate.ID)
		}
		return result.Plan, nil
	}
	payload, err := normalizeContinuationCommand(command)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	encodedCommand, err := json.Marshal(payload)
	if err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("encode continuation command: %w", err)
	}
	// A task-revision continuation binds its command to the durable Task; a
	// pre-materialization authoring continuation carries no Task identity, so
	// its command subject is the frozen source the run is already bound to.
	commandSubjectID := payload.TaskID
	if commandSubjectID == "" {
		_, subject, err := loadContinuationSubjectBinding(ctx, service.core, payload.RunID)
		if err != nil {
			return workflowkit.ContinuationPlan{}, err
		}
		commandSubjectID = subject.Binding.SubjectID
	}
	commandRecord, err := service.core.store.CreateContinuationCommand(ctx, store.CreateContinuationCommandRequest{
		CommandKey:  payload.CommandKey,
		SubjectID:   commandSubjectID,
		RunID:       payload.RunID,
		PayloadJSON: string(encodedCommand),
		Actor:       command.Actor,
		Reason:      command.Reason,
	})
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}

	// A command is durable before its plan is available. Once a plan exists it
	// is authoritative for every idempotent replay, including after the run's
	// execution epoch has advanced during Execute.
	if existing, err := service.core.store.GetFrozenPlanByCommand(ctx, commandRecord.ID); err != nil {
		return workflowkit.ContinuationPlan{}, err
	} else if existing != nil {
		return decodeFrozenContinuationPlan(ctx, service.core, *existing)
	}

	compiled, err := service.compileContinuationPlan(ctx, payload, commandRecord.ID)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	plan := compiled.Plan
	encodedPlan, err := json.Marshal(plan.Snapshot())
	if err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("encode frozen continuation plan: %w", err)
	}
	stored, err := service.core.store.CreateFrozenPlan(ctx, store.CreateFrozenPlanRequest{
		ID:                  plan.ID(),
		CommandID:           commandRecord.ID,
		SubjectID:           compiled.Subject.Binding.SubjectID,
		SubjectRevisionID:   compiled.Subject.Binding.RevisionID,
		SubjectDigest:       string(compiled.Subject.Binding.Digest),
		WorkflowFingerprint: compiled.Run.DefinitionHash,
		PlanFingerprint:     string(plan.Fingerprint()),
		PayloadJSON:         string(encodedPlan),
		ExpiresAt:           plan.Snapshot().ExpiresAt,
		Actor:               commandRecord.Actor,
		Reason:              commandRecord.Reason,
	})
	if err != nil {
		// A concurrent planner with this same durable command may have won the
		// one-plan-per-command race. Returning its frozen result preserves
		// idempotency without reinterpreting selectors.
		if errors.Is(err, store.ErrIdempotencyConflict) {
			if existing, lookupErr := service.core.store.GetFrozenPlanByCommand(ctx, commandRecord.ID); lookupErr == nil && existing != nil {
				return decodeFrozenContinuationPlan(ctx, service.core, *existing)
			}
		}
		return workflowkit.ContinuationPlan{}, err
	}
	return decodeFrozenContinuationPlan(ctx, service.core, stored)
}

// PreviewTaskContinuation computes the same frozen plan that a durable
// continuation command would create, without persisting a command, plan,
// audit record, job, or execution. Preview identities are intentionally
// ephemeral and cannot be passed to ExecuteTaskContinuation.
func (service *TaskContinuationService) PreviewTaskContinuation(ctx context.Context, command ContinueTaskCommand) (workflowkit.ContinuationPlan, error) {
	if service == nil || service.core == nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("task continuation service is not configured")
	}
	payload, err := normalizeContinuationCommand(command)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	previewCommandID, err := store.NewUUIDv7()
	if err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("allocate continuation preview command ID: %w", err)
	}
	compiled, err := service.compileContinuationPlan(ctx, payload, previewCommandID)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	return compiled.Plan, nil
}

type compiledContinuationPlan struct {
	Plan    workflowkit.ContinuationPlan
	Run     store.WorkflowRun
	Subject workflowRunSubject
}

// compileContinuationPlan performs only pure reads plus UUID allocation. It
// is shared by durable planning and dry-run preview so their frozen workflow,
// invalidation, target, and TTL semantics cannot drift.
func (service *TaskContinuationService) compileContinuationPlan(ctx context.Context, payload normalizedContinuationCommand, commandID string) (compiledContinuationPlan, error) {
	run, subject, err := loadContinuationSubjectBinding(ctx, service.core, payload.RunID)
	if err != nil {
		return compiledContinuationPlan{}, err
	}
	if err := matchContinuationCheckpointSubject(payload.Expected, run, subject); err != nil {
		return compiledContinuationPlan{}, err
	}
	if run.Status == store.WorkflowRunInDoubt {
		return compiledContinuationPlan{}, fmt.Errorf("%w: workflow run %s", store.ErrContinuationReconciliationRequired, run.ID)
	}
	frozen, err := decodeFrozenRunDefinition(run)
	if err != nil {
		return compiledContinuationPlan{}, err
	}
	workflow := frozen.Workflow
	payload, err = expandContinuationStageGroups(payload, workflow)
	if err != nil {
		return compiledContinuationPlan{}, err
	}
	expiresAt := service.core.now().UTC().Add(frozen.ContinuationPlanTTL)
	state, err := service.observer.ObserveSubject(ctx, run, subject, workflow)
	if err != nil {
		return compiledContinuationPlan{}, err
	}
	if state.InDoubt {
		return compiledContinuationPlan{}, fmt.Errorf("%w: workflow run %s has unresolved stage or node evidence", store.ErrContinuationReconciliationRequired, run.ID)
	}
	targets, strategy, err := continuationTargets(payload, run, workflow, state, subject.isAuthoringSession())
	if err != nil {
		return compiledContinuationPlan{}, err
	}

	invalidation, err := workflowkit.PlanInvalidation(workflow, workflowkit.InvalidationRequest{
		RecomputeNodes: targets,
		ReuseStates:    state.ReuseStates,
		Matcher:        workflowadapter.HarborResourceMatch,
	})
	if err != nil {
		return compiledContinuationPlan{}, fmt.Errorf("plan continuation invalidation: %w", err)
	}
	// The frozen plan fingerprint includes its UUID, so allocate an independent
	// UUIDv7 before freezing. A plan must never derive or reuse its command
	// identity; command-key idempotency is enforced by the durable caller.
	planID, err := store.NewUUIDv7()
	if err != nil {
		return compiledContinuationPlan{}, fmt.Errorf("allocate continuation plan ID: %w", err)
	}
	requiredScheduledInputs := make(map[workflowkit.NodeID][]workflowkit.ArtifactBinding)
	if subject.isAuthoringSession() {
		bindings, err := service.authoringRepairAdmissionReportInput(ctx, run, subject, workflow, state, invalidation)
		if err != nil {
			return compiledContinuationPlan{}, err
		}
		if len(bindings) > 0 {
			requiredScheduledInputs[workflowkit.StageKey(workflowadapter.AuthoringRepair)] = bindings
		}
	}
	snapshot, err := buildSameRunContinuationPlan(planID, commandID, continuationPlanInput{
		Expected:                    payload.Expected,
		ExternalEffectConfirmations: payload.ExternalEffectConfirmations,
		RequiredScheduledInputs:     requiredScheduledInputs,
	}, run, subject.Binding.RevisionID, subject.Binding.Digest, workflow, state, invalidation, targets, strategy, expiresAt, subject.isAuthoringSession())
	if err != nil {
		return compiledContinuationPlan{}, err
	}
	plan, err := workflowkit.FreezeContinuationPlan(snapshot, workflow)
	if err != nil {
		return compiledContinuationPlan{}, fmt.Errorf("freeze continuation plan: %w", err)
	}
	return compiledContinuationPlan{Plan: plan, Run: run, Subject: subject}, nil
}

// GetTaskContinuationPlan reads and verifies a stored immutable plan against
// the exact workflow descriptor frozen in its source run manifest.
func (service *TaskContinuationService) GetTaskContinuationPlan(ctx context.Context, planID string) (workflowkit.ContinuationPlan, error) {
	if service == nil || service.core == nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("task continuation service is not configured")
	}
	return getFrozenContinuationPlan(ctx, service.core, planID)
}

// getFrozenContinuationPlan verifies a plan against the workflow frozen in
// its source Run. Several lifecycle services consume the same immutable plan,
// so this intentionally belongs below the task-revision facade.
func getFrozenContinuationPlan(ctx context.Context, core *lifecycleServiceCore, planID string) (workflowkit.ContinuationPlan, error) {
	if core == nil || core.store == nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("continuation plan store is not configured")
	}
	plan, err := core.store.GetFrozenPlan(ctx, planID)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	if plan == nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("%w: plan %s", ErrTaskContinuationNotFound, planID)
	}
	return decodeFrozenContinuationPlan(ctx, core, *plan)
}

// ExecuteTaskContinuation consumes only a previously frozen plan. It creates
// the continuation execution, queues its durable worker job, advances the
// execution epoch, and writes its outbox event through one Store transaction.
// It does not invoke a ChangeProvider or recompute selector semantics.
func (service *TaskContinuationService) ExecuteTaskContinuation(ctx context.Context, planID string) (store.ContinuationExecution, error) {
	if service == nil || service.core == nil {
		return store.ContinuationExecution{}, fmt.Errorf("task continuation service is not configured")
	}
	plan, err := service.GetTaskContinuationPlan(ctx, planID)
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	snapshot := plan.Snapshot()
	if snapshot.Strategy == workflowkit.StrategyReviseSubject {
		if service.core.changes == nil {
			return store.ContinuationExecution{}, fmt.Errorf("change provider service is not configured")
		}
		commit, err := service.core.changes.ExecuteTaskChange(ctx, planID, "", "")
		if err != nil {
			return store.ContinuationExecution{}, err
		}
		return commit.Execution, nil
	}
	executionKey := continuationExecutionKey(snapshot.PlanID)
	if existing, err := service.core.store.GetContinuationExecutionByIdempotency(ctx, executionKey); err != nil {
		return store.ContinuationExecution{}, err
	} else if existing != nil {
		if existing.PlanID != snapshot.PlanID || existing.RunID != snapshot.SourceRunID {
			return store.ContinuationExecution{}, fmt.Errorf("%w: continuation execution key %s", store.ErrIdempotencyConflict, executionKey)
		}
		return *existing, nil
	}
	if plan.IsExpired(service.core.now().UTC()) {
		return store.ContinuationExecution{}, fmt.Errorf("%w: %s", store.ErrContinuationPlanExpired, planID)
	}
	command, err := service.core.store.GetContinuationCommand(ctx, snapshot.CommandID)
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	if command == nil {
		return store.ContinuationExecution{}, fmt.Errorf("%w: command %s", ErrTaskContinuationNotFound, snapshot.CommandID)
	}
	run, err := service.core.store.GetWorkflowRun(ctx, snapshot.SourceRunID)
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	if run == nil {
		return store.ContinuationExecution{}, fmt.Errorf("%w: source run %s", ErrTaskContinuationNotFound, snapshot.SourceRunID)
	}
	frozen, err := decodeFrozenRunDefinition(*run)
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	payload, err := json.Marshal(continuationExecutionPayload{
		Format:          continuationExecutionFormat,
		PlanID:          snapshot.PlanID,
		CommandID:       snapshot.CommandID,
		PlanFingerprint: plan.Fingerprint(),
		RunID:           snapshot.SourceRunID,
		SourceRunID:     snapshot.SourceRunID,
		QuotaPolicy:     frozen.QuotaPolicy.Clone(),
	})
	if err != nil {
		return store.ContinuationExecution{}, fmt.Errorf("encode continuation execution: %w", err)
	}
	commit, err := service.core.store.CommitContinuationExecution(ctx, store.CommitContinuationExecutionRequest{
		PlanID:         snapshot.PlanID,
		RunID:          snapshot.SourceRunID,
		IdempotencyKey: executionKey,
		PayloadJSON:    string(payload),
		Expected:       storeCheckpoint(snapshot.BaseCheckpoint),
		Actor:          command.Actor,
		Reason:         command.Reason,
	})
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	return commit.Execution, nil
}

type normalizedContinuationCommand struct {
	Format                      string                                   `json:"format"`
	CommandKey                  string                                   `json:"command_key"`
	TaskID                      string                                   `json:"task_id"`
	RunID                       string                                   `json:"run_id"`
	Expected                    workflowkit.CheckpointRef                `json:"expected"`
	TargetStageGroups           []string                                 `json:"target_stage_groups"`
	TargetNodeIDs               []workflowkit.NodeID                     `json:"target_node_ids"`
	ForceSelected               bool                                     `json:"force_selected"`
	ExternalEffectConfirmations []workflowkit.ExternalEffectConfirmation `json:"external_effect_confirmations"`
}

type continuationExecutionPayload struct {
	Format          string                  `json:"format"`
	PlanID          string                  `json:"plan_id"`
	CommandID       string                  `json:"command_id"`
	PlanFingerprint workflowkit.Fingerprint `json:"plan_fingerprint"`
	// RunID is the run the durable worker must dispatch. SourceRunID preserves
	// lineage for plans whose execution is routed to a child run.
	RunID       string                              `json:"run_id"`
	SourceRunID string                              `json:"source_run_id"`
	QuotaPolicy workflowadapter.ResolvedQuotaPolicy `json:"quota_policy"`
}

// normalizeContinuationCommand has no service dependencies, so content and
// no-content continuation paths share the exact immutable command shape.
func normalizeContinuationCommand(command ContinueTaskCommand) (normalizedContinuationCommand, error) {
	if command.Change != nil {
		return normalizedContinuationCommand{}, fmt.Errorf("content changes must be planned through ChangeProviderService")
	}
	if strings.TrimSpace(command.CommandKey) == "" {
		return normalizedContinuationCommand{}, fmt.Errorf("continuation command key is required")
	}
	if strings.TrimSpace(command.TaskID) != "" {
		// A task-revision continuation carries the durable Task as its command
		// subject and checkpoint subject. A pre-materialization authoring
		// recovery carries no Task identity: its checkpoint subject is the
		// frozen source/session instead, so the equality check is deferred to
		// the subject-aware planner.
		if err := store.ValidateUUIDv7(command.TaskID); err != nil {
			return normalizedContinuationCommand{}, err
		}
		if command.Expected.SubjectID != command.TaskID {
			return normalizedContinuationCommand{}, fmt.Errorf("continuation checkpoint task does not match command task")
		}
	}
	if err := store.ValidateUUIDv7(command.RunID); err != nil {
		return normalizedContinuationCommand{}, err
	}
	if strings.TrimSpace(command.Actor) == "" || strings.TrimSpace(command.Reason) == "" {
		return normalizedContinuationCommand{}, fmt.Errorf("continuation actor and reason are required")
	}
	if err := validateContinuationCheckpoint(command.Expected); err != nil {
		return normalizedContinuationCommand{}, err
	}
	targets := append([]workflowkit.NodeID(nil), command.TargetNodeIDs...)
	sort.Slice(targets, func(left, right int) bool { return targets[left] < targets[right] })
	for index, target := range targets {
		if strings.TrimSpace(string(target)) == "" {
			return normalizedContinuationCommand{}, fmt.Errorf("continuation target node is required")
		}
		if index > 0 && targets[index-1] == target {
			return normalizedContinuationCommand{}, fmt.Errorf("duplicate continuation target node %q", target)
		}
	}
	groups := append([]string(nil), command.TargetStageGroups...)
	for index := range groups {
		groups[index] = strings.TrimSpace(groups[index])
	}
	sort.Strings(groups)
	for index, group := range groups {
		if group == "" {
			return normalizedContinuationCommand{}, fmt.Errorf("continuation target stage group is required")
		}
		if index > 0 && groups[index-1] == group {
			return normalizedContinuationCommand{}, fmt.Errorf("duplicate continuation target stage group %q", group)
		}
	}
	if command.ForceSelected && len(targets) == 0 && len(groups) == 0 {
		return normalizedContinuationCommand{}, fmt.Errorf("force-selected continuation requires an explicit target")
	}
	confirmations := append([]workflowkit.ExternalEffectConfirmation(nil), command.ExternalEffectConfirmations...)
	sort.Slice(confirmations, func(left, right int) bool { return confirmations[left].NodeID < confirmations[right].NodeID })
	for index, confirmation := range confirmations {
		if strings.TrimSpace(string(confirmation.NodeID)) == "" || strings.TrimSpace(confirmation.IdempotencyKey) == "" || strings.TrimSpace(confirmation.Actor) == "" || confirmation.ConfirmedAt.IsZero() {
			return normalizedContinuationCommand{}, fmt.Errorf("external effect confirmation is incomplete")
		}
		if index > 0 && confirmations[index-1].NodeID == confirmation.NodeID {
			return normalizedContinuationCommand{}, fmt.Errorf("duplicate external effect confirmation for %q", confirmation.NodeID)
		}
		confirmations[index].ConfirmedAt = confirmation.ConfirmedAt.UTC()
	}
	return normalizedContinuationCommand{
		Format:                      continuationCommandFormat,
		CommandKey:                  strings.TrimSpace(command.CommandKey),
		TaskID:                      command.TaskID,
		RunID:                       command.RunID,
		Expected:                    command.Expected,
		TargetStageGroups:           groups,
		TargetNodeIDs:               targets,
		ForceSelected:               command.ForceSelected,
		ExternalEffectConfirmations: confirmations,
	}, nil
}

func currentContinuationCheckpoint(ctx context.Context, core *lifecycleServiceCore, runID string) (workflowkit.CheckpointRef, error) {
	run, subject, err := loadContinuationSubjectBinding(ctx, core, runID)
	if err != nil {
		return workflowkit.CheckpointRef{}, err
	}
	checkpoint := workflowkit.CheckpointRef{
		Sequence:            uint64(run.Version),
		ExecutionEpoch:      run.ExecutionEpoch,
		SubjectID:           subject.Binding.SubjectID,
		SubjectRevisionID:   subject.Binding.RevisionID,
		SubjectDigest:       subject.Binding.Digest,
		WorkflowFingerprint: workflowkit.Fingerprint(run.DefinitionHash),
	}
	switch {
	case subject.isTaskRevision() && subject.Task != nil:
		checkpoint.SubjectVersion = subject.Task.Version
	case subject.isAuthoringSession():
		checkpoint.SubjectVersion = store.AuthoringSessionControlSubjectVersion
	default:
		return workflowkit.CheckpointRef{}, fmt.Errorf("workflow Run %s has no supported continuation subject", run.ID)
	}
	return checkpoint, nil
}

// loadContinuationSubjectBinding resolves the exact durable subject of any
// continuation target. A task-revision Run resolves its Task/TaskRevision;
// a pre-materialization authoring Run resolves its immutable source/session.
func loadContinuationSubjectBinding(ctx context.Context, core *lifecycleServiceCore, runID string) (store.WorkflowRun, workflowRunSubject, error) {
	if core == nil || core.store == nil {
		return store.WorkflowRun{}, workflowRunSubject{}, fmt.Errorf("task continuation service is not configured")
	}
	run, err := core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return store.WorkflowRun{}, workflowRunSubject{}, err
	}
	if run == nil {
		return store.WorkflowRun{}, workflowRunSubject{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	subject, err := core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return store.WorkflowRun{}, workflowRunSubject{}, err
	}
	return *run, subject, nil
}

// loadContinuationRunBinding resolves the exact task-revision lineage used
// by continuation and content-change commands without constructing a service.
func loadContinuationRunBinding(ctx context.Context, core *lifecycleServiceCore, runID string) (store.WorkflowRun, store.TaskV2, store.TaskRevision, error) {
	if core == nil || core.store == nil {
		return store.WorkflowRun{}, store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("task continuation service is not configured")
	}
	run, err := core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return store.WorkflowRun{}, store.TaskV2{}, store.TaskRevision{}, err
	}
	if run == nil {
		return store.WorkflowRun{}, store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	task, err := core.store.GetTaskV2(ctx, run.TaskID)
	if err != nil {
		return store.WorkflowRun{}, store.TaskV2{}, store.TaskRevision{}, err
	}
	if task == nil {
		return store.WorkflowRun{}, store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, run.TaskID)
	}
	revision, err := core.store.GetTaskRevision(ctx, run.RevisionID)
	if err != nil {
		return store.WorkflowRun{}, store.TaskV2{}, store.TaskRevision{}, err
	}
	if revision == nil {
		return store.WorkflowRun{}, store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("%w: revision %s", ErrLifecycleNotFound, run.RevisionID)
	}
	if revision.TaskID != task.ID {
		return store.WorkflowRun{}, store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("workflow run revision does not belong to its task")
	}
	return *run, *task, *revision, nil
}

func decodeFrozenContinuationPlan(ctx context.Context, core *lifecycleServiceCore, stored store.FrozenPlan) (workflowkit.ContinuationPlan, error) {
	if core == nil || core.store == nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("continuation plan store is not configured")
	}
	var snapshot workflowkit.ContinuationPlanSnapshot
	if err := decodeStrictJSON(stored.PayloadJSON, &snapshot); err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("decode frozen continuation plan %s: %w", stored.ID, err)
	}
	if snapshot.PlanID != stored.ID || snapshot.CommandID != stored.CommandID || snapshot.SubjectRevisionID != stored.SubjectRevisionID ||
		snapshot.SubjectDigest != workflowkit.SubjectDigest(stored.SubjectDigest) || !snapshot.ExpiresAt.Equal(stored.ExpiresAt) {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("stored continuation plan %s has inconsistent immutable fields", stored.ID)
	}
	run, err := core.store.GetWorkflowRun(ctx, snapshot.SourceRunID)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	if run == nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("%w: source run %s", ErrTaskContinuationNotFound, snapshot.SourceRunID)
	}
	if run.DefinitionHash != stored.WorkflowFingerprint {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("stored continuation plan %s does not match source run definition", stored.ID)
	}
	workflow, err := decodeFrozenWorkflow(*run)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	plan, err := workflowkit.FreezeContinuationPlan(snapshot, workflow)
	if err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("validate stored continuation plan %s: %w", stored.ID, err)
	}
	if plan.Fingerprint() != workflowkit.Fingerprint(stored.PlanFingerprint) {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("stored continuation plan %s fingerprint does not match payload", stored.ID)
	}
	return plan, nil
}

type frozenRunDefinition struct {
	Workflow                 workflowkit.WorkflowDescriptor
	InitialExecutionPlan     workflowkit.ExecutionPlan
	ExecutionSpecFingerprint workflowkit.Fingerprint
	ContinuationPlanTTL      time.Duration
	ControlGracePeriod       time.Duration
	CandidateProviderBudget  workflowadapter.CandidateProviderBudget
	QuotaPolicy              workflowadapter.ResolvedQuotaPolicy
	ReviewStages             []workflowadapter.ReviewStage
	DeploymentCatalogReceipt []byte
}

func decodeFrozenRunDefinition(run store.WorkflowRun) (frozenRunDefinition, error) {
	var manifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
		return frozenRunDefinition{}, fmt.Errorf("decode frozen run manifest %s: %w", run.ID, err)
	}
	if manifest.Format != "harbor.workflow-run-manifest.v2" || manifest.RunID != run.ID {
		return frozenRunDefinition{}, fmt.Errorf("frozen run manifest %s does not match its control-plane run", run.ID)
	}
	if err := validateRunManifestSubject(manifest, run); err != nil {
		return frozenRunDefinition{}, fmt.Errorf("frozen run manifest %s subject: %w", run.ID, err)
	}
	_, _, executionSpecFingerprint, err := canonicalFrozenRunExecutionSpec(manifest, run)
	if err != nil {
		return frozenRunDefinition{}, fmt.Errorf("validate frozen run manifest %s execution specification: %w", run.ID, err)
	}
	catalogReceipt, err := canonicalManifestDeploymentCatalogReceipt(manifest)
	if err != nil {
		return frozenRunDefinition{}, fmt.Errorf("validate frozen run manifest %s deployment catalog receipt: %w", run.ID, err)
	}
	if manifest.Resolved.ContinuationPlanTTL <= 0 || manifest.Resolved.ContinuationPlanTTL > workflowadapter.MaxContinuationPlanTTL {
		return frozenRunDefinition{}, fmt.Errorf("frozen run manifest %s continuation plan TTL must be within (0, %s]", run.ID, workflowadapter.MaxContinuationPlanTTL)
	}
	workflow := manifest.Resolved.Descriptor.Clone()
	fingerprint, err := workflow.Fingerprint()
	if err != nil {
		return frozenRunDefinition{}, fmt.Errorf("validate frozen workflow descriptor %s: %w", run.ID, err)
	}
	if fingerprint != workflowkit.Fingerprint(run.DefinitionHash) || manifest.Resolved.DefinitionFingerprint != fingerprint {
		return frozenRunDefinition{}, fmt.Errorf("frozen run manifest %s definition fingerprint mismatch", run.ID)
	}
	initialExecutionPlan := manifest.InitialExecutionPlan.Clone()
	if err := initialExecutionPlan.Validate(workflow); err != nil {
		return frozenRunDefinition{}, fmt.Errorf("validate frozen initial execution plan %s: %w", run.ID, err)
	}
	quotaPolicy := manifest.Resolved.QuotaPolicy.Clone()
	if err := quotaPolicy.ValidateForDescriptor(workflow); err != nil {
		return frozenRunDefinition{}, fmt.Errorf("validate frozen quota policy %s: %w", run.ID, err)
	}
	if manifest.Resolved.ControlGracePeriod < 0 {
		return frozenRunDefinition{}, fmt.Errorf("frozen run manifest %s has a negative control grace period", run.ID)
	}
	if err := manifest.Resolved.CandidateProviderBudget.Validate(); err != nil {
		return frozenRunDefinition{}, fmt.Errorf("frozen run manifest %s candidate provider budget: %w", run.ID, err)
	}
	reviewStages := append([]workflowadapter.ReviewStage(nil), manifest.Resolved.ReviewStages...)
	seenReviewStages := make(map[workflowkit.StageKey]struct{}, len(reviewStages))
	for _, review := range reviewStages {
		if _, duplicate := seenReviewStages[review.StageKey]; duplicate {
			return frozenRunDefinition{}, fmt.Errorf("frozen run manifest %s repeats review stage %q", run.ID, review.StageKey)
		}
		stage, found := workflow.Stage(review.StageKey)
		if !found || !stage.Capabilities.Has(workflowkit.CapabilityApprove) || review.ReviewKind == "" ||
			review.DecisionArtifact.Name == "" || review.DecisionArtifact.SchemaVersion == "" {
			return frozenRunDefinition{}, fmt.Errorf("frozen run manifest %s has an invalid review stage %q", run.ID, review.StageKey)
		}
		foundOutput := false
		for _, output := range stage.Outputs {
			if output.Name == review.DecisionArtifact.Name && output.SchemaVersion == review.DecisionArtifact.SchemaVersion {
				foundOutput = true
				break
			}
		}
		if !foundOutput {
			return frozenRunDefinition{}, fmt.Errorf("frozen run manifest %s review stage %q does not declare its decision artifact", run.ID, review.StageKey)
		}
		seenReviewStages[review.StageKey] = struct{}{}
	}
	for _, stage := range workflow.Stages {
		if stage.Capabilities.Has(workflowkit.CapabilityApprove) {
			if _, found := seenReviewStages[stage.Key]; !found {
				return frozenRunDefinition{}, fmt.Errorf("frozen run manifest %s omits review metadata for stage %q", run.ID, stage.Key)
			}
		}
	}
	return frozenRunDefinition{
		Workflow:                 workflow,
		InitialExecutionPlan:     initialExecutionPlan,
		ExecutionSpecFingerprint: executionSpecFingerprint,
		ContinuationPlanTTL:      manifest.Resolved.ContinuationPlanTTL,
		ControlGracePeriod:       manifest.Resolved.ControlGracePeriod,
		CandidateProviderBudget:  manifest.Resolved.CandidateProviderBudget,
		QuotaPolicy:              quotaPolicy,
		ReviewStages:             reviewStages,
		DeploymentCatalogReceipt: append([]byte(nil), catalogReceipt...),
	}, nil
}

// canonicalFrozenRunExecutionSpec proves the embedded execution specification
// is the exact canonical selection sealed with this Run. It intentionally
// performs no filesystem access; worker callers separately compare it with
// the managed companion file before beginning work.
func canonicalFrozenRunExecutionSpec(manifest runManifest, run store.WorkflowRun) (workflowadapter.RunExecutionSpec, []byte, workflowkit.Fingerprint, error) {
	if manifest.Inputs == nil || manifest.Inputs.Format != runManifestInputsFormat || len(manifest.ExecutionSpec) == 0 {
		return workflowadapter.RunExecutionSpec{}, nil, "", fmt.Errorf("run manifest has no canonical execution specification")
	}
	if manifest.Inputs.ProfileFingerprint != manifest.Resolved.ExecutionProfileFingerprint {
		return workflowadapter.RunExecutionSpec{}, nil, "", fmt.Errorf("run manifest execution profile fingerprint does not match resolved workflow")
	}
	specification, err := workflowadapter.ParseRunExecutionSpecJSON(manifest.ExecutionSpec)
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, nil, "", err
	}
	canonical, err := specification.CanonicalJSON()
	if err != nil || !bytes.Equal(canonical, manifest.ExecutionSpec) {
		return workflowadapter.RunExecutionSpec{}, nil, "", fmt.Errorf("run manifest execution specification is not canonical")
	}
	fingerprint, err := specification.Fingerprint()
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, nil, "", err
	}
	if fingerprint != manifest.Inputs.ExecutionSpecFingerprint {
		return workflowadapter.RunExecutionSpec{}, nil, "", fmt.Errorf("run manifest execution specification fingerprint does not match inputs")
	}
	binding, err := specification.Selection.SubjectBinding()
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, nil, "", fmt.Errorf("run manifest execution specification subject: %w", err)
	}
	if binding.SubjectID != run.SubjectID || binding.RevisionID != run.SubjectRevisionID || string(binding.Digest) != run.SubjectDigest {
		return workflowadapter.RunExecutionSpec{}, nil, "", fmt.Errorf("run manifest execution specification selection does not match Run")
	}
	return specification, canonical, fingerprint, nil
}

func (definition frozenRunDefinition) ReviewStage(key workflowkit.StageKey) (workflowadapter.ReviewStage, bool) {
	for _, review := range definition.ReviewStages {
		if review.StageKey == key {
			return review, true
		}
	}
	return workflowadapter.ReviewStage{}, false
}

func decodeFrozenWorkflow(run store.WorkflowRun) (workflowkit.WorkflowDescriptor, error) {
	frozen, err := decodeFrozenRunDefinition(run)
	if err != nil {
		return workflowkit.WorkflowDescriptor{}, err
	}
	return frozen.Workflow, nil
}

func matchContinuationCheckpoint(expected workflowkit.CheckpointRef, run store.WorkflowRun, task store.TaskV2, revision store.TaskRevision) error {
	if expected.Sequence != uint64(run.Version) || expected.ExecutionEpoch != run.ExecutionEpoch || expected.SubjectVersion != task.Version ||
		expected.SubjectID != task.ID || expected.SubjectRevisionID != revision.ID || expected.SubjectDigest != workflowkit.SubjectDigest(revision.TaskDigest) ||
		expected.WorkflowFingerprint != workflowkit.Fingerprint(run.DefinitionHash) {
		return fmt.Errorf("%w: continuation checkpoint is stale", store.ErrOptimisticLock)
	}
	return nil
}

// matchContinuationCheckpointSubject is the subject-neutral counterpart of
// matchContinuationCheckpoint. The authoring-session checkpoint uses the
// stable AuthoringSessionControlSubjectVersion, mirroring execution-control
// and store-side continuation commits so a stale recovery can never silently
// rebind to newer source/session facts.
func matchContinuationCheckpointSubject(expected workflowkit.CheckpointRef, run store.WorkflowRun, subject workflowRunSubject) error {
	if expected.Sequence != uint64(run.Version) || expected.ExecutionEpoch != run.ExecutionEpoch ||
		expected.SubjectID != subject.Binding.SubjectID || expected.SubjectRevisionID != subject.Binding.RevisionID ||
		expected.SubjectDigest != subject.Binding.Digest || expected.WorkflowFingerprint != workflowkit.Fingerprint(run.DefinitionHash) {
		return fmt.Errorf("%w: continuation checkpoint is stale", store.ErrOptimisticLock)
	}
	switch {
	case subject.isTaskRevision() && subject.Task != nil:
		if expected.SubjectVersion != subject.Task.Version {
			return fmt.Errorf("%w: continuation checkpoint is stale", store.ErrOptimisticLock)
		}
	case subject.isAuthoringSession():
		if expected.SubjectVersion != store.AuthoringSessionControlSubjectVersion {
			return fmt.Errorf("%w: continuation checkpoint is stale", store.ErrOptimisticLock)
		}
	default:
		return fmt.Errorf("workflow Run %s has no supported continuation subject", run.ID)
	}
	return nil
}

func validateContinuationCheckpoint(checkpoint workflowkit.CheckpointRef) error {
	if checkpoint.Sequence == 0 || checkpoint.ExecutionEpoch < 0 || checkpoint.SubjectVersion < 0 {
		return fmt.Errorf("invalid continuation checkpoint versions")
	}
	if err := store.ValidateUUIDv7(checkpoint.SubjectID); err != nil {
		return err
	}
	if err := store.ValidateUUIDv7(checkpoint.SubjectRevisionID); err != nil {
		return err
	}
	if err := checkpoint.SubjectDigest.Validate(); err != nil {
		return err
	}
	return checkpoint.WorkflowFingerprint.Validate()
}

func storeCheckpoint(checkpoint workflowkit.CheckpointRef) store.ControlCheckpointRef {
	return store.ControlCheckpointRef{
		Sequence:            checkpoint.Sequence,
		ExecutionEpoch:      checkpoint.ExecutionEpoch,
		SubjectVersion:      checkpoint.SubjectVersion,
		SubjectID:           checkpoint.SubjectID,
		SubjectRevisionID:   checkpoint.SubjectRevisionID,
		SubjectDigest:       string(checkpoint.SubjectDigest),
		WorkflowFingerprint: string(checkpoint.WorkflowFingerprint),
	}
}

func continuationExecutionKey(planID string) string {
	return "continuation-execution:" + planID
}

// derivedContinuationPlanID gives one command a deterministic, distinct
func decodeStrictJSON(raw string, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

type continuationRunState struct {
	ReuseStates []workflowkit.StageReuseState
	Generations map[workflowkit.NodeID]int
	Latest      map[workflowkit.NodeID]store.StageAttempt
	InDoubt     bool
}

type continuationStateObserver interface {
	Observe(context.Context, store.WorkflowRun, store.TaskRevision, workflowkit.WorkflowDescriptor) (continuationRunState, error)
}

// continuationSubjectStateObserver is the subject-neutral counterpart used by
// pre-materialization authoring recovery. Keeping the task-revision interface
// above preserves the narrow contract used by task continuation and candidate
// planning while sharing the durable artifact-lineage verification below.
type continuationSubjectStateObserver interface {
	ObserveSubject(context.Context, store.WorkflowRun, workflowRunSubject, workflowkit.WorkflowDescriptor) (continuationRunState, error)
}

type storeContinuationStateObserver struct {
	dataStore *store.Store
	objects   *workflowruntime.ArtifactObjectStore
}

func (observer storeContinuationStateObserver) Observe(ctx context.Context, run store.WorkflowRun, revision store.TaskRevision, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
	return observer.ObserveSubject(ctx, run, taskRevisionSubjectForLineage(run, revision), workflow)
}

func (observer storeContinuationStateObserver) ObserveSubject(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
	if observer.dataStore == nil {
		return continuationRunState{}, fmt.Errorf("continuation state store is required")
	}
	if observer.objects == nil {
		return continuationRunState{}, fmt.Errorf("continuation artifact object store is required")
	}
	if !subject.isTaskRevision() && !subject.isAuthoringSession() {
		return continuationRunState{}, fmt.Errorf("continuation state subject is unsupported")
	}
	attempts, err := observer.dataStore.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		return continuationRunState{}, err
	}
	state := continuationRunState{
		ReuseStates: make([]workflowkit.StageReuseState, 0, len(workflow.Stages)),
		Generations: make(map[workflowkit.NodeID]int, len(workflow.Stages)),
		Latest:      make(map[workflowkit.NodeID]store.StageAttempt, len(workflow.Stages)),
	}
	for _, attempt := range attempts {
		nodeID := workflowkit.NodeID(attempt.StageKey)
		if _, exists := workflow.Stage(nodeID); !exists {
			return continuationRunState{}, fmt.Errorf("stage attempt %s refers to unavailable frozen stage %q", attempt.ID, attempt.StageKey)
		}
		state.Latest[nodeID] = attempt
		if attempt.ExecutionStatus == store.StageExecutionInDoubt || attempt.ExecutionStatus == store.StageExecutionReconciling {
			state.InDoubt = true
		}
		nodes, err := observer.dataStore.ListNodeAttempts(ctx, attempt.ID)
		if err != nil {
			return continuationRunState{}, err
		}
		for _, node := range nodes {
			if node.Status == store.NodeAttemptInDoubt {
				state.InDoubt = true
			}
			if generation := state.Generations[workflowkit.NodeID(node.NodeID)]; node.Generation > generation {
				state.Generations[workflowkit.NodeID(node.NodeID)] = node.Generation
			}
		}
	}
	for _, stage := range workflow.Stages {
		state.ReuseStates = append(state.ReuseStates, observer.reuseStateForSubject(ctx, run, subject, stage, state.Latest[stage.Key]))
	}
	return state, nil
}

func (observer storeContinuationStateObserver) reuseStateForSubject(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, stage workflowkit.StageDescriptor, latest store.StageAttempt) workflowkit.StageReuseState {
	missing := workflowkit.StageReuseState{NodeID: stage.Key}
	if latest.ID == "" || latest.ExecutionStatus != store.StageExecutionCompleted || strings.TrimSpace(latest.ArtifactManifestID) == "" {
		return missing
	}
	if err := store.ValidateUUIDv7(latest.ArtifactManifestID); err != nil {
		return missing
	}
	references, err := observer.dataStore.ListArtifactRefs(ctx, latest.ArtifactManifestID)
	if err != nil || len(references) == 0 {
		return missing
	}
	manifest, err := loadStageArtifactManifestIndex(ctx, observer.dataStore, latest.ArtifactManifestID)
	if err != nil {
		return missing
	}
	outputs := make(map[string]workflowkit.ArtifactSpec, len(stage.Outputs))
	for _, output := range stage.Outputs {
		outputs[output.Name] = output
	}
	seenOutputs := make(map[string]struct{}, len(outputs))
	var expectedInputs workflowkit.Fingerprint
	var currentInputs []workflowkit.ArtifactBinding
	for _, reference := range references {
		if reference.RunID != run.ID || reference.StageKey != string(stage.Key) || reference.SubjectRevisionID != subject.subjectRevisionID() ||
			reference.SubjectDigest != subject.subjectDigest() || reference.WorkflowFingerprint != run.DefinitionHash || reference.AttemptID != latest.ID {
			return missing
		}
		specification, exists := outputs[reference.ArtifactKey]
		if !exists || specification.SchemaVersion != reference.SchemaVersion {
			return missing
		}
		if _, duplicate := seenOutputs[reference.ArtifactKey]; duplicate {
			return missing
		}
		seenOutputs[reference.ArtifactKey] = struct{}{}
		if err := verifyStageArtifactCandidateWithManifestForSubject(ctx, observer.objects, manifest, run, subject, stageArtifactCandidate{attempt: latest, ref: reference}); err != nil {
			return missing
		}
		var bindings []workflowkit.ArtifactBinding
		if err := decodeStrictJSON(reference.InputBindingsJSON, &bindings); err != nil {
			return missing
		}
		fingerprint, err := workflowkit.FingerprintArtifactBindings(bindings)
		if err != nil || fingerprint != workflowkit.Fingerprint(reference.InputFingerprint) {
			return missing
		}
		attemptInputs := workflowkit.Fingerprint(latest.InputFingerprint)
		if attemptInputs != fingerprint {
			if err := attemptInputs.Validate(); err != nil {
				return missing
			}
			// The artifact bytes and manifest are intact, but the completed
			// attempt no longer claims the inputs that produced them. Preserve
			// this observation so invalidation can surface fingerprint drift
			// instead of treating it as an unexplained missing artifact.
			return workflowkit.StageReuseState{
				NodeID:                   stage.Key,
				Present:                  true,
				ArtifactsIntact:          true,
				ExpectedInputFingerprint: attemptInputs,
				CurrentInputs:            append([]workflowkit.ArtifactBinding(nil), bindings...),
			}
		}
		if expectedInputs == "" {
			expectedInputs = fingerprint
			currentInputs = append([]workflowkit.ArtifactBinding(nil), bindings...)
		} else if expectedInputs != fingerprint || !sameArtifactBindings(currentInputs, bindings) {
			return missing
		}
	}
	for output := range outputs {
		if _, exists := seenOutputs[output]; !exists {
			return missing
		}
	}
	if expectedInputs == "" {
		return missing
	}
	return workflowkit.StageReuseState{
		NodeID:                   stage.Key,
		Present:                  true,
		ArtifactsIntact:          true,
		ExpectedInputFingerprint: expectedInputs,
		CurrentInputs:            currentInputs,
	}
}

func sameArtifactBindings(left, right []workflowkit.ArtifactBinding) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// expandContinuationStageGroups resolves a user-facing StageCatalog group
// against the descriptor frozen in the target Run. The CLI accepts groups
// only; exact node keys remain an internal plan representation. Expansion
// happens after the frozen descriptor is loaded so a later catalog change
// cannot silently change a persisted command's interpretation.
func expandContinuationStageGroups(command normalizedContinuationCommand, workflow workflowkit.WorkflowDescriptor) (normalizedContinuationCommand, error) {
	if len(command.TargetStageGroups) == 0 {
		return command, nil
	}
	byGroup := make(map[string][]workflowkit.NodeID)
	for _, stage := range workflow.Stages {
		byGroup[stage.Group] = append(byGroup[stage.Group], stage.Key)
	}
	targets := make(map[workflowkit.NodeID]struct{}, len(command.TargetNodeIDs))
	for _, nodeID := range command.TargetNodeIDs {
		targets[nodeID] = struct{}{}
	}
	for _, group := range command.TargetStageGroups {
		nodes, exists := byGroup[group]
		if !exists || len(nodes) == 0 {
			return normalizedContinuationCommand{}, fmt.Errorf("%w: unknown frozen stage group %q", ErrTaskContinuationTarget, group)
		}
		for _, nodeID := range nodes {
			targets[nodeID] = struct{}{}
		}
	}
	command.TargetNodeIDs = make([]workflowkit.NodeID, 0, len(targets))
	for nodeID := range targets {
		command.TargetNodeIDs = append(command.TargetNodeIDs, nodeID)
	}
	sort.Slice(command.TargetNodeIDs, func(left, right int) bool {
		return command.TargetNodeIDs[left] < command.TargetNodeIDs[right]
	})
	return command, nil
}

// authoringRepairAdmissionReportInput binds the latest completed package
// admission report as a continuation-plan-only repair input whenever the
// authoring repair stage is scheduled by invalidation. The report is never
// resolved from ordinary stage lineage, so an open repair conversation sees
// the exact violation that failed packaging without a stale lineage report
// claiming to supersede the open repair.
func (service *TaskContinuationService) authoringRepairAdmissionReportInput(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, workflow workflowkit.WorkflowDescriptor, state continuationRunState, invalidation workflowkit.InvalidationPlan) ([]workflowkit.ArtifactBinding, error) {
	repair, exists := workflow.Stage(workflowadapter.AuthoringRepair)
	if !exists {
		return nil, nil
	}
	scheduled := false
	for _, entry := range invalidation.Entries {
		if entry.NodeID != repair.Key {
			continue
		}
		switch entry.Impact {
		case workflowkit.ImpactInvalidate, workflowkit.ImpactRequiresConfirmation:
			scheduled = true
		}
	}
	if !scheduled {
		return nil, nil
	}
	attempt, found := state.Latest[workflowadapter.CodeEdgePackageAdmission]
	if !found || attempt.ExecutionStatus != store.StageExecutionCompleted || strings.TrimSpace(attempt.ArtifactManifestID) == "" {
		return nil, nil
	}
	references, err := service.core.store.ListArtifactRefs(ctx, attempt.ArtifactManifestID)
	if err != nil {
		return nil, err
	}
	for _, reference := range references {
		if reference.ArtifactKey != workflowadapter.StandardAuthoringPackageAdmissionReportArtifact {
			continue
		}
		if reference.RunID != run.ID || reference.SubjectRevisionID != subject.subjectRevisionID() || reference.SubjectDigest != subject.subjectDigest() || reference.WorkflowFingerprint != run.DefinitionHash {
			return nil, fmt.Errorf("admission report artifact %s does not match frozen run lineage", reference.ID)
		}
		binding := workflowkit.ArtifactBinding{
			Name:          workflowadapter.StandardAuthoringPackageAdmissionReportArtifact,
			ArtifactID:    workflowkit.ArtifactID(reference.ID),
			ContentDigest: workflowkit.Fingerprint(reference.ContentDigest),
			SchemaVersion: reference.SchemaVersion,
		}
		if err := binding.Validate(); err != nil {
			return nil, err
		}
		return []workflowkit.ArtifactBinding{binding}, nil
	}
	return nil, nil
}

func continuationTargets(command normalizedContinuationCommand, run store.WorkflowRun, workflow workflowkit.WorkflowDescriptor, state continuationRunState, allowContentStages bool) ([]workflowkit.NodeID, workflowkit.ContinuationStrategy, error) {
	rejectContent := func(targets []workflowkit.NodeID) error {
		if !allowContentStages {
			return rejectContentContinuationTargets(workflow, targets)
		}
		// A pre-materialization authoring recovery may re-schedule content
		// stages inside the same frozen source/session subject, but CodeEdge
		// evaluator and operator-only stages still require their dedicated
		// lifecycle operations.
		for _, nodeID := range targets {
			stage, exists := workflow.Stage(nodeID)
			if !exists {
				return fmt.Errorf("%w: unknown frozen stage %q", ErrTaskContinuationTarget, nodeID)
			}
			if isCodeEdgeEvaluatorNode(workflow, nodeID) {
				return fmt.Errorf("%w: CodeEdge evaluator stage %q requires TrialExecution reconciliation and cannot use an ordinary stage retry", ErrTaskContinuationTarget, nodeID)
			}
			if stage.OperatorOnly() {
				return fmt.Errorf("%w: stage %q is operator-only and requires its explicit lifecycle operation", ErrTaskContinuationTarget, nodeID)
			}
		}
		return nil
	}
	if len(command.TargetNodeIDs) > 0 {
		for _, nodeID := range command.TargetNodeIDs {
			stage, exists := workflow.Stage(nodeID)
			if !exists {
				return nil, "", fmt.Errorf("%w: unknown frozen stage %q", ErrTaskContinuationTarget, nodeID)
			}
			if stage.OperatorOnly() {
				return nil, "", fmt.Errorf("%w: stage %q is operator-only and requires its explicit lifecycle operation", ErrTaskContinuationTarget, nodeID)
			}
			if latest, exists := state.Latest[nodeID]; exists && latest.ExecutionStatus == store.StageExecutionCompleted && !command.ForceSelected {
				return nil, "", fmt.Errorf("%w: successful stage %q requires force_selected", ErrTaskContinuationTarget, nodeID)
			}
		}
		if err := rejectContent(command.TargetNodeIDs); err != nil {
			return nil, "", err
		}
		if command.ForceSelected {
			return append([]workflowkit.NodeID(nil), command.TargetNodeIDs...), workflowkit.StrategyRecompute, nil
		}
		return append([]workflowkit.NodeID(nil), command.TargetNodeIDs...), workflowkit.StrategyRetryAttempt, nil
	}
	if command.ForceSelected {
		return nil, "", fmt.Errorf("%w: force_selected requires explicit stages", ErrTaskContinuationTarget)
	}
	order, err := workflow.TopologicalStages()
	if err != nil {
		return nil, "", err
	}
	if allowContentStages {
		// A completed content stage with a needs_repair verdict must be
		// recomputed inside the same immutable source/session subject. This is
		// the sanctioned way for an authoring run to open a fresh repair
		// conversation from its durable repair ledger/context artifacts.
		repairTargets := make([]workflowkit.NodeID, 0)
		for _, nodeID := range order {
			latest, exists := state.Latest[nodeID]
			if !exists || latest.ExecutionStatus != store.StageExecutionCompleted || latest.Verdict != store.VerdictNeedsRepair {
				continue
			}
			repairTargets = append(repairTargets, nodeID)
		}
		if len(repairTargets) > 0 {
			if err := rejectContent(repairTargets); err != nil {
				return nil, "", err
			}
			return repairTargets, workflowkit.StrategyRecompute, nil
		}
	}
	targets := make([]workflowkit.NodeID, 0)
	for _, nodeID := range order {
		latest, exists := state.Latest[nodeID]
		if !exists {
			continue
		}
		switch latest.ExecutionStatus {
		case store.StageExecutionInfraFailed, store.StageExecutionInterrupted, store.StageExecutionCanceled:
			targets = append(targets, nodeID)
		case store.StageExecutionQueued, store.StageExecutionRunning:
			if run.Status == store.WorkflowRunPaused {
				targets = append(targets, nodeID)
			}
		}
	}
	if len(targets) > 0 {
		if err := rejectContent(targets); err != nil {
			return nil, "", err
		}
		return targets, workflowkit.StrategyRetryAttempt, nil
	}
	if run.Status == store.WorkflowRunCanceled || run.Status == store.WorkflowRunPaused || run.Status == store.WorkflowRunFailedRecoverable || run.Status == store.WorkflowRunWaitingContinuation || run.Status == store.WorkflowRunInterrupted {
		// No stage attempt may exist when a run is canceled before a worker
		// starts. Selecting the source root is deterministic; missing lineage
		// then conservatively expands the complete affected closure.
		targets := []workflowkit.NodeID{order[0]}
		if err := rejectContent(targets); err != nil {
			return nil, "", err
		}
		return targets, workflowkit.StrategyRetryAttempt, nil
	}
	return nil, "", fmt.Errorf("%w: no failed, interrupted, canceled, or selected stage", ErrTaskContinuationTarget)
}

func rejectContentContinuationTargets(workflow workflowkit.WorkflowDescriptor, targets []workflowkit.NodeID) error {
	for _, nodeID := range targets {
		stage, exists := workflow.Stage(nodeID)
		if !exists {
			return fmt.Errorf("%w: unknown frozen stage %q", ErrTaskContinuationTarget, nodeID)
		}
		if isCodeEdgeEvaluatorNode(workflow, nodeID) {
			return fmt.Errorf("%w: CodeEdge evaluator stage %q requires TrialExecution reconciliation and cannot use an ordinary stage retry", ErrTaskContinuationTarget, nodeID)
		}
		if isContentChangingStage(stage) {
			return fmt.Errorf("%w: stage %q requires a candidate revision and ChangeProvider transaction", ErrTaskContinuationTarget, nodeID)
		}
		if stage.OperatorOnly() {
			return fmt.Errorf("%w: stage %q is operator-only and requires its explicit lifecycle operation", ErrTaskContinuationTarget, nodeID)
		}
	}
	return nil
}

func isContentChangingStage(stage workflowkit.StageDescriptor) bool {
	return stage.Effect == workflowkit.EffectContentProducer || stage.Effect == workflowkit.EffectContentMutator
}

type continuationPlanInput struct {
	Expected                    workflowkit.CheckpointRef
	ExternalEffectConfirmations []workflowkit.ExternalEffectConfirmation
	RequiredScheduledInputs     map[workflowkit.NodeID][]workflowkit.ArtifactBinding
}

// buildSameRunContinuationPlan owns the common immutable transition shape for
// task-revision and pre-materialization authoring recoveries. The latter is
// the one bounded case where content-producing stages remain inside the same
// immutable source/session subject, before materialize_task exists.
func buildSameRunContinuationPlan(planID, commandID string, input continuationPlanInput, run store.WorkflowRun, subjectRevisionID string, subjectDigest workflowkit.SubjectDigest, workflow workflowkit.WorkflowDescriptor, state continuationRunState, invalidation workflowkit.InvalidationPlan, targets []workflowkit.NodeID, strategy workflowkit.ContinuationStrategy, expiresAt time.Time, allowContentStages bool) (workflowkit.ContinuationPlanSnapshot, error) {
	targetSet := make(map[workflowkit.NodeID]struct{}, len(targets))
	for _, target := range targets {
		targetSet[target] = struct{}{}
	}
	entryByNode := make(map[workflowkit.NodeID]workflowkit.InvalidationEntry, len(invalidation.Entries))
	for _, entry := range invalidation.Entries {
		entryByNode[entry.NodeID] = entry
	}
	reuseByNode := make(map[workflowkit.NodeID]workflowkit.StageReuseState, len(state.ReuseStates))
	for _, reuse := range state.ReuseStates {
		reuseByNode[reuse.NodeID] = reuse
	}
	confirmations := make(map[workflowkit.NodeID]workflowkit.ExternalEffectConfirmation, len(input.ExternalEffectConfirmations))
	for _, confirmation := range input.ExternalEffectConfirmations {
		confirmations[confirmation.NodeID] = confirmation
	}
	requiredScheduledInputs := make(map[workflowkit.NodeID][]workflowkit.ArtifactBinding, len(input.RequiredScheduledInputs))
	for nodeID, bindings := range input.RequiredScheduledInputs {
		stage, found := workflow.Stage(nodeID)
		if !found || stage.OperatorOnly() {
			return workflowkit.ContinuationPlanSnapshot{}, fmt.Errorf("required scheduled inputs refer to unavailable stage %q", nodeID)
		}
		copyBindings := append([]workflowkit.ArtifactBinding(nil), bindings...)
		if _, err := workflowkit.FingerprintArtifactBindings(copyBindings); err != nil {
			return workflowkit.ContinuationPlanSnapshot{}, fmt.Errorf("required scheduled inputs for stage %q: %w", nodeID, err)
		}
		requiredScheduledInputs[nodeID] = copyBindings
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		return workflowkit.ContinuationPlanSnapshot{}, err
	}
	transitions := make([]workflowkit.NodeTransition, 0, len(workflow.Stages))
	scheduled := make([]workflowkit.NodeID, 0, len(workflow.Stages))
	externalConfirmations := make([]workflowkit.ExternalEffectConfirmation, 0, len(confirmations))
	for _, stage := range workflow.Stages {
		entry, exists := entryByNode[stage.Key]
		if !exists {
			return workflowkit.ContinuationPlanSnapshot{}, fmt.Errorf("invalidation did not cover stage %q", stage.Key)
		}
		generation := state.Generations[stage.Key]
		transition := workflowkit.NodeTransition{
			NodeID:                   stage.Key,
			FromGeneration:           generation,
			ToGeneration:             generation,
			ExpectedInputFingerprint: emptyInputs,
		}
		if stage.OperatorOnly() {
			// Packaging and other operator-only stages stay visible in every
			// continuation snapshot, but their lifecycle service owns intent,
			// authorization, versioning, idempotency, and reconciliation.
			transition.Disposition = workflowkit.DispositionOperatorOnly
			transition.ReasonCodes = []workflowkit.PlanReason{"operator_only_lifecycle_action"}
			transitions = append(transitions, transition)
			continue
		}
		switch entry.Impact {
		case workflowkit.ImpactPreserve:
			reuse, present := reuseByNode[stage.Key]
			if !present || !reuse.Present || !reuse.ArtifactsIntact {
				return workflowkit.ContinuationPlanSnapshot{}, fmt.Errorf("invalidation attempted to preserve unavailable stage %q", stage.Key)
			}
			transition.Disposition = workflowkit.DispositionPreserve
			transition.ReasonCodes = []workflowkit.PlanReason{"artifact_reused"}
			transition.ExpectedInputFingerprint = reuse.ExpectedInputFingerprint
			transition.InputBindings = append([]workflowkit.ArtifactBinding(nil), reuse.CurrentInputs...)
		case workflowkit.ImpactInvalidate:
			if !allowContentStages && isContentChangingStage(stage) {
				return workflowkit.ContinuationPlanSnapshot{}, fmt.Errorf("%w: invalidation would schedule content-changing stage %q without a candidate revision", ErrTaskContinuationTarget, stage.Key)
			}
			transition.Disposition = workflowkit.DispositionSchedule
			transition.ReasonCodes = continuationReasons(entry, targetSet, strategy)
			if required, present := requiredScheduledInputs[stage.Key]; present {
				transition.InputBindings = append([]workflowkit.ArtifactBinding(nil), required...)
				transition.ExpectedInputFingerprint, err = workflowkit.FingerprintArtifactBindings(transition.InputBindings)
				if err != nil {
					return workflowkit.ContinuationPlanSnapshot{}, err
				}
				delete(requiredScheduledInputs, stage.Key)
			}
			if strategy == workflowkit.StrategyRecompute {
				transition.ToGeneration++
			}
			scheduled = append(scheduled, stage.Key)
		case workflowkit.ImpactRequiresConfirmation:
			confirmation, confirmed := confirmations[stage.Key]
			if confirmed {
				transition.Disposition = workflowkit.DispositionSchedule
				transition.ReasonCodes = continuationReasons(entry, targetSet, strategy)
				if required, present := requiredScheduledInputs[stage.Key]; present {
					transition.InputBindings = append([]workflowkit.ArtifactBinding(nil), required...)
					transition.ExpectedInputFingerprint, err = workflowkit.FingerprintArtifactBindings(transition.InputBindings)
					if err != nil {
						return workflowkit.ContinuationPlanSnapshot{}, err
					}
					delete(requiredScheduledInputs, stage.Key)
				}
				if strategy == workflowkit.StrategyRecompute {
					transition.ToGeneration++
				}
				scheduled = append(scheduled, stage.Key)
				externalConfirmations = append(externalConfirmations, confirmation)
			} else {
				transition.Disposition = workflowkit.DispositionInvalidate
				transition.ReasonCodes = []workflowkit.PlanReason{"external_confirmation_required"}
			}
		case workflowkit.ImpactOperatorOnly:
			return workflowkit.ContinuationPlanSnapshot{}, fmt.Errorf("operator-only invalidation impact on automatically dispatchable stage %q", stage.Key)
		default:
			return workflowkit.ContinuationPlanSnapshot{}, fmt.Errorf("unsupported invalidation impact %q", entry.Impact)
		}
		transitions = append(transitions, transition)
	}
	if len(requiredScheduledInputs) != 0 {
		for nodeID := range requiredScheduledInputs {
			return workflowkit.ContinuationPlanSnapshot{}, fmt.Errorf("required repair input stage %q is not scheduled", nodeID)
		}
	}
	for nodeID := range confirmations {
		found := false
		for _, transition := range transitions {
			if transition.NodeID == nodeID && transition.Disposition == workflowkit.DispositionSchedule {
				found = true
				break
			}
		}
		if !found {
			return workflowkit.ContinuationPlanSnapshot{}, fmt.Errorf("external effect confirmation for non-scheduled stage %q", nodeID)
		}
	}
	schedule, err := sequentialSchedule(workflow, scheduled)
	if err != nil {
		return workflowkit.ContinuationPlanSnapshot{}, err
	}
	checkpointFingerprint, err := continuationCheckpointFingerprint(input.Expected)
	if err != nil {
		return workflowkit.ContinuationPlanSnapshot{}, err
	}
	return workflowkit.ContinuationPlanSnapshot{
		PlanID:                      planID,
		CommandID:                   commandID,
		Strategy:                    strategy,
		BaseCheckpoint:              input.Expected,
		NextExecutionEpoch:          run.ExecutionEpoch + 1,
		SourceRunID:                 run.ID,
		TargetRunRelation:           workflowkit.RelationSameRunAttempt,
		SubjectRevisionID:           subjectRevisionID,
		SubjectDigest:               subjectDigest,
		Nodes:                       transitions,
		Schedule:                    schedule,
		Assertions:                  []workflowkit.PlanAssertion{{Kind: workflowkit.AssertionCheckpointCurrent, Subject: run.ID, Expected: checkpointFingerprint}},
		ExternalEffectConfirmations: externalConfirmations,
		ExpiresAt:                   expiresAt.UTC(),
	}, nil
}

func continuationReasons(entry workflowkit.InvalidationEntry, targets map[workflowkit.NodeID]struct{}, strategy workflowkit.ContinuationStrategy) []workflowkit.PlanReason {
	reasons := make([]workflowkit.PlanReason, 0, len(entry.Reasons)+1)
	if _, selected := targets[entry.NodeID]; selected {
		if strategy == workflowkit.StrategyRecompute {
			reasons = append(reasons, "force_recompute")
		} else {
			reasons = append(reasons, "retry_requested")
		}
	}
	for _, reason := range entry.Reasons {
		reasons = append(reasons, workflowkit.PlanReason(reason))
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "invalidation_required")
	}
	sort.Slice(reasons, func(left, right int) bool { return reasons[left] < reasons[right] })
	unique := reasons[:0]
	for _, reason := range reasons {
		if len(unique) == 0 || unique[len(unique)-1] != reason {
			unique = append(unique, reason)
		}
	}
	return unique
}

func sequentialSchedule(workflow workflowkit.WorkflowDescriptor, scheduled []workflowkit.NodeID) ([]workflowkit.ScheduleBatch, error) {
	wanted := make(map[workflowkit.NodeID]struct{}, len(scheduled))
	for _, nodeID := range scheduled {
		wanted[nodeID] = struct{}{}
	}
	order, err := workflow.TopologicalStages()
	if err != nil {
		return nil, err
	}
	batches := make([]workflowkit.ScheduleBatch, 0, len(scheduled))
	for _, nodeID := range order {
		if _, schedule := wanted[nodeID]; !schedule {
			continue
		}
		batches = append(batches, workflowkit.ScheduleBatch{ID: fmt.Sprintf("stage-%03d-%s", len(batches)+1, nodeID), NodeIDs: []workflowkit.NodeID{nodeID}})
	}
	return batches, nil
}

func continuationCheckpointFingerprint(checkpoint workflowkit.CheckpointRef) (workflowkit.Fingerprint, error) {
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes("harbor.task-continuation-checkpoint.v1", encoded)
}
