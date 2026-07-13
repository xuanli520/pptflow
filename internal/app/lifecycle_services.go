package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
)

var (
	// ErrLifecycleNotFound is returned when a requested V2 lifecycle entity is
	// absent. It intentionally wraps the control-plane error so callers can
	// distinguish a missing object from a stale optimistic version.
	ErrLifecycleNotFound = errors.New("lifecycle: record not found")
)

// LifecycleServices is the application-layer entry point for V2 entities. UI
// and CLI adapters should depend on the individual services below rather than
// modify workspaces, SQLite records, or task snapshots directly.
type LifecycleServices struct {
	Tasks         *TaskService
	Revisions     *RevisionService
	Runs          *RunService
	Reviews       *ReviewService
	Releases      *ReleaseService
	Deletion      *DeletionService
	Control       *ExecutionControlService
	Budgets       *BudgetGrantService
	Continuations *TaskContinuationService
	Changes       *ChangeProviderService
	Candidates    *CandidateRetentionService
	Inspection    *LifecycleInspectionService
	LocalRuntime  *LocalRuntimeService

	core *lifecycleServiceCore
}

type lifecycleServiceCore struct {
	store    *store.Store
	layout   managedLayout
	objects  *workflowruntime.ArtifactObjectStore
	template workflowadapter.WorkflowTemplate
	now      func() time.Time
	changes  *ChangeProviderService
}

// NewLifecycleServices wires a V2 control plane to its managed local
// filesystem. It does not create an execution profile: profiles are always
// explicit per StartRun request under the confirmed budget policy.
func NewLifecycleServices(root string, dataStore *store.Store) (*LifecycleServices, error) {
	if dataStore == nil {
		return nil, fmt.Errorf("lifecycle store is required")
	}
	layout, err := newManagedLayout(root)
	if err != nil {
		return nil, err
	}
	objects, err := workflowruntime.NewArtifactObjectStore(filepath.Join(layout.root, "objects"))
	if err != nil {
		return nil, fmt.Errorf("create lifecycle artifact store: %w", err)
	}
	template := workflowadapter.StandardWorkflowTemplate()
	if err := template.Validate(); err != nil {
		return nil, fmt.Errorf("validate built-in workflow template: %w", err)
	}
	core := &lifecycleServiceCore{
		store:    dataStore,
		layout:   layout,
		objects:  objects,
		template: template,
		now:      time.Now,
	}
	continuations := newTaskContinuationService(core)
	changes := newChangeProviderService(core)
	core.changes = changes
	return &LifecycleServices{
		Tasks:         &TaskService{core: core},
		Revisions:     &RevisionService{core: core},
		Runs:          &RunService{core: core},
		Reviews:       &ReviewService{core: core},
		Releases:      &ReleaseService{core: core},
		Deletion:      &DeletionService{core: core},
		Control:       &ExecutionControlService{core: core},
		Budgets:       &BudgetGrantService{core: core},
		Continuations: continuations,
		Changes:       changes,
		Candidates:    &CandidateRetentionService{core: core},
		Inspection:    &LifecycleInspectionService{core: core},
		LocalRuntime:  &LocalRuntimeService{core: core},
		core:          core,
	}, nil
}

// Store exposes the durable V2 store for read-only integration adapters. New
// mutations should use an application service so auditing and filesystem
// invariants remain centralized.
func (services *LifecycleServices) Store() *store.Store {
	if services == nil || services.core == nil {
		return nil
	}
	return services.core.store
}

// TaskService owns stable task identity and lifecycle-state mutations.
type TaskService struct{ core *lifecycleServiceCore }

type CreateDraftTaskRequest struct {
	ID             string
	Slug           string
	Title          string
	MetadataJSON   string
	SourceRepo     string
	SourceCommit   string
	Actor          string
	Reason         string
	LegacyIdentity string
}

// CreateDraft creates a path-independent Task with a UUIDv7 identity. The
// caller may supply a UUIDv7 only when it needs to bind a filesystem action to
// the same identity; a collision is rejected by the store.
func (service *TaskService) CreateDraft(ctx context.Context, request CreateDraftTaskRequest) (store.TaskV2, error) {
	if service == nil || service.core == nil {
		return store.TaskV2{}, fmt.Errorf("task service is not configured")
	}
	id := strings.TrimSpace(request.ID)
	if id == "" {
		var err error
		id, err = store.NewUUIDv7()
		if err != nil {
			return store.TaskV2{}, fmt.Errorf("allocate task ID: %w", err)
		}
	}
	return service.core.store.CreateTaskV2(ctx, store.CreateTaskV2Request{
		ID:             id,
		Slug:           request.Slug,
		Title:          request.Title,
		MetadataJSON:   request.MetadataJSON,
		SourceRepo:     request.SourceRepo,
		SourceCommit:   request.SourceCommit,
		LifecycleState: store.TaskLifecycleDraft,
		IdentityState:  store.TaskIdentityCanonical,
		LegacyIdentity: request.LegacyIdentity,
		Actor:          request.Actor,
		Reason:         request.Reason,
	})
}

type ImportTaskRequest struct {
	CreateDraftTaskRequest
	SourceDirectory string
	ProposalDigest  string
	ChangeSummary   string
}

// ImportTask creates a stable task and its first immutable imported revision.
// Snapshot materialization precedes one atomic task/revision transaction, so
// an import failure cannot leave a path-derived or empty partial Task behind.
func (service *TaskService) ImportTask(ctx context.Context, request ImportTaskRequest) (store.TaskV2, store.TaskRevision, error) {
	if service == nil || service.core == nil {
		return store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("task service is not configured")
	}
	if err := harborrun.ValidateManagedTaskSnapshotV2(request.SourceDirectory); err != nil {
		return store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("validate imported task: %w", err)
	}
	return service.createTaskWithInitialSnapshot(ctx, request.CreateDraftTaskRequest, CreateRevisionFromSnapshotRequest{
		Origin:          store.RevisionOriginImported,
		SourceDirectory: request.SourceDirectory,
		ProposalDigest:  request.ProposalDigest,
		ChangeSummary:   request.ChangeSummary,
		Actor:           request.Actor,
		Reason:          request.Reason,
	})
}

func (service *TaskService) createTaskWithInitialSnapshot(ctx context.Context, taskRequest CreateDraftTaskRequest, revisionRequest CreateRevisionFromSnapshotRequest) (store.TaskV2, store.TaskRevision, error) {
	return service.createTaskWithInitialSnapshotIdentity(ctx, taskRequest, revisionRequest, store.TaskIdentityCanonical)
}

func (service *TaskService) createTaskWithInitialSnapshotIdentity(ctx context.Context, taskRequest CreateDraftTaskRequest, revisionRequest CreateRevisionFromSnapshotRequest, identityState store.TaskIdentityState) (store.TaskV2, store.TaskRevision, error) {
	if err := harborrun.ValidateManagedTaskSnapshotV2(revisionRequest.SourceDirectory); err != nil {
		return store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("validate initial task snapshot: %w", err)
	}
	taskID := strings.TrimSpace(taskRequest.ID)
	var err error
	if taskID == "" {
		taskID, err = store.NewUUIDv7()
		if err != nil {
			return store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("allocate task ID: %w", err)
		}
	}
	if err := store.ValidateUUIDv7(taskID); err != nil {
		return store.TaskV2{}, store.TaskRevision{}, err
	}
	revisionID := strings.TrimSpace(revisionRequest.ID)
	if revisionID == "" {
		revisionID, err = store.NewUUIDv7()
		if err != nil {
			return store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("allocate revision ID: %w", err)
		}
	}
	if err := store.ValidateUUIDv7(revisionID); err != nil {
		return store.TaskV2{}, store.TaskRevision{}, err
	}
	revisions := &RevisionService{core: service.core}
	prepared, cleanup, err := revisions.prepareSnapshot(ctx, taskID, revisionID, revisionRequest.SourceDirectory)
	if err != nil {
		return store.TaskV2{}, store.TaskRevision{}, err
	}
	committed := false
	defer func() {
		if !committed {
			cleanup()
		}
	}()
	created, err := service.core.store.CreateTaskWithRevision(ctx, store.CreateTaskWithRevisionRequest{
		Task: store.CreateTaskV2Request{
			ID:             taskID,
			Slug:           taskRequest.Slug,
			Title:          taskRequest.Title,
			MetadataJSON:   taskRequest.MetadataJSON,
			SourceRepo:     taskRequest.SourceRepo,
			SourceCommit:   taskRequest.SourceCommit,
			LifecycleState: store.TaskLifecycleDraft,
			IdentityState:  identityState,
			LegacyIdentity: taskRequest.LegacyIdentity,
			Actor:          taskRequest.Actor,
			Reason:         taskRequest.Reason,
		},
		Revision: store.CreateTaskRevisionRequest{
			ID:             revisionID,
			TaskID:         taskID,
			Origin:         revisionRequest.Origin,
			TaskDigest:     prepared.TaskDigest,
			ProposalDigest: revisionRequest.ProposalDigest,
			ManifestID:     prepared.ManifestObjectID,
			State:          store.RevisionStateSealed,
			ChangeSummary:  revisionRequest.ChangeSummary,
			MetadataJSON:   revisionRequest.MetadataJSON,
			Actor:          revisionRequest.Actor,
			Reason:         revisionRequest.Reason,
		},
	})
	if err != nil {
		return store.TaskV2{}, store.TaskRevision{}, err
	}
	committed = true
	return created.Task, created.Revision, nil
}

type ForkTaskRequest struct {
	SourceTaskID     string
	SourceRevisionID string
	ID               string
	Slug             string
	Title            string
	MetadataJSON     string
	Actor            string
	Reason           string
}

// ForkTask creates a distinct task identity whose first revision copies an
// immutable source snapshot. A fork is lineage, not a shared mutable task
// directory, so future changes cannot alter the source task's evidence.
func (service *TaskService) ForkTask(ctx context.Context, request ForkTaskRequest) (store.TaskV2, store.TaskRevision, error) {
	if service == nil || service.core == nil {
		return store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("task service is not configured")
	}
	sourceTask, err := service.Get(ctx, request.SourceTaskID)
	if err != nil {
		return store.TaskV2{}, store.TaskRevision{}, err
	}
	sourceRevisionID := strings.TrimSpace(request.SourceRevisionID)
	if sourceRevisionID == "" {
		sourceRevisionID = sourceTask.CurrentRevisionID
	}
	if sourceRevisionID == "" {
		return store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("source task has no current revision")
	}
	sourceRevision, err := (&RevisionService{core: service.core}).Get(ctx, sourceRevisionID)
	if err != nil {
		return store.TaskV2{}, store.TaskRevision{}, err
	}
	if sourceRevision.TaskID != sourceTask.ID {
		return store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("source revision belongs to another task")
	}
	snapshot, err := (&RevisionService{core: service.core}).SnapshotDirectory(sourceTask.ID, sourceRevision.ID)
	if err != nil {
		return store.TaskV2{}, store.TaskRevision{}, err
	}
	return service.createTaskWithInitialSnapshot(ctx, CreateDraftTaskRequest{
		ID:           request.ID,
		Slug:         request.Slug,
		Title:        request.Title,
		MetadataJSON: request.MetadataJSON,
		SourceRepo:   sourceTask.SourceRepo,
		SourceCommit: sourceTask.SourceCommit,
		Actor:        request.Actor,
		Reason:       request.Reason,
	}, CreateRevisionFromSnapshotRequest{
		Origin:          store.RevisionOriginFork,
		SourceDirectory: snapshot,
		ProposalDigest:  sourceRevision.ProposalDigest,
		ChangeSummary:   "forked from task " + sourceTask.ID + " revision " + sourceRevision.ID,
		MetadataJSON:    sourceRevision.MetadataJSON,
		Actor:           request.Actor,
		Reason:          request.Reason,
	})
}

func (service *TaskService) Get(ctx context.Context, taskID string) (store.TaskV2, error) {
	if service == nil || service.core == nil {
		return store.TaskV2{}, fmt.Errorf("task service is not configured")
	}
	task, err := service.core.store.GetTaskV2(ctx, taskID)
	if err != nil {
		return store.TaskV2{}, err
	}
	if task == nil {
		return store.TaskV2{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, taskID)
	}
	return *task, nil
}

func (service *TaskService) List(ctx context.Context, includeDeleted bool) ([]store.TaskV2, error) {
	if service == nil || service.core == nil {
		return nil, fmt.Errorf("task service is not configured")
	}
	return service.core.store.ListTasksV2(ctx, includeDeleted)
}

type UpdateTaskRequest struct {
	TaskID          string
	ExpectedVersion int64
	Slug            string
	Title           string
	MetadataJSON    string
	Actor           string
	Reason          string
}

func (service *TaskService) Update(ctx context.Context, request UpdateTaskRequest) (store.TaskV2, error) {
	if service == nil || service.core == nil {
		return store.TaskV2{}, fmt.Errorf("task service is not configured")
	}
	return service.core.store.UpdateTaskV2(ctx, store.UpdateTaskV2Request{
		TaskID:          request.TaskID,
		ExpectedVersion: request.ExpectedVersion,
		Slug:            request.Slug,
		Title:           request.Title,
		MetadataJSON:    request.MetadataJSON,
		Actor:           request.Actor,
		Reason:          request.Reason,
	})
}

func (service *TaskService) Archive(ctx context.Context, taskID string, expectedVersion int64, actor, reason string) (store.TaskV2, error) {
	return service.transition(ctx, taskID, expectedVersion, store.TaskLifecycleArchived, actor, reason)
}

func (service *TaskService) SoftDelete(ctx context.Context, taskID string, expectedVersion int64, actor, reason string) (store.TaskV2, error) {
	return service.transition(ctx, taskID, expectedVersion, store.TaskLifecycleDeleted, actor, reason)
}

func (service *TaskService) Restore(ctx context.Context, taskID string, expectedVersion int64, state store.TaskLifecycleState, actor, reason string) (store.TaskV2, error) {
	if state == "" || state == store.TaskLifecycleDeleted {
		return store.TaskV2{}, fmt.Errorf("restore target lifecycle state is required")
	}
	return service.transition(ctx, taskID, expectedVersion, state, actor, reason)
}

func (service *TaskService) transition(ctx context.Context, taskID string, expectedVersion int64, state store.TaskLifecycleState, actor, reason string) (store.TaskV2, error) {
	if service == nil || service.core == nil {
		return store.TaskV2{}, fmt.Errorf("task service is not configured")
	}
	return service.core.store.UpdateTaskV2(ctx, store.UpdateTaskV2Request{
		TaskID:          taskID,
		ExpectedVersion: expectedVersion,
		LifecycleState:  state,
		Actor:           actor,
		Reason:          reason,
	})
}

// RevisionService materializes immutable V2 snapshots and controls revision
// lifecycle state. It never exposes a mutable path for a sealed revision.
type RevisionService struct{ core *lifecycleServiceCore }

type CreateRevisionFromSnapshotRequest struct {
	ID               string
	TaskID           string
	ParentRevisionID string
	Origin           store.RevisionOrigin
	SourceDirectory  string
	ProposalDigest   string
	ChangeSummary    string
	MetadataJSON     string
	Actor            string
	Reason           string
}

type revisionSnapshotManifest struct {
	Format       string    `json:"format"`
	TaskID       string    `json:"task_id"`
	RevisionID   string    `json:"revision_id"`
	TaskDigest   string    `json:"task_digest"`
	SnapshotPath string    `json:"snapshot_path"`
	CreatedAt    time.Time `json:"created_at"`
}

type preparedRevisionSnapshot struct {
	TaskID            string
	RevisionID        string
	TaskDigest        string
	ManifestObjectID  string
	RevisionDirectory string
}

// prepareSnapshot materializes all immutable filesystem state before a
// control-plane transaction references it. The returned cleanup is safe only
// until its caller has committed the corresponding revision row.
func (service *RevisionService) prepareSnapshot(ctx context.Context, taskID, revisionID, sourceDirectory string) (preparedRevisionSnapshot, func(), error) {
	if err := service.core.layout.ensureRoot(); err != nil {
		return preparedRevisionSnapshot{}, nil, err
	}
	revisionDirectory := service.core.layout.revisionDirectory(taskID, revisionID)
	if err := os.MkdirAll(filepath.Dir(revisionDirectory), 0o750); err != nil {
		return preparedRevisionSnapshot{}, nil, fmt.Errorf("create revision parent: %w", err)
	}
	if err := os.Mkdir(revisionDirectory, 0o750); err != nil {
		if errors.Is(err, os.ErrExist) {
			return preparedRevisionSnapshot{}, nil, fmt.Errorf("revision directory already exists: %s", revisionDirectory)
		}
		return preparedRevisionSnapshot{}, nil, fmt.Errorf("create revision directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(revisionDirectory) }
	digest, err := materializeManagedSnapshot(ctx, sourceDirectory, service.core.layout.snapshotDirectory(taskID, revisionID))
	if err != nil {
		cleanup()
		return preparedRevisionSnapshot{}, nil, err
	}
	manifest := revisionSnapshotManifest{
		Format:       "harbor.task-revision-manifest.v2",
		TaskID:       taskID,
		RevisionID:   revisionID,
		TaskDigest:   digest,
		SnapshotPath: "snapshot",
		CreatedAt:    service.core.now().UTC(),
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		cleanup()
		return preparedRevisionSnapshot{}, nil, fmt.Errorf("encode revision manifest: %w", err)
	}
	manifestObject, err := service.core.objects.PutBytes(ctx, manifestBytes)
	if err != nil {
		cleanup()
		return preparedRevisionSnapshot{}, nil, fmt.Errorf("store revision manifest object: %w", err)
	}
	if err := writeNewJSON(service.core.layout.revisionManifestPath(taskID, revisionID), manifest); err != nil {
		cleanup()
		return preparedRevisionSnapshot{}, nil, fmt.Errorf("write revision manifest: %w", err)
	}
	return preparedRevisionSnapshot{
		TaskID:            taskID,
		RevisionID:        revisionID,
		TaskDigest:        digest,
		ManifestObjectID:  string(manifestObject.Digest),
		RevisionDirectory: revisionDirectory,
	}, cleanup, nil
}

// CreateFromSnapshot turns a validated, strict Harbor task directory into an
// immutable TaskRevision. The copied snapshot is private to the revision and
// its digest is computed after bytes are materialized, never from a mutable
// source directory after the control-plane record exists.
func (service *RevisionService) CreateFromSnapshot(ctx context.Context, request CreateRevisionFromSnapshotRequest) (store.TaskRevision, error) {
	if service == nil || service.core == nil {
		return store.TaskRevision{}, fmt.Errorf("revision service is not configured")
	}
	if err := store.ValidateUUIDv7(request.TaskID); err != nil {
		return store.TaskRevision{}, err
	}
	revisionID := strings.TrimSpace(request.ID)
	if revisionID == "" {
		var err error
		revisionID, err = store.NewUUIDv7()
		if err != nil {
			return store.TaskRevision{}, fmt.Errorf("allocate revision ID: %w", err)
		}
	}
	if err := store.ValidateUUIDv7(revisionID); err != nil {
		return store.TaskRevision{}, err
	}
	if request.ParentRevisionID != "" {
		if err := store.ValidateUUIDv7(request.ParentRevisionID); err != nil {
			return store.TaskRevision{}, err
		}
	}
	if request.Origin == "" {
		return store.TaskRevision{}, fmt.Errorf("revision origin is required")
	}
	prepared, cleanup, err := service.prepareSnapshot(ctx, request.TaskID, revisionID, request.SourceDirectory)
	if err != nil {
		return store.TaskRevision{}, err
	}
	committed := false
	defer func() {
		if !committed {
			cleanup()
		}
	}()
	revision, err := service.core.store.CreateTaskRevision(ctx, store.CreateTaskRevisionRequest{
		ID:               revisionID,
		TaskID:           request.TaskID,
		ParentRevisionID: request.ParentRevisionID,
		Origin:           request.Origin,
		TaskDigest:       prepared.TaskDigest,
		ProposalDigest:   request.ProposalDigest,
		ManifestID:       prepared.ManifestObjectID,
		State:            store.RevisionStateSealed,
		ChangeSummary:    request.ChangeSummary,
		MetadataJSON:     request.MetadataJSON,
		Actor:            request.Actor,
		Reason:           request.Reason,
	})
	if err != nil {
		return store.TaskRevision{}, err
	}
	committed = true
	return revision, nil
}

func (service *RevisionService) Get(ctx context.Context, revisionID string) (store.TaskRevision, error) {
	if service == nil || service.core == nil {
		return store.TaskRevision{}, fmt.Errorf("revision service is not configured")
	}
	revision, err := service.core.store.GetTaskRevision(ctx, revisionID)
	if err != nil {
		return store.TaskRevision{}, err
	}
	if revision == nil {
		return store.TaskRevision{}, fmt.Errorf("%w: revision %s", ErrLifecycleNotFound, revisionID)
	}
	return *revision, nil
}

func (service *RevisionService) List(ctx context.Context, taskID string) ([]store.TaskRevision, error) {
	if service == nil || service.core == nil {
		return nil, fmt.Errorf("revision service is not configured")
	}
	return service.core.store.ListTaskRevisions(ctx, taskID)
}

func (service *RevisionService) SnapshotDirectory(taskID, revisionID string) (string, error) {
	if service == nil || service.core == nil {
		return "", fmt.Errorf("revision service is not configured")
	}
	if err := store.ValidateUUIDv7(taskID); err != nil {
		return "", err
	}
	if err := store.ValidateUUIDv7(revisionID); err != nil {
		return "", err
	}
	path := service.core.layout.snapshotDirectory(taskID, revisionID)
	if info, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: snapshot for revision %s", ErrLifecycleNotFound, revisionID)
		}
		return "", err
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("revision snapshot is not a real directory")
	}
	return path, nil
}

func (service *RevisionService) MarkValidated(ctx context.Context, revisionID string, expectedStateVersion int64, evidenceManifest string, actor, reason string) (store.TaskRevision, error) {
	if service == nil || service.core == nil {
		return store.TaskRevision{}, fmt.Errorf("revision service is not configured")
	}
	return service.core.store.TransitionTaskRevisionState(ctx, store.TransitionTaskRevisionStateRequest{
		RevisionID:                 revisionID,
		ExpectedStateVersion:       expectedStateVersion,
		State:                      store.RevisionStateValidated,
		ValidationEvidenceManifest: evidenceManifest,
		Actor:                      actor,
		Reason:                     reason,
	})
}

func (service *RevisionService) MarkReleased(ctx context.Context, revisionID string, expectedStateVersion int64, actor, reason string) (store.TaskRevision, error) {
	if service == nil || service.core == nil {
		return store.TaskRevision{}, fmt.Errorf("revision service is not configured")
	}
	return service.core.store.TransitionTaskRevisionState(ctx, store.TransitionTaskRevisionStateRequest{
		RevisionID:           revisionID,
		ExpectedStateVersion: expectedStateVersion,
		State:                store.RevisionStateReleased,
		Actor:                actor,
		Reason:               reason,
	})
}

type CreateRollbackRevisionRequest struct {
	TaskID           string
	TargetRevisionID string
	ParentRevisionID string
	ChangeSummary    string
	MetadataJSON     string
	Actor            string
	Reason           string
}

// CreateRollbackRevision creates a new immutable revision copied from a prior
// snapshot. It never repoints history or mutates the prior revision, so a
// rollback remains visible as normal revision lineage.
func (service *RevisionService) CreateRollbackRevision(ctx context.Context, request CreateRollbackRevisionRequest) (store.TaskRevision, error) {
	if service == nil || service.core == nil {
		return store.TaskRevision{}, fmt.Errorf("revision service is not configured")
	}
	target, err := service.Get(ctx, request.TargetRevisionID)
	if err != nil {
		return store.TaskRevision{}, err
	}
	if target.TaskID != request.TaskID {
		return store.TaskRevision{}, fmt.Errorf("rollback target belongs to another task")
	}
	parentID := strings.TrimSpace(request.ParentRevisionID)
	if parentID == "" {
		task, err := service.core.store.GetTaskV2(ctx, request.TaskID)
		if err != nil {
			return store.TaskRevision{}, err
		}
		if task == nil {
			return store.TaskRevision{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, request.TaskID)
		}
		parentID = task.CurrentRevisionID
	}
	snapshot, err := service.SnapshotDirectory(target.TaskID, target.ID)
	if err != nil {
		return store.TaskRevision{}, err
	}
	metadata := request.MetadataJSON
	if strings.TrimSpace(metadata) == "" {
		metadata = target.MetadataJSON
	}
	summary := request.ChangeSummary
	if strings.TrimSpace(summary) == "" {
		summary = "rollback to revision " + target.ID
	}
	return service.CreateFromSnapshot(ctx, CreateRevisionFromSnapshotRequest{
		TaskID:           request.TaskID,
		ParentRevisionID: parentID,
		Origin:           store.RevisionOriginRollback,
		SourceDirectory:  snapshot,
		ProposalDigest:   target.ProposalDigest,
		ChangeSummary:    summary,
		MetadataJSON:     metadata,
		Actor:            request.Actor,
		Reason:           request.Reason,
	})
}

type RevisionFileDiff struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
}

type RevisionDiff struct {
	LeftRevisionID  string             `json:"left_revision_id"`
	RightRevisionID string             `json:"right_revision_id"`
	Files           []RevisionFileDiff `json:"files"`
}

// Diff compares the exact policy file bytes in two immutable snapshots. The
// output is deliberately a digest-level diff; callers that need a textual
// review can read the two immutable snapshots without trusting mutable paths.
func (service *RevisionService) Diff(ctx context.Context, leftRevisionID, rightRevisionID string) (RevisionDiff, error) {
	if service == nil || service.core == nil {
		return RevisionDiff{}, fmt.Errorf("revision service is not configured")
	}
	left, err := service.Get(ctx, leftRevisionID)
	if err != nil {
		return RevisionDiff{}, err
	}
	right, err := service.Get(ctx, rightRevisionID)
	if err != nil {
		return RevisionDiff{}, err
	}
	if left.TaskID != right.TaskID {
		return RevisionDiff{}, fmt.Errorf("revision diff requires revisions from the same task")
	}
	leftSnapshot, err := service.SnapshotDirectory(left.TaskID, left.ID)
	if err != nil {
		return RevisionDiff{}, err
	}
	rightSnapshot, err := service.SnapshotDirectory(right.TaskID, right.ID)
	if err != nil {
		return RevisionDiff{}, err
	}
	result := RevisionDiff{LeftRevisionID: left.ID, RightRevisionID: right.ID}
	for _, file := range taskpolicy.CanonicalFiles() {
		leftBytes, leftErr := os.ReadFile(filepath.Join(leftSnapshot, filepath.FromSlash(file.Path)))
		rightBytes, rightErr := os.ReadFile(filepath.Join(rightSnapshot, filepath.FromSlash(file.Path)))
		if errors.Is(leftErr, os.ErrNotExist) && errors.Is(rightErr, os.ErrNotExist) && file.Environment {
			continue
		}
		if leftErr != nil || rightErr != nil {
			return RevisionDiff{}, fmt.Errorf("read revision diff file %s: left=%v right=%v", file.Path, leftErr, rightErr)
		}
		result.Files = append(result.Files, RevisionFileDiff{Path: file.Path, Changed: !bytes.Equal(leftBytes, rightBytes)})
	}
	return result, nil
}

// ReviewService owns durable review decisions and the review-gated current
// revision switch. A change to task bytes must produce a new revision, so an
// approval is bound to the exact revision digest in the control plane.
type ReviewService struct{ core *lifecycleServiceCore }

func (service *ReviewService) Request(ctx context.Context, revisionID, evidenceManifest, actor, reason string) (store.ReviewRequest, error) {
	if service == nil || service.core == nil {
		return store.ReviewRequest{}, fmt.Errorf("review service is not configured")
	}
	return service.core.store.CreateReviewRequest(ctx, store.CreateReviewRequest{
		RevisionID:             revisionID,
		EvidenceManifestDigest: evidenceManifest,
		Actor:                  actor,
		Reason:                 reason,
	})
}

type DecideReviewRequest struct {
	ID                     string
	ReviewRequestID        string
	RevisionID             string
	Action                 store.ReviewDecisionAction
	ExpectedRevisionDigest string
	Actor                  string
	Reason                 string
}

func (service *ReviewService) Decide(ctx context.Context, request DecideReviewRequest) (store.ReviewDecision, error) {
	if service == nil || service.core == nil {
		return store.ReviewDecision{}, fmt.Errorf("review service is not configured")
	}
	if strings.TrimSpace(request.ID) != "" {
		if err := store.ValidateUUIDv7(request.ID); err != nil {
			return store.ReviewDecision{}, err
		}
		existing, err := service.core.store.ListReviewDecisionsForRequest(ctx, request.ReviewRequestID)
		if err != nil {
			return store.ReviewDecision{}, err
		}
		for _, decision := range existing {
			if decision.ID != request.ID {
				continue
			}
			if decision.RevisionID != request.RevisionID || decision.Action != request.Action ||
				decision.ExpectedRevisionDigest != request.ExpectedRevisionDigest || decision.Actor != strings.TrimSpace(request.Actor) ||
				decision.Reason != strings.TrimSpace(request.Reason) {
				return store.ReviewDecision{}, fmt.Errorf("%w: review decision id %s", store.ErrIdempotencyConflict, request.ID)
			}
			return decision, nil
		}
	}
	return service.core.store.RecordReviewDecision(ctx, store.RecordReviewDecisionRequest{
		ID:                     request.ID,
		ReviewRequestID:        request.ReviewRequestID,
		RevisionID:             request.RevisionID,
		Action:                 request.Action,
		ExpectedRevisionDigest: request.ExpectedRevisionDigest,
		Actor:                  request.Actor,
		Reason:                 request.Reason,
	})
}

func (service *ReviewService) PromoteCurrent(ctx context.Context, taskID, revisionID string, expectedTaskVersion int64, actor, reason string) (store.TaskV2, error) {
	if service == nil || service.core == nil {
		return store.TaskV2{}, fmt.Errorf("review service is not configured")
	}
	promoted, err := service.core.store.PromoteTaskCurrentRevision(ctx, store.PromoteCurrentRevisionRequest{
		TaskID:          taskID,
		RevisionID:      revisionID,
		ExpectedVersion: expectedTaskVersion,
		Actor:           actor,
		Reason:          reason,
	})
	if err != nil {
		return store.TaskV2{}, err
	}
	// A reviewed first revision makes a draft task ready for normal runs. This
	// remains a lifecycle projection; the current-revision safety gate was
	// already enforced by the atomic promotion above.
	if promoted.LifecycleState != store.TaskLifecycleDraft {
		return promoted, nil
	}
	return service.core.store.UpdateTaskV2(ctx, store.UpdateTaskV2Request{
		TaskID:          promoted.ID,
		ExpectedVersion: promoted.Version,
		LifecycleState:  store.TaskLifecycleReady,
		Actor:           actor,
		Reason:          reason,
	})
}

// RunService freezes a code-versioned template and a fully explicit profile
// into every run manifest before any work is scheduled.
type RunService struct{ core *lifecycleServiceCore }

type StartRunRequest struct {
	ID             string
	TaskID         string
	RevisionID     string
	Profile        workflowadapter.ExecutionProfile
	ParentRunID    string
	Trigger        string
	ExecutionEpoch int
	Actor          string
	Reason         string
}

type runManifest struct {
	Format   string                           `json:"format"`
	RunID    string                           `json:"run_id"`
	TaskID   string                           `json:"task_id"`
	Revision string                           `json:"revision_id"`
	Resolved workflowadapter.ResolvedWorkflow `json:"resolved_workflow"`
	Created  time.Time                        `json:"created_at"`
}

// workflowRunExecutionPayload is the durable child-worker handoff. It carries
// the complete frozen quota snapshot as well as the definition identity so a
// worker never asks a caller or a current code path to recompute claims. The
// worker still verifies this duplicated snapshot against the immutable run
// manifest before admitting work.
type workflowRunExecutionPayload struct {
	Format         string                              `json:"format"`
	RunID          string                              `json:"run_id"`
	DefinitionHash string                              `json:"definition_hash"`
	QuotaPolicy    workflowadapter.ResolvedQuotaPolicy `json:"quota_policy"`
}

const workflowRunExecutionPayloadFormat = "harbor.workflow-run-execution.v3"

// StartRun accepts only a complete, explicit profile. The resolved manifest is
// written before the run row so a durable record never points at a definition
// compiled from later code or mutable options.
func (service *RunService) StartRun(ctx context.Context, request StartRunRequest) (store.WorkflowRun, error) {
	if service == nil || service.core == nil {
		return store.WorkflowRun{}, fmt.Errorf("run service is not configured")
	}
	if err := store.ValidateUUIDv7(request.TaskID); err != nil {
		return store.WorkflowRun{}, err
	}
	if err := store.ValidateUUIDv7(request.RevisionID); err != nil {
		return store.WorkflowRun{}, err
	}
	if strings.TrimSpace(request.ParentRunID) != "" {
		if err := store.ValidateUUIDv7(request.ParentRunID); err != nil {
			return store.WorkflowRun{}, err
		}
	}
	if strings.TrimSpace(request.Trigger) == "" {
		return store.WorkflowRun{}, fmt.Errorf("run trigger is required")
	}
	resolved, err := service.core.template.Compile(request.Profile)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("compile explicit execution profile: %w", err)
	}
	runID := strings.TrimSpace(request.ID)
	if runID == "" {
		runID, err = store.NewUUIDv7()
		if err != nil {
			return store.WorkflowRun{}, fmt.Errorf("allocate run ID: %w", err)
		}
	}
	if err := store.ValidateUUIDv7(runID); err != nil {
		return store.WorkflowRun{}, err
	}
	if err := service.core.layout.ensureRoot(); err != nil {
		return store.WorkflowRun{}, err
	}
	runDirectory := service.core.layout.runDirectory(runID)
	if err := os.MkdirAll(filepath.Dir(runDirectory), 0o750); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("create run parent: %w", err)
	}
	if err := os.Mkdir(runDirectory, 0o750); err != nil {
		if errors.Is(err, os.ErrExist) {
			return store.WorkflowRun{}, fmt.Errorf("run directory already exists: %s", runDirectory)
		}
		return store.WorkflowRun{}, fmt.Errorf("create run directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(runDirectory)
		}
	}()
	manifest := runManifest{
		Format:   "harbor.workflow-run-manifest.v2",
		RunID:    runID,
		TaskID:   request.TaskID,
		Revision: request.RevisionID,
		Resolved: resolved.Clone(),
		Created:  service.core.now().UTC(),
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("encode run manifest: %w", err)
	}
	if err := writeNewJSON(filepath.Join(runDirectory, "run-manifest.json"), manifest); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("write run manifest: %w", err)
	}
	dispatchPayload, err := json.Marshal(workflowRunExecutionPayload{
		Format: workflowRunExecutionPayloadFormat, RunID: runID, DefinitionHash: string(resolved.DefinitionFingerprint), QuotaPolicy: resolved.QuotaPolicy.Clone(),
	})
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("encode initial workflow run dispatch: %w", err)
	}
	run, err := service.core.store.CreateWorkflowRun(ctx, store.CreateWorkflowRunRequest{
		ID:                      runID,
		TaskID:                  request.TaskID,
		RevisionID:              request.RevisionID,
		WorkflowTemplateID:      resolved.TemplateID,
		WorkflowTemplateVersion: resolved.TemplateVersion,
		ResolvedProfileHash:     string(resolved.ExecutionProfileFingerprint),
		DefinitionHash:          string(resolved.DefinitionFingerprint),
		RunManifestJSON:         string(encoded),
		ParentRunID:             request.ParentRunID,
		Trigger:                 request.Trigger,
		ExecutionEpoch:          request.ExecutionEpoch,
		Actor:                   request.Actor,
		Reason:                  request.Reason,
		Dispatch: &store.WorkflowRunDispatchRequest{
			CommandType:    "workflow_run.execute",
			PayloadJSON:    string(dispatchPayload),
			IdempotencyKey: "workflow-run-execution:" + runID,
		},
	})
	if err != nil {
		return store.WorkflowRun{}, err
	}
	committed = true
	return run, nil
}

func (service *RunService) Get(ctx context.Context, runID string) (store.WorkflowRun, error) {
	if service == nil || service.core == nil {
		return store.WorkflowRun{}, fmt.Errorf("run service is not configured")
	}
	run, err := service.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if run == nil {
		return store.WorkflowRun{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	return *run, nil
}

func (service *RunService) ListForTask(ctx context.Context, taskID string) ([]store.WorkflowRun, error) {
	if service == nil || service.core == nil {
		return nil, fmt.Errorf("run service is not configured")
	}
	return service.core.store.ListWorkflowRunsForTask(ctx, taskID)
}

// GetStageAttempt reads a durable stage attempt for read-only projections.
func (service *RunService) GetStageAttempt(ctx context.Context, stageAttemptID string) (store.StageAttempt, error) {
	if service == nil || service.core == nil {
		return store.StageAttempt{}, fmt.Errorf("run service is not configured")
	}
	attempt, err := service.core.store.GetStageAttempt(ctx, stageAttemptID)
	if err != nil {
		return store.StageAttempt{}, err
	}
	if attempt == nil {
		return store.StageAttempt{}, fmt.Errorf("%w: stage attempt %s", ErrLifecycleNotFound, stageAttemptID)
	}
	return *attempt, nil
}

// ListStageAttempts returns the durable attempts belonging to a Run. The
// method is read-only and exists so UI/API projections do not access SQLite
// directly.
func (service *RunService) ListStageAttempts(ctx context.Context, runID string) ([]store.StageAttempt, error) {
	if service == nil || service.core == nil {
		return nil, fmt.Errorf("run service is not configured")
	}
	if err := store.ValidateUUIDv7(runID); err != nil {
		return nil, err
	}
	return service.core.store.ListStageAttemptsForRun(ctx, runID)
}
