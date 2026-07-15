package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// LifecycleMutationAction names the typed application commands exposed to UI
// and CLI adapters. It deliberately mirrors lifecycle semantics rather than
// a presentation shortcut or a filesystem operation.
type LifecycleMutationAction string

const (
	LifecycleMutationCreateDraft LifecycleMutationAction = "task.create"
	LifecycleMutationImport      LifecycleMutationAction = "task.import"
	LifecycleMutationFork        LifecycleMutationAction = "task.fork"
	LifecycleMutationArchive     LifecycleMutationAction = "task.archive"
	LifecycleMutationSoftDelete  LifecycleMutationAction = "task.soft_delete"
	LifecycleMutationRestore     LifecycleMutationAction = "task.restore"
	LifecycleMutationStartRun    LifecycleMutationAction = "run.start"
	// LifecycleMutationCodeEdgeEvaluator records the strict child Run used to
	// collect the Qwen then Opus pass@4 evidence for one approved Phase-1 Run.
	// It is distinct from generic run.start so a frozen generic input bundle can
	// never be replayed as a production evaluator invocation.
	LifecycleMutationCodeEdgeEvaluator LifecycleMutationAction = "run.codeedge_evaluator"
	// LifecycleMutationCodeEdgeEvaluatorEvidenceHandoff adopts one completed
	// child Run's verified evidence into its Phase-1 parent. It is not a generic
	// artifact import and accepts no caller-supplied receipt bytes.
	LifecycleMutationCodeEdgeEvaluatorEvidenceHandoff LifecycleMutationAction = "run.codeedge_evaluator_evidence_handoff"
	LifecycleMutationPackage                          LifecycleMutationAction = "release.local_package"
	LifecycleMutationWithdraw                         LifecycleMutationAction = "release.withdraw"
	LifecycleMutationEdit                             LifecycleMutationAction = "task.edit"
	LifecycleMutationReview                           LifecycleMutationAction = "review.decide"
)

// LifecycleMutationCheckpoint is the complete optimistic identity captured by
// a lifecycle preview. Zero-value entity groups are valid only when the action
// has no existing target, such as creating or importing a new task.
type LifecycleMutationCheckpoint struct {
	TaskID               string `json:"task_id,omitempty"`
	TaskVersion          int64  `json:"task_version,omitempty"`
	RevisionID           string `json:"revision_id,omitempty"`
	RevisionStateVersion int64  `json:"revision_state_version,omitempty"`
	RevisionDigest       string `json:"revision_digest,omitempty"`
	RunID                string `json:"run_id,omitempty"`
	RunVersion           int64  `json:"run_version,omitempty"`
	RunExecutionEpoch    int    `json:"run_execution_epoch,omitempty"`
	RunDefinitionHash    string `json:"run_definition_hash,omitempty"`
	// CodeEdgeComplianceRecordID and CodeEdgeAuthorizationFingerprint bind a
	// package confirmation to the exact immutable authorization observed for
	// its selected Run. They are populated only when that Run has a recorded
	// CodeEdge final-compliance result.
	CodeEdgeComplianceRecordID       string `json:"codeedge_compliance_record_id,omitempty"`
	CodeEdgeAuthorizationFingerprint string `json:"codeedge_authorization_fingerprint,omitempty"`
	ReleaseID                        string `json:"release_id,omitempty"`
	ReleaseRecordVersion             int64  `json:"release_record_version,omitempty"`
	ReviewRequestID                  string `json:"review_request_id,omitempty"`
	ReviewRevisionID                 string `json:"review_revision_id,omitempty"`
	ReviewState                      string `json:"review_state,omitempty"`
	ReviewEvidenceDigest             string `json:"review_evidence_digest,omitempty"`
}

// LifecycleMutationCommandBase holds common operator provenance and the
// checkpoint supplied by the preview. IdempotencyKey is always caller-created
// UUIDv7 and is retained unchanged when a response is lost.
type LifecycleMutationCommandBase struct {
	IdempotencyKey string                      `json:"idempotency_key"`
	Actor          string                      `json:"actor"`
	Reason         string                      `json:"reason"`
	Expected       LifecycleMutationCheckpoint `json:"expected"`
}

type CreateDraftLifecycleCommand struct {
	LifecycleMutationCommandBase
	Slug         string `json:"slug"`
	Title        string `json:"title"`
	MetadataJSON string `json:"metadata_json"`
	SourceRepo   string `json:"source_repo,omitempty"`
	SourceCommit string `json:"source_commit,omitempty"`
}

type ImportLifecycleCommand struct {
	LifecycleMutationCommandBase
	Slug           string `json:"slug"`
	Title          string `json:"title"`
	MetadataJSON   string `json:"metadata_json"`
	SourceRepo     string `json:"source_repo,omitempty"`
	SourceCommit   string `json:"source_commit,omitempty"`
	SourcePath     string `json:"source_path"`
	ProposalDigest string `json:"proposal_digest,omitempty"`
	ChangeSummary  string `json:"change_summary,omitempty"`
}

type ForkLifecycleCommand struct {
	LifecycleMutationCommandBase
	Slug         string `json:"slug"`
	Title        string `json:"title"`
	MetadataJSON string `json:"metadata_json"`
}

type RestoreLifecycleCommand struct {
	LifecycleMutationCommandBase
	RestoreState store.TaskLifecycleState `json:"restore_state"`
}

type StartRunLifecycleCommand struct {
	LifecycleMutationCommandBase
	ProfilePath       string `json:"profile_path"`
	ExecutionSpecPath string `json:"execution_spec_path"`
	// ParentRunID is optional for the generic run-start surface. The strict
	// evaluator launch service always requires and freezes it.
	ParentRunID string `json:"parent_run_id,omitempty"`
	Trigger     string `json:"trigger"`
}

// CodeEdgeEvaluatorLaunchCommand intentionally contains no profile, spec,
// provider, model, endpoint, secret, or argv fields. Those facts are owned by
// the catalog/lock-attested definition provider installed at composition time.
type CodeEdgeEvaluatorLaunchCommand struct {
	LifecycleMutationCommandBase
	ParentRunID string `json:"parent_run_id"`
}

// CodeEdgeEvaluatorEvidenceHandoffCommand deliberately accepts only the two
// durable Run identities. The application service rebuilds and verifies every
// receipt, trial, artifact, catalog, and manifest fact itself; callers cannot
// smuggle a handoff document or any evidence bytes across this boundary.
type CodeEdgeEvaluatorEvidenceHandoffCommand struct {
	LifecycleMutationCommandBase
	ParentRunID string `json:"parent_run_id"`
	ChildRunID  string `json:"child_run_id"`
}

// PreparedCodeEdgeEvaluatorEvidenceHandoff is the durable first-confirmation
// receipt for an evidence adoption. Confirming it later consumes precisely the
// captured parent checkpoint and child identity; a direct confirm is rejected.
type PreparedCodeEdgeEvaluatorEvidenceHandoff struct {
	OperationID          string
	ParentRunID          string
	ChildRunID           string
	HandoffFingerprint   workflowkit.Fingerprint
	QwenTrialFingerprint workflowkit.Fingerprint
	OpusTrialFingerprint workflowkit.Fingerprint
}

type codeEdgeEvaluatorEvidenceHandoffPayload struct {
	Format      string `json:"format"`
	ParentRunID string `json:"parent_run_id"`
	ChildRunID  string `json:"child_run_id"`
}

type PackageLifecycleCommand struct {
	LifecycleMutationCommandBase
	ReleaseVersion string `json:"release_version"`
}

type EditLifecycleCommand struct {
	LifecycleMutationCommandBase
	UnifiedDiff string `json:"unified_diff"`
}

// DecideReviewLifecycleCommand records one immutable decision for the exact
// ReviewRequest facts captured in a Task Hub preview. The decision action is a
// typed domain enum rather than a TUI-local string.
type DecideReviewLifecycleCommand struct {
	LifecycleMutationCommandBase
	Decision store.ReviewDecisionAction `json:"decision"`
}

// LifecycleMutationReceipt is a UI-safe typed outcome. It is also the only
// payload stored in the V12 operation result, allowing a retry to return the
// original identities without recomputing against newer lifecycle state.
type LifecycleMutationReceipt struct {
	Action                              LifecycleMutationAction `json:"action"`
	OperationID                         string                  `json:"operation_id"`
	TaskID                              string                  `json:"task_id,omitempty"`
	TaskVersion                         int64                   `json:"task_version,omitempty"`
	RevisionID                          string                  `json:"revision_id,omitempty"`
	RevisionStateVersion                int64                   `json:"revision_state_version,omitempty"`
	RunID                               string                  `json:"run_id,omitempty"`
	ParentRunID                         string                  `json:"parent_run_id,omitempty"`
	ChildRunID                          string                  `json:"child_run_id,omitempty"`
	RunVersion                          int64                   `json:"run_version,omitempty"`
	EvaluatorEvidenceHandoffID          string                  `json:"evaluator_evidence_handoff_id,omitempty"`
	EvaluatorEvidenceHandoffFingerprint string                  `json:"evaluator_evidence_handoff_fingerprint,omitempty"`
	ReleaseID                           string                  `json:"release_id,omitempty"`
	ReleaseVersion                      string                  `json:"release_version,omitempty"`
	DeletionRecordID                    string                  `json:"deletion_record_id,omitempty"`
	ReviewRequestID                     string                  `json:"review_request_id,omitempty"`
	ReviewDecisionID                    string                  `json:"review_decision_id,omitempty"`
	ReviewDecision                      string                  `json:"review_decision,omitempty"`
	PlanID                              string                  `json:"plan_id,omitempty"`
	ExecutionID                         string                  `json:"execution_id,omitempty"`
	CandidateID                         string                  `json:"candidate_id,omitempty"`
	Summary                             string                  `json:"summary"`
}

// LifecycleMutationService is the typed application boundary for Task Hub
// lifecycle actions. It owns V12 idempotency receipts and checkpoint checks;
// presentation adapters never write the store or a managed directory.
type LifecycleMutationService struct{ core *lifecycleServiceCore }

func newLifecycleMutationService(core *lifecycleServiceCore) *LifecycleMutationService {
	return &LifecycleMutationService{core: core}
}

// CaptureCheckpoint reads all existing entities that a lifecycle preview is
// about to bind. It rejects cross-task target combinations before a UI can
// display a confirmation that would later target a different entity.
func (service *LifecycleMutationService) CaptureCheckpoint(ctx context.Context, taskID, revisionID, runID, releaseID string) (LifecycleMutationCheckpoint, error) {
	return service.captureCheckpoint(ctx, taskID, revisionID, runID, releaseID, "")
}

// CaptureReviewCheckpoint captures the same complete lifecycle identity as a
// regular mutation preview, plus the immutable review request binding. Review
// requests have no mutable record version, so state, revision, and evidence
// digest are the full optimistic facts that must be retained by a TUI form.
func (service *LifecycleMutationService) CaptureReviewCheckpoint(ctx context.Context, taskID, revisionID, reviewRequestID string) (LifecycleMutationCheckpoint, error) {
	if strings.TrimSpace(reviewRequestID) == "" {
		return LifecycleMutationCheckpoint{}, fmt.Errorf("review checkpoint requires a ReviewRequest")
	}
	runID := ""
	if service == nil || service.core == nil || service.core.store == nil {
		return LifecycleMutationCheckpoint{}, fmt.Errorf("lifecycle mutation service is not configured")
	}
	binding, err := service.core.store.GetReviewGateBindingByReviewRequest(ctx, reviewRequestID)
	if err != nil {
		return LifecycleMutationCheckpoint{}, err
	}
	if binding != nil {
		runID = binding.RunID
	}
	return service.captureCheckpoint(ctx, taskID, revisionID, runID, "", reviewRequestID)
}

func (service *LifecycleMutationService) captureCheckpoint(ctx context.Context, taskID, revisionID, runID, releaseID, reviewRequestID string) (LifecycleMutationCheckpoint, error) {
	if service == nil || service.core == nil {
		return LifecycleMutationCheckpoint{}, fmt.Errorf("lifecycle mutation service is not configured")
	}
	checkpoint := LifecycleMutationCheckpoint{}
	var run *store.WorkflowRun
	var revision *store.TaskRevision
	var release *store.LocalPackageRelease
	var review *store.ReviewRequest
	var err error
	loadRevision := func(id string) error {
		loaded, loadErr := service.core.store.GetTaskRevision(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		if loaded == nil {
			return fmt.Errorf("%w: revision %s", ErrLifecycleNotFound, id)
		}
		revision = loaded
		checkpoint.RevisionID, checkpoint.RevisionStateVersion, checkpoint.RevisionDigest = revision.ID, revision.StateVersion, revision.TaskDigest
		if taskID == "" {
			taskID = revision.TaskID
		}
		return nil
	}
	if runID = strings.TrimSpace(runID); runID != "" {
		run, err = service.core.store.GetWorkflowRun(ctx, runID)
		if err != nil {
			return LifecycleMutationCheckpoint{}, err
		}
		if run == nil {
			return LifecycleMutationCheckpoint{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
		}
		checkpoint.RunID, checkpoint.RunVersion, checkpoint.RunExecutionEpoch, checkpoint.RunDefinitionHash = run.ID, run.Version, run.ExecutionEpoch, run.DefinitionHash
		compliance, complianceErr := service.core.store.GetCodeEdgeComplianceRecordForRun(ctx, run.ID)
		if complianceErr != nil {
			return LifecycleMutationCheckpoint{}, complianceErr
		}
		if compliance != nil {
			checkpoint.CodeEdgeComplianceRecordID = compliance.ID
			checkpoint.CodeEdgeAuthorizationFingerprint = compliance.AuthorizationFingerprint
		}
		if taskID == "" {
			taskID = run.TaskID
		}
		if revisionID == "" {
			revisionID = run.RevisionID
		}
	}
	if revisionID = strings.TrimSpace(revisionID); revisionID != "" {
		if err := loadRevision(revisionID); err != nil {
			return LifecycleMutationCheckpoint{}, err
		}
	}
	if releaseID = strings.TrimSpace(releaseID); releaseID != "" {
		release, err = service.core.store.GetLocalPackageRelease(ctx, releaseID)
		if err != nil {
			return LifecycleMutationCheckpoint{}, err
		}
		if release == nil {
			return LifecycleMutationCheckpoint{}, fmt.Errorf("%w: release %s", ErrLifecycleNotFound, releaseID)
		}
		checkpoint.ReleaseID, checkpoint.ReleaseRecordVersion = release.ID, release.RecordVersion
		if taskID == "" {
			taskID = release.TaskID
		}
		if revisionID == "" {
			revisionID = release.RevisionID
			if err := loadRevision(revisionID); err != nil {
				return LifecycleMutationCheckpoint{}, err
			}
		}
	}
	if reviewRequestID = strings.TrimSpace(reviewRequestID); reviewRequestID != "" {
		review, err = service.core.store.GetReviewRequest(ctx, reviewRequestID)
		if err != nil {
			return LifecycleMutationCheckpoint{}, err
		}
		if review == nil {
			return LifecycleMutationCheckpoint{}, fmt.Errorf("%w: review request %s", ErrLifecycleNotFound, reviewRequestID)
		}
		checkpoint.ReviewRequestID = review.ID
		checkpoint.ReviewRevisionID = review.RevisionID
		checkpoint.ReviewState = review.State
		checkpoint.ReviewEvidenceDigest = review.EvidenceManifestDigest
		if revisionID == "" {
			revisionID = review.RevisionID
			if err := loadRevision(revisionID); err != nil {
				return LifecycleMutationCheckpoint{}, err
			}
		}
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return checkpoint, nil
	}
	task, err := service.core.store.GetTaskV2(ctx, taskID)
	if err != nil {
		return LifecycleMutationCheckpoint{}, err
	}
	if task == nil {
		return LifecycleMutationCheckpoint{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, taskID)
	}
	checkpoint.TaskID, checkpoint.TaskVersion = task.ID, task.Version
	if revision != nil && revision.TaskID != task.ID {
		return LifecycleMutationCheckpoint{}, fmt.Errorf("revision %s does not belong to task %s", revision.ID, task.ID)
	}
	if run != nil && (run.TaskID != task.ID || (revision != nil && run.RevisionID != revision.ID)) {
		return LifecycleMutationCheckpoint{}, fmt.Errorf("run %s does not match lifecycle checkpoint task or revision", run.ID)
	}
	if release != nil && (release.TaskID != task.ID || (revision != nil && release.RevisionID != revision.ID)) {
		return LifecycleMutationCheckpoint{}, fmt.Errorf("release %s does not match lifecycle checkpoint task or revision", release.ID)
	}
	if review != nil && (revision == nil || review.RevisionID != revision.ID) {
		return LifecycleMutationCheckpoint{}, fmt.Errorf("review request %s does not match lifecycle checkpoint revision", review.ID)
	}
	return checkpoint, nil
}

func (service *LifecycleMutationService) CreateDraft(ctx context.Context, command CreateDraftLifecycleCommand) (LifecycleMutationReceipt, error) {
	if !command.Expected.empty() {
		return LifecycleMutationReceipt{}, fmt.Errorf("create draft must not carry an existing lifecycle checkpoint")
	}
	taskID, err := store.NewUUIDv7()
	if err != nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("allocate task identity: %w", err)
	}
	op, replay, err := service.begin(ctx, LifecycleMutationCreateDraft, command.LifecycleMutationCommandBase, command, lifecycleOperationTargets{TaskID: taskID})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	task, err := (&TaskService{core: service.core}).CreateDraft(ctx, CreateDraftTaskRequest{
		ID: op.TaskID, Slug: command.Slug, Title: command.Title, MetadataJSON: command.MetadataJSON,
		SourceRepo: command.SourceRepo, SourceCommit: command.SourceCommit, Actor: op.Actor, Reason: op.Reason,
	})
	if err != nil {
		if !errors.Is(err, store.ErrIdentityCollision) {
			return LifecycleMutationReceipt{}, err
		}
		existing, lookupErr := service.core.store.GetTaskV2(ctx, op.TaskID)
		if lookupErr != nil || existing == nil || !sameDraftLifecycleTask(*existing, command) {
			if lookupErr != nil {
				return LifecycleMutationReceipt{}, lookupErr
			}
			return LifecycleMutationReceipt{}, err
		}
		task = *existing
	}
	return service.complete(ctx, op, LifecycleMutationReceipt{Action: LifecycleMutationCreateDraft, TaskID: task.ID, TaskVersion: task.Version, Summary: "Task 草稿已创建"})
}

func (service *LifecycleMutationService) Import(ctx context.Context, command ImportLifecycleCommand) (LifecycleMutationReceipt, error) {
	if !command.Expected.empty() {
		return LifecycleMutationReceipt{}, fmt.Errorf("import must not carry an existing lifecycle checkpoint")
	}
	if receipt, replayed, err := service.completedOperationReplay(ctx, LifecycleMutationImport, command.LifecycleMutationCommandBase); err != nil {
		return LifecycleMutationReceipt{}, err
	} else if replayed {
		return receipt, nil
	}
	sourcePath, sourceDigest, err := lifecycleImportSource(command.SourcePath)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	taskID, err := store.NewUUIDv7()
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	revisionID, err := store.NewUUIDv7()
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	payload := struct {
		Command      ImportLifecycleCommand `json:"command"`
		SourcePath   string                 `json:"source_path"`
		SourceDigest string                 `json:"source_digest"`
	}{Command: command, SourcePath: sourcePath, SourceDigest: sourceDigest}
	op, replay, err := service.begin(ctx, LifecycleMutationImport, command.LifecycleMutationCommandBase, payload, lifecycleOperationTargets{TaskID: taskID, RevisionID: revisionID})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if recovered, ok, recoverErr := service.recoverInitialTask(ctx, op); recoverErr != nil || ok {
		if recoverErr != nil {
			return LifecycleMutationReceipt{}, recoverErr
		}
		return service.complete(ctx, op, recovered)
	}
	task, revision, err := (&TaskService{core: service.core}).ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{ID: op.TaskID, Slug: command.Slug, Title: command.Title, MetadataJSON: command.MetadataJSON, SourceRepo: command.SourceRepo, SourceCommit: command.SourceCommit, Actor: op.Actor, Reason: op.Reason},
		InitialRevisionID:      op.RevisionID,
		SourceDirectory:        sourcePath,
		ProposalDigest:         command.ProposalDigest,
		ChangeSummary:          command.ChangeSummary,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return service.complete(ctx, op, LifecycleMutationReceipt{Action: LifecycleMutationImport, TaskID: task.ID, TaskVersion: task.Version, RevisionID: revision.ID, RevisionStateVersion: revision.StateVersion, Summary: "Task 已从受管快照导入"})
}

func (service *LifecycleMutationService) Fork(ctx context.Context, command ForkLifecycleCommand) (LifecycleMutationReceipt, error) {
	if command.Expected.TaskID == "" || command.Expected.RevisionID == "" {
		return LifecycleMutationReceipt{}, fmt.Errorf("fork requires a source task and revision checkpoint")
	}
	taskID, err := store.NewUUIDv7()
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	revisionID, err := store.NewUUIDv7()
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	op, replay, err := service.begin(ctx, LifecycleMutationFork, command.LifecycleMutationCommandBase, command, lifecycleOperationTargets{TaskID: taskID, RevisionID: revisionID})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if err := service.validateCheckpoint(ctx, command.Expected); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if recovered, ok, recoverErr := service.recoverInitialTask(ctx, op); recoverErr != nil || ok {
		if recoverErr != nil {
			return LifecycleMutationReceipt{}, recoverErr
		}
		return service.complete(ctx, op, recovered)
	}
	task, revision, err := (&TaskService{core: service.core}).ForkTask(ctx, ForkTaskRequest{
		SourceTaskID: command.Expected.TaskID, SourceRevisionID: command.Expected.RevisionID, ID: op.TaskID, InitialRevisionID: op.RevisionID,
		Slug: command.Slug, Title: command.Title, MetadataJSON: command.MetadataJSON, Actor: op.Actor, Reason: op.Reason,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return service.complete(ctx, op, LifecycleMutationReceipt{Action: LifecycleMutationFork, TaskID: task.ID, TaskVersion: task.Version, RevisionID: revision.ID, RevisionStateVersion: revision.StateVersion, Summary: "Task Fork 已创建"})
}

func (service *LifecycleMutationService) Archive(ctx context.Context, base LifecycleMutationCommandBase) (LifecycleMutationReceipt, error) {
	return service.executeTaskTransition(ctx, LifecycleMutationArchive, base, store.TaskLifecycleArchived)
}

func (service *LifecycleMutationService) SoftDelete(ctx context.Context, base LifecycleMutationCommandBase) (LifecycleMutationReceipt, error) {
	return service.executeTaskTransition(ctx, LifecycleMutationSoftDelete, base, store.TaskLifecycleDeleted)
}

func (service *LifecycleMutationService) Restore(ctx context.Context, command RestoreLifecycleCommand) (LifecycleMutationReceipt, error) {
	if command.RestoreState == "" || command.RestoreState == store.TaskLifecycleDeleted {
		return LifecycleMutationReceipt{}, fmt.Errorf("restore target lifecycle state is required")
	}
	return service.executeTaskTransition(ctx, LifecycleMutationRestore, command.LifecycleMutationCommandBase, command.RestoreState)
}

func (service *LifecycleMutationService) StartRun(ctx context.Context, command StartRunLifecycleCommand) (LifecycleMutationReceipt, error) {
	if receipt, replayed, err := service.completedOperationReplay(ctx, LifecycleMutationStartRun, command.LifecycleMutationCommandBase); err != nil {
		return LifecycleMutationReceipt{}, err
	} else if replayed {
		return receipt, nil
	}
	inputs, err := service.prepareStartRunInputs(ctx, command)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	payload := struct {
		InputBundleID                 string                                                `json:"input_bundle_id"`
		ProfileFingerprint            string                                                `json:"profile_fingerprint"`
		ExecutionSpecFingerprint      string                                                `json:"execution_spec_fingerprint"`
		DeploymentCatalogReceipt      []byte                                                `json:"deployment_catalog_receipt,omitempty"`
		DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity `json:"deployment_catalog_lock_identity,omitempty"`
	}{
		InputBundleID:                 inputs.Bundle.IdempotencyKey,
		ProfileFingerprint:            string(inputs.Bundle.ProfileFingerprint),
		ExecutionSpecFingerprint:      string(inputs.Bundle.ExecutionSpecFingerprint),
		DeploymentCatalogReceipt:      append([]byte(nil), inputs.DeploymentCatalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(inputs.DeploymentCatalogLockIdentity),
	}
	op, replay, err := service.begin(ctx, LifecycleMutationStartRun, command.LifecycleMutationCommandBase, payload, lifecycleOperationTargets{TaskID: command.Expected.TaskID, RevisionID: command.Expected.RevisionID, RunID: runID})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if existing, lookupErr := service.core.store.GetWorkflowRun(ctx, op.RunID); lookupErr != nil {
		return LifecycleMutationReceipt{}, lookupErr
	} else if existing != nil {
		if existing.TaskID != op.TaskID || existing.RevisionID != op.RevisionID || existing.ResolvedProfileHash != string(inputs.Bundle.ProfileFingerprint) {
			return LifecycleMutationReceipt{}, fmt.Errorf("%w: run %s does not match lifecycle operation", store.ErrIdempotencyConflict, existing.ID)
		}
		return service.complete(ctx, op, receiptForRun(LifecycleMutationStartRun, *existing))
	}
	if err := service.validateCheckpoint(ctx, command.Expected); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if strings.TrimSpace(command.Trigger) == "" {
		return LifecycleMutationReceipt{}, fmt.Errorf("run trigger is required")
	}
	run, err := (&RunService{core: service.core}).StartRun(ctx, StartRunRequest{
		ID: op.RunID, TaskID: op.TaskID, RevisionID: op.RevisionID,
		Profile: inputs.Profile, ExecutionSpec: inputs.ExecutionSpec,
		InputBundleID:                 inputs.Bundle.IdempotencyKey,
		ProfileFingerprint:            inputs.Bundle.ProfileFingerprint,
		ExecutionSpecFingerprint:      inputs.Bundle.ExecutionSpecFingerprint,
		DeploymentCatalogReceipt:      append([]byte(nil), inputs.DeploymentCatalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(inputs.DeploymentCatalogLockIdentity),
		ParentRunID:                   inputs.Bundle.ParentRunID,
		Trigger:                       inputs.Bundle.Trigger, Actor: op.Actor, Reason: op.Reason,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return service.complete(ctx, op, receiptForRun(LifecycleMutationStartRun, run))
}

// PrepareCodeEdgeEvaluatorEvidenceHandoff writes the first-confirmation
// lifecycle receipt only after read-only verification of the closed
// parent/child evidence graph. It deliberately creates no handoff and no
// provider side effect.
func (service *LifecycleMutationService) PrepareCodeEdgeEvaluatorEvidenceHandoff(ctx context.Context, command CodeEdgeEvaluatorEvidenceHandoffCommand) (PreparedCodeEdgeEvaluatorEvidenceHandoff, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return PreparedCodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("lifecycle mutation service is not configured")
	}
	if err := validateCodeEdgeEvaluatorEvidenceHandoffCommand(command); err != nil {
		return PreparedCodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	plan, err := (&CodeEdgeEvaluatorEvidenceHandoffService{core: service.core}).Plan(ctx, command.ParentRunID, command.ChildRunID)
	if err != nil {
		return PreparedCodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	op, replay, err := service.begin(ctx, LifecycleMutationCodeEdgeEvaluatorEvidenceHandoff, command.LifecycleMutationCommandBase, codeEdgeEvaluatorEvidenceHandoffPayload{
		Format: "harbor.codeedge-evaluator-evidence-handoff.prepare.v1", ParentRunID: strings.TrimSpace(command.ParentRunID), ChildRunID: strings.TrimSpace(command.ChildRunID),
	}, lifecycleOperationTargets{TaskID: command.Expected.TaskID, RevisionID: command.Expected.RevisionID, RunID: command.ParentRunID})
	if err != nil {
		return PreparedCodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	if replay != nil {
		return PreparedCodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("completed CodeEdge evaluator evidence handoff cannot be prepared again")
	}
	if err := service.validateCheckpoint(ctx, command.Expected); err != nil {
		return PreparedCodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	return PreparedCodeEdgeEvaluatorEvidenceHandoff{
		OperationID:          op.ID,
		ParentRunID:          plan.ParentRunID,
		ChildRunID:           plan.ChildRunID,
		HandoffFingerprint:   plan.HandoffFingerprint,
		QwenTrialFingerprint: plan.QwenTrialFingerprint,
		OpusTrialFingerprint: plan.OpusTrialFingerprint,
	}, nil
}

// AdoptCodeEdgeEvaluatorEvidenceHandoff is the sole generic lifecycle-mutation
// route for adopting a completed child evaluator Run. It is UUIDv7-idempotent
// and keeps its durable operation separate from the handoff identity, so a
// lost response can be replayed without manufacturing a second evidence
// bridge. The child is never supplied as a caller-owned evidence payload.
func (service *LifecycleMutationService) AdoptCodeEdgeEvaluatorEvidenceHandoff(ctx context.Context, command CodeEdgeEvaluatorEvidenceHandoffCommand) (LifecycleMutationReceipt, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("lifecycle mutation service is not configured")
	}
	if err := validateCodeEdgeEvaluatorEvidenceHandoffCommand(command); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if receipt, replayed, err := service.completedOperationReplay(ctx, LifecycleMutationCodeEdgeEvaluatorEvidenceHandoff, command.LifecycleMutationCommandBase); err != nil {
		return LifecycleMutationReceipt{}, err
	} else if replayed {
		return receipt, nil
	}
	prepared, err := service.core.store.GetLifecycleOperationByIdempotencyKey(ctx, command.IdempotencyKey)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if prepared == nil || prepared.Action != string(LifecycleMutationCodeEdgeEvaluatorEvidenceHandoff) || prepared.State != store.LifecycleOperationPrepared {
		return LifecycleMutationReceipt{}, fmt.Errorf("CodeEdge evaluator evidence handoff must be prepared before confirmation")
	}
	op, replay, err := service.begin(ctx, LifecycleMutationCodeEdgeEvaluatorEvidenceHandoff, command.LifecycleMutationCommandBase, codeEdgeEvaluatorEvidenceHandoffPayload{
		Format: "harbor.codeedge-evaluator-evidence-handoff.prepare.v1", ParentRunID: strings.TrimSpace(command.ParentRunID), ChildRunID: strings.TrimSpace(command.ChildRunID),
	}, lifecycleOperationTargets{TaskID: command.Expected.TaskID, RevisionID: command.Expected.RevisionID, RunID: command.ParentRunID})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if err := service.validateCheckpoint(ctx, command.Expected); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	handoff, err := (&CodeEdgeEvaluatorEvidenceHandoffService{core: service.core}).Record(ctx, RecordCodeEdgeEvaluatorEvidenceHandoffRequest{
		ID:             op.IdempotencyKey,
		IdempotencyKey: op.IdempotencyKey,
		ParentRunID:    command.ParentRunID,
		ChildRunID:     command.ChildRunID,
		Actor:          op.Actor,
		Reason:         op.Reason,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return service.complete(ctx, op, LifecycleMutationReceipt{
		Action:                              LifecycleMutationCodeEdgeEvaluatorEvidenceHandoff,
		TaskID:                              handoff.TaskID,
		RevisionID:                          handoff.RevisionID,
		RunID:                               handoff.ParentRunID,
		ParentRunID:                         handoff.ParentRunID,
		ChildRunID:                          handoff.ChildRunID,
		EvaluatorEvidenceHandoffID:          handoff.ID,
		EvaluatorEvidenceHandoffFingerprint: handoff.HandoffFingerprint,
		Summary:                             "已采用并验证 CodeEdge evaluator child 证据",
	})
}

func validateCodeEdgeEvaluatorEvidenceHandoffCommand(command CodeEdgeEvaluatorEvidenceHandoffCommand) error {
	if err := store.ValidateUUIDv7(strings.TrimSpace(command.ParentRunID)); err != nil {
		return fmt.Errorf("CodeEdge evaluator parent Run: %w", err)
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(command.ChildRunID)); err != nil {
		return fmt.Errorf("CodeEdge evaluator child Run: %w", err)
	}
	if command.Expected.RunID != strings.TrimSpace(command.ParentRunID) || command.Expected.RunVersion <= 0 || command.Expected.RunDefinitionHash == "" {
		return fmt.Errorf("CodeEdge evaluator evidence handoff requires the captured parent Run checkpoint")
	}
	if command.Expected.TaskID == "" || command.Expected.RevisionID == "" || command.Expected.RevisionDigest == "" {
		return fmt.Errorf("CodeEdge evaluator evidence handoff requires the captured TaskRevision checkpoint")
	}
	return nil
}

func (service *LifecycleMutationService) Package(ctx context.Context, command PackageLifecycleCommand) (LifecycleMutationReceipt, error) {
	if strings.TrimSpace(command.ReleaseVersion) == "" {
		return LifecycleMutationReceipt{}, fmt.Errorf("local package release version is required")
	}
	op, replay, err := service.begin(ctx, LifecycleMutationPackage, command.LifecycleMutationCommandBase, command, lifecycleOperationTargets{TaskID: command.Expected.TaskID, RevisionID: command.Expected.RevisionID, RunID: command.Expected.RunID, ReleaseID: command.IdempotencyKey})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if existing, lookupErr := service.core.store.GetLocalPackageRelease(ctx, op.ReleaseID); lookupErr != nil {
		return LifecycleMutationReceipt{}, lookupErr
	} else if existing == nil {
		if err := service.validateCheckpoint(ctx, command.Expected); err != nil {
			return LifecycleMutationReceipt{}, err
		}
	}
	result, err := (&ReleaseService{core: service.core}).PackageRevision(ctx, PackageRevisionRequest{
		RevisionID: op.RevisionID, ExpectedStateVersion: command.Expected.RevisionStateVersion, ReleaseVersion: command.ReleaseVersion,
		RunID: op.RunID, ExpectedComplianceRecordID: command.Expected.CodeEdgeComplianceRecordID,
		ExpectedAuthorizationFingerprint: command.Expected.CodeEdgeAuthorizationFingerprint,
		IdempotencyKey:                   op.IdempotencyKey, Actor: op.Actor, Reason: op.Reason,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return service.complete(ctx, op, LifecycleMutationReceipt{Action: LifecycleMutationPackage, TaskID: result.Release.TaskID, RevisionID: result.Release.RevisionID, ReleaseID: result.Release.ID, ReleaseVersion: result.Release.ReleaseVersion, Summary: "本地 package 已生成并记录"})
}

func (service *LifecycleMutationService) Withdraw(ctx context.Context, base LifecycleMutationCommandBase) (LifecycleMutationReceipt, error) {
	op, replay, err := service.begin(ctx, LifecycleMutationWithdraw, base, base, lifecycleOperationTargets{TaskID: base.Expected.TaskID, RevisionID: base.Expected.RevisionID, ReleaseID: base.Expected.ReleaseID})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if base.Expected.ReleaseID == "" || base.Expected.ReleaseRecordVersion <= 0 {
		return LifecycleMutationReceipt{}, fmt.Errorf("withdraw requires a selected release record checkpoint")
	}
	result, err := (&ReleaseService{core: service.core}).Withdraw(ctx, WithdrawReleaseRequest{
		ReleaseID: op.ReleaseID, ExpectedReleaseVersion: op.ExpectedReleaseRecordVersion, IdempotencyKey: op.IdempotencyKey, Actor: op.Actor, Reason: op.Reason,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return service.complete(ctx, op, LifecycleMutationReceipt{Action: LifecycleMutationWithdraw, TaskID: result.Release.TaskID, RevisionID: result.Release.RevisionID, ReleaseID: result.Release.ID, ReleaseVersion: result.Release.ReleaseVersion, ExecutionID: result.Operation.ID, Summary: "release 已撤回；本地 package 与证据仍被保留"})
}

// DecideReview records a reviewed action only when the full Task, revision,
// and ReviewRequest checkpoint still matches what the operator confirmed. The
// inner review transaction remains the state/digest CAS authority; the V12
// operation makes its result durable and replayable across a lost response.
func (service *LifecycleMutationService) DecideReview(ctx context.Context, command DecideReviewLifecycleCommand) (LifecycleMutationReceipt, error) {
	if err := validateReviewDecisionCommand(command); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	op, replay, err := service.begin(ctx, LifecycleMutationReview, command.LifecycleMutationCommandBase, command, lifecycleOperationTargets{
		TaskID: command.Expected.TaskID, RevisionID: command.Expected.RevisionID,
	})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if existing, found, err := service.findReviewDecision(ctx, op.ReviewRequestID, op.IdempotencyKey); err != nil {
		return LifecycleMutationReceipt{}, err
	} else if found {
		if err := validateRecoveredReviewDecision(existing, op, command.Decision); err != nil {
			return LifecycleMutationReceipt{}, err
		}
		return service.complete(ctx, op, receiptForReviewDecision(op, existing))
	}
	if err := service.validateCheckpoint(ctx, command.Expected); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	decision, err := (&ReviewService{core: service.core}).Decide(ctx, DecideReviewRequest{
		ID:                     op.IdempotencyKey,
		ReviewRequestID:        op.ReviewRequestID,
		RevisionID:             op.RevisionID,
		Action:                 command.Decision,
		ExpectedRevisionDigest: op.ExpectedRevisionDigest,
		Actor:                  op.Actor,
		Reason:                 op.Reason,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return service.complete(ctx, op, receiptForReviewDecision(op, decision))
}

func (service *LifecycleMutationService) PrepareManualPatch(ctx context.Context, command EditLifecycleCommand) (LifecycleMutationReceipt, error) {
	if command.Expected.TaskID == "" || command.Expected.RevisionID == "" || command.Expected.RunID == "" {
		return LifecycleMutationReceipt{}, fmt.Errorf("edit requires an explicit Task, revision, and Run checkpoint")
	}
	if strings.TrimSpace(command.UnifiedDiff) == "" {
		return LifecycleMutationReceipt{}, fmt.Errorf("unified diff is required")
	}
	op, replay, err := service.begin(ctx, LifecycleMutationEdit, command.LifecycleMutationCommandBase, command, lifecycleOperationTargets{TaskID: command.Expected.TaskID, RevisionID: command.Expected.RevisionID, RunID: command.Expected.RunID})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if err := service.validateCheckpoint(ctx, command.Expected); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	payload, err := json.Marshal(struct {
		Format string `json:"format"`
		Diff   string `json:"diff"`
	}{Format: "harbor.local-unified-diff.v1", Diff: command.UnifiedDiff})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	plan, err := service.core.changes.PlanTaskChange(ctx, ContinueTaskCommand{
		CommandKey: op.IdempotencyKey, TaskID: op.TaskID, RunID: op.RunID, Expected: continuationCheckpoint(command.Expected), Actor: op.Actor, Reason: op.Reason,
	}, TaskChangeRequest{
		ProviderID: LocalPatchProviderID, OperationKey: "task-hub-edit:" + op.IdempotencyKey, Payload: payload,
		Findings: FindingBundle{Format: "harbor.findings.v1", RevisionID: op.RevisionID, RevisionDigest: command.Expected.RevisionDigest},
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if plan.NoOp || plan.Plan.ID() == "" {
		return LifecycleMutationReceipt{}, fmt.Errorf("%w: manual patch produced no changed revision", ErrChangeNoOp)
	}
	return service.complete(ctx, op, LifecycleMutationReceipt{Action: LifecycleMutationEdit, TaskID: op.TaskID, RevisionID: op.RevisionID, RunID: op.RunID, PlanID: plan.Plan.ID(), CandidateID: plan.Candidate.ID, Summary: "已在隔离候选快照中准备 unified diff；等待冻结计划确认"})
}

func (service *LifecycleMutationService) ExecuteManualPatch(ctx context.Context, base LifecycleMutationCommandBase, planID string) (LifecycleMutationReceipt, error) {
	op, err := service.core.store.GetLifecycleOperationByIdempotencyKey(ctx, base.IdempotencyKey)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if op == nil || op.Action != string(LifecycleMutationEdit) || op.State != store.LifecycleOperationCompleted {
		return LifecycleMutationReceipt{}, fmt.Errorf("manual patch lifecycle operation is not prepared")
	}
	receipt, err := decodeLifecycleMutationReceipt(*op)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if strings.TrimSpace(planID) == "" || receipt.PlanID != planID {
		return LifecycleMutationReceipt{}, fmt.Errorf("manual patch plan does not belong to lifecycle operation")
	}
	commit, err := service.core.changes.ExecuteTaskChange(ctx, planID, op.Actor, op.Reason)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	receipt.ExecutionID = commit.Execution.ID
	receipt.RevisionID = commit.Revision.ID
	receipt.RevisionStateVersion = commit.Revision.StateVersion
	receipt.RunID = commit.Run.ID
	receipt.RunVersion = commit.Run.Version
	receipt.Summary = "候选 revision 已通过冻结计划提交"
	return receipt, nil
}

func (service *LifecycleMutationService) executeTaskTransition(ctx context.Context, action LifecycleMutationAction, base LifecycleMutationCommandBase, target store.TaskLifecycleState) (LifecycleMutationReceipt, error) {
	if base.Expected.TaskID == "" || base.Expected.TaskVersion <= 0 {
		return LifecycleMutationReceipt{}, fmt.Errorf("%s requires a Task version checkpoint", action)
	}
	deletionID := ""
	if action == LifecycleMutationSoftDelete {
		var err error
		deletionID, err = store.NewUUIDv7()
		if err != nil {
			return LifecycleMutationReceipt{}, err
		}
	}
	op, replay, err := service.begin(ctx, action, base, struct {
		Target store.TaskLifecycleState `json:"target_lifecycle_state"`
	}{Target: target}, lifecycleOperationTargets{TaskID: base.Expected.TaskID, DeletionRecordID: deletionID, TargetLifecycleState: target})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	result, err := service.core.store.ExecuteLifecycleTaskTransition(ctx, store.ExecuteLifecycleTaskTransitionRequest{OperationID: op.ID, ExpectedVersion: op.Version})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	receipt := LifecycleMutationReceipt{Action: action, OperationID: result.Operation.ID, TaskID: result.Task.ID, TaskVersion: result.Task.Version, Summary: lifecycleTransitionSummary(action)}
	if result.DeletionRecord != nil {
		receipt.DeletionRecordID = result.DeletionRecord.ID
	}
	return receipt, nil
}

type lifecycleOperationTargets struct {
	TaskID               string
	RevisionID           string
	RunID                string
	ReleaseID            string
	DeletionRecordID     string
	TargetLifecycleState store.TaskLifecycleState
}

func (service *LifecycleMutationService) begin(ctx context.Context, action LifecycleMutationAction, base LifecycleMutationCommandBase, payload any, targets lifecycleOperationTargets) (store.LifecycleOperation, *LifecycleMutationReceipt, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.LifecycleOperation{}, nil, fmt.Errorf("lifecycle mutation service is not configured")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(base.IdempotencyKey)); err != nil {
		return store.LifecycleOperation{}, nil, err
	}
	if strings.TrimSpace(base.Actor) == "" || strings.TrimSpace(base.Reason) == "" {
		return store.LifecycleOperation{}, nil, fmt.Errorf("lifecycle mutation actor and reason are required")
	}
	fingerprint, err := lifecycleMutationFingerprint(action, base, payload)
	if err != nil {
		return store.LifecycleOperation{}, nil, err
	}
	started, err := service.core.store.BeginLifecycleOperation(ctx, store.BeginLifecycleOperationRequest{
		IdempotencyKey: base.IdempotencyKey, Action: string(action), RequestFingerprint: fingerprint,
		TaskID: targets.TaskID, RevisionID: targets.RevisionID, RunID: targets.RunID, ReleaseID: targets.ReleaseID, DeletionRecordID: targets.DeletionRecordID, TargetLifecycleState: targets.TargetLifecycleState,
		ExpectedTaskID: base.Expected.TaskID, ExpectedRevisionID: base.Expected.RevisionID, ExpectedRunID: base.Expected.RunID,
		ExpectedReleaseID: base.Expected.ReleaseID, ExpectedReviewRequestID: base.Expected.ReviewRequestID,
		ExpectedTaskVersion: base.Expected.TaskVersion, ExpectedRevisionStateVersion: base.Expected.RevisionStateVersion, ExpectedRevisionDigest: base.Expected.RevisionDigest,
		ExpectedRunVersion: base.Expected.RunVersion, ExpectedRunExecutionEpoch: base.Expected.RunExecutionEpoch, ExpectedRunDefinitionHash: base.Expected.RunDefinitionHash,
		ExpectedReleaseRecordVersion: base.Expected.ReleaseRecordVersion, ExpectedReviewRevisionID: base.Expected.ReviewRevisionID, ExpectedReviewState: base.Expected.ReviewState, ExpectedReviewEvidenceDigest: base.Expected.ReviewEvidenceDigest,
		ExpectedCodeEdgeComplianceRecordID:       base.Expected.CodeEdgeComplianceRecordID,
		ExpectedCodeEdgeAuthorizationFingerprint: base.Expected.CodeEdgeAuthorizationFingerprint,
		ReviewRequestID:                          base.Expected.ReviewRequestID, Actor: base.Actor, Reason: base.Reason,
	})
	if err != nil {
		return store.LifecycleOperation{}, nil, err
	}
	if started.Operation.State == store.LifecycleOperationCompleted {
		receipt, err := decodeLifecycleMutationReceipt(started.Operation)
		if err != nil {
			return store.LifecycleOperation{}, nil, err
		}
		return started.Operation, &receipt, nil
	}
	return started.Operation, nil, nil
}

// completedOperationReplay returns a durable immutable receipt before a caller
// rereads mutable local input such as an import snapshot or profile file. A
// prepared operation intentionally falls through to BeginLifecycleOperation,
// which still verifies its complete request fingerprint against current input.
func (service *LifecycleMutationService) completedOperationReplay(ctx context.Context, action LifecycleMutationAction, base LifecycleMutationCommandBase) (LifecycleMutationReceipt, bool, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return LifecycleMutationReceipt{}, false, fmt.Errorf("lifecycle mutation service is not configured")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(base.IdempotencyKey)); err != nil {
		return LifecycleMutationReceipt{}, false, err
	}
	operation, err := service.core.store.GetLifecycleOperationByIdempotencyKey(ctx, base.IdempotencyKey)
	if err != nil {
		return LifecycleMutationReceipt{}, false, err
	}
	if operation == nil {
		return LifecycleMutationReceipt{}, false, nil
	}
	if operation.Action != string(action) {
		return LifecycleMutationReceipt{}, false, fmt.Errorf("%w: lifecycle operation key %s", store.ErrIdempotencyConflict, base.IdempotencyKey)
	}
	if operation.State != store.LifecycleOperationCompleted {
		return LifecycleMutationReceipt{}, false, nil
	}
	receipt, err := decodeLifecycleMutationReceipt(*operation)
	if err != nil {
		return LifecycleMutationReceipt{}, false, err
	}
	return receipt, true, nil
}

// ReplayCompleted returns an immutable V12 receipt without rereading a
// mutable command input or reconstructing its prior checkpoint. CLI adapters
// use this before resolving a checkpoint so completed operations created
// before V14's Expected*ID columns remain safely replayable.
func (service *LifecycleMutationService) ReplayCompleted(ctx context.Context, action LifecycleMutationAction, idempotencyKey string) (LifecycleMutationReceipt, bool, error) {
	return service.completedOperationReplay(ctx, action, LifecycleMutationCommandBase{IdempotencyKey: idempotencyKey})
}

func (service *LifecycleMutationService) complete(ctx context.Context, operation store.LifecycleOperation, receipt LifecycleMutationReceipt) (LifecycleMutationReceipt, error) {
	receipt.OperationID = operation.ID
	if receipt.Action == "" {
		receipt.Action = LifecycleMutationAction(operation.Action)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	completed, err := service.core.store.CompleteLifecycleOperation(ctx, store.CompleteLifecycleOperationRequest{OperationID: operation.ID, ExpectedVersion: operation.Version, ResultJSON: string(encoded)})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return decodeLifecycleMutationReceipt(completed)
}

func (service *LifecycleMutationService) recoverInitialTask(ctx context.Context, operation store.LifecycleOperation) (LifecycleMutationReceipt, bool, error) {
	if operation.TaskID == "" || operation.RevisionID == "" {
		return LifecycleMutationReceipt{}, false, nil
	}
	task, err := service.core.store.GetTaskV2(ctx, operation.TaskID)
	if err != nil {
		return LifecycleMutationReceipt{}, false, err
	}
	if task == nil {
		return LifecycleMutationReceipt{}, false, nil
	}
	revision, err := service.core.store.GetTaskRevision(ctx, operation.RevisionID)
	if err != nil {
		return LifecycleMutationReceipt{}, false, err
	}
	if revision == nil || revision.TaskID != task.ID {
		return LifecycleMutationReceipt{}, false, fmt.Errorf("incomplete imported/forked lifecycle operation %s", operation.ID)
	}
	action := LifecycleMutationAction(operation.Action)
	if action != LifecycleMutationImport && action != LifecycleMutationFork {
		return LifecycleMutationReceipt{}, false, fmt.Errorf("lifecycle operation %s cannot recover an initial task", operation.ID)
	}
	return LifecycleMutationReceipt{Action: action, TaskID: task.ID, TaskVersion: task.Version, RevisionID: revision.ID, RevisionStateVersion: revision.StateVersion, Summary: "已重放已提交的初始 TaskRevision"}, true, nil
}

func (service *LifecycleMutationService) validateCheckpoint(ctx context.Context, expected LifecycleMutationCheckpoint) error {
	if expected.TaskID == "" {
		return fmt.Errorf("lifecycle mutation checkpoint requires a Task")
	}
	hasReview := expected.ReviewRequestID != "" || expected.ReviewRevisionID != "" || expected.ReviewState != "" || expected.ReviewEvidenceDigest != ""
	if hasReview && (expected.ReviewRequestID == "" || expected.ReviewRevisionID == "" || expected.ReviewState == "" || expected.ReviewEvidenceDigest == "") {
		return fmt.Errorf("lifecycle mutation review checkpoint is incomplete")
	}
	var current LifecycleMutationCheckpoint
	var err error
	if hasReview {
		current, err = service.CaptureReviewCheckpoint(ctx, expected.TaskID, expected.RevisionID, expected.ReviewRequestID)
	} else {
		current, err = service.CaptureCheckpoint(ctx, expected.TaskID, expected.RevisionID, expected.RunID, expected.ReleaseID)
	}
	if err != nil {
		return err
	}
	if current.TaskID != expected.TaskID || current.TaskVersion != expected.TaskVersion ||
		current.RevisionID != expected.RevisionID || current.RevisionStateVersion != expected.RevisionStateVersion || current.RevisionDigest != expected.RevisionDigest ||
		current.RunID != expected.RunID || current.RunVersion != expected.RunVersion || current.RunExecutionEpoch != expected.RunExecutionEpoch || current.RunDefinitionHash != expected.RunDefinitionHash ||
		current.CodeEdgeComplianceRecordID != expected.CodeEdgeComplianceRecordID || current.CodeEdgeAuthorizationFingerprint != expected.CodeEdgeAuthorizationFingerprint ||
		current.ReleaseID != expected.ReleaseID || current.ReleaseRecordVersion != expected.ReleaseRecordVersion ||
		current.ReviewRequestID != expected.ReviewRequestID || current.ReviewRevisionID != expected.ReviewRevisionID || current.ReviewState != expected.ReviewState || current.ReviewEvidenceDigest != expected.ReviewEvidenceDigest {
		return fmt.Errorf("%w: lifecycle mutation checkpoint is stale", store.ErrOptimisticLock)
	}
	return nil
}

func validateReviewDecisionCommand(command DecideReviewLifecycleCommand) error {
	expected := command.Expected
	if expected.TaskID == "" || expected.TaskVersion <= 0 || expected.RevisionID == "" || expected.RevisionStateVersion <= 0 || expected.RevisionDigest == "" {
		return fmt.Errorf("review decision requires a complete TaskRevision checkpoint")
	}
	if expected.ReviewRequestID == "" || expected.ReviewRevisionID == "" || expected.ReviewEvidenceDigest == "" {
		return fmt.Errorf("review decision requires a complete ReviewRequest checkpoint")
	}
	if expected.ReviewRevisionID != expected.RevisionID || expected.ReviewState != "open" {
		return fmt.Errorf("review decision requires an open ReviewRequest for the selected revision")
	}
	switch command.Decision {
	case store.ReviewDecisionApprove, store.ReviewDecisionRequestChanges, store.ReviewDecisionRejectTerminal:
		return nil
	default:
		return fmt.Errorf("invalid review decision action %q", command.Decision)
	}
}

func (service *LifecycleMutationService) findReviewDecision(ctx context.Context, reviewRequestID, decisionID string) (store.ReviewDecision, bool, error) {
	decisions, err := service.core.store.ListReviewDecisionsForRequest(ctx, reviewRequestID)
	if err != nil {
		return store.ReviewDecision{}, false, err
	}
	for _, decision := range decisions {
		if decision.ID == decisionID {
			return decision, true, nil
		}
	}
	return store.ReviewDecision{}, false, nil
}

func validateRecoveredReviewDecision(decision store.ReviewDecision, operation store.LifecycleOperation, action store.ReviewDecisionAction) error {
	if decision.ReviewRequestID != operation.ReviewRequestID || decision.RevisionID != operation.RevisionID ||
		decision.Action != action || decision.ExpectedRevisionDigest != operation.ExpectedRevisionDigest ||
		decision.Actor != operation.Actor || decision.Reason != operation.Reason {
		return fmt.Errorf("%w: review decision %s", store.ErrIdempotencyConflict, decision.ID)
	}
	return nil
}

func receiptForReviewDecision(operation store.LifecycleOperation, decision store.ReviewDecision) LifecycleMutationReceipt {
	summary := "审核决定已记录"
	switch decision.Action {
	case store.ReviewDecisionApprove:
		summary = "审核已批准"
	case store.ReviewDecisionRequestChanges:
		summary = "审核已请求修改"
	case store.ReviewDecisionRejectTerminal:
		summary = "审核已终止拒绝"
	}
	return LifecycleMutationReceipt{
		Action: LifecycleMutationReview, TaskID: operation.TaskID, TaskVersion: operation.ExpectedTaskVersion,
		RevisionID: decision.RevisionID, RevisionStateVersion: operation.ExpectedRevisionStateVersion,
		ReviewRequestID: decision.ReviewRequestID, ReviewDecisionID: decision.ID, ReviewDecision: string(decision.Action), Summary: summary,
	}
}

func lifecycleMutationFingerprint(action LifecycleMutationAction, base LifecycleMutationCommandBase, payload any) (string, error) {
	encoded, err := json.Marshal(struct {
		Action   LifecycleMutationAction     `json:"action"`
		Actor    string                      `json:"actor"`
		Reason   string                      `json:"reason"`
		Expected LifecycleMutationCheckpoint `json:"expected"`
		Payload  any                         `json:"payload"`
	}{Action: action, Actor: strings.TrimSpace(base.Actor), Reason: strings.TrimSpace(base.Reason), Expected: base.Expected, Payload: payload})
	if err != nil {
		return "", fmt.Errorf("fingerprint lifecycle mutation: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func decodeLifecycleMutationReceipt(operation store.LifecycleOperation) (LifecycleMutationReceipt, error) {
	var receipt LifecycleMutationReceipt
	if operation.State != store.LifecycleOperationCompleted {
		return LifecycleMutationReceipt{}, fmt.Errorf("invalid lifecycle mutation receipt %s", operation.ID)
	}
	if err := json.Unmarshal([]byte(operation.ResultJSON), &receipt); err == nil && receipt.OperationID == operation.ID && receipt.Action == LifecycleMutationAction(operation.Action) {
		return receipt, nil
	}
	// Task transitions complete their V12 receipt in the same transaction as
	// the Task/DeletionRecord writes. Their store-level receipt retains a full
	// entity snapshot for recovery rather than importing this app-only result
	// type; project its immutable facts here for a UI/CLI replay.
	switch LifecycleMutationAction(operation.Action) {
	case LifecycleMutationArchive, LifecycleMutationSoftDelete, LifecycleMutationRestore:
		return LifecycleMutationReceipt{
			Action:           LifecycleMutationAction(operation.Action),
			OperationID:      operation.ID,
			TaskID:           operation.TaskID,
			TaskVersion:      operation.ExpectedTaskVersion + 1,
			DeletionRecordID: operation.DeletionRecordID,
			Summary:          lifecycleTransitionSummary(LifecycleMutationAction(operation.Action)),
		}, nil
	}
	return LifecycleMutationReceipt{}, fmt.Errorf("invalid lifecycle mutation receipt %s", operation.ID)
}

func lifecycleReplayResult(replay *LifecycleMutationReceipt, err error) (LifecycleMutationReceipt, error) {
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if replay != nil {
		return *replay, nil
	}
	return LifecycleMutationReceipt{}, nil
}

func (checkpoint LifecycleMutationCheckpoint) empty() bool {
	return checkpoint.TaskID == "" && checkpoint.TaskVersion == 0 && checkpoint.RevisionID == "" && checkpoint.RevisionStateVersion == 0 && checkpoint.RevisionDigest == "" &&
		checkpoint.RunID == "" && checkpoint.RunVersion == 0 && checkpoint.RunExecutionEpoch == 0 && checkpoint.RunDefinitionHash == "" && checkpoint.CodeEdgeComplianceRecordID == "" && checkpoint.CodeEdgeAuthorizationFingerprint == "" &&
		checkpoint.ReleaseID == "" && checkpoint.ReleaseRecordVersion == 0 && checkpoint.ReviewRequestID == "" && checkpoint.ReviewRevisionID == "" && checkpoint.ReviewState == "" && checkpoint.ReviewEvidenceDigest == ""
}

func sameDraftLifecycleTask(task store.TaskV2, command CreateDraftLifecycleCommand) bool {
	return task.Slug == strings.TrimSpace(command.Slug) && task.Title == strings.TrimSpace(command.Title) && task.SourceRepo == strings.TrimSpace(command.SourceRepo) && task.SourceCommit == strings.TrimSpace(command.SourceCommit) && sameLifecycleJSON(task.MetadataJSON, command.MetadataJSON)
}

func sameLifecycleJSON(left, right string) bool {
	canonical := func(value string) string {
		if strings.TrimSpace(value) == "" {
			return "{}"
		}
		var decoded any
		if json.Unmarshal([]byte(strings.TrimSpace(value)), &decoded) != nil {
			return ""
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
	return canonical(left) == canonical(right)
}

func lifecycleImportSource(path string) (string, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", fmt.Errorf("import source snapshot directory is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve import source snapshot directory: %w", err)
	}
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(absPath)
	if err != nil {
		return "", "", fmt.Errorf("digest import source snapshot: %w", err)
	}
	return absPath, digest, nil
}

func continuationCheckpoint(checkpoint LifecycleMutationCheckpoint) workflowkit.CheckpointRef {
	return workflowkit.CheckpointRef{
		Sequence: uint64(checkpoint.RunVersion), ExecutionEpoch: checkpoint.RunExecutionEpoch, SubjectVersion: checkpoint.TaskVersion,
		SubjectID: checkpoint.TaskID, SubjectRevisionID: checkpoint.RevisionID, SubjectDigest: workflowkit.SubjectDigest(checkpoint.RevisionDigest), WorkflowFingerprint: workflowkit.Fingerprint(checkpoint.RunDefinitionHash),
	}
}

func receiptForRun(action LifecycleMutationAction, run store.WorkflowRun) LifecycleMutationReceipt {
	return LifecycleMutationReceipt{Action: action, TaskID: run.TaskID, RunID: run.ID, RunVersion: run.Version, RevisionID: run.RevisionID, Summary: "冻结 Run 已入队"}
}

func lifecycleTransitionSummary(action LifecycleMutationAction) string {
	switch action {
	case LifecycleMutationArchive:
		return "Task 已归档"
	case LifecycleMutationSoftDelete:
		return "Task 已软删除并写入删除记录"
	case LifecycleMutationRestore:
		return "Task 已恢复到明确的生命周期状态"
	default:
		return "Task 生命周期状态已更新"
	}
}
