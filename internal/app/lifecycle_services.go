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

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
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
	Tasks     *TaskService
	Revisions *RevisionService
	Runs      *RunService
	Reviews   *ReviewService
	// AuthoringReviews owns source/session-bound review gates that exist
	// before a generated task has its first TaskRevision. It is deliberately
	// separate from Reviews, whose contract is restricted to TaskRevision.
	AuthoringReviews *AuthoringReviewService
	Releases         *ReleaseService
	Deletion         *DeletionService
	Control          *ExecutionControlService
	Budgets          *BudgetGrantService
	Continuations    *TaskContinuationService
	Changes          *ChangeProviderService
	Repairs          *RepairLoopService
	Candidates       *CandidateRetentionService
	// Transcripts expires raw Agent response material while retaining its
	// immutable diagnostic and audit metadata.
	Transcripts *AgentTranscriptRetentionService
	Inspection  *LifecycleInspectionService
	// TaskBoard is the compact application boundary consumed by the terminal
	// task board. It projects durable state and delegates its mutations to the
	// existing authoring, review, and activation services.
	TaskBoard      *TaskBoardService
	LocalRuntime   *LocalRuntimeService
	WorkerHandoffs *RunWorkerHandoffService
	// RunActivations consumes the durable local queue-delivery events that
	// wake controlled child workers. It is optional in non-production
	// compositions, where tests and read-only control planes intentionally do
	// not spawn processes.
	RunActivations *RunActivationService
	Mutations      *LifecycleMutationService
	// AuthoringLaunches owns source capture and the source/session half of a
	// Standard task creation.
	AuthoringLaunches *StandardAuthoringLaunchService

	core *lifecycleServiceCore
}

type lifecycleServiceCore struct {
	store              *store.Store
	layout             managedLayout
	objects            *workflowruntime.ArtifactObjectStore
	operationResolver  workflowadapter.StageOperationResolver
	deploymentCatalogs *deploymentCatalogRegistry
	now                func() time.Time
	changes            *ChangeProviderService
	repairs            *RepairLoopService
}

// LifecycleServicesOptions supplies controlled integrations used by the V2
// application boundary. A RunExecutionSpec is admitted only when every frozen
// stage operation resolves through OperationResolver without performing work.
type LifecycleServicesOptions struct {
	OperationResolver workflowadapter.StageOperationResolver
	// ChangeProviders are exact externally controlled change implementations
	// installed at composition time. In particular, AgentRepairProvider must
	// never be synthesized from PATH, ambient model defaults, or caller input.
	// An omitted agent provider leaves automated repair unavailable while the
	// explicit local patch provider remains available.
	ChangeProviders []ChangeProvider
	// DeploymentCatalogResolver opts this lifecycle composition into the
	// immutable production operation-catalog receipt contract. It is separate
	// from OperationResolver so test-only accept-all resolvers do not acquire a
	// fabricated production identity. When omitted, an OperationResolver that
	// itself exposes the receipt contract is used automatically.
	DeploymentCatalogResolver DeploymentCatalogReceiptResolver
	// RequireDeploymentCatalog turns a missing catalog-aware receipt resolver
	// into a construction error. Production CLI/worker composition should set
	// this; non-production tests can retain the default false value.
	RequireDeploymentCatalog bool
	// DeploymentCatalogResolvers installs immutable deployment catalog/lock
	// verifiers keyed by their exact closed workflow template. It is the
	// multi-template successor to DeploymentCatalogResolver: a StartRun,
	// replay, or worker claim selects only the binding named by its frozen
	// RunExecutionSpec.Template. The legacy DeploymentCatalogResolver, when
	// supplied, is converted into one additional template-keyed binding; a
	// duplicate template is rejected rather than becoming a fallback.
	DeploymentCatalogResolvers []TemplateDeploymentCatalogResolver
	// StandardAuthoringSourceCapturer and StandardAuthoringRunDefinitionProvider
	// are the deployment-owned inputs for Standard authoring. The caller selects
	// only a validated immutable HTTPS/SSH source coordinate; capture mechanics,
	// execution definition, models, secrets, and catalog/lock remain closed.
	// Omitting either leaves the launch surface fail-closed while retaining the
	// rest of the lifecycle control plane.
	StandardAuthoringSourceCapturer        StandardAuthoringSourceCapturer
	StandardAuthoringRunDefinitionProvider StandardAuthoringRunDefinitionProvider
	// RunWorkerHandoffLauncher is the composition-owned local process boundary
	// used for automatic delivery of queued Run work. The application layer
	// retains the durable reserve/spawn/claim protocol; this port only starts
	// the child after a durable handoff has been reserved.
	RunWorkerHandoffLauncher RunWorkerHandoffLauncher
}

// NewLifecycleServices wires a V2 control plane to its managed local
// filesystem. It does not create an execution profile: profiles are always
// explicit per StartRun request under the confirmed budget policy.
func NewLifecycleServices(root string, dataStore *store.Store) (*LifecycleServices, error) {
	return NewLifecycleServicesWithOptions(root, dataStore, LifecycleServicesOptions{})
}

// NewLifecycleServicesWithOptions wires a V2 control plane with its controlled
// execution-operation resolver. Callers that do not yet install a resolver
// retain a usable read/control plane, but StartRun rejects admission before
// creating an input bundle, Run, durable job, or outbox record.
func NewLifecycleServicesWithOptions(root string, dataStore *store.Store, options LifecycleServicesOptions) (*LifecycleServices, error) {
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
	operationResolver := options.OperationResolver
	if operationResolver == nil {
		operationResolver = unavailableStageOperationResolver{}
	}
	catalogResolvers := append([]TemplateDeploymentCatalogResolver(nil), options.DeploymentCatalogResolvers...)
	catalogResolver := options.DeploymentCatalogResolver
	if catalogResolver == nil && len(catalogResolvers) == 0 {
		if derived, ok := operationResolver.(DeploymentCatalogReceiptResolver); ok {
			catalogResolver = derived
		}
	}
	if catalogResolver != nil {
		catalogResolvers = append(catalogResolvers, TemplateDeploymentCatalogResolver{
			Template: catalogResolver.Receipt().Template,
			Resolver: catalogResolver,
		})
	}
	if len(catalogResolvers) == 0 && options.RequireDeploymentCatalog {
		return nil, fmt.Errorf("%w: a catalog-aware operation resolver is required", stageprovider.ErrDeploymentOperationCatalogUnavailable)
	}
	catalogRegistry, err := newDeploymentCatalogRegistry(catalogResolvers)
	if err != nil {
		return nil, err
	}
	core := &lifecycleServiceCore{
		store:              dataStore,
		layout:             layout,
		objects:            objects,
		operationResolver:  operationResolver,
		deploymentCatalogs: catalogRegistry,
		now:                time.Now,
	}
	activations := &RunActivationService{core: core, launcher: options.RunWorkerHandoffLauncher}
	continuations := newTaskContinuationService(core)
	changes := newChangeProviderService(core)
	for _, provider := range options.ChangeProviders {
		changes.Register(provider)
	}
	core.changes = changes
	repairs := newRepairLoopService(core, continuations)
	core.repairs = repairs
	mutations := newLifecycleMutationService(core)
	inspection := &LifecycleInspectionService{core: core}
	control := &ExecutionControlService{core: core}
	authoringReviews := &AuthoringReviewService{core: core}
	authoringLaunches := newStandardAuthoringLaunchService(core, options.StandardAuthoringSourceCapturer, options.StandardAuthoringRunDefinitionProvider)
	runs := &RunService{core: core}
	taskBoard := newTaskBoardService(core, inspection, authoringLaunches, authoringReviews, mutations, activations, continuations, runs, control, options.RunWorkerHandoffLauncher)
	services := &LifecycleServices{
		Tasks:             &TaskService{core: core},
		Revisions:         &RevisionService{core: core},
		Runs:              runs,
		Reviews:           &ReviewService{core: core},
		AuthoringReviews:  authoringReviews,
		Releases:          &ReleaseService{core: core},
		Deletion:          &DeletionService{core: core},
		Control:           control,
		Budgets:           &BudgetGrantService{core: core},
		Continuations:     continuations,
		Changes:           changes,
		Repairs:           repairs,
		Candidates:        &CandidateRetentionService{core: core},
		Transcripts:       &AgentTranscriptRetentionService{core: core},
		Inspection:        inspection,
		TaskBoard:         taskBoard,
		LocalRuntime:      &LocalRuntimeService{core: core},
		WorkerHandoffs:    &RunWorkerHandoffService{core: core},
		RunActivations:    activations,
		Mutations:         mutations,
		AuthoringLaunches: authoringLaunches,
		core:              core,
	}
	services.LocalRuntime.services = services
	return services, nil
}

// unavailableStageOperationResolver makes missing execution wiring explicit.
// It is deliberately installed instead of treating a nil resolver as an
// allow-all fallback, which would admit a Run that no controlled worker can
// prove it is able to execute.
type unavailableStageOperationResolver struct{}

func (unavailableStageOperationResolver) ValidateStageOperation(workflowadapter.StageOperationResolution) error {
	return fmt.Errorf("controlled stage operation resolver is not configured")
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

// CatalogLockAttestedWorkflowkitProviderResolver returns the one production
// provider resolver that was installed when these services were composed. It
// remains for the single-template composition API; a multi-template package
// instead exposes TemplateWorkflowkitProviderOperationResolver through
// WorkflowkitProviderOperationResolver below. A worker must never reuse an
// arbitrary test resolver, mutable registry, or PATH fallback.
func (services *LifecycleServices) CatalogLockAttestedWorkflowkitProviderResolver() *stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver {
	if services == nil || services.core == nil {
		return nil
	}
	resolver, _ := services.core.operationResolver.(*stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver)
	return resolver
}

// WorkflowkitProviderOperationResolver exposes the controlled production
// provider boundary installed by composition. It can be either one
// catalog-lock-attested template bundle or the explicit template router. Nil
// means this is a read/control-plane composition and the worker must retain
// its rejecting provider resolver.
func (services *LifecycleServices) WorkflowkitProviderOperationResolver() stageprovider.WorkflowkitProviderOperationResolver {
	if services == nil || services.core == nil {
		return nil
	}
	resolver, _ := services.core.operationResolver.(stageprovider.WorkflowkitProviderOperationResolver)
	return resolver
}

// TaskService owns stable task identity and lifecycle-state mutations.
type TaskService struct{ core *lifecycleServiceCore }

type CreateDraftTaskRequest struct {
	ID           string
	Slug         string
	Title        string
	MetadataJSON string
	SourceRepo   string
	SourceCommit string
	Actor        string
	Reason       string
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
		Actor:          request.Actor,
		Reason:         request.Reason,
	})
}

type ImportTaskRequest struct {
	CreateDraftTaskRequest
	InitialRevisionID string
	SourceDirectory   string
	ProposalDigest    string
	ChangeSummary     string
}

// ImportTask creates a stable task and its first immutable imported revision.
// Snapshot materialization precedes one atomic task/revision transaction, so
// an import failure cannot leave a path-derived or empty partial Task behind.
func (service *TaskService) ImportTask(ctx context.Context, request ImportTaskRequest) (store.TaskV2, store.TaskRevision, error) {
	if service == nil || service.core == nil {
		return store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("task service is not configured")
	}
	if err := taskpolicy.ValidateManagedSnapshotV2(request.SourceDirectory); err != nil {
		return store.TaskV2{}, store.TaskRevision{}, fmt.Errorf("validate imported task: %w", err)
	}
	return service.createTaskWithInitialSnapshot(ctx, request.CreateDraftTaskRequest, CreateRevisionFromSnapshotRequest{
		ID:              request.InitialRevisionID,
		Origin:          store.RevisionOriginImported,
		SourceDirectory: request.SourceDirectory,
		ProposalDigest:  request.ProposalDigest,
		ChangeSummary:   request.ChangeSummary,
		Actor:           request.Actor,
		Reason:          request.Reason,
	})
}

func (service *TaskService) createTaskWithInitialSnapshot(ctx context.Context, taskRequest CreateDraftTaskRequest, revisionRequest CreateRevisionFromSnapshotRequest) (store.TaskV2, store.TaskRevision, error) {
	if err := taskpolicy.ValidateManagedSnapshotV2(revisionRequest.SourceDirectory); err != nil {
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
	SourceTaskID      string
	SourceRevisionID  string
	ID                string
	InitialRevisionID string
	Slug              string
	Title             string
	MetadataJSON      string
	Actor             string
	Reason            string
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
		ID:              request.InitialRevisionID,
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
	gate, err := service.core.store.GetReviewGateBindingByReviewRequest(ctx, request.ReviewRequestID)
	if err != nil {
		return store.ReviewDecision{}, err
	}
	if gate != nil {
		return service.decideReviewGate(ctx, *gate, request)
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
	ID                       string
	TaskID                   string
	RevisionID               string
	Profile                  workflowadapter.ExecutionProfile
	ExecutionSpec            workflowadapter.RunExecutionSpec
	InputBundleID            string
	ProfileFingerprint       workflowkit.Fingerprint
	ExecutionSpecFingerprint workflowkit.Fingerprint
	// DeploymentCatalogReceipt is supplied only by a previously frozen
	// StartRun input bundle. Direct callers leave it empty and StartRun freezes
	// the receipt from the explicitly configured catalog-aware resolver.
	DeploymentCatalogReceipt []byte
	ParentRunID              string
	Trigger                  string
	ExecutionEpoch           int
	Actor                    string
	Reason                   string
}

type runManifest struct {
	Format string `json:"format"`
	RunID  string `json:"run_id"`
	// Subject* is the generic, kernel-facing immutable subject identity.  The
	// task/revision fields below remain the task-lifecycle projection and are
	// intentionally empty for an AuthoringSession Run.
	SubjectKind              store.WorkflowRunSubjectKind     `json:"subject_kind,omitempty"`
	SubjectID                string                           `json:"subject_id,omitempty"`
	SubjectRevisionID        string                           `json:"subject_revision_id,omitempty"`
	SubjectDigest            string                           `json:"subject_digest,omitempty"`
	AuthoringSessionID       string                           `json:"authoring_session_id,omitempty"`
	TaskID                   string                           `json:"task_id"`
	Revision                 string                           `json:"revision_id"`
	Resolved                 workflowadapter.ResolvedWorkflow `json:"resolved_workflow"`
	InitialExecutionPlan     workflowkit.ExecutionPlan        `json:"initial_execution_plan"`
	Inputs                   *runManifestInputs               `json:"inputs,omitempty"`
	ExecutionSpec            json.RawMessage                  `json:"execution_spec,omitempty"`
	DeploymentCatalogReceipt json.RawMessage                  `json:"deployment_catalog_receipt,omitempty"`
	// LegacyDeploymentCatalogLockIdentity tolerates the lock identity field
	// written by binaries from before runtime lock resolution. It is never
	// read or re-persisted; deployment lock identity is resolved at runtime
	// against the currently installed deployment.
	LegacyDeploymentCatalogLockIdentity json.RawMessage `json:"deployment_catalog_lock_identity,omitempty"`
	RestartOfRunID                      string          `json:"restart_of_run_id,omitempty"`
	Created                             time.Time       `json:"created_at"`
}

type runManifestInputs struct {
	Format                            string                    `json:"format"`
	BundleID                          string                    `json:"bundle_id,omitempty"`
	ProfileFingerprint                workflowkit.Fingerprint   `json:"profile_fingerprint"`
	RequestedExecutionSpecFingerprint workflowkit.Fingerprint   `json:"requested_execution_spec_fingerprint"`
	ExecutionSpecFingerprint          workflowkit.Fingerprint   `json:"execution_spec_fingerprint"`
	ManagedInputs                     []runManifestManagedInput `json:"managed_inputs,omitempty"`
}

const runManifestInputsFormat = "harbor.run-manifest-inputs.v1"

func decodeRunManifest(run store.WorkflowRun) (runManifest, error) {
	var manifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
		return runManifest{}, err
	}
	if manifest.Format != "harbor.workflow-run-manifest.v2" || manifest.RunID != run.ID || manifest.Inputs == nil || manifest.Inputs.Format != runManifestInputsFormat {
		return runManifest{}, fmt.Errorf("run manifest does not match workflow run")
	}
	if err := validateRunManifestSubject(manifest, run); err != nil {
		return runManifest{}, err
	}
	return manifest, nil
}

// validateRunManifestSubject proves the durable row, its on-disk manifest,
// and the typed execution specification all use the same closed subject
// coordinate.  Older task-only test fixtures may omit the duplicate generic
// fields, but a newly-created AuthoringSession Run must always persist them:
// it has no task-revision projection to fall back to.
func validateRunManifestSubject(manifest runManifest, run store.WorkflowRun) error {
	if manifest.TaskID != run.TaskID || manifest.Revision != run.RevisionID {
		return fmt.Errorf("run manifest task projection does not match workflow run")
	}
	switch run.SubjectKind {
	case store.WorkflowRunSubjectTaskRevision:
		if run.TaskID == "" || run.RevisionID == "" || run.AuthoringSessionID != "" {
			return fmt.Errorf("workflow run has invalid task-revision subject")
		}
		// Keep fixture compatibility while requiring exact equality whenever a
		// generic projection is present in a newly written manifest.
		if manifest.SubjectKind != "" || manifest.SubjectID != "" || manifest.SubjectRevisionID != "" || manifest.SubjectDigest != "" || manifest.AuthoringSessionID != "" {
			if manifest.SubjectKind != run.SubjectKind || manifest.SubjectID != run.SubjectID || manifest.SubjectRevisionID != run.SubjectRevisionID || manifest.SubjectDigest != run.SubjectDigest || manifest.AuthoringSessionID != "" {
				return fmt.Errorf("run manifest generic task-revision subject does not match workflow run")
			}
		}
	case store.WorkflowRunSubjectAuthoringSession:
		if run.TaskID != "" || run.RevisionID != "" || run.AuthoringSessionID == "" ||
			manifest.SubjectKind != run.SubjectKind || manifest.SubjectID != run.SubjectID ||
			manifest.SubjectRevisionID != run.SubjectRevisionID || manifest.SubjectDigest != run.SubjectDigest ||
			manifest.AuthoringSessionID != run.AuthoringSessionID {
			return fmt.Errorf("run manifest generic authoring-session subject does not match workflow run")
		}
	default:
		return fmt.Errorf("workflow run has unsupported subject kind %q", run.SubjectKind)
	}
	return nil
}

// workflowRunExecutionPayload is the durable child-worker handoff. It carries
// the complete frozen quota snapshot as well as the definition identity so a
// worker never asks a caller or a current code path to recompute claims. The
// worker still verifies this duplicated snapshot against the immutable run
// manifest before admitting work.
type workflowRunExecutionPayload struct {
	Format                   string                              `json:"format"`
	RunID                    string                              `json:"run_id"`
	DefinitionHash           string                              `json:"definition_hash"`
	ExecutionSpecFingerprint workflowkit.Fingerprint             `json:"execution_spec_fingerprint"`
	QuotaPolicy              workflowadapter.ResolvedQuotaPolicy `json:"quota_policy"`
}

const workflowRunExecutionPayloadFormat = "harbor.workflow-run-execution.v3"

// StartRun accepts only a complete, explicit profile. The resolved manifest is
// written before the run row so a durable record never points at a definition
// compiled from later code or mutable options.
func (service *RunService) StartRun(ctx context.Context, request StartRunRequest) (store.WorkflowRun, error) {
	if service == nil || service.core == nil {
		return store.WorkflowRun{}, fmt.Errorf("run service is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
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
	if err := service.validateRunParent(ctx, request); err != nil {
		return store.WorkflowRun{}, err
	}
	template, err := resolveFrozenRunTemplate(request.Profile, request.ExecutionSpec)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	_, requestedSpecificationFingerprint, err := service.validateRunExecutionSpec(ctx, request)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	catalogReceipt, err := service.core.resolveStartRunDeploymentCatalogReceipt(request.ExecutionSpec.Template, request.DeploymentCatalogReceipt)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("freeze deployment catalog receipt for run: %w", err)
	}
	resolved, err := template.Compile(request.Profile)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("compile explicit execution profile: %w", err)
	}
	profileCanonical, err := request.Profile.CanonicalJSON()
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("canonicalize explicit execution profile: %w", err)
	}
	profileFingerprint, err := request.Profile.Fingerprint()
	if err != nil || profileFingerprint != resolved.ExecutionProfileFingerprint {
		return store.WorkflowRun{}, fmt.Errorf("explicit execution profile fingerprint does not match compiled workflow")
	}
	if request.ProfileFingerprint != "" && request.ProfileFingerprint != resolved.ExecutionProfileFingerprint {
		return store.WorkflowRun{}, fmt.Errorf("%w: supplied execution profile fingerprint does not match compiled profile", store.ErrIdempotencyConflict)
	}
	initialExecutionPlan, err := workflowkit.CompileDependencyExecutionPlan(resolved.Descriptor)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("compile initial dependency execution plan: %w", err)
	}
	if request.ExecutionSpecFingerprint != "" && request.ExecutionSpecFingerprint != requestedSpecificationFingerprint {
		return store.WorkflowRun{}, fmt.Errorf("%w: supplied execution specification fingerprint does not match canonical specification", store.ErrIdempotencyConflict)
	}
	if strings.TrimSpace(request.InputBundleID) != "" {
		if err := store.ValidateUUIDv7(request.InputBundleID); err != nil {
			return store.WorkflowRun{}, fmt.Errorf("run input bundle ID: %w", err)
		}
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
	revision, err := service.core.store.GetTaskRevision(ctx, request.RevisionID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if revision == nil || revision.TaskID != request.TaskID {
		return store.WorkflowRun{}, fmt.Errorf("%w: TaskRevision %s", ErrLifecycleNotFound, request.RevisionID)
	}
	if existing, err := service.core.store.GetWorkflowRun(ctx, runID); err != nil {
		return store.WorkflowRun{}, err
	} else if existing != nil {
		manifest, err := decodeRunManifest(*existing)
		if err != nil {
			return store.WorkflowRun{}, fmt.Errorf("%w: workflow run %s manifest: %v", store.ErrIdempotencyConflict, existing.ID, err)
		}
		finalSpecification, _, err := service.prepareManagedInitialRunInputs(ctx, runID, *revision, request.ExecutionSpec, manifest.Inputs.ManagedInputs)
		if err != nil {
			return store.WorkflowRun{}, fmt.Errorf("%w: workflow run %s managed inputs: %v", store.ErrIdempotencyConflict, existing.ID, err)
		}
		finalRequest := request
		finalRequest.ExecutionSpec = finalSpecification
		finalRequest.ExecutionSpecFingerprint = ""
		finalSpecificationCanonical, finalSpecificationFingerprint, err := service.validateRunExecutionSpec(ctx, finalRequest)
		if err != nil {
			return store.WorkflowRun{}, err
		}
		if err := service.validateReplayedWorkflowRun(*existing, request, resolved, profileCanonical, requestedSpecificationFingerprint, finalSpecificationCanonical, finalSpecificationFingerprint, initialExecutionPlan, catalogReceipt); err != nil {
			return store.WorkflowRun{}, err
		}
		if err := service.ensureRunInputArtifacts(ctx, *existing, manifest); err != nil {
			return store.WorkflowRun{}, err
		}
		if err := service.ensureInitialWorkflowRunDispatch(ctx, *existing, manifest); err != nil {
			return store.WorkflowRun{}, err
		}
		return *existing, nil
	}
	if err := service.core.layout.ensureRoot(); err != nil {
		return store.WorkflowRun{}, err
	}
	runDirectory := service.core.layout.runDirectory(runID)
	if err := os.MkdirAll(filepath.Dir(runDirectory), 0o750); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("create run parent: %w", err)
	}
	createdRunDirectory := false
	manifestPath := filepath.Join(runDirectory, "run-manifest.json")
	if err := os.Mkdir(runDirectory, 0o750); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return store.WorkflowRun{}, fmt.Errorf("create run directory: %w", err)
		}
	} else {
		createdRunDirectory = true
	}
	if !createdRunDirectory {
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			return store.WorkflowRun{}, fmt.Errorf("run directory already exists without a recoverable frozen manifest: %s", runDirectory)
		}
		var recovered runManifest
		if err := decodeStrictJSON(string(raw), &recovered); err != nil || recovered.Inputs == nil || recovered.Inputs.Format != runManifestInputsFormat || recovered.Inputs.RequestedExecutionSpecFingerprint != requestedSpecificationFingerprint {
			return store.WorkflowRun{}, fmt.Errorf("run directory already exists without a matching recoverable frozen manifest: %s", runDirectory)
		}
		// The deterministic materializer below adopts only this manifest's
		// immutable input identities; it still recreates and verifies the
		// exact object bytes before the recovered Run can be committed.
		_ = recovered
	}
	var recoveredInputs []runManifestManagedInput
	if !createdRunDirectory {
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			return store.WorkflowRun{}, err
		}
		var recovered runManifest
		if err := decodeStrictJSON(string(raw), &recovered); err != nil || recovered.Inputs == nil {
			return store.WorkflowRun{}, fmt.Errorf("decode recoverable run manifest: %w", err)
		}
		recoveredInputs = append([]runManifestManagedInput(nil), recovered.Inputs.ManagedInputs...)
	}
	finalSpecification, managedInputs, err := service.prepareManagedInitialRunInputs(ctx, runID, *revision, request.ExecutionSpec, recoveredInputs)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	finalRequest := request
	finalRequest.ExecutionSpec = finalSpecification
	finalRequest.ExecutionSpecFingerprint = ""
	finalSpecificationCanonical, finalSpecificationFingerprint, err := service.validateRunExecutionSpec(ctx, finalRequest)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if !createdRunDirectory {
		storedManifest, err := readRecoverableRunManifest(manifestPath, runID, request, resolved, profileCanonical, requestedSpecificationFingerprint, finalSpecificationCanonical, finalSpecificationFingerprint, initialExecutionPlan, catalogReceipt)
		if err != nil || !storedManifest {
			if err == nil {
				err = fmt.Errorf("run directory already exists without a recoverable frozen manifest: %s", runDirectory)
			}
			return store.WorkflowRun{}, err
		}
	}
	committed := false
	defer func() {
		if !committed && createdRunDirectory {
			_ = os.RemoveAll(runDirectory)
		}
	}()
	manifest := runManifest{
		Format:               "harbor.workflow-run-manifest.v2",
		RunID:                runID,
		SubjectKind:          store.WorkflowRunSubjectTaskRevision,
		SubjectID:            request.TaskID,
		SubjectRevisionID:    request.RevisionID,
		SubjectDigest:        revision.TaskDigest,
		TaskID:               request.TaskID,
		Revision:             request.RevisionID,
		Resolved:             resolved.Clone(),
		InitialExecutionPlan: initialExecutionPlan.Clone(),
		Inputs: &runManifestInputs{
			Format:                            runManifestInputsFormat,
			BundleID:                          strings.TrimSpace(request.InputBundleID),
			ProfileFingerprint:                resolved.ExecutionProfileFingerprint,
			RequestedExecutionSpecFingerprint: requestedSpecificationFingerprint,
			ExecutionSpecFingerprint:          finalSpecificationFingerprint,
			ManagedInputs:                     append([]runManifestManagedInput(nil), managedInputs...),
		},
		ExecutionSpec:            append(json.RawMessage(nil), finalSpecificationCanonical...),
		DeploymentCatalogReceipt: append(json.RawMessage(nil), catalogReceipt...),
		Created:                  service.core.now().UTC(),
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("encode run manifest: %w", err)
	}
	initialInputs, err := initialManagedRunInputArtifactRequests(runID, request.TaskID, request.RevisionID, revision.TaskDigest, request.Actor, managedInputs)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("prepare initial managed run inputs: %w", err)
	}
	dispatch, _, err := initialWorkflowRunDispatch(runID, string(resolved.DefinitionFingerprint), manifest)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if createdRunDirectory {
		if err := writeNewBytes(filepath.Join(runDirectory, runExecutionProfileFileName), profileCanonical); err != nil {
			return store.WorkflowRun{}, fmt.Errorf("write frozen execution profile: %w", err)
		}
		if err := writeNewBytes(filepath.Join(runDirectory, runExecutionSpecFileName), finalSpecificationCanonical); err != nil {
			return store.WorkflowRun{}, fmt.Errorf("write frozen execution specification: %w", err)
		}
		if len(catalogReceipt) != 0 {
			if err := writeNewBytes(filepath.Join(runDirectory, deploymentCatalogReceiptFileName), catalogReceipt); err != nil {
				return store.WorkflowRun{}, fmt.Errorf("write frozen deployment catalog receipt: %w", err)
			}
		}
		if err := writeNewJSON(manifestPath, manifest); err != nil {
			return store.WorkflowRun{}, fmt.Errorf("write run manifest: %w", err)
		}
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
		InitialInputArtifacts:   initialInputs,
		Dispatch:                &dispatch,
	})
	if err != nil {
		if errors.Is(err, store.ErrIdentityCollision) {
			if existing, lookupErr := service.core.store.GetWorkflowRun(ctx, runID); lookupErr == nil && existing != nil {
				if validateErr := service.validateReplayedWorkflowRun(*existing, request, resolved, profileCanonical, requestedSpecificationFingerprint, finalSpecificationCanonical, finalSpecificationFingerprint, initialExecutionPlan, catalogReceipt); validateErr == nil {
					existingManifest, manifestErr := decodeRunManifest(*existing)
					if manifestErr == nil && service.ensureRunInputArtifacts(ctx, *existing, existingManifest) == nil && service.ensureInitialWorkflowRunDispatch(ctx, *existing, existingManifest) == nil {
						return *existing, nil
					}
				}
			}
		}
		return store.WorkflowRun{}, err
	}
	committed = true
	return run, nil
}

// resolveFrozenRunTemplate resolves the single closed workflow template that
// both explicit Run inputs claim to use. Profile and execution specification
// are independently caller-controlled files before StartRun freezes them, so
// accepting one of them while silently compiling the other against the other
// would execute a different DAG than the one it sealed.
//
// There is deliberately no default or "current template" fallback here.  The
// closed registry owns availability, and both references must be present,
// registered, and byte-for-byte equal before any managed Run directory or
// durable work can be created.
func resolveFrozenRunTemplate(profile workflowadapter.ExecutionProfile, specification workflowadapter.RunExecutionSpec) (workflowadapter.WorkflowTemplate, error) {
	profileTemplate, err := workflowadapter.ResolveWorkflowTemplate(profile.Template)
	if err != nil {
		return workflowadapter.WorkflowTemplate{}, fmt.Errorf("resolve frozen execution profile template: %w", err)
	}
	if _, err := workflowadapter.ResolveWorkflowTemplate(specification.Template); err != nil {
		return workflowadapter.WorkflowTemplate{}, fmt.Errorf("resolve frozen execution specification template: %w", err)
	}
	if !profile.Template.Equal(specification.Template) {
		return workflowadapter.WorkflowTemplate{}, fmt.Errorf("frozen execution profile template %s@%s does not match execution specification template %s@%s", profile.Template.ID, profile.Template.Version, specification.Template.ID, specification.Template.Version)
	}
	return profileTemplate, nil
}

func readRecoverableRunManifest(path, runID string, request StartRunRequest, resolved workflowadapter.ResolvedWorkflow, profileCanonical []byte, requestedSpecificationFingerprint workflowkit.Fingerprint, finalSpecificationCanonical []byte, finalSpecificationFingerprint workflowkit.Fingerprint, initialExecutionPlan workflowkit.ExecutionPlan, catalogReceipt []byte) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read existing run manifest: %w", err)
	}
	var manifest runManifest
	if err := decodeStrictJSON(string(raw), &manifest); err != nil {
		return false, fmt.Errorf("decode existing run manifest: %w", err)
	}
	if manifest.Format != "harbor.workflow-run-manifest.v2" || manifest.RunID != runID || manifest.TaskID != request.TaskID || manifest.Revision != request.RevisionID ||
		manifest.Resolved.TemplateID != resolved.TemplateID || manifest.Resolved.TemplateVersion != resolved.TemplateVersion ||
		manifest.Resolved.ExecutionProfileFingerprint != resolved.ExecutionProfileFingerprint || manifest.Resolved.DefinitionFingerprint != resolved.DefinitionFingerprint ||
		!manifestMatchesExecutionSpec(manifest, request, requestedSpecificationFingerprint, finalSpecificationCanonical, finalSpecificationFingerprint) ||
		!manifestMatchesInitialExecutionPlan(manifest, resolved.Descriptor, initialExecutionPlan) ||
		!manifestMatchesDeploymentCatalogReceipt(manifest, catalogReceipt) {
		return false, fmt.Errorf("existing run manifest does not match the requested frozen profile")
	}
	if len(catalogReceipt) != 0 {
		receiptPath := filepath.Join(filepath.Dir(path), deploymentCatalogReceiptFileName)
		receiptRaw, receiptErr := readManagedRunReceiptFile(receiptPath)
		if receiptErr != nil {
			return false, receiptErr
		}
		if !bytes.Equal(receiptRaw, catalogReceipt) {
			return false, fmt.Errorf("existing managed deployment catalog receipt does not match the requested frozen profile")
		}
	}
	profileRaw, profileErr := readManagedRunExecutionInputFile(filepath.Join(filepath.Dir(path), runExecutionProfileFileName), "execution profile")
	if profileErr != nil {
		return false, profileErr
	}
	if !bytes.Equal(profileRaw, profileCanonical) {
		return false, fmt.Errorf("existing managed execution profile does not match the requested frozen profile")
	}
	specificationRaw, specificationErr := readManagedRunExecutionInputFile(filepath.Join(filepath.Dir(path), runExecutionSpecFileName), "execution specification")
	if specificationErr != nil {
		return false, specificationErr
	}
	if !bytes.Equal(specificationRaw, finalSpecificationCanonical) {
		return false, fmt.Errorf("existing managed execution specification does not match the requested frozen profile")
	}
	return true, nil
}

func (service *RunService) validateReplayedWorkflowRun(run store.WorkflowRun, request StartRunRequest, resolved workflowadapter.ResolvedWorkflow, profileCanonical []byte, requestedSpecificationFingerprint workflowkit.Fingerprint, finalSpecificationCanonical []byte, finalSpecificationFingerprint workflowkit.Fingerprint, initialExecutionPlan workflowkit.ExecutionPlan, catalogReceipt []byte) error {
	if run.TaskID != request.TaskID || run.RevisionID != request.RevisionID || run.WorkflowTemplateID != resolved.TemplateID ||
		run.WorkflowTemplateVersion != resolved.TemplateVersion || run.ResolvedProfileHash != string(resolved.ExecutionProfileFingerprint) ||
		run.DefinitionHash != string(resolved.DefinitionFingerprint) || run.ParentRunID != request.ParentRunID ||
		run.Trigger != request.Trigger || run.ExecutionEpoch != request.ExecutionEpoch {
		return fmt.Errorf("%w: workflow run %s does not match requested immutable definition", store.ErrIdempotencyConflict, run.ID)
	}
	var manifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil ||
		!manifestMatchesExecutionSpec(manifest, request, requestedSpecificationFingerprint, finalSpecificationCanonical, finalSpecificationFingerprint) ||
		!manifestMatchesInitialExecutionPlan(manifest, resolved.Descriptor, initialExecutionPlan) ||
		!manifestMatchesDeploymentCatalogReceipt(manifest, catalogReceipt) {
		return fmt.Errorf("%w: workflow run %s execution specification", store.ErrIdempotencyConflict, run.ID)
	}
	if service == nil || service.core == nil {
		return fmt.Errorf("run service is not configured")
	}
	profile, _, err := service.core.verifyRunManagedExecutionInputs(context.Background(), run)
	if err != nil {
		return fmt.Errorf("%w: workflow run %s managed execution inputs: %w", store.ErrIdempotencyConflict, run.ID, err)
	}
	canonicalProfile, err := profile.CanonicalJSON()
	if err != nil || !bytes.Equal(canonicalProfile, profileCanonical) {
		return fmt.Errorf("%w: workflow run %s execution profile", store.ErrIdempotencyConflict, run.ID)
	}
	if err := service.core.verifyRunDeploymentCatalogReceipt(run); err != nil {
		return fmt.Errorf("%w: workflow run %s deployment catalog receipt: %w", store.ErrIdempotencyConflict, run.ID, err)
	}
	return nil
}

func (service *RunService) validateRunExecutionSpec(ctx context.Context, request StartRunRequest) ([]byte, workflowkit.Fingerprint, error) {
	canonical, err := request.ExecutionSpec.CanonicalJSON()
	if err != nil {
		return nil, "", fmt.Errorf("validate explicit execution specification: %w", err)
	}
	fingerprint, err := request.ExecutionSpec.Fingerprint()
	if err != nil {
		return nil, "", fmt.Errorf("fingerprint explicit execution specification: %w", err)
	}
	revision, err := service.core.store.GetTaskRevision(ctx, request.RevisionID)
	if err != nil {
		return nil, "", err
	}
	if revision == nil || revision.TaskID != request.TaskID {
		return nil, "", fmt.Errorf("%w: TaskRevision %s", ErrLifecycleNotFound, request.RevisionID)
	}
	selection := request.ExecutionSpec.Selection
	if selection.TaskID != request.TaskID || selection.RevisionID != request.RevisionID || string(selection.RevisionDigest) != revision.TaskDigest {
		return nil, "", fmt.Errorf("%w: execution specification selection does not match TaskRevision", store.ErrOptimisticLock)
	}
	if err := validateRunExecutionSpecOperationResolver(request.ExecutionSpec, service.core.operationResolver); err != nil {
		return nil, "", err
	}
	if err := service.core.validateDeploymentCatalogExecutionSpec(request.ExecutionSpec); err != nil {
		return nil, "", err
	}
	return canonical, fingerprint, nil
}

// validateRunParent rejects any child Run rooted in a pre-materialization
// authoring session. Standard Authoring 3.0 materializes a task but never
// dispatches a child Run from that session.
func (service *RunService) validateRunParent(ctx context.Context, request StartRunRequest) error {
	if service == nil || service.core == nil || service.core.store == nil {
		return fmt.Errorf("run service is not configured")
	}
	parentID := strings.TrimSpace(request.ParentRunID)
	if parentID == "" {
		return nil
	}
	parent, err := service.core.store.GetWorkflowRun(ctx, parentID)
	if err != nil {
		return err
	}
	if parent == nil {
		return fmt.Errorf("%w: parent workflow Run %s", ErrLifecycleNotFound, parentID)
	}
	if parent.SubjectKind == store.WorkflowRunSubjectAuthoringSession {
		return fmt.Errorf("Standard authoring 3.0 does not permit an automatic child Run")
	}
	return nil
}

func validateRunExecutionSpecOperationResolver(specification workflowadapter.RunExecutionSpec, resolver workflowadapter.StageOperationResolver) error {
	if resolver == nil {
		return fmt.Errorf("validate explicit execution specification operations: stage operation resolver is not configured")
	}
	if err := specification.ValidateWithOperationResolver(resolver); err != nil {
		return fmt.Errorf("validate explicit execution specification operations: %w", err)
	}
	return nil
}

func manifestMatchesExecutionSpec(manifest runManifest, request StartRunRequest, requestedFingerprint workflowkit.Fingerprint, expectedCanonical []byte, finalFingerprint workflowkit.Fingerprint) bool {
	if manifest.Inputs == nil || manifest.Inputs.Format != runManifestInputsFormat ||
		manifest.Inputs.ProfileFingerprint != manifest.Resolved.ExecutionProfileFingerprint ||
		manifest.Inputs.RequestedExecutionSpecFingerprint != requestedFingerprint ||
		manifest.Inputs.ExecutionSpecFingerprint != finalFingerprint || len(manifest.ExecutionSpec) == 0 {
		return false
	}
	if request.InputBundleID != "" && manifest.Inputs.BundleID != request.InputBundleID {
		return false
	}
	specification, err := workflowadapter.ParseRunExecutionSpecJSON(manifest.ExecutionSpec)
	if err != nil {
		return false
	}
	canonical, err := specification.CanonicalJSON()
	if err != nil || !bytes.Equal(canonical, expectedCanonical) {
		return false
	}
	storedFingerprint, err := specification.Fingerprint()
	if err != nil || storedFingerprint != finalFingerprint {
		return false
	}
	return specification.Selection.TaskID == request.TaskID && specification.Selection.RevisionID == request.RevisionID
}

func manifestMatchesInitialExecutionPlan(manifest runManifest, workflow workflowkit.WorkflowDescriptor, expected workflowkit.ExecutionPlan) bool {
	if err := manifest.InitialExecutionPlan.Validate(workflow); err != nil {
		return false
	}
	return manifest.InitialExecutionPlan.Fingerprint == expected.Fingerprint
}

func manifestMatchesDeploymentCatalogReceipt(manifest runManifest, expected []byte) bool {
	canonical, err := canonicalManifestDeploymentCatalogReceipt(manifest)
	if err != nil {
		return false
	}
	return bytes.Equal(canonical, expected)
}

func readManagedRunReceiptFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect managed deployment catalog receipt: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("managed deployment catalog receipt is not a regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read managed deployment catalog receipt: %w", err)
	}
	return raw, nil
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
