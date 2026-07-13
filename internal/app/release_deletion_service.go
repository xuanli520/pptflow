package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// ReleaseService creates only local immutable packages. Its naming preserves
// the domain's Release entity while deliberately excluding remote publication,
// upload state, provider clients, and destination configuration.
type ReleaseService struct{ core *lifecycleServiceCore }

type PackageRevisionRequest struct {
	RevisionID             string
	ExpectedStateVersion   int64
	ReleaseVersion         string
	Channel                string
	ExpectedChannelVersion int64
	// IdempotencyKey binds a package-only request to one immutable local
	// release. It must be UUIDv7 when supplied. Channel movement is a separate
	// mutable command and therefore cannot share this key.
	IdempotencyKey string
	Actor          string
	Reason         string
}

type LocalPackageResult struct {
	Release     store.LocalPackageRelease
	PackagePath string
	ReceiptPath string
}

// WithdrawReleaseRequest captures the reviewed release record version and the
// UUIDv7 idempotency key retained by CLI/TUI confirmation flows. Withdrawals
// are durable local operations; they do not remove a package or its pinned
// evidence.
type WithdrawReleaseRequest struct {
	ReleaseID              string
	ExpectedReleaseVersion int64
	IdempotencyKey         string
	Actor                  string
	Reason                 string
}

type WithdrawReleaseResult struct {
	Release   store.LocalPackageRelease
	Operation store.ReleaseWithdrawOperation
	Receipt   store.ReleaseWithdrawReceipt
}

// PackageRevision creates a new immutable package object and records a local
// Release. The Release version is globally unique in SQLite. No request from
// this service can upload or copy bytes outside the managed package directory.
func (service *ReleaseService) PackageRevision(ctx context.Context, request PackageRevisionRequest) (LocalPackageResult, error) {
	if service == nil || service.core == nil {
		return LocalPackageResult{}, fmt.Errorf("release service is not configured")
	}
	if err := validateLocalReleaseVersion(request.ReleaseVersion); err != nil {
		return LocalPackageResult{}, err
	}
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if idempotencyKey != "" {
		if err := store.ValidateUUIDv7(idempotencyKey); err != nil {
			return LocalPackageResult{}, err
		}
		if strings.TrimSpace(request.Channel) != "" {
			return LocalPackageResult{}, fmt.Errorf("local package idempotency cannot include a mutable release channel")
		}
		existing, err := service.core.store.GetLocalPackageRelease(ctx, idempotencyKey)
		if err != nil {
			return LocalPackageResult{}, err
		}
		if existing != nil {
			return service.replayLocalPackage(ctx, request, *existing)
		}
	}
	revision, err := service.core.store.GetTaskRevision(ctx, request.RevisionID)
	if err != nil {
		return LocalPackageResult{}, err
	}
	if revision == nil {
		return LocalPackageResult{}, fmt.Errorf("%w: revision %s", ErrLifecycleNotFound, request.RevisionID)
	}
	if revision.State == store.RevisionStateValidated && request.ExpectedStateVersion <= 0 {
		return LocalPackageResult{}, fmt.Errorf("expected revision state version is required to create a local package")
	}
	if revision.State != store.RevisionStateValidated && revision.State != store.RevisionStateReleased {
		return LocalPackageResult{}, fmt.Errorf("revision %s must be validated before local packaging", revision.ID)
	}
	task, err := service.core.store.GetTaskV2(ctx, revision.TaskID)
	if err != nil {
		return LocalPackageResult{}, err
	}
	if task == nil {
		return LocalPackageResult{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, revision.TaskID)
	}
	if task.CurrentRevisionID != revision.ID {
		return LocalPackageResult{}, fmt.Errorf("only the current reviewed revision may be packaged locally")
	}
	if strings.TrimSpace(revision.ValidationEvidenceManifest) == "" {
		return LocalPackageResult{}, fmt.Errorf("released revision has no validation evidence manifest")
	}
	if err := service.core.layout.ensureRoot(); err != nil {
		return LocalPackageResult{}, err
	}
	snapshot, err := (&RevisionService{core: service.core}).SnapshotDirectory(task.ID, revision.ID)
	if err != nil {
		return LocalPackageResult{}, err
	}
	packageDirectory := service.core.layout.releaseDirectory(request.ReleaseVersion)
	expectedReceipt := localPackageReceipt{
		Format:         localPackageReceiptFormat,
		TaskID:         task.ID,
		RevisionID:     revision.ID,
		TaskDigest:     revision.TaskDigest,
		ReleaseVersion: request.ReleaseVersion,
	}
	object, packagePath, reused, err := existingLocalPackage(ctx, service.core.objects, packageDirectory, expectedReceipt)
	if err != nil {
		return LocalPackageResult{}, err
	}
	if !reused {
		object, packagePath, err = packageManagedSnapshot(ctx, service.core.objects, snapshot, packageDirectory, task.Slug, revision.CreatedAt)
		if err != nil {
			return LocalPackageResult{}, err
		}
		receipt := expectedReceipt
		receipt.Package = object
		receipt.CreatedAt = service.core.now().UTC()
		receiptPath := filepath.Join(packageDirectory, "receipt.json")
		if err := writeNewJSON(receiptPath, receipt); err != nil {
			_ = os.RemoveAll(packageDirectory)
			return LocalPackageResult{}, fmt.Errorf("write local package receipt: %w", err)
		}
	}
	receiptPath := filepath.Join(packageDirectory, "receipt.json")
	if revision.State == store.RevisionStateValidated {
		updatedRevision, transitionErr := service.core.store.TransitionTaskRevisionState(ctx, store.TransitionTaskRevisionStateRequest{
			RevisionID:           revision.ID,
			ExpectedStateVersion: request.ExpectedStateVersion,
			State:                store.RevisionStateReleased,
			Actor:                request.Actor,
			Reason:               request.Reason,
		})
		if transitionErr != nil {
			return LocalPackageResult{}, transitionErr
		}
		revision = &updatedRevision
	}
	release, err := service.core.store.GetLocalPackageReleaseByVersion(ctx, request.ReleaseVersion)
	if err != nil {
		return LocalPackageResult{}, err
	}
	if release != nil {
		if idempotencyKey != "" && release.ID != idempotencyKey {
			return LocalPackageResult{}, fmt.Errorf("%w: local package version %s belongs to another operation", store.ErrIdempotencyConflict, request.ReleaseVersion)
		}
		if release.TaskID != task.ID || release.RevisionID != revision.ID || release.TaskDigest != revision.TaskDigest || release.PackageRef != string(object.Digest) || release.EvidenceRef != revision.ValidationEvidenceManifest {
			return LocalPackageResult{}, fmt.Errorf("local release version %s is already bound to another immutable package", request.ReleaseVersion)
		}
	} else {
		created, createErr := service.core.store.CreateLocalPackageRelease(ctx, store.CreateLocalPackageReleaseRequest{
			IdempotencyKey: idempotencyKey,
			ReleaseVersion: request.ReleaseVersion,
			RevisionID:     revision.ID,
			TaskID:         task.ID,
			TaskDigest:     revision.TaskDigest,
			PackageRef:     string(object.Digest),
			EvidenceRef:    revision.ValidationEvidenceManifest,
			Actor:          request.Actor,
			Reason:         request.Reason,
		})
		if createErr != nil {
			return LocalPackageResult{}, createErr
		}
		release = &created
	}
	if strings.TrimSpace(request.Channel) != "" {
		channel, channelErr := service.core.store.GetReleaseChannel(ctx, request.Channel)
		if channelErr != nil {
			return LocalPackageResult{}, channelErr
		}
		if channel == nil || channel.ReleaseID != release.ID {
			if _, err := service.core.store.SetReleaseChannel(ctx, store.SetReleaseChannelRequest{
				Channel:         request.Channel,
				ReleaseID:       release.ID,
				ExpectedVersion: request.ExpectedChannelVersion,
				Actor:           request.Actor,
				Reason:          request.Reason,
			}); err != nil {
				return LocalPackageResult{}, err
			}
		}
	}
	if err := service.publishPackagedTask(ctx, *task, revision.ID, request.Actor, request.Reason); err != nil {
		return LocalPackageResult{}, err
	}
	return LocalPackageResult{Release: *release, PackagePath: packagePath, ReceiptPath: receiptPath}, nil
}

// replayLocalPackage verifies the immutable package before returning a prior
// result. A response may be lost after the release record commits, so this
// path also repairs the final ready -> published projection when it is still
// safe to do so for the same current revision.
func (service *ReleaseService) replayLocalPackage(ctx context.Context, request PackageRevisionRequest, release store.LocalPackageRelease) (LocalPackageResult, error) {
	if release.ReleaseVersion != strings.TrimSpace(request.ReleaseVersion) || release.RevisionID != strings.TrimSpace(request.RevisionID) {
		return LocalPackageResult{}, fmt.Errorf("%w: local package key %s", store.ErrIdempotencyConflict, release.ID)
	}
	if actor := strings.TrimSpace(request.Actor); actor != "" && release.CreatedBy != actor {
		return LocalPackageResult{}, fmt.Errorf("%w: local package key %s", store.ErrIdempotencyConflict, release.ID)
	}
	if err := service.core.layout.ensureRoot(); err != nil {
		return LocalPackageResult{}, err
	}
	expected := localPackageReceipt{
		Format:         localPackageReceiptFormat,
		TaskID:         release.TaskID,
		RevisionID:     release.RevisionID,
		TaskDigest:     release.TaskDigest,
		ReleaseVersion: release.ReleaseVersion,
	}
	_, packagePath, reused, err := existingLocalPackage(ctx, service.core.objects, service.core.layout.releaseDirectory(release.ReleaseVersion), expected)
	if err != nil {
		return LocalPackageResult{}, err
	}
	if !reused {
		return LocalPackageResult{}, fmt.Errorf("local package release %s has no verifiable immutable receipt", release.ID)
	}
	task, err := service.core.store.GetTaskV2(ctx, release.TaskID)
	if err != nil {
		return LocalPackageResult{}, err
	}
	if task == nil {
		return LocalPackageResult{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, release.TaskID)
	}
	if err := service.publishPackagedTask(ctx, *task, release.RevisionID, request.Actor, request.Reason); err != nil {
		return LocalPackageResult{}, err
	}
	return LocalPackageResult{
		Release:     release,
		PackagePath: packagePath,
		ReceiptPath: filepath.Join(service.core.layout.releaseDirectory(release.ReleaseVersion), "receipt.json"),
	}, nil
}

func (service *ReleaseService) publishPackagedTask(ctx context.Context, task store.TaskV2, revisionID, actor, reason string) error {
	if task.LifecycleState != store.TaskLifecycleReady || task.CurrentRevisionID != revisionID {
		return nil
	}
	_, err := service.core.store.UpdateTaskV2(ctx, store.UpdateTaskV2Request{
		TaskID:          task.ID,
		ExpectedVersion: task.Version,
		LifecycleState:  store.TaskLifecyclePublished,
		Actor:           actor,
		Reason:          reason,
	})
	return err
}

func (service *ReleaseService) List(ctx context.Context, taskID string) ([]store.LocalPackageRelease, error) {
	if service == nil || service.core == nil {
		return nil, fmt.Errorf("release service is not configured")
	}
	return service.core.store.ListLocalPackageReleasesForTask(ctx, taskID)
}

// Withdraw creates or replays one durable withdrawal operation. The receipt
// confirms the exact immutable Release record version that was withdrawn.
func (service *ReleaseService) Withdraw(ctx context.Context, request WithdrawReleaseRequest) (WithdrawReleaseResult, error) {
	if service == nil || service.core == nil {
		return WithdrawReleaseResult{}, fmt.Errorf("release service is not configured")
	}
	result, err := service.core.store.ExecuteReleaseWithdraw(ctx, store.ExecuteReleaseWithdrawRequest{
		ReleaseID:              request.ReleaseID,
		ExpectedReleaseVersion: request.ExpectedReleaseVersion,
		IdempotencyKey:         request.IdempotencyKey,
		Actor:                  request.Actor,
		Reason:                 request.Reason,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return WithdrawReleaseResult{}, fmt.Errorf("%w: release %s", ErrLifecycleNotFound, request.ReleaseID)
		}
		return WithdrawReleaseResult{}, err
	}
	return WithdrawReleaseResult{Release: result.Release, Operation: result.Operation, Receipt: result.Receipt}, nil
}

func (service *ReleaseService) PromoteChannel(ctx context.Context, channel, releaseID string, expectedVersion int64, actor, reason string) (store.ReleaseChannel, error) {
	if service == nil || service.core == nil {
		return store.ReleaseChannel{}, fmt.Errorf("release service is not configured")
	}
	return service.core.store.SetReleaseChannel(ctx, store.SetReleaseChannelRequest{
		Channel:         channel,
		ReleaseID:       releaseID,
		ExpectedVersion: expectedVersion,
		Actor:           actor,
		Reason:          reason,
	})
}

// DeletionService separates reversible state transitions from irreversible
// purge work. The latter cannot proceed while any control-plane dependency
// still pins the task or its evidence.
type DeletionService struct{ core *lifecycleServiceCore }

// PurgeTaskRequest is the explicit irreversible-purge command. The expected
// task version and client-generated idempotency key bind the user's reviewed
// preflight to exactly one durable purge operation.
type PurgeTaskRequest struct {
	TaskID              string
	ExpectedTaskVersion int64
	IdempotencyKey      string
	Actor               string
	Reason              string
}

type PurgeTaskResult struct {
	Operation    store.TaskPurgeOperation    `json:"operation"`
	Dependencies store.PurgeDependencyReport `json:"dependencies"`
	Purged       bool                        `json:"purged"`
	InProgress   bool                        `json:"in_progress"`
}

// PurgeTaskBlocker is a stable, machine-readable reason why a task cannot be
// irreversibly purged. The referenced IDs remain in the dependency report so
// callers can show the blocking records without reconstructing store queries.
type PurgeTaskBlocker struct {
	Code string   `json:"code"`
	IDs  []string `json:"ids,omitempty"`
}

// PurgeTaskPreview is a read-only preflight for irreversible task cleanup.
// It intentionally does not create a deletion record or perform filesystem
// work; callers must use it before presenting any purge confirmation.
type PurgeTaskPreview struct {
	Task         store.TaskV2                `json:"task"`
	Dependencies store.PurgeDependencyReport `json:"dependencies"`
	Blockers     []PurgeTaskBlocker          `json:"blockers"`
	Eligible     bool                        `json:"eligible"`
	WillMutate   bool                        `json:"will_mutate"`
}

// PreviewPurgeTask reports the lifecycle and durable dependency blockers for
// a task without changing SQLite state or its managed task directory.
func (service *DeletionService) PreviewPurgeTask(ctx context.Context, taskID string) (PurgeTaskPreview, error) {
	if service == nil || service.core == nil {
		return PurgeTaskPreview{}, fmt.Errorf("deletion service is not configured")
	}
	task, err := service.core.store.GetTaskV2(ctx, taskID)
	if err != nil {
		return PurgeTaskPreview{}, err
	}
	if task == nil {
		return PurgeTaskPreview{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, taskID)
	}
	dependencies, err := service.core.store.QueryPurgeDependencies(ctx, store.PurgeDependencyQuery{
		EntityType: "task",
		EntityID:   task.ID,
	})
	if err != nil {
		return PurgeTaskPreview{}, err
	}
	completed, err := service.core.store.GetCompletedTaskPurge(ctx, task.ID)
	if err != nil {
		return PurgeTaskPreview{}, err
	}
	inProgress, err := service.core.store.GetInProgressTaskPurge(ctx, task.ID)
	if err != nil {
		return PurgeTaskPreview{}, err
	}
	blockers := purgeTaskBlockers(*task, dependencies)
	if inProgress != nil {
		blockers = append(blockers, PurgeTaskBlocker{Code: "task_purge_in_progress", IDs: []string{inProgress.ID}})
	}
	if completed != nil {
		blockers = append(blockers, PurgeTaskBlocker{Code: "task_already_purged", IDs: []string{completed.ID}})
	}
	return PurgeTaskPreview{
		Task:         *task,
		Dependencies: dependencies,
		Blockers:     blockers,
		Eligible:     len(blockers) == 0,
		WillMutate:   false,
	}, nil
}

func purgeTaskBlockers(task store.TaskV2, dependencies store.PurgeDependencyReport) []PurgeTaskBlocker {
	blockers := make([]PurgeTaskBlocker, 0, 9)
	if task.LifecycleState != store.TaskLifecycleDeleted {
		blockers = append(blockers, PurgeTaskBlocker{Code: "task_not_soft_deleted"})
	}
	for _, blocker := range []struct {
		code string
		ids  []string
	}{
		{code: "active_workspace", ids: dependencies.ActiveWorkspaceIDs},
		{code: "active_run", ids: dependencies.ActiveRunIDs},
		{code: "active_job", ids: dependencies.ActiveJobIDs},
		{code: "active_lease", ids: dependencies.ActiveLeaseIDs},
		{code: "pending_outbox", ids: dependencies.PendingOutboxIDs},
		{code: "release_pin", ids: dependencies.ReleaseIDs},
		{code: "artifact_manifest", ids: dependencies.ArtifactManifestIDs},
		{code: "artifact_ref", ids: dependencies.ArtifactRefIDs},
	} {
		if len(blocker.ids) == 0 {
			continue
		}
		blockers = append(blockers, PurgeTaskBlocker{Code: blocker.code, IDs: append([]string(nil), blocker.ids...)})
	}
	return blockers
}

func (service *DeletionService) SoftDeleteTask(ctx context.Context, taskID string, expectedVersion int64, actor, reason string) (store.TaskV2, store.DeletionRecord, error) {
	if service == nil || service.core == nil {
		return store.TaskV2{}, store.DeletionRecord{}, fmt.Errorf("deletion service is not configured")
	}
	task, err := service.core.store.UpdateTaskV2(ctx, store.UpdateTaskV2Request{
		TaskID:          taskID,
		ExpectedVersion: expectedVersion,
		LifecycleState:  store.TaskLifecycleDeleted,
		Actor:           actor,
		Reason:          reason,
	})
	if err != nil {
		return store.TaskV2{}, store.DeletionRecord{}, err
	}
	record, err := service.core.store.CreateDeletionRecord(ctx, store.CreateDeletionRecordRequest{
		EntityType: "task",
		EntityID:   task.ID,
		Action:     "soft_delete",
		Actor:      actor,
		Reason:     reason,
	})
	if err != nil {
		return task, store.DeletionRecord{}, err
	}
	record, err = service.core.store.TransitionDeletionRecord(ctx, store.TransitionDeletionRecordRequest{
		DeletionRecordID: record.ID,
		ExpectedVersion:  record.Version,
		State:            store.DeletionCompleted,
		Actor:            actor,
		Reason:           reason,
	})
	return task, record, err
}

func (service *DeletionService) RestoreTask(ctx context.Context, taskID string, expectedVersion int64, restoreState store.TaskLifecycleState, actor, reason string) (store.TaskV2, error) {
	if service == nil || service.core == nil {
		return store.TaskV2{}, fmt.Errorf("deletion service is not configured")
	}
	if restoreState == "" || restoreState == store.TaskLifecycleDeleted {
		return store.TaskV2{}, fmt.Errorf("restore target lifecycle state is required")
	}
	return service.core.store.UpdateTaskV2(ctx, store.UpdateTaskV2Request{
		TaskID:          taskID,
		ExpectedVersion: expectedVersion,
		LifecycleState:  restoreState,
		Actor:           actor,
		Reason:          reason,
	})
}

// PurgeTask executes one explicit irreversible-purge command. PrepareTaskPurge
// atomically captures the reviewed task version and dependency facts before it
// grants the fencing lease. Finalization rechecks those facts while holding a
// SQLite writer transaction across the filesystem boundary. Replaying the same
// idempotency key never creates a second operation or removes a replacement
// directory after completion.
func (service *DeletionService) PurgeTask(ctx context.Context, request PurgeTaskRequest) (PurgeTaskResult, error) {
	if service == nil || service.core == nil {
		return PurgeTaskResult{}, fmt.Errorf("deletion service is not configured")
	}
	prepared, err := service.core.store.PrepareTaskPurge(ctx, store.PrepareTaskPurgeRequest{
		TaskID:              request.TaskID,
		ExpectedTaskVersion: request.ExpectedTaskVersion,
		IdempotencyKey:      request.IdempotencyKey,
		Actor:               request.Actor,
		Reason:              request.Reason,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return PurgeTaskResult{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, request.TaskID)
		}
		return PurgeTaskResult{}, err
	}
	result := purgeTaskResult(prepared.Operation)
	if prepared.Operation.State == store.TaskPurgeBlocked || prepared.Operation.State == store.TaskPurgeCompleted {
		return result, nil
	}
	if !prepared.Acquired {
		result.InProgress = true
		return result, nil
	}

	finalized, err := service.core.store.FinalizeTaskPurge(ctx, store.FinalizeTaskPurgeRequest{
		OperationID:     prepared.Operation.ID,
		ExpectedVersion: prepared.Operation.Version,
		Actor:           request.Actor,
		Reason:          request.Reason,
		RemoveDirectory: func() error {
			return removeManagedTaskDirectory(ctx, service.core.layout, request.TaskID)
		},
	})
	if err == nil {
		return purgeTaskResult(finalized.Operation), nil
	}
	if !errors.Is(err, store.ErrTaskPurgeFilesystem) {
		return result, err
	}

	// The SQLite transaction deliberately rolls back on a filesystem error, so
	// record the failure in a fresh transaction and release the lease. A retry
	// with this same idempotency key will reclaim a new fenced lease and safely
	// retry an absent or partially removed directory.
	failed, failureErr := service.core.store.RecordTaskPurgeFailure(ctx, store.RecordTaskPurgeFailureRequest{
		OperationID:     prepared.Operation.ID,
		ExpectedVersion: prepared.Operation.Version,
		Actor:           request.Actor,
		Reason:          request.Reason,
		ErrorText:       err.Error(),
	})
	if failureErr != nil {
		return result, fmt.Errorf("task purge operation %s filesystem failure was not durably recorded: %w", prepared.Operation.ID, failureErr)
	}
	result = purgeTaskResult(failed)
	result.InProgress = true
	return result, fmt.Errorf("task purge operation %s: %w", failed.ID, err)
}

func purgeTaskResult(operation store.TaskPurgeOperation) PurgeTaskResult {
	return PurgeTaskResult{
		Operation:    operation,
		Dependencies: operation.Dependencies,
		Purged:       operation.State == store.TaskPurgeCompleted,
	}
}

// removeManagedTaskDirectory only traverses a file descriptor rooted at the
// managed root. os.Root prevents a path replacement or nested symbolic link
// from escaping that root while RemoveAll uses no-follow directory traversal.
// A missing task directory is success: it is the expected replay state after a
// process dies after filesystem removal but before FinalizeTaskPurge commits.
func removeManagedTaskDirectory(ctx context.Context, layout managedLayout, taskID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.ValidateUUIDv7(taskID); err != nil {
		return err
	}
	if err := layout.ensureRoot(); err != nil {
		return err
	}

	root, err := os.OpenRoot(layout.root)
	if err != nil {
		return fmt.Errorf("open managed root for purge: %w", err)
	}
	defer root.Close()

	tasksInfo, err := root.Lstat(managedTasksDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed task parent for purge: %w", err)
	}
	if tasksInfo.Mode()&os.ModeSymlink != 0 || !tasksInfo.IsDir() {
		return fmt.Errorf("refusing to purge through an unsafe managed task parent")
	}
	tasksRoot, err := root.OpenRoot(managedTasksDirectory)
	if err != nil {
		return fmt.Errorf("open managed task parent for purge: %w", err)
	}
	defer tasksRoot.Close()

	taskInfo, err := tasksRoot.Lstat(taskID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed task directory for purge: %w", err)
	}
	if taskInfo.Mode()&os.ModeSymlink != 0 || !taskInfo.IsDir() {
		return fmt.Errorf("refusing to purge unsafe managed task directory")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tasksRoot.RemoveAll(taskID); err != nil {
		return fmt.Errorf("purge managed task directory: %w", err)
	}
	return nil
}

func encodeLocalPackageReceipt(receipt localPackageReceipt) (string, error) {
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func localPackageReceiptPath(root, version string) string {
	return filepath.Join(root, "packages", strings.TrimSpace(version), "receipt.json")
}
