package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/runtime/codexruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var (
	ErrChangeProviderNotFound       = errors.New("change provider: provider is not configured")
	ErrChangeReconciliationRequired = errors.New("change provider: operation requires reconciliation")
	ErrChangeNoOp                   = errors.New("change provider: candidate did not change the task digest")
)

// ChangePlanResult is the read model shown before a content-change execution
// is confirmed. A no-op result intentionally has no Plan because it cannot
// create a false TaskRevision or dispatch work under a changed digest.
type ChangePlanResult struct {
	Candidate      store.RevisionCandidate      `json:"candidate"`
	Operation      store.ChangeOperation        `json:"operation"`
	PreparedChange store.PreparedChange         `json:"prepared_change"`
	Receipt        store.MutationReceipt        `json:"receipt"`
	Plan           workflowkit.ContinuationPlan `json:"plan"`
	NoOp           bool                         `json:"no_op"`
}

// ChangeProviderService owns the content-changing branch of the unified
// continuation protocol. It persists a command and operation key before a
// provider writes, supplies only a candidate checkout, and hands execution to
// the same frozen-plan/worker model used by no-content continuation.
type ChangeProviderService struct {
	core      *lifecycleServiceCore
	providers map[string]ChangeProvider
	observer  continuationStateObserver
}

func newChangeProviderService(core *lifecycleServiceCore) *ChangeProviderService {
	service := &ChangeProviderService{core: core, providers: make(map[string]ChangeProvider)}
	service.observer = storeContinuationStateObserver{dataStore: core.store, objects: core.objects}
	service.Register(LocalPatchProvider{})
	service.Register(AgentRepairProvider{Agent: codexruntime.New(nil, "", nil)})
	return service
}

// Register enables embedding applications to replace an adapter with a typed
// runtime implementation. Registration is intentionally local to service
// construction; callers cannot change a provider after a command is frozen.
func (service *ChangeProviderService) Register(provider ChangeProvider) {
	if service == nil || provider == nil || strings.TrimSpace(provider.ID()) == "" {
		return
	}
	service.providers[provider.ID()] = provider
}

func (service *ChangeProviderService) PlanTaskChange(ctx context.Context, command ContinueTaskCommand, change TaskChangeRequest) (ChangePlanResult, error) {
	if service == nil || service.core == nil {
		return ChangePlanResult{}, fmt.Errorf("change provider service is not configured")
	}
	normalized, provider, err := service.normalizeChangeCommand(ctx, command, change)
	if err != nil {
		return ChangePlanResult{}, err
	}
	encodedCommand, err := json.Marshal(normalized)
	if err != nil {
		return ChangePlanResult{}, fmt.Errorf("encode content continuation command: %w", err)
	}
	commandRecord, err := service.core.store.CreateContinuationCommand(ctx, store.CreateContinuationCommandRequest{
		CommandKey: command.CommandKey, SubjectID: command.TaskID, RunID: command.RunID, PayloadJSON: string(encodedCommand),
		Actor: command.Actor, Reason: command.Reason,
	})
	if err != nil {
		return ChangePlanResult{}, err
	}
	// The immutable command captures the sole provenance for every later
	// candidate, provider, frozen-plan, and execution audit event. A replay
	// must never replace it with the caller's current form values.
	command.Actor = commandRecord.Actor
	command.Reason = commandRecord.Reason
	if existing, err := service.core.store.GetFrozenPlanByCommand(ctx, commandRecord.ID); err != nil {
		return ChangePlanResult{}, err
	} else if existing != nil {
		candidate, err := service.core.store.GetRevisionCandidateByCommand(ctx, commandRecord.ID)
		if err != nil || candidate == nil {
			if err != nil {
				return ChangePlanResult{}, err
			}
			return ChangePlanResult{}, fmt.Errorf("stored revision plan has no candidate")
		}
		if !existing.ExpiresAt.After(service.core.now().UTC()) && candidate.State != store.RevisionCandidateCommitted {
			if err := service.expireCandidatePlan(ctx, *candidate, command.Actor, "frozen continuation plan expired before candidate execution"); err != nil {
				return ChangePlanResult{}, err
			}
			return ChangePlanResult{}, fmt.Errorf("%w: %s", store.ErrContinuationPlanExpired, existing.ID)
		}
		candidate, err = service.recoverFrozenCandidatePlanBinding(ctx, *candidate, *existing, command)
		if err != nil {
			return ChangePlanResult{}, err
		}
		plan, err := (&TaskContinuationService{core: service.core}).decodeFrozenPlan(ctx, *existing)
		if err != nil {
			return ChangePlanResult{}, err
		}
		result := ChangePlanResult{Candidate: *candidate, Plan: plan}
		if candidate.PreparedChangeID != "" {
			if change, err := service.core.store.GetPreparedChange(ctx, candidate.PreparedChangeID); err == nil && change != nil {
				result.PreparedChange = *change
			}
		}
		if candidate.MutationReceiptID != "" {
			if receipt, err := service.core.store.GetMutationReceipt(ctx, candidate.MutationReceiptID); err == nil && receipt != nil {
				result.Receipt = *receipt
			}
		}
		return result, nil
	}

	run, task, revision, err := (&TaskContinuationService{core: service.core}).loadRunBinding(ctx, command.RunID)
	if err != nil {
		return ChangePlanResult{}, err
	}
	if err := matchContinuationCheckpoint(command.Expected, run, task, revision); err != nil {
		return ChangePlanResult{}, err
	}
	if run.Status == store.WorkflowRunInDoubt {
		return ChangePlanResult{}, fmt.Errorf("%w: workflow run %s", store.ErrContinuationReconciliationRequired, run.ID)
	}
	frozen, err := decodeFrozenRunDefinition(run)
	if err != nil {
		return ChangePlanResult{}, err
	}
	candidate, err := service.ensureCandidate(ctx, commandRecord, command, normalized, task, revision, run, frozen)
	if err != nil {
		return ChangePlanResult{}, err
	}
	operation, preparedChange, receipt, err := service.prepareCandidateChange(ctx, candidate, command, normalized, provider, frozen.Workflow)
	if err != nil {
		return ChangePlanResult{Candidate: candidate, Operation: operation, PreparedChange: preparedChange, Receipt: receipt}, err
	}
	updatedCandidate, err := service.core.store.GetRevisionCandidate(ctx, candidate.ID)
	if err != nil {
		return ChangePlanResult{}, err
	}
	if updatedCandidate == nil {
		return ChangePlanResult{}, fmt.Errorf("candidate disappeared after provider result")
	}
	candidate = *updatedCandidate
	result := ChangePlanResult{Candidate: candidate, Operation: operation, PreparedChange: preparedChange, Receipt: receipt}
	if candidate.State == store.RevisionCandidateNoOp || receipt.Outcome == store.MutationReceiptNoOp {
		result.NoOp = true
		return result, fmt.Errorf("%w: candidate %s", ErrChangeNoOp, candidate.ID)
	}
	if candidate.State != store.RevisionCandidatePrepared || receipt.Outcome != store.MutationReceiptApplied {
		return result, fmt.Errorf("%w: candidate %s is %s", ErrChangeReconciliationRequired, candidate.ID, candidate.State)
	}
	plan, candidate, err := service.freezeCandidatePlan(ctx, candidate, commandRecord, command, run, task, revision, frozen)
	if err != nil {
		return result, err
	}
	result.Candidate = candidate
	result.Plan = plan
	return result, nil
}

type normalizedChangeCommand struct {
	Format  string                        `json:"format"`
	Command normalizedContinuationCommand `json:"command"`
	Change  normalizedTaskChange          `json:"change"`
}

type normalizedTaskChange struct {
	ProviderID         string          `json:"provider_id"`
	OperationKey       string          `json:"operation_key"`
	Payload            json.RawMessage `json:"payload"`
	Findings           FindingBundle   `json:"findings"`
	MaxRepairRounds    int             `json:"max_repair_rounds,omitempty"`
	RepairSessionID    string          `json:"repair_session_id,omitempty"`
	RepairRoundOrdinal int             `json:"repair_round_ordinal,omitempty"`
}

func (service *ChangeProviderService) normalizeChangeCommand(ctx context.Context, command ContinueTaskCommand, change TaskChangeRequest) (normalizedChangeCommand, ChangeProvider, error) {
	base := &TaskContinuationService{core: service.core}
	noContentCommand := command
	noContentCommand.Change = nil
	normalizedCommand, err := base.normalizeCommand(noContentCommand)
	if err != nil {
		return normalizedChangeCommand{}, nil, err
	}
	providerID := strings.TrimSpace(change.ProviderID)
	provider, ok := service.providers[providerID]
	if !ok {
		return normalizedChangeCommand{}, nil, fmt.Errorf("%w: %s", ErrChangeProviderNotFound, providerID)
	}
	operationKey := strings.TrimSpace(change.OperationKey)
	if operationKey == "" {
		return normalizedChangeCommand{}, nil, fmt.Errorf("change operation key is required")
	}
	payload, err := provider.ValidatePayload(change.Payload)
	if err != nil {
		return normalizedChangeCommand{}, nil, fmt.Errorf("validate %s payload: %w", providerID, err)
	}
	if providerID == AgentRepairProviderID && change.MaxRepairRounds <= 0 {
		return normalizedChangeCommand{}, nil, fmt.Errorf("automated repair requires max repair rounds")
	}
	if providerID != AgentRepairProviderID && change.MaxRepairRounds != 0 {
		return normalizedChangeCommand{}, nil, fmt.Errorf("manual patch does not accept automatic repair rounds")
	}
	if change.repairSessionID != "" {
		if err := service.validateAutomaticRepairRound(ctx, command, change, providerID); err != nil {
			return normalizedChangeCommand{}, nil, err
		}
	} else if providerID == LocalPatchProviderID && len(change.Findings.Findings) == 0 {
		// A direct local unified-diff edit is a user-authored content change rather
		// than a repair response. It still binds to the exact Run/revision
		// checkpoint, but it has no fabricated checker artifact.
		if change.Findings.Format != "harbor.findings.v1" || change.Findings.RevisionID != command.Expected.SubjectRevisionID || change.Findings.RevisionDigest != string(command.Expected.SubjectDigest) {
			return normalizedChangeCommand{}, nil, fmt.Errorf("manual patch without findings must bind the selected revision checkpoint")
		}
	} else {
		if err := change.Findings.Validate(command.Expected.SubjectRevisionID, string(command.Expected.SubjectDigest)); err != nil {
			return normalizedChangeCommand{}, nil, err
		}
		if err := service.validateFindingEvidence(ctx, command.RunID, command.Expected.SubjectRevisionID, string(command.Expected.SubjectDigest), change.Findings); err != nil {
			return normalizedChangeCommand{}, nil, err
		}
	}
	return normalizedChangeCommand{
		Format: "harbor.content-continuation-command.v1", Command: normalizedCommand,
		Change: normalizedTaskChange{ProviderID: providerID, OperationKey: operationKey, Payload: payload, Findings: change.Findings, MaxRepairRounds: change.MaxRepairRounds,
			RepairSessionID: change.repairSessionID, RepairRoundOrdinal: change.repairRoundOrdinal},
	}, provider, nil
}

// validateFindingEvidence makes a repair input traceable to immutable report
// bytes before it can create a durable command or give a provider write access.
// The stage artifact manifest is the authority for object metadata and size;
// the artifact ref alone is not sufficient to prove that report bytes exist.
func (service *ChangeProviderService) validateFindingEvidence(ctx context.Context, runID, revisionID, revisionDigest string, bundle FindingBundle) error {
	if service == nil || service.core == nil || service.core.store == nil || service.core.objects == nil {
		return fmt.Errorf("change provider evidence validation is not configured")
	}
	for index, finding := range bundle.Findings {
		reference, err := service.core.store.GetArtifactRef(ctx, finding.ReportArtifactID)
		if err != nil {
			return fmt.Errorf("load finding %d report artifact: %w", index, err)
		}
		if reference == nil {
			return fmt.Errorf("finding %d report artifact %s does not exist", index, finding.ReportArtifactID)
		}
		if reference.RunID != runID || reference.SubjectRevisionID != revisionID || reference.SubjectDigest != revisionDigest ||
			reference.StageKey != finding.StageKey || reference.ContentDigest != finding.ReportContentDigest {
			return fmt.Errorf("finding %d report artifact does not match the run, revision, stage, or digest", index)
		}
		stageAttempt, err := service.core.store.GetStageAttempt(ctx, reference.AttemptID)
		if err != nil {
			return fmt.Errorf("load finding %d report stage attempt: %w", index, err)
		}
		if stageAttempt == nil || stageAttempt.RunID != runID || stageAttempt.StageKey != finding.StageKey || stageAttempt.ArtifactManifestID != reference.ManifestID {
			return fmt.Errorf("finding %d report artifact does not belong to its declared stage attempt", index)
		}
		manifest, err := loadStageArtifactManifestIndex(ctx, service.core.store, reference.ManifestID)
		if err != nil {
			return fmt.Errorf("validate finding %d report manifest: %w", index, err)
		}
		if manifest.manifest.SubjectRevisionID != revisionID || manifest.manifest.SubjectDigest != revisionDigest ||
			manifest.payload.RunID != runID || manifest.payload.StageAttemptID != stageAttempt.ID || string(manifest.payload.StageKey) != finding.StageKey {
			return fmt.Errorf("finding %d report manifest does not match immutable lineage", index)
		}
		object, err := manifest.objectFor(*reference)
		if err != nil {
			return fmt.Errorf("validate finding %d report reference: %w", index, err)
		}
		if err := VerifyStageArtifactObject(ctx, service.core.objects, object); err != nil {
			return fmt.Errorf("verify finding %d report object: %w", index, err)
		}
	}
	return nil
}

func (service *ChangeProviderService) ensureCandidate(ctx context.Context, commandRecord store.ContinuationCommand, command ContinueTaskCommand, normalized normalizedChangeCommand, task store.TaskV2, revision store.TaskRevision, run store.WorkflowRun, frozen frozenRunDefinition) (store.RevisionCandidate, error) {
	if existing, err := service.core.store.GetRevisionCandidateByCommand(ctx, commandRecord.ID); err != nil {
		return store.RevisionCandidate{}, err
	} else if existing != nil {
		return *existing, nil
	}
	leaseTTL, err := candidateLeaseTTL(frozen.Workflow)
	if err != nil {
		return store.RevisionCandidate{}, err
	}
	owner := "candidate:" + commandRecord.ID
	lease, err := service.core.store.AcquireLease(ctx, store.AcquireLeaseRequest{
		ResourceType: "task_revision_candidate", ResourceID: task.ID, Owner: owner, TTL: leaseTTL,
		Actor: command.Actor, Reason: command.Reason,
	})
	if err != nil {
		return store.RevisionCandidate{}, err
	}
	releaseLease := true
	defer func() {
		if releaseLease {
			_, _ = service.core.store.ReleaseLease(context.Background(), store.ReleaseLeaseRequest{LeaseID: lease.ID, Owner: lease.Owner, FencingToken: lease.FencingToken, ExpectedVersion: lease.Version, Actor: command.Actor, Reason: "candidate creation did not commit"})
		}
	}()
	candidateID, err := store.NewUUIDv7()
	if err != nil {
		return store.RevisionCandidate{}, err
	}
	targetRevisionID, err := store.NewUUIDv7()
	if err != nil {
		return store.RevisionCandidate{}, err
	}
	targetRunID, err := store.NewUUIDv7()
	if err != nil {
		return store.RevisionCandidate{}, err
	}
	checkout := service.core.layout.candidateCheckoutDirectory(task.ID, candidateID)
	if err := os.MkdirAll(filepath.Dir(checkout), 0o750); err != nil {
		return store.RevisionCandidate{}, fmt.Errorf("create candidate parent: %w", err)
	}
	baseSnapshot, err := (&RevisionService{core: service.core}).SnapshotDirectory(task.ID, revision.ID)
	if err != nil {
		return store.RevisionCandidate{}, err
	}
	digest, err := materializeManagedSnapshot(ctx, baseSnapshot, checkout)
	if err != nil {
		return store.RevisionCandidate{}, fmt.Errorf("materialize candidate checkout: %w", err)
	}
	if digest != revision.TaskDigest {
		_ = os.RemoveAll(service.core.layout.candidateDirectory(task.ID, candidateID))
		return store.RevisionCandidate{}, fmt.Errorf("candidate materialization digest differs from sealed base revision")
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(service.core.layout.candidateDirectory(task.ID, candidateID))
		}
	}()
	repairSessionID := ""
	round := 0
	if normalized.Change.ProviderID == AgentRepairProviderID {
		if normalized.Change.RepairSessionID != "" {
			session, err := service.core.store.GetRepairSession(ctx, normalized.Change.RepairSessionID)
			if err != nil {
				return store.RevisionCandidate{}, err
			}
			if session == nil || session.Status != store.RepairSessionOpen || normalized.Change.RepairRoundOrdinal <= 1 || normalized.Change.RepairRoundOrdinal > session.MaxRounds {
				return store.RevisionCandidate{}, fmt.Errorf("automatic repair session round is not actionable")
			}
			repairSessionID, round = session.ID, normalized.Change.RepairRoundOrdinal
		} else {
			session, err := service.core.store.CreateRepairSession(ctx, store.CreateRepairSessionRequest{
				CommandID: commandRecord.ID, SubjectID: task.ID, BaseRevisionID: revision.ID, MaxRounds: normalized.Change.MaxRepairRounds,
				FindingsJSON: mustJSON(normalized.Change.Findings), PolicyJSON: mustJSON(map[string]any{"format": "harbor.repair-policy.v1", "max_rounds": normalized.Change.MaxRepairRounds}),
				IdempotencyKey: "repair-session:" + commandRecord.ID, Actor: command.Actor, Reason: command.Reason,
			})
			if err != nil {
				return store.RevisionCandidate{}, err
			}
			repairSessionID, round = session.ID, 1
		}
	}
	candidate, err := service.core.store.CreateRevisionCandidate(ctx, store.CreateRevisionCandidateRequest{
		ID: candidateID, TaskID: task.ID, SourceRunID: run.ID, CommandID: commandRecord.ID, RepairSessionID: repairSessionID,
		RoundOrdinal: round, BaseRevisionID: revision.ID, BaseDigest: revision.TaskDigest, TargetRevisionID: targetRevisionID,
		TargetRunID: targetRunID, ExpectedTaskVersion: task.Version, ProviderID: normalized.Change.ProviderID,
		CheckoutRelpath: service.core.layout.candidateCheckoutRelpath(task.ID, candidateID), FindingsJSON: mustJSON(normalized.Change.Findings),
		LeaseID: lease.ID, LeaseOwner: lease.Owner, LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version,
		Actor: command.Actor, Reason: command.Reason,
	})
	if err != nil {
		return store.RevisionCandidate{}, err
	}
	releaseLease = false
	cleanup = false
	return candidate, nil
}

func candidateLeaseTTL(workflow workflowkit.WorkflowDescriptor) (time.Duration, error) {
	stage, found := workflow.Stage(workflowkit.StageKey("task_repair"))
	if !found {
		return 0, fmt.Errorf("frozen workflow has no task_repair stage for candidate lease")
	}
	ttl := stage.Budget.AttemptTimeout + stage.Budget.ShutdownGrace
	if ttl <= 0 {
		return 0, fmt.Errorf("task_repair candidate lease budget is invalid")
	}
	return ttl, nil
}

func (service *ChangeProviderService) prepareCandidateChange(ctx context.Context, candidate store.RevisionCandidate, command ContinueTaskCommand, normalized normalizedChangeCommand, provider ChangeProvider, workflow workflowkit.WorkflowDescriptor) (store.ChangeOperation, store.PreparedChange, store.MutationReceipt, error) {
	operation, err := service.core.store.GetChangeOperationByKey(ctx, normalized.Change.OperationKey)
	if err != nil {
		return store.ChangeOperation{}, store.PreparedChange{}, store.MutationReceipt{}, err
	}
	if operation == nil {
		created, err := service.core.store.CreateChangeOperation(ctx, store.CreateChangeOperationRequest{
			CandidateID: candidate.ID, ProviderID: provider.ID(), OperationKey: normalized.Change.OperationKey, PayloadJSON: string(normalized.Change.Payload),
			Actor: command.Actor, Reason: command.Reason,
		})
		if err != nil {
			return store.ChangeOperation{}, store.PreparedChange{}, store.MutationReceipt{}, err
		}
		operation = &created
	}
	if operation.CandidateID != candidate.ID || operation.ProviderID != provider.ID() {
		return store.ChangeOperation{}, store.PreparedChange{}, store.MutationReceipt{}, fmt.Errorf("%w: operation key belongs to another candidate/provider", store.ErrIdempotencyConflict)
	}
	if operation.State == store.ChangeOperationSucceeded {
		change, err := service.core.store.GetPreparedChange(ctx, candidate.PreparedChangeID)
		if err != nil || change == nil {
			if err != nil {
				return *operation, store.PreparedChange{}, store.MutationReceipt{}, err
			}
			return *operation, store.PreparedChange{}, store.MutationReceipt{}, fmt.Errorf("successful candidate operation is missing prepared change")
		}
		receipt, err := service.core.store.GetMutationReceipt(ctx, operation.ReceiptID)
		if err != nil || receipt == nil {
			if err != nil {
				return *operation, *change, store.MutationReceipt{}, err
			}
			return *operation, *change, store.MutationReceipt{}, fmt.Errorf("successful candidate operation is missing receipt")
		}
		return *operation, *change, *receipt, nil
	}
	if operation.State == store.ChangeOperationRunning || operation.State == store.ChangeOperationUnknown {
		return *operation, store.PreparedChange{}, store.MutationReceipt{}, fmt.Errorf("%w: operation %s is %s", ErrChangeReconciliationRequired, operation.ID, operation.State)
	}
	if operation.State == store.ChangeOperationFailed {
		return *operation, store.PreparedChange{}, store.MutationReceipt{}, fmt.Errorf("%w: operation %s failed", ErrChangeReconciliationRequired, operation.ID)
	}
	lease, err := service.currentCandidateLease(ctx, candidate)
	if err != nil {
		return *operation, store.PreparedChange{}, store.MutationReceipt{}, err
	}
	started, candidate, err := service.core.store.StartChangeOperation(ctx, store.StartChangeOperationRequest{
		OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: lease.ID, LeaseOwner: lease.Owner,
		LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Actor: command.Actor, Reason: command.Reason,
	})
	if err != nil {
		return *operation, store.PreparedChange{}, store.MutationReceipt{}, err
	}
	checkout, err := service.candidateCheckout(candidate)
	if err != nil {
		return started, store.PreparedChange{}, store.MutationReceipt{}, err
	}
	timeout, err := candidateLeaseTTL(workflow)
	if err != nil {
		return started, store.PreparedChange{}, store.MutationReceipt{}, err
	}
	providerReceipt, lease, applyErr := service.applyWithCandidateLeaseHeartbeat(ctx, provider, ChangeProviderRequest{Candidate: candidate, Checkout: checkout, Payload: normalized.Change.Payload,
		Findings: normalized.Change.Findings, Actor: command.Actor, Reason: command.Reason, RoundOrdinal: candidate.RoundOrdinal, Timeout: timeout}, lease, timeout)
	if applyErr != nil {
		_, _, markErr := service.core.store.MarkChangeOperationUnknown(ctx, started.ID, started.Version, command.Actor, "provider returned an error; reconcile candidate before retry")
		if markErr != nil {
			return started, store.PreparedChange{}, store.MutationReceipt{}, fmt.Errorf("provider error: %v; mark operation unknown: %w", applyErr, markErr)
		}
		return started, store.PreparedChange{}, store.MutationReceipt{}, fmt.Errorf("%w: %v", ErrChangeReconciliationRequired, applyErr)
	}
	if err := taskpolicy.ValidateManagedSnapshotV2(checkout); err != nil {
		_, _, _ = service.core.store.MarkChangeOperationUnknown(ctx, started.ID, started.Version, command.Actor, "provider output violated strict managed task policy")
		return started, store.PreparedChange{}, store.MutationReceipt{}, fmt.Errorf("%w: validate candidate after provider: %v", ErrChangeReconciliationRequired, err)
	}
	afterDigest, err := taskpolicy.ComputeManagedTaskDigestV2(checkout)
	if err != nil {
		return started, store.PreparedChange{}, store.MutationReceipt{}, err
	}
	changedPaths, err := service.changedCandidatePaths(candidate)
	if err != nil {
		return started, store.PreparedChange{}, store.MutationReceipt{}, err
	}
	sort.Strings(changedPaths)
	if err := validateChangeProviderReceipt(provider.ID(), providerReceipt, changedPaths); err != nil {
		_, _, markErr := service.core.store.MarkChangeOperationUnknown(ctx, started.ID, started.Version, command.Actor, "provider receipt does not match the verified candidate change set")
		if markErr != nil {
			return started, store.PreparedChange{}, store.MutationReceipt{}, fmt.Errorf("provider receipt validation: %v; mark operation unknown: %w", err, markErr)
		}
		return started, store.PreparedChange{}, store.MutationReceipt{}, fmt.Errorf("%w: provider receipt validation: %v", ErrChangeReconciliationRequired, err)
	}
	outcome := store.MutationReceiptApplied
	if afterDigest == candidate.BaseDigest {
		outcome = store.MutationReceiptNoOp
	}
	providerReceipt.ChangedPaths = changedPaths
	receiptJSON := mustJSON(map[string]any{"format": "harbor.change-provider-receipt-envelope.v1", "before_digest": candidate.BaseDigest, "after_digest": afterDigest, "provider": providerReceipt})
	preparedPayload := mustJSON(map[string]any{"format": "harbor.prepared-change-payload.v1", "provider_id": provider.ID(), "payload": json.RawMessage(normalized.Change.Payload), "findings": normalized.Change.Findings, "changed_paths": changedPaths})
	preparedChangeID, err := store.NewUUIDv7()
	if err != nil {
		return started, store.PreparedChange{}, store.MutationReceipt{}, err
	}
	mutationReceiptID, err := store.NewUUIDv7()
	if err != nil {
		return started, store.PreparedChange{}, store.MutationReceipt{}, err
	}
	finalOperation, finalCandidate, change, receipt, err := service.core.store.FinalizeChangeOperation(ctx, store.FinalizeChangeOperationRequest{
		OperationID: started.ID, ExpectedVersion: started.Version, LeaseID: lease.ID, LeaseOwner: lease.Owner,
		LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Outcome: outcome, AfterDigest: afterDigest,
		ObservedChangesJSON: mustJSON(changedPaths), PreparedChangeID: preparedChangeID, PreparedChangePayloadJSON: preparedPayload,
		MutationReceiptID: mutationReceiptID, MutationReceiptJSON: receiptJSON, MutationReceiptKey: "candidate-receipt:" + started.OperationKey,
		Actor: command.Actor, Reason: command.Reason,
	})
	if err != nil {
		return started, store.PreparedChange{}, store.MutationReceipt{}, err
	}
	_ = finalCandidate
	return finalOperation, change, receipt, nil
}

func validateChangeProviderReceipt(providerID string, receipt ChangeProviderReceipt, actualChangedPaths []string) error {
	if strings.TrimSpace(receipt.Format) == "" {
		return fmt.Errorf("provider receipt format is required")
	}
	if receipt.ProviderID != providerID {
		return fmt.Errorf("provider receipt provider ID %q does not match %q", receipt.ProviderID, providerID)
	}
	if len(receipt.ChangedPaths) == 0 {
		return nil
	}
	declared := append([]string(nil), receipt.ChangedPaths...)
	sort.Strings(declared)
	for index, path := range declared {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("provider receipt contains an empty changed path")
		}
		if index > 0 && path == declared[index-1] {
			return fmt.Errorf("provider receipt contains duplicate changed path %q", path)
		}
	}
	if len(declared) != len(actualChangedPaths) {
		return fmt.Errorf("provider receipt changed paths do not match verified candidate changes")
	}
	for index := range declared {
		if declared[index] != actualChangedPaths[index] {
			return fmt.Errorf("provider receipt changed paths do not match verified candidate changes")
		}
	}
	return nil
}

// applyWithCandidateLeaseHeartbeat keeps a provider's candidate write fence
// alive for the exact frozen repair budget. A heartbeat failure cancels the
// provider context and leaves its operation reconcile-required instead of
// allowing an unfenced late result to be sealed.
func (service *ChangeProviderService) applyWithCandidateLeaseHeartbeat(ctx context.Context, provider ChangeProvider, request ChangeProviderRequest, lease store.Lease, ttl time.Duration) (ChangeProviderReceipt, store.Lease, error) {
	if request.Timeout <= 0 {
		return ChangeProviderReceipt{}, lease, fmt.Errorf("candidate provider timeout is required")
	}
	if ttl <= 0 {
		return ChangeProviderReceipt{}, lease, fmt.Errorf("candidate lease TTL is required")
	}
	// Candidate checkout materialization can consume most of an initially
	// acquired lease. Renew before handing write access to a provider so the
	// first delayed ticker cannot leave a live provider without a current fence.
	renewed, err := service.core.store.HeartbeatLease(ctx, store.HeartbeatLeaseRequest{
		LeaseID: lease.ID, Owner: lease.Owner, FencingToken: lease.FencingToken, ExpectedVersion: lease.Version,
		TTL: ttl, Actor: request.Actor, Reason: "candidate provider start heartbeat",
	})
	if err != nil {
		return ChangeProviderReceipt{}, lease, fmt.Errorf("start candidate lease heartbeat: %w", err)
	}
	lease = renewed
	providerContext, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	// Lease renewal has a different lifetime from the provider's execution
	// budget. A provider may return at the same instant a ticker fires; using
	// providerContext for the store call would turn our own normal cancellation
	// into a false fencing failure. The stop channel below is the authoritative
	// normal-completion signal, while the parent context still stops a worker
	// whose caller has gone away.
	heartbeatContext, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	interval := 20 * time.Second
	if candidateInterval := ttl / 3; candidateInterval > 0 && candidateInterval < interval {
		interval = candidateInterval
	}
	// A short deployment lease must still receive at least three renewal
	// opportunities before expiry. Keeping the old 100ms floor could turn a
	// valid 150ms lease into a single-race-window fence under scheduler load.
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	stopHeartbeats := make(chan struct{})
	done := make(chan struct{})
	var heartbeatMu sync.Mutex
	latest := lease
	var heartbeatErr error
	go func() {
		defer close(done)
		current := lease
		for {
			select {
			case <-stopHeartbeats:
				return
			case <-providerContext.Done():
				return
			case <-ticker.C:
				// Favor a completed provider when stop and ticker become ready
				// together. A heartbeat that starts before the stop is observed is
				// still fenced by its current version and is safe to finish.
				select {
				case <-stopHeartbeats:
					return
				default:
				}
				next, err := service.core.store.HeartbeatLease(heartbeatContext, store.HeartbeatLeaseRequest{
					LeaseID: current.ID, Owner: current.Owner, FencingToken: current.FencingToken, ExpectedVersion: current.Version,
					TTL: ttl, Actor: request.Actor, Reason: "candidate provider heartbeat",
				})
				if err != nil {
					// Parent cancellation and a provider budget expiry are normal
					// terminal paths for this loop, not a lost write fence.
					if heartbeatContext.Err() != nil || providerContext.Err() != nil {
						return
					}
					heartbeatMu.Lock()
					heartbeatErr = err
					heartbeatMu.Unlock()
					cancel()
					return
				}
				current = next
				heartbeatMu.Lock()
				latest = current
				heartbeatMu.Unlock()
			}
		}
	}()
	receipt, applyErr := provider.Apply(providerContext, request)
	providerErr := providerContext.Err()
	// Stop the renewal loop before canceling providerContext. Otherwise a
	// concurrently selected ticker observes our cancellation as a failed
	// heartbeat and incorrectly forces reconciliation of a completed change.
	close(stopHeartbeats)
	<-done
	cancel()
	heartbeatMu.Lock()
	latestLease := latest
	lastHeartbeatErr := heartbeatErr
	heartbeatMu.Unlock()
	if lastHeartbeatErr != nil {
		return receipt, latestLease, fmt.Errorf("candidate lease heartbeat: %w", lastHeartbeatErr)
	}
	if providerErr != nil {
		return receipt, latestLease, providerErr
	}
	return receipt, latestLease, applyErr
}

func (service *ChangeProviderService) currentCandidateLease(ctx context.Context, candidate store.RevisionCandidate) (store.Lease, error) {
	lease, err := service.core.store.GetLease(ctx, candidate.LeaseID)
	if err != nil {
		return store.Lease{}, err
	}
	if lease == nil {
		return store.Lease{}, fmt.Errorf("candidate lease is missing")
	}
	if lease.State != store.LeaseActive || lease.Owner != candidate.LeaseOwner || lease.FencingToken != candidate.LeaseFencingToken || !lease.ExpiresAt.After(service.core.now().UTC()) {
		return store.Lease{}, fmt.Errorf("%w: candidate lease", store.ErrLeaseHeld)
	}
	return *lease, nil
}

func (service *ChangeProviderService) candidateCheckout(candidate store.RevisionCandidate) (string, error) {
	relative, err := filepath.Rel(service.core.layout.root, service.core.layout.candidateCheckoutDirectory(candidate.TaskID, candidate.ID))
	if err != nil {
		return "", err
	}
	if filepath.ToSlash(relative) != candidate.CheckoutRelpath {
		return "", fmt.Errorf("candidate checkout path does not match managed layout")
	}
	path := service.core.layout.candidateCheckoutDirectory(candidate.TaskID, candidate.ID)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("candidate checkout is not a real directory")
	}
	return path, nil
}

func (service *ChangeProviderService) changedCandidatePaths(candidate store.RevisionCandidate) ([]string, error) {
	base, err := (&RevisionService{core: service.core}).SnapshotDirectory(candidate.TaskID, candidate.BaseRevisionID)
	if err != nil {
		return nil, err
	}
	checkout, err := service.candidateCheckout(candidate)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, file := range taskpolicy.CanonicalFiles() {
		baseBytes, baseErr := os.ReadFile(filepath.Join(base, filepath.FromSlash(file.Path)))
		candidateBytes, candidateErr := os.ReadFile(filepath.Join(checkout, filepath.FromSlash(file.Path)))
		if os.IsNotExist(baseErr) && os.IsNotExist(candidateErr) && file.Environment {
			continue
		}
		if baseErr != nil || candidateErr != nil {
			return nil, fmt.Errorf("read candidate changed path %s: base=%v candidate=%v", file.Path, baseErr, candidateErr)
		}
		if string(baseBytes) != string(candidateBytes) {
			changed = append(changed, file.Path)
		}
	}
	return changed, nil
}

func (service *ChangeProviderService) freezeCandidatePlan(ctx context.Context, candidate store.RevisionCandidate, commandRecord store.ContinuationCommand, command ContinueTaskCommand, run store.WorkflowRun, task store.TaskV2, revision store.TaskRevision, frozen frozenRunDefinition) (workflowkit.ContinuationPlan, store.RevisionCandidate, error) {
	state, err := service.observer.Observe(ctx, run, revision, frozen.Workflow)
	if err != nil {
		return workflowkit.ContinuationPlan{}, candidate, err
	}
	if state.InDoubt {
		return workflowkit.ContinuationPlan{}, candidate, fmt.Errorf("%w: source run has unresolved evidence", store.ErrContinuationReconciliationRequired)
	}
	planID, err := store.NewUUIDv7()
	if err != nil {
		return workflowkit.ContinuationPlan{}, candidate, err
	}
	planSnapshot, err := buildRevisionCandidatePlan(planID, commandRecord.ID, command, run, revision, candidate, frozen.Workflow, state, service.core.now().UTC().Add(frozen.ContinuationPlanTTL))
	if err != nil {
		return workflowkit.ContinuationPlan{}, candidate, err
	}
	plan, err := workflowkit.FreezeContinuationPlan(planSnapshot, frozen.Workflow)
	if err != nil {
		return workflowkit.ContinuationPlan{}, candidate, fmt.Errorf("freeze content continuation plan: %w", err)
	}
	manifestID, err := service.ensureCandidateFinalSnapshot(ctx, candidate)
	if err != nil {
		return workflowkit.ContinuationPlan{}, candidate, err
	}
	childManifest, err := service.ensureCandidateChildRunManifest(candidate, run)
	if err != nil {
		return workflowkit.ContinuationPlan{}, candidate, err
	}
	encoded, err := json.Marshal(plan.Snapshot())
	if err != nil {
		return workflowkit.ContinuationPlan{}, candidate, err
	}
	stored, candidate, err := service.core.store.CreateAndBindRevisionCandidatePlan(ctx, store.CreateAndBindRevisionCandidatePlanRequest{
		Plan: store.CreateFrozenPlanRequest{
			ID: plan.ID(), CommandID: commandRecord.ID, PreparedChangeID: candidate.PreparedChangeID, SubjectID: task.ID,
			SubjectRevisionID: candidate.TargetRevisionID, SubjectDigest: candidate.AfterDigest, WorkflowFingerprint: run.DefinitionHash,
			PlanFingerprint: string(plan.Fingerprint()), PayloadJSON: string(encoded), ExpiresAt: plan.Snapshot().ExpiresAt,
			Actor: command.Actor, Reason: command.Reason,
		},
		CandidateID: candidate.ID, ExpectedCandidateVersion: candidate.Version, FinalManifestID: manifestID,
		ChildRunManifestJSON: childManifest, Actor: command.Actor, Reason: command.Reason,
	})
	if err != nil {
		return workflowkit.ContinuationPlan{}, candidate, err
	}
	decoded, err := (&TaskContinuationService{core: service.core}).decodeFrozenPlan(ctx, stored)
	if err != nil {
		return workflowkit.ContinuationPlan{}, candidate, err
	}
	return decoded, candidate, nil
}

// recoverFrozenCandidatePlanBinding closes the compatibility window for a
// process that died after older code wrote a frozen plan but before it bound
// the candidate. It reuses the original immutable plan bytes and expiration;
// it never re-plans or silently renews a 24-hour TTL.
func (service *ChangeProviderService) recoverFrozenCandidatePlanBinding(ctx context.Context, candidate store.RevisionCandidate, plan store.FrozenPlan, command ContinueTaskCommand) (*store.RevisionCandidate, error) {
	if candidate.FrozenPlanID == plan.ID && candidate.FinalManifestID != "" && candidate.ChildRunManifestJSON != "" {
		return &candidate, nil
	}
	if candidate.State != store.RevisionCandidatePrepared {
		return nil, fmt.Errorf("%w: revision candidate %s is %s", ErrChangeReconciliationRequired, candidate.ID, candidate.State)
	}
	run, task, revision, err := (&TaskContinuationService{core: service.core}).loadRunBinding(ctx, candidate.SourceRunID)
	if err != nil {
		return nil, err
	}
	if task.ID != candidate.TaskID || revision.ID != candidate.BaseRevisionID {
		return nil, fmt.Errorf("revision candidate source binding changed")
	}
	manifestID, err := service.ensureCandidateFinalSnapshot(ctx, candidate)
	if err != nil {
		return nil, err
	}
	childManifest, err := service.ensureCandidateChildRunManifest(candidate, run)
	if err != nil {
		return nil, err
	}
	stored, bound, err := service.core.store.CreateAndBindRevisionCandidatePlan(ctx, store.CreateAndBindRevisionCandidatePlanRequest{
		Plan:                     frozenPlanCreateRequest(plan, command),
		CandidateID:              candidate.ID,
		ExpectedCandidateVersion: candidate.Version,
		FinalManifestID:          manifestID,
		ChildRunManifestJSON:     childManifest,
		Actor:                    command.Actor,
		Reason:                   command.Reason,
	})
	if err != nil {
		return nil, err
	}
	if stored.ID != plan.ID {
		return nil, fmt.Errorf("recovered candidate plan identity mismatch")
	}
	return &bound, nil
}

func frozenPlanCreateRequest(plan store.FrozenPlan, command ContinueTaskCommand) store.CreateFrozenPlanRequest {
	return store.CreateFrozenPlanRequest{
		ID:                  plan.ID,
		CommandID:           plan.CommandID,
		PreparedChangeID:    plan.PreparedChangeID,
		SubjectID:           plan.SubjectID,
		SubjectRevisionID:   plan.SubjectRevisionID,
		SubjectDigest:       plan.SubjectDigest,
		WorkflowFingerprint: plan.WorkflowFingerprint,
		PlanFingerprint:     plan.PlanFingerprint,
		PayloadJSON:         plan.PayloadJSON,
		ExpiresAt:           plan.ExpiresAt,
		Actor:               command.Actor,
		Reason:              command.Reason,
	}
}

func (service *ChangeProviderService) expireCandidatePlan(ctx context.Context, candidate store.RevisionCandidate, actor, reason string) error {
	_, err := service.core.store.ExpireRevisionCandidate(ctx, store.ExpireRevisionCandidateRequest{
		CandidateID: candidate.ID, ExpectedVersion: candidate.Version, Actor: actor, Reason: reason,
	})
	return err
}

func buildRevisionCandidatePlan(planID, commandID string, command ContinueTaskCommand, run store.WorkflowRun, revision store.TaskRevision, candidate store.RevisionCandidate, workflow workflowkit.WorkflowDescriptor, state continuationRunState, expiresAt time.Time) (workflowkit.ContinuationPlanSnapshot, error) {
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		return workflowkit.ContinuationPlanSnapshot{}, err
	}
	transitions := make([]workflowkit.NodeTransition, 0, len(workflow.Stages))
	scheduled := make([]workflowkit.NodeID, 0, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		transition := workflowkit.NodeTransition{NodeID: stage.Key, FromGeneration: 0, ToGeneration: 0, ExpectedInputFingerprint: emptyInputs}
		if stage.Effect == workflowkit.EffectExternalSideEffect {
			transition.Disposition = workflowkit.DispositionInvalidate
			transition.ReasonCodes = []workflowkit.PlanReason{"candidate_revision_changed", "external_confirmation_required"}
		} else {
			transition.Disposition = workflowkit.DispositionSchedule
			transition.ToGeneration = 1
			transition.ReasonCodes = []workflowkit.PlanReason{"candidate_revision_changed"}
			scheduled = append(scheduled, stage.Key)
		}
		transitions = append(transitions, transition)
	}
	schedule, err := sequentialSchedule(workflow, scheduled)
	if err != nil {
		return workflowkit.ContinuationPlanSnapshot{}, err
	}
	checkpointFingerprint, err := continuationCheckpointFingerprint(command.Expected)
	if err != nil {
		return workflowkit.ContinuationPlanSnapshot{}, err
	}
	return workflowkit.ContinuationPlanSnapshot{
		PlanID: planID, CommandID: commandID, Strategy: workflowkit.StrategyReviseSubject, BaseCheckpoint: command.Expected,
		NextExecutionEpoch: run.ExecutionEpoch + 1, SourceRunID: run.ID, TargetRunRelation: workflowkit.RelationChildRun,
		PreparedChangeID: candidate.PreparedChangeID, SubjectRevisionID: candidate.TargetRevisionID,
		SubjectDigest: workflowkit.SubjectDigest(candidate.AfterDigest), CandidateRevisionID: candidate.ID, Nodes: transitions,
		Schedule: schedule, Assertions: []workflowkit.PlanAssertion{{Kind: workflowkit.AssertionCheckpointCurrent, Subject: run.ID, Expected: checkpointFingerprint}},
		ExpiresAt: expiresAt.UTC(),
	}, nil
}

func (service *ChangeProviderService) ensureCandidateFinalSnapshot(ctx context.Context, candidate store.RevisionCandidate) (string, error) {
	if candidate.FinalManifestID != "" {
		return candidate.FinalManifestID, nil
	}
	checkout, err := service.candidateCheckout(candidate)
	if err != nil {
		return "", err
	}
	prepared, cleanup, err := (&RevisionService{core: service.core}).prepareSnapshot(ctx, candidate.TaskID, candidate.TargetRevisionID, checkout)
	if err == nil {
		_ = cleanup // The unreferenced filesystem snapshot is intentionally retained for candidate crash recovery.
		if prepared.TaskDigest != candidate.AfterDigest {
			return "", fmt.Errorf("candidate final snapshot digest differs from prepared change")
		}
		return prepared.ManifestObjectID, nil
	}
	if !strings.Contains(err.Error(), "revision directory already exists") {
		return "", err
	}
	path := service.core.layout.snapshotDirectory(candidate.TaskID, candidate.TargetRevisionID)
	if validateErr := taskpolicy.ValidateManagedSnapshotV2(path); validateErr != nil {
		return "", fmt.Errorf("validate recovered candidate final snapshot: %w", validateErr)
	}
	digest, digestErr := taskpolicy.ComputeManagedTaskDigestV2(path)
	if digestErr != nil || digest != candidate.AfterDigest {
		return "", fmt.Errorf("recovered candidate final snapshot digest mismatch: %v", digestErr)
	}
	raw, readErr := os.ReadFile(service.core.layout.revisionManifestPath(candidate.TaskID, candidate.TargetRevisionID))
	if readErr != nil {
		return "", readErr
	}
	object, objectErr := service.core.objects.PutBytes(ctx, raw)
	if objectErr != nil {
		return "", objectErr
	}
	return string(object.Digest), nil
}

func (service *ChangeProviderService) ensureCandidateChildRunManifest(candidate store.RevisionCandidate, source store.WorkflowRun) (string, error) {
	var original runManifest
	if err := decodeStrictJSON(source.RunManifestJSON, &original); err != nil {
		return "", err
	}
	catalogReceipt, err := canonicalManifestDeploymentCatalogReceipt(original)
	if err != nil {
		return "", fmt.Errorf("decode source run deployment catalog receipt: %w", err)
	}
	lockIdentity, err := canonicalManifestDeploymentCatalogLockIdentity(original)
	if err != nil {
		return "", fmt.Errorf("decode source run deployment catalog lock identity: %w", err)
	}
	if err := service.core.verifyRunDeploymentCatalogReceipt(source); err != nil {
		return "", fmt.Errorf("verify source run deployment catalog receipt: %w", err)
	}
	if err := original.InitialExecutionPlan.Validate(original.Resolved.Descriptor); err != nil {
		return "", fmt.Errorf("validate source run initial execution plan: %w", err)
	}
	manifest := runManifest{Format: "harbor.workflow-run-manifest.v2", RunID: candidate.TargetRunID, TaskID: candidate.TaskID,
		Revision: candidate.TargetRevisionID, Resolved: original.Resolved.Clone(), InitialExecutionPlan: original.InitialExecutionPlan.Clone(),
		DeploymentCatalogReceipt:      append(json.RawMessage(nil), catalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(lockIdentity), Created: service.core.now().UTC()}
	path := filepath.Join(service.core.layout.runDirectory(candidate.TargetRunID), "run-manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	if len(catalogReceipt) != 0 {
		receiptPath := filepath.Join(filepath.Dir(path), deploymentCatalogReceiptFileName)
		if err := writeNewBytes(receiptPath, catalogReceipt); err != nil {
			if !os.IsExist(err) {
				return "", fmt.Errorf("write candidate child deployment catalog receipt: %w", err)
			}
			existingReceipt, readErr := readManagedRunReceiptFile(receiptPath)
			if readErr != nil {
				return "", readErr
			}
			if !bytes.Equal(existingReceipt, catalogReceipt) {
				return "", fmt.Errorf("existing candidate child deployment catalog receipt conflicts")
			}
		}
	}
	if lockIdentity != nil {
		canonicalLockIdentity, lockErr := canonicalDeploymentCatalogLockIdentity(*lockIdentity)
		if lockErr != nil {
			return "", fmt.Errorf("canonicalize candidate child deployment catalog lock identity: %w", lockErr)
		}
		lockPath := filepath.Join(filepath.Dir(path), deploymentCatalogLockIdentityFileName)
		if lockErr := writeNewBytes(lockPath, canonicalLockIdentity); lockErr != nil {
			if !os.IsExist(lockErr) {
				return "", fmt.Errorf("write candidate child deployment catalog lock identity: %w", lockErr)
			}
			existingLockIdentityRaw, readErr := readManagedRunLockIdentityFile(lockPath)
			if readErr != nil {
				return "", readErr
			}
			existingLockIdentity, existingCanonicalLockIdentity, parseErr := parseDeploymentCatalogLockIdentityJSON(existingLockIdentityRaw)
			if parseErr != nil || !bytes.Equal(existingLockIdentityRaw, existingCanonicalLockIdentity) || existingLockIdentity != *lockIdentity {
				return "", fmt.Errorf("existing candidate child deployment catalog lock identity conflicts: %v", parseErr)
			}
		}
	}
	if err := writeNewJSON(path, manifest); err != nil {
		if !os.IsExist(err) {
			return "", err
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		var existing runManifest
		if decodeErr := decodeStrictJSON(string(raw), &existing); decodeErr != nil || existing.RunID != manifest.RunID || existing.TaskID != manifest.TaskID || existing.Revision != manifest.Revision || !manifestMatchesDeploymentCatalogReceipt(existing, catalogReceipt) || !manifestMatchesDeploymentCatalogLockIdentity(existing, lockIdentity) {
			return "", fmt.Errorf("existing candidate child run manifest conflicts: %v", decodeErr)
		}
		return string(raw), nil
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("internal JSON serialization failed: %v", err))
	}
	return string(encoded)
}

// ExecuteTaskChange consumes only the plan/candidate facts frozen by
// PlanTaskChange. It never invokes a provider, reapplies a diff, or recomputes
// a selector during execution.
func (service *ChangeProviderService) ExecuteTaskChange(ctx context.Context, planID string, actor, reason string) (store.RevisionCandidateContinuationCommit, error) {
	if service == nil || service.core == nil {
		return store.RevisionCandidateContinuationCommit{}, fmt.Errorf("change provider service is not configured")
	}
	plan, err := (&TaskContinuationService{core: service.core}).GetTaskContinuationPlan(ctx, planID)
	if err != nil {
		return store.RevisionCandidateContinuationCommit{}, err
	}
	snapshot := plan.Snapshot()
	if snapshot.Strategy != workflowkit.StrategyReviseSubject || snapshot.CandidateRevisionID == "" {
		return store.RevisionCandidateContinuationCommit{}, fmt.Errorf("plan %s is not a revision candidate continuation", planID)
	}
	command, err := service.core.store.GetContinuationCommand(ctx, snapshot.CommandID)
	if err != nil || command == nil {
		if err != nil {
			return store.RevisionCandidateContinuationCommit{}, err
		}
		return store.RevisionCandidateContinuationCommit{}, fmt.Errorf("continuation command is missing")
	}
	if actor = strings.TrimSpace(actor); actor == "" {
		actor = command.Actor
	} else if actor != command.Actor {
		return store.RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: candidate continuation actor differs from frozen command", store.ErrIdempotencyConflict)
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = command.Reason
	} else if reason != command.Reason {
		return store.RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: candidate continuation reason differs from frozen command", store.ErrIdempotencyConflict)
	}
	sourceRun, err := service.core.store.GetWorkflowRun(ctx, snapshot.SourceRunID)
	if err != nil {
		return store.RevisionCandidateContinuationCommit{}, err
	}
	if sourceRun == nil {
		return store.RevisionCandidateContinuationCommit{}, fmt.Errorf("source workflow run is missing")
	}
	frozen, err := decodeFrozenRunDefinition(*sourceRun)
	if err != nil {
		return store.RevisionCandidateContinuationCommit{}, err
	}
	candidate, err := service.core.store.GetRevisionCandidate(ctx, snapshot.CandidateRevisionID)
	if err != nil {
		return store.RevisionCandidateContinuationCommit{}, err
	}
	if candidate == nil {
		return store.RevisionCandidateContinuationCommit{}, fmt.Errorf("revision candidate is missing for plan %s", snapshot.PlanID)
	}
	payload := continuationExecutionPayload{Format: continuationExecutionFormat, PlanID: snapshot.PlanID, CommandID: snapshot.CommandID,
		PlanFingerprint: plan.Fingerprint(), RunID: candidate.TargetRunID, SourceRunID: snapshot.SourceRunID, QuotaPolicy: frozen.QuotaPolicy.Clone()}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return store.RevisionCandidateContinuationCommit{}, err
	}
	commit, err := service.core.store.CommitRevisionCandidateContinuation(ctx, store.CommitRevisionCandidateContinuationRequest{
		PlanID: snapshot.PlanID, CandidateID: snapshot.CandidateRevisionID, IdempotencyKey: continuationExecutionKey(snapshot.PlanID),
		PayloadJSON: string(encoded), Expected: storeCheckpoint(snapshot.BaseCheckpoint), Actor: actor, Reason: reason,
	})
	if !errors.Is(err, store.ErrContinuationPlanExpired) {
		return commit, err
	}
	candidate, candidateErr := service.core.store.GetRevisionCandidate(ctx, snapshot.CandidateRevisionID)
	if candidateErr != nil {
		return store.RevisionCandidateContinuationCommit{}, candidateErr
	}
	if candidate == nil {
		return store.RevisionCandidateContinuationCommit{}, fmt.Errorf("revision candidate is missing for expired plan %s", planID)
	}
	if expireErr := service.expireCandidatePlan(ctx, *candidate, actor, "frozen continuation plan expired before candidate execution"); expireErr != nil {
		return store.RevisionCandidateContinuationCommit{}, fmt.Errorf("expire candidate for %w: %v", err, expireErr)
	}
	return store.RevisionCandidateContinuationCommit{}, err
}
