package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringHandoffCommandType          = "standard_authoring.handoff"
	standardAuthoringHandoffRedriveCommandType   = "standard_authoring.handoff.redrive"
	standardAuthoringHandoffReconcileCommandType = "standard_authoring.handoff.reconcile"
	standardAuthoringHandoffPayloadFormat        = "harbor.standard-authoring-handoff-job.v1"
	standardAuthoringHandoffRunTrigger           = "standard-authoring.materialized"
)

var (
	// ErrCodeEdgePhase1DefinitionUnavailable is intentionally distinct from a
	// generic Run-start error. A persisted authoring_task_handoff remains
	// inspectable and retryable when this deployment has not installed the
	// closed Phase-1 definition; the runtime must never invent a profile,
	// provider, catalog, or external operation as a fallback.
	ErrCodeEdgePhase1DefinitionUnavailable = errors.New("CodeEdge Phase-1 run definition is unavailable")
	// ErrCodeEdgePhase1DefinitionInvalid masks deployment-provider details at
	// this application boundary. Definition providers may inspect local
	// deployment state, but an invalid result must not become a caller-visible
	// source of endpoint, secret, or filesystem information.
	ErrCodeEdgePhase1DefinitionInvalid = errors.New("CodeEdge Phase-1 run definition did not pass controlled validation")
)

// CodeEdgePhase1RunDefinitionRequest contains the complete immutable bridge
// from Standard authoring to its task-bound child. It deliberately contains no
// user-selected profile/specification, command, image, model, endpoint,
// credential, or workspace path. Deployment composition is the only source of
// the returned definition.
type CodeEdgePhase1RunDefinitionRequest struct {
	TaskID             string
	RevisionID         string
	RevisionDigest     workflowkit.SubjectDigest
	AuthoringRunID     string
	AuthoringSourceID  string
	AuthoringSessionID string
	TaskSnapshot       workflowadapter.ArtifactReference
}

// CodeEdgePhase1RunDefinition is the closed child definition supplied by a
// deployment-owned provider. StartRun rebinds its intrinsic task_snapshot to
// a fresh managed Run input after proving it is byte-identical to TaskSnapshot;
// the provider therefore never obtains an object-store path or mutable task
// directory.
type CodeEdgePhase1RunDefinition struct {
	Profile       workflowadapter.ExecutionProfile
	ExecutionSpec workflowadapter.RunExecutionSpec
}

// CodeEdgePhase1RunDefinitionProvider is implemented only by controlled
// deployment composition. CLI/TUI and source-authoring executors never create
// one from user input or ambient command defaults.
type CodeEdgePhase1RunDefinitionProvider interface {
	DefinitionForCodeEdgePhase1Run(context.Context, CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error)
}

// StandardAuthoringHandoffRequest is the durable job's closed input. All IDs
// are allocated before the job is published; retries reuse the exact child Run
// identity rather than allocating a second run after a crash.
type StandardAuthoringHandoffRequest struct {
	AuthoringRunID    string
	StageAttemptID    string
	HandoffArtifactID string
	ChildRunID        string
	Actor             string
	Reason            string
}

// RedriveStandardAuthoringHandoffCommand is the explicit operator action for
// a handoff delivery held in_doubt because a controlled Phase-1 definition was
// unavailable or invalid. It publishes a separate durable delivery record;
// it never rewrites the original in_doubt fact or allocates a new child Run.
// The caller-provided key must be a UUIDv7 so a lost reply can be replayed
// without creating another redrive delivery.
type RedriveStandardAuthoringHandoffCommand struct {
	AuthoringRunID string
	IdempotencyKey string
	Actor          string
	Reason         string
}

// ReconcileStandardAuthoringHandoffCommand explicitly authorizes a new,
// idempotent delivery attempt after an in_doubt handoff such as a lost worker
// fence. It reuses the original immutable payload and preallocated child Run
// identity; the original failure record remains an immutable historical fact.
type ReconcileStandardAuthoringHandoffCommand struct {
	AuthoringRunID string
	IdempotencyKey string
	Actor          string
	Reason         string
}

// standardAuthoringHandoffPayload is intentionally small enough to be carried
// by a durable local job. The artifact itself is re-read and fully verified at
// execution time; this payload is an address, not an authority to fabricate a
// task/revision handoff.
type standardAuthoringHandoffPayload struct {
	Format            string `json:"format"`
	AuthoringRunID    string `json:"authoring_run_id"`
	StageAttemptID    string `json:"stage_attempt_id"`
	HandoffArtifactID string `json:"handoff_artifact_id"`
	ChildRunID        string `json:"child_run_id"`
}

// StandardAuthoringHandoffService consumes a persisted materialize_task
// receipt and freezes the independent task-bound CodeEdge Phase-1 Run. It is
// intentionally an application lifecycle service, not an executor callback:
// Stage output persistence is the authority boundary and this service never
// starts task-bound work under the AuthoringSession subject.
type StandardAuthoringHandoffService struct {
	core        *lifecycleServiceCore
	definitions CodeEdgePhase1RunDefinitionProvider
}

// Available reports whether a deployment has supplied the closed child
// definition. False is fail-closed, not a reason to construct a generic
// Phase-1 profile from the authoring Run.
func (service *StandardAuthoringHandoffService) Available() bool {
	return service != nil && service.core != nil && service.core.store != nil && service.definitions != nil
}

// Redrive explicitly republishes an in_doubt Standard authoring handoff after
// a controlled deployment definition has been installed or corrected. It is
// deliberately not called by worker recovery: absence of a definition is a
// deployment decision, not a transient execution failure.
func (service *StandardAuthoringHandoffService) Redrive(ctx context.Context, command RedriveStandardAuthoringHandoffCommand) (store.DurableJob, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.DurableJob{}, fmt.Errorf("Standard authoring handoff service is not configured")
	}
	if !service.Available() {
		return store.DurableJob{}, ErrCodeEdgePhase1DefinitionUnavailable
	}
	return service.publishRecovery(ctx, standardAuthoringHandoffRedriveCommandType, "redrive", command.AuthoringRunID, command.IdempotencyKey, command.Actor, command.Reason, true)
}

// Reconcile explicitly republishes an in_doubt handoff whose outcome must be
// checked after a lost worker fence or another unknown delivery. Unlike
// Redrive, reconciliation does not require a currently installed definition:
// a preallocated child Run may already exist and can be proven/replayed by the
// controlled worker without constructing a new identity. Any still-missing
// dependency is recorded by the new delivery with its own safe diagnosis.
func (service *StandardAuthoringHandoffService) Reconcile(ctx context.Context, command ReconcileStandardAuthoringHandoffCommand) (store.DurableJob, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.DurableJob{}, fmt.Errorf("Standard authoring handoff service is not configured")
	}
	return service.publishRecovery(ctx, standardAuthoringHandoffReconcileCommandType, "reconcile", command.AuthoringRunID, command.IdempotencyKey, command.Actor, command.Reason, false)
}

func (service *StandardAuthoringHandoffService) publishRecovery(ctx context.Context, commandType, action, authoringRunID, idempotencyKey, actor, reason string, requireRecoverableFailure bool) (store.DurableJob, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return store.DurableJob{}, err
	}
	runID := strings.TrimSpace(authoringRunID)
	key := strings.TrimSpace(idempotencyKey)
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(reason)
	if err := store.ValidateUUIDv7(runID); err != nil {
		return store.DurableJob{}, fmt.Errorf("Standard authoring handoff %s Run ID: %w", action, err)
	}
	if err := store.ValidateUUIDv7(key); err != nil {
		return store.DurableJob{}, fmt.Errorf("Standard authoring handoff %s idempotency key: %w", action, err)
	}
	if actor == "" || reason == "" {
		return store.DurableJob{}, fmt.Errorf("Standard authoring handoff %s actor and reason are required", action)
	}
	original, payload, err := standardAuthoringHandoffJobForRun(ctx, service.core.store, runID)
	if err != nil {
		return store.DurableJob{}, err
	}
	recoveryKey := "standard-authoring-handoff-" + action + ":" + original.ID + ":" + key
	if existing, err := service.core.store.GetDurableJobByIdempotency(ctx, recoveryKey); err != nil {
		return store.DurableJob{}, err
	} else if existing != nil {
		existingPayload, payloadErr := standardAuthoringHandoffJobPayload(*existing)
		if payloadErr != nil || existing.CommandType != commandType ||
			existingPayload != payload {
			return store.DurableJob{}, fmt.Errorf("existing Standard authoring handoff %s does not match its immutable payload", action)
		}
		return *existing, nil
	}
	if original.State != store.JobInDoubt {
		return store.DurableJob{}, fmt.Errorf("Standard authoring handoff %s is %s, not eligible for explicit %s", original.ID, original.State, action)
	}
	if requireRecoverableFailure && !isRecoverableHandoffFailure(original.Failure) {
		return store.DurableJob{}, fmt.Errorf("Standard authoring handoff %s has failure %q, not eligible for explicit redrive", original.ID, durableJobFailureCode(original.Failure))
	}
	jobs, err := service.core.store.ListDurableJobsForRun(ctx, original.RunID)
	if err != nil {
		return store.DurableJob{}, err
	}
	if standardAuthoringHandoffFailureResolved(original, jobs) {
		return store.DurableJob{}, fmt.Errorf("Standard authoring handoff %s has already recovered successfully", original.ID)
	}
	if err := rejectDeterministicHandoffRecovery(original, payload, jobs); err != nil {
		return store.DurableJob{}, err
	}
	return service.core.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: commandType, EntityType: original.EntityType, EntityID: original.EntityID,
		RunID: original.RunID, StageAttemptID: original.StageAttemptID, Priority: original.Priority, PayloadJSON: original.PayloadJSON,
		IdempotencyKey: recoveryKey, Actor: actor, Reason: reason,
	})
}

// rejectDeterministicHandoffRecovery prevents an immutable original
// in_doubt record from becoming an accidental retry token after a later
// recovery delivery has conclusively failed validation. The later failure is
// the newest durable fact and requires repair or a new Run, not another
// ordinary redrive/reconcile attempt.
func rejectDeterministicHandoffRecovery(original store.DurableJob, payload standardAuthoringHandoffPayload, jobs []store.DurableJob) error {
	for _, job := range jobs {
		if job.CommandType != standardAuthoringHandoffRedriveCommandType && job.CommandType != standardAuthoringHandoffReconcileCommandType {
			continue
		}
		candidatePayload, payloadErr := standardAuthoringHandoffJobPayload(job)
		if payloadErr != nil {
			return fmt.Errorf("existing Standard authoring handoff recovery job %s has an invalid immutable payload: %w", job.ID, payloadErr)
		}
		if candidatePayload != payload || job.State != store.JobFailed {
			continue
		}
		return fmt.Errorf("Standard authoring handoff %s has deterministic recovery failure %q; repair the source or create a new run", original.ID, durableJobFailureCode(job.Failure))
	}
	return nil
}

// Consume creates or replays exactly one child Run. It accepts only the
// durable job coordinates, reloads the persisted handoff artifact, and checks
// source/session/materialization/revision lineage before asking the deployment
// provider for the frozen Phase-1 definition.
func (service *StandardAuthoringHandoffService) Consume(ctx context.Context, request StandardAuthoringHandoffRequest) (store.WorkflowRun, error) {
	if service == nil || service.core == nil || service.core.store == nil || service.core.objects == nil {
		return store.WorkflowRun{}, fmt.Errorf("Standard authoring handoff service is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return store.WorkflowRun{}, err
	}
	for _, identity := range []struct {
		label string
		value string
	}{
		{"authoring Run", request.AuthoringRunID},
		{"materialize stage attempt", request.StageAttemptID},
		{"authoring task handoff artifact", request.HandoffArtifactID},
		{"CodeEdge Phase-1 child Run", request.ChildRunID},
	} {
		if err := store.ValidateUUIDv7(strings.TrimSpace(identity.value)); err != nil {
			return store.WorkflowRun{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The Standard authoring handoff identifiers do not match its persisted lineage.", "identity", fmt.Errorf("Standard authoring handoff %s: %w", identity.label, err))
		}
	}

	run, err := service.core.store.GetWorkflowRun(ctx, request.AuthoringRunID)
	if err != nil {
		return store.WorkflowRun{}, handoffStorageFailure(request, "authoring_run", err)
	}
	if run == nil {
		return store.WorkflowRun{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The Standard authoring handoff does not name its persisted authoring run.", "authoring_run", fmt.Errorf("%w: Standard authoring Run %s", ErrLifecycleNotFound, request.AuthoringRunID))
	}
	if !isCurrentStandardAuthoringRun(*run) || run.SubjectKind != store.WorkflowRunSubjectAuthoringSession {
		return store.WorkflowRun{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The Standard authoring handoff run does not match its frozen source lineage.", "authoring_run", fmt.Errorf("Standard authoring handoff Run is not %s@%s", workflowadapter.StandardAuthoringWorkflowTemplateID, workflowadapter.StandardAuthoringWorkflowTemplateVersion))
	}
	if err := service.core.verifyRunDeploymentCatalogReceipt(*run); err != nil {
		return store.WorkflowRun{}, handoffDefinitionFailure(request, handoffDefinitionInvalidCode, "The controlled CodeEdge Phase-1 definition is invalid.", fmt.Errorf("verify frozen Standard authoring deployment catalog receipt: %w", err))
	}
	subject, err := service.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return store.WorkflowRun{}, handoffStorageFailure(request, "authoring_subject", err)
	}
	if !subject.isAuthoringSession() {
		return store.WorkflowRun{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The Standard authoring handoff run has no matching source session.", "authoring_subject", fmt.Errorf("Standard authoring handoff Run has no source/session subject"))
	}

	handoff, snapshot, err := service.readPersistedHandoff(ctx, *run, subject, request)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if err := service.validateMaterializedHandoff(ctx, *run, subject, handoff, snapshot, request); err != nil {
		return store.WorkflowRun{}, err
	}
	handoffFingerprint, err := handoff.Fingerprint()
	if err != nil {
		return store.WorkflowRun{}, handoffDeterministicFailure(request, handoffMaterializationInvalidCode, "The Standard authoring handoff materialization is invalid.", "handoff_fingerprint", err)
	}
	durableHandoff, err := service.core.store.PrepareAuthoringPhase1Handoff(ctx, store.PrepareAuthoringPhase1HandoffRequest{
		AuthoringRunID:     run.ID,
		AuthoringSessionID: handoff.AuthoringSessionID,
		AuthoringSourceID:  handoff.AuthoringSourceID,
		HandoffArtifactID:  request.HandoffArtifactID,
		HandoffFingerprint: string(handoffFingerprint),
		TaskID:             handoff.TaskID,
		RevisionID:         handoff.RevisionID,
		TaskDigest:         string(handoff.RevisionDigest),
		ChildRunID:         request.ChildRunID,
		IdempotencyKey:     "standard-authoring-phase1-handoff:" + run.ID,
		Actor:              strings.TrimSpace(request.Actor),
		Reason:             strings.TrimSpace(request.Reason),
	})
	if err != nil {
		if handoffDeterministicStoreConflict(err) {
			return store.WorkflowRun{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The durable Standard authoring handoff record conflicts with its artifact lineage.", "prepare_handoff", err)
		}
		return store.WorkflowRun{}, handoffStorageFailure(request, "prepare_handoff", err)
	}
	if err := validateDurableAuthoringPhase1Handoff(durableHandoff, *run, subject, handoff, request.HandoffArtifactID, handoffFingerprint); err != nil {
		return store.WorkflowRun{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The durable Standard authoring handoff record does not match its artifact lineage.", "durable_handoff", err)
	}
	if existing, lookupErr := service.core.store.GetWorkflowRun(ctx, durableHandoff.ChildRunID); lookupErr != nil {
		return store.WorkflowRun{}, handoffStorageFailure(request, "child_run", lookupErr)
	} else if existing != nil {
		if err := validateExistingAuthoringPhase1Child(*existing, durableHandoff, *run, handoff); err != nil {
			return store.WorkflowRun{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The existing CodeEdge Phase-1 child run does not match the handoff lineage.", "child_run", err)
		}
		return *existing, nil
	}
	if service.definitions == nil {
		// The prepared record and the original handoff artifact remain durable.
		// A later controlled composition can consume the same record and its
		// preallocated child identity without rerunning materialize_task.
		return store.WorkflowRun{}, handoffDefinitionFailure(request, handoffDefinitionUnavailableCode, "CodeEdge Phase-1 run definition is unavailable.", ErrCodeEdgePhase1DefinitionUnavailable)
	}

	definition, err := service.definitions.DefinitionForCodeEdgePhase1Run(ctx, CodeEdgePhase1RunDefinitionRequest{
		TaskID:             handoff.TaskID,
		RevisionID:         handoff.RevisionID,
		RevisionDigest:     handoff.RevisionDigest,
		AuthoringRunID:     run.ID,
		AuthoringSourceID:  handoff.AuthoringSourceID,
		AuthoringSessionID: handoff.AuthoringSessionID,
		TaskSnapshot:       handoff.TaskSnapshot,
	})
	if err != nil {
		return store.WorkflowRun{}, handoffDefinitionFailure(request, handoffDefinitionInvalidCode, "The controlled CodeEdge Phase-1 definition is invalid.", fmt.Errorf("%w: %v", ErrCodeEdgePhase1DefinitionInvalid, err))
	}
	if err := validateCodeEdgePhase1HandoffDefinition(definition, handoff); err != nil {
		return store.WorkflowRun{}, handoffDefinitionFailure(request, handoffDefinitionInvalidCode, "The controlled CodeEdge Phase-1 definition is invalid.", err)
	}

	child, err := (&RunService{core: service.core}).StartRun(ctx, StartRunRequest{
		ID:                       durableHandoff.ChildRunID,
		TaskID:                   handoff.TaskID,
		RevisionID:               handoff.RevisionID,
		Profile:                  definition.Profile,
		ExecutionSpec:            definition.ExecutionSpec,
		ParentRunID:              run.ID,
		authoringPhase1HandoffID: durableHandoff.ID,
		Trigger:                  standardAuthoringHandoffRunTrigger,
		ExecutionEpoch:           0,
		Actor:                    strings.TrimSpace(request.Actor),
		Reason:                   strings.TrimSpace(request.Reason),
	})
	if err != nil {
		if handoffDeterministicStoreConflict(err) {
			return store.WorkflowRun{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The CodeEdge Phase-1 child run conflicts with the handoff lineage.", "create_child_run", err)
		}
		return store.WorkflowRun{}, handoffStorageFailure(request, "create_child_run", err)
	}
	if err := validateExistingAuthoringPhase1Child(child, durableHandoff, *run, handoff); err != nil {
		return store.WorkflowRun{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The CodeEdge Phase-1 child run does not match the handoff lineage.", "child_run", err)
	}
	return child, nil
}

// handoffDeterministicStoreConflict identifies Store contract failures that
// cannot become valid by retrying the same immutable handoff delivery. They
// must remain failed rather than exposing an inappropriate redrive action.
func handoffDeterministicStoreConflict(err error) bool {
	return errors.Is(err, store.ErrIdempotencyConflict) ||
		errors.Is(err, store.ErrIdentityCollision) ||
		errors.Is(err, store.ErrImmutable) ||
		errors.Is(err, store.ErrInvalidTransition) ||
		errors.Is(err, store.ErrInvalidUUIDv7Identity)
}

func validateDurableAuthoringPhase1Handoff(record store.AuthoringPhase1Handoff, run store.WorkflowRun, subject workflowRunSubject, handoff workflowadapter.StandardAuthoringTaskHandoff, artifactID string, fingerprint workflowkit.Fingerprint) error {
	if record.AuthoringRunID != run.ID || record.AuthoringSessionID != subject.AuthoringSession.ID || record.AuthoringSourceID != subject.AuthoringSource.ID ||
		record.HandoffArtifactID != artifactID || record.HandoffFingerprint != string(fingerprint) || record.TaskID != handoff.TaskID ||
		record.RevisionID != handoff.RevisionID || record.TaskDigest != string(handoff.RevisionDigest) {
		return fmt.Errorf("%w: durable Standard authoring Phase-1 handoff differs from persisted receipt", store.ErrIdempotencyConflict)
	}
	return nil
}

func validateExistingAuthoringPhase1Child(child store.WorkflowRun, record store.AuthoringPhase1Handoff, parent store.WorkflowRun, handoff workflowadapter.StandardAuthoringTaskHandoff) error {
	if child.ID != record.ChildRunID || child.ParentRunID != parent.ID || child.SubjectKind != store.WorkflowRunSubjectTaskRevision ||
		child.TaskID != handoff.TaskID || child.RevisionID != handoff.RevisionID || child.SubjectDigest != string(handoff.RevisionDigest) ||
		child.WorkflowTemplateID != workflowadapter.CodeEdgePhase1WorkflowTemplateID || child.WorkflowTemplateVersion != workflowadapter.CodeEdgePhase1WorkflowTemplateVersion ||
		child.Trigger != standardAuthoringHandoffRunTrigger {
		return fmt.Errorf("%w: Standard authoring handoff child Run does not match frozen lineage", store.ErrIdempotencyConflict)
	}
	return nil
}

func validateCodeEdgePhase1HandoffDefinition(definition CodeEdgePhase1RunDefinition, handoff workflowadapter.StandardAuthoringTaskHandoff) error {
	if !definition.Profile.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) ||
		!definition.ExecutionSpec.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return fmt.Errorf("%w: definition templates do not bind the CodeEdge Phase-1 template", ErrCodeEdgePhase1DefinitionInvalid)
	}
	selection, err := handoff.ChildSelection()
	if err != nil {
		return err
	}
	actualSelection, err := definition.ExecutionSpec.Selection.Canonical()
	if err != nil || actualSelection != selection {
		if err != nil {
			return fmt.Errorf("%w: canonicalize definition selection: %v", ErrCodeEdgePhase1DefinitionInvalid, err)
		}
		return fmt.Errorf("%w: definition selection differs from the immutable handoff", ErrCodeEdgePhase1DefinitionInvalid)
	}
	if err := definition.Profile.Validate(); err != nil {
		return fmt.Errorf("%w: validate definition profile: %v", ErrCodeEdgePhase1DefinitionInvalid, err)
	}
	if err := definition.ExecutionSpec.Validate(); err != nil {
		return fmt.Errorf("%w: validate definition execution specification: %v", ErrCodeEdgePhase1DefinitionInvalid, err)
	}
	return nil
}

// readPersistedHandoff verifies the exact completed materialize_task output
// from its ArtifactRef and immutable manifest before parsing it. In
// particular, a caller cannot pass JSON copied from a mutable workspace or a
// similarly named artifact from another Run/attempt.
func (service *StandardAuthoringHandoffService) readPersistedHandoff(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, request StandardAuthoringHandoffRequest) (workflowadapter.StandardAuthoringTaskHandoff, workflowadapter.ArtifactReference, error) {
	expectedSchema, err := workflowadapter.StandardAuthoringTaskHandoffSchemaForTemplate(workflowadapter.TemplateReference{ID: run.WorkflowTemplateID, Version: run.WorkflowTemplateVersion})
	if err != nil {
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDefinitionFailure(request, handoffDefinitionInvalidCode, "The controlled Standard authoring handoff definition is invalid.", err)
	}
	attempt, err := service.core.store.GetStageAttempt(ctx, request.StageAttemptID)
	if err != nil {
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffStorageFailure(request, "materialize_stage", err)
	}
	if attempt == nil || attempt.RunID != run.ID || attempt.StageKey != workflowadapter.MaterializeTask ||
		attempt.ExecutionStatus != store.StageExecutionCompleted ||
		(attempt.Verdict != store.VerdictPass && attempt.Verdict != store.VerdictAdvisory) || strings.TrimSpace(attempt.ArtifactManifestID) == "" {
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The Standard authoring handoff does not reference a completed materialize stage.", "materialize_stage", fmt.Errorf("Standard authoring handoff does not reference a completed materialize_task stage"))
	}
	reference, err := service.core.store.GetArtifactRef(ctx, request.HandoffArtifactID)
	if err != nil {
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffStorageFailure(request, "handoff_artifact", err)
	}
	if reference == nil || reference.ManifestID != attempt.ArtifactManifestID || reference.RunID != run.ID ||
		reference.StageKey != workflowadapter.MaterializeTask || reference.AttemptID != attempt.ID ||
		reference.ArtifactKey != workflowadapter.StandardAuthoringTaskHandoffArtifact || reference.SchemaVersion != expectedSchema {
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The Standard authoring handoff artifact does not match its frozen lineage.", "handoff_artifact", fmt.Errorf("Standard authoring handoff artifact does not match frozen materialize_task lineage"))
	}
	index, err := loadStageArtifactManifestIndex(ctx, service.core.store, reference.ManifestID)
	if err != nil {
		if errors.Is(err, errStageArtifactStorageUnavailable) {
			return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffStorageFailure(request, "artifact_manifest", err)
		}
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The Standard authoring handoff artifact manifest is invalid.", "artifact_manifest", err)
	}
	candidate := stageArtifactCandidate{attempt: *attempt, ref: *reference}
	if err := verifyStageArtifactCandidateWithManifestForSubject(ctx, service.core.objects, index, run, subject, candidate); err != nil {
		if errors.Is(err, errStageArtifactStorageUnavailable) {
			return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffStorageFailure(request, "artifact_lineage", err)
		}
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The Standard authoring handoff artifact does not pass lineage verification.", "artifact_lineage", err)
	}
	object, err := index.objectFor(*reference)
	if err != nil {
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The Standard authoring handoff artifact object is invalid.", "artifact_object", err)
	}
	raw, err := service.core.objects.ReadAll(ctx, object)
	if err != nil {
		if artifactObjectUnavailable(err) {
			return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffMaterializationInvalidCode, "The persisted Standard authoring handoff artifact is missing or invalid.", "artifact_object", fmt.Errorf("read persisted Standard authoring handoff: %w", err))
		}
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffStorageFailure(request, "artifact_object", fmt.Errorf("read persisted Standard authoring handoff: %w", err))
	}
	handoff, err := workflowadapter.ParseStandardAuthoringTaskHandoffJSON(raw)
	if err != nil {
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffMaterializationInvalidCode, "The persisted Standard authoring handoff document is invalid.", "handoff_document", err)
	}
	if handoff.AdmissionReceipt != nil {
		admission, admissionErr := service.core.store.GetArtifactRef(ctx, string(handoff.AdmissionReceipt.ID))
		if admissionErr != nil {
			return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffStorageFailure(request, "admission_receipt", admissionErr)
		}
		if admission == nil || admission.RunID != run.ID || admission.StageKey != workflowadapter.CodeEdgePackageAdmission ||
			admission.ContentDigest != string(handoff.AdmissionReceipt.ContentDigest) || admission.SchemaVersion != handoff.AdmissionReceipt.SchemaVersion {
			return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffAdmissionReceiptMissingCode, "The Standard authoring admission receipt is missing or does not match its handoff.", "admission_receipt", fmt.Errorf("Standard authoring handoff admission receipt is not persisted"))
		}
	}

	references, err := service.core.store.ListArtifactRefs(ctx, reference.ManifestID)
	if err != nil {
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffStorageFailure(request, "task_snapshot", err)
	}
	var snapshot *store.ArtifactRef
	for index := range references {
		candidate := &references[index]
		if candidate.ArtifactKey != "task_snapshot" {
			continue
		}
		if snapshot != nil {
			return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffSnapshotDigestMismatchCode, "The Standard authoring task snapshot does not have one stable digest.", "task_snapshot", fmt.Errorf("Standard authoring materialize_task has duplicate task_snapshot artifacts"))
		}
		snapshot = candidate
	}
	if snapshot == nil || snapshot.ID != string(handoff.TaskSnapshot.ID) || snapshot.ContentDigest != string(handoff.TaskSnapshot.ContentDigest) ||
		snapshot.SchemaVersion != handoff.TaskSnapshot.SchemaVersion || snapshot.RunID != run.ID || snapshot.StageKey != workflowadapter.MaterializeTask ||
		snapshot.AttemptID != attempt.ID || snapshot.SubjectRevisionID != subject.subjectRevisionID() || snapshot.SubjectDigest != subject.subjectDigest() ||
		snapshot.WorkflowFingerprint != run.DefinitionHash {
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffSnapshotDigestMismatchCode, "The Standard authoring task snapshot digest does not match its persisted artifact.", "task_snapshot", fmt.Errorf("Standard authoring handoff task_snapshot does not name its persisted stage artifact"))
	}
	snapshotCandidate := stageArtifactCandidate{attempt: *attempt, ref: *snapshot}
	if err := verifyStageArtifactCandidateWithManifestForSubject(ctx, service.core.objects, index, run, subject, snapshotCandidate); err != nil {
		if errors.Is(err, errStageArtifactStorageUnavailable) {
			return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffStorageFailure(request, "task_snapshot", err)
		}
		return workflowadapter.StandardAuthoringTaskHandoff{}, workflowadapter.ArtifactReference{}, handoffDeterministicFailure(request, handoffSnapshotDigestMismatchCode, "The Standard authoring task snapshot does not pass digest verification.", "task_snapshot", fmt.Errorf("verify Standard authoring handoff task_snapshot artifact: %w", err))
	}
	return handoff, handoff.TaskSnapshot, nil
}

func (service *StandardAuthoringHandoffService) validateMaterializedHandoff(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, handoff workflowadapter.StandardAuthoringTaskHandoff, snapshot workflowadapter.ArtifactReference, request StandardAuthoringHandoffRequest) error {
	if err := handoff.Validate(); err != nil {
		return handoffDeterministicFailure(request, handoffMaterializationInvalidCode, "The Standard authoring handoff materialization is invalid.", "handoff_document", err)
	}
	if handoff.AuthoringRunID != run.ID || handoff.AuthoringSourceID != subject.AuthoringSource.ID ||
		handoff.AuthoringSessionID != subject.AuthoringSession.ID || handoff.AuthoringSourceDigest != workflowkit.SubjectDigest(subject.subjectDigest()) ||
		handoff.TaskID != subject.TargetTask.ID || handoff.TaskSnapshot != snapshot {
		return handoffDeterministicFailure(request, handoffArtifactLineageInvalidCode, "The Standard authoring handoff does not match its source lineage.", "source_lineage", fmt.Errorf("Standard authoring handoff does not match its source/session Run"))
	}
	materialization, err := service.core.store.GetAuthoringTaskMaterializationForRun(ctx, run.ID)
	if err != nil {
		return handoffStorageFailure(request, "materialization", err)
	}
	if materialization == nil || materialization.SessionID != subject.AuthoringSession.ID || materialization.SourceID != subject.AuthoringSource.ID ||
		materialization.TaskID != handoff.TaskID || materialization.RevisionID != handoff.RevisionID || materialization.TaskDigest != string(handoff.RevisionDigest) {
		return handoffDeterministicFailure(request, handoffMaterializationInvalidCode, "The Standard authoring handoff has no matching task materialization.", "materialization", fmt.Errorf("Standard authoring handoff has no matching durable task materialization"))
	}
	revision, err := service.core.store.GetTaskRevision(ctx, handoff.RevisionID)
	if err != nil {
		return handoffStorageFailure(request, "revision", err)
	}
	if revision == nil || revision.TaskID != handoff.TaskID || revision.TaskDigest != string(handoff.RevisionDigest) ||
		revision.Origin != store.RevisionOriginGenerated || revision.State != store.RevisionStateSealed {
		return handoffDeterministicFailure(request, handoffMaterializationInvalidCode, "The Standard authoring handoff revision is not a sealed materialization.", "revision", fmt.Errorf("Standard authoring handoff revision is not the sealed generated materialization"))
	}
	// The materialize stage's snapshot is a deterministic archive of this
	// sealed revision. Recreate the archive from the managed snapshot and
	// compare its content address before allowing a child to bind it.
	expected, err := materializeManagedTaskSnapshotObject(ctx, service.core, *revision)
	if err != nil {
		return handoffStorageFailure(request, "task_snapshot", err)
	}
	if expected.Digest != handoff.TaskSnapshot.ContentDigest {
		details := handoffFailureDetails(request, "task_snapshot")
		details["expected_digest"] = string(expected.Digest)
		details["actual_digest"] = string(handoff.TaskSnapshot.ContentDigest)
		return newStandardAuthoringHandoffFailure(store.JobFailed, handoffSnapshotDigestMismatchCode, "The Standard authoring task snapshot digest does not match its sealed revision.", details, fmt.Errorf("Standard authoring handoff task_snapshot digest does not match sealed TaskRevision"))
	}
	return nil
}

// standardAuthoringHandoffJobPayload validates a job without accepting a
// caller-owned handoff document. It is shared by enqueue/recovery paths.
func standardAuthoringHandoffJobPayload(job store.DurableJob) (standardAuthoringHandoffPayload, error) {
	var payload standardAuthoringHandoffPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		return standardAuthoringHandoffPayload{}, fmt.Errorf("%w: decode Standard authoring handoff payload: %w", ErrFrozenExecutionPayload, err)
	}
	if (job.CommandType != standardAuthoringHandoffCommandType && job.CommandType != standardAuthoringHandoffRedriveCommandType && job.CommandType != standardAuthoringHandoffReconcileCommandType) || job.EntityType != "artifact_ref" ||
		payload.Format != standardAuthoringHandoffPayloadFormat || payload.AuthoringRunID != job.RunID ||
		payload.StageAttemptID != job.StageAttemptID || payload.HandoffArtifactID != job.EntityID {
		return standardAuthoringHandoffPayload{}, fmt.Errorf("%w: Standard authoring handoff job does not match its immutable payload", ErrFrozenExecutionPayload)
	}
	for _, identity := range []struct {
		label string
		value string
	}{
		{"authoring Run", payload.AuthoringRunID}, {"stage attempt", payload.StageAttemptID},
		{"handoff artifact", payload.HandoffArtifactID}, {"child Run", payload.ChildRunID},
	} {
		if err := store.ValidateUUIDv7(identity.value); err != nil {
			return standardAuthoringHandoffPayload{}, fmt.Errorf("%w: Standard authoring handoff payload %s: %w", ErrFrozenExecutionPayload, identity.label, err)
		}
	}
	return payload, nil
}

// standardAuthoringHandoffFailureResolved reports whether an immutable
// original in_doubt handoff has a later, independently persisted successful
// recovery delivery for exactly the same payload. The original failure stays
// queryable as history, but it is no longer an actionable current failure.
func standardAuthoringHandoffFailureResolved(original store.DurableJob, jobs []store.DurableJob) bool {
	if original.CommandType != standardAuthoringHandoffCommandType || original.State != store.JobInDoubt || original.Failure == nil {
		return false
	}
	payload, err := standardAuthoringHandoffJobPayload(original)
	if err != nil {
		return false
	}
	for _, job := range jobs {
		if job.State != store.JobSucceeded || (job.CommandType != standardAuthoringHandoffRedriveCommandType && job.CommandType != standardAuthoringHandoffReconcileCommandType) {
			continue
		}
		candidate, candidateErr := standardAuthoringHandoffJobPayload(job)
		if candidateErr == nil && candidate == payload {
			return true
		}
	}
	return false
}

// standardAuthoringHandoffJobForRun resolves the single original delivery
// record. Redrive jobs deliberately do not participate here: the original job
// holds the authoritative in_doubt state and its payload reserves ChildRunID.
func standardAuthoringHandoffJobForRun(ctx context.Context, dataStore *store.Store, runID string) (store.DurableJob, standardAuthoringHandoffPayload, error) {
	if dataStore == nil {
		return store.DurableJob{}, standardAuthoringHandoffPayload{}, fmt.Errorf("Standard authoring handoff store is unavailable")
	}
	jobs, err := dataStore.ListDurableJobsForRun(ctx, runID)
	if err != nil {
		return store.DurableJob{}, standardAuthoringHandoffPayload{}, err
	}
	var original *store.DurableJob
	for index := range jobs {
		candidate := jobs[index]
		if candidate.CommandType != standardAuthoringHandoffCommandType {
			continue
		}
		if original != nil {
			return store.DurableJob{}, standardAuthoringHandoffPayload{}, fmt.Errorf("Standard authoring Run %s has multiple original handoff jobs", runID)
		}
		original = &candidate
	}
	if original == nil {
		return store.DurableJob{}, standardAuthoringHandoffPayload{}, fmt.Errorf("Standard authoring Run %s has no durable handoff job", runID)
	}
	payload, err := standardAuthoringHandoffJobPayload(*original)
	if err != nil {
		return store.DurableJob{}, standardAuthoringHandoffPayload{}, err
	}
	return *original, payload, nil
}
