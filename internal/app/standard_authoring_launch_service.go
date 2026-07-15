package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringSourceRepositoryURL and StandardAuthoringSourceCommit
	// are the approved, immutable source coordinate for the first Standard
	// authoring flow. They deliberately are not CLI/API fields: changing the
	// subject requires a reviewed source-contract revision rather than a
	// caller-selected repository, branch, or local path.
	StandardAuthoringSourceRepositoryURL = "https://github.com/tower-rs/tower-http.git"
	StandardAuthoringSourceCommit        = "f066e10ebc07ea9050a2ce4576315abfa568edf4"

	// StandardAuthoringSourceSnapshotSchemaVersion identifies the safe,
	// content-addressed Git archive produced by the controlled source capturer.
	// The bytes are stored only in the managed object store; neither this value
	// nor AuthoringSource carries a caller filesystem path.
	StandardAuthoringSourceSnapshotSchemaVersion = "harbor.source-snapshot.v1"

	standardAuthoringLaunchAction                 LifecycleMutationAction = "authoring.start"
	standardAuthoringLaunchSessionManifestFormat                          = "harbor.standard-authoring-session.v1"
	standardAuthoringLaunchSessionManifestVersion                         = "1"
	standardAuthoringLaunchTrigger                                        = "authoring.standard.create"
	standardAuthoringLaunchIdentityDomain                                 = "harbor.standard-authoring.launch.identity.v1"
	standardAuthoringLaunchDefinitionDomain                               = "harbor.standard-authoring.launch-definition.v1"
)

var (
	// ErrStandardAuthoringLaunchUnavailable is intentionally stable and does
	// not expose a deployment path, provider configuration, or credential. A
	// normal lifecycle composition remains usable for read/control operations
	// when the Standard source capture capability has not been installed.
	ErrStandardAuthoringLaunchUnavailable = errors.New("standard authoring launch is not configured")
)

// StandardAuthoringSourceSnapshot is an immutable archive capture returned
// by a composition-owned capturer. RepositoryURL and CommitSHA must be the
// exact constants above; Content is validated as a bounded, safe archive
// before it enters the content-addressed managed object store.
type StandardAuthoringSourceSnapshot struct {
	RepositoryURL string
	CommitSHA     string
	SchemaVersion string
	Content       []byte
}

// StandardAuthoringSourceCapturer is the only boundary allowed to acquire
// the fixed public source before an AuthoringSource exists. It accepts no
// repository, ref, checkout directory, model, or secret input from a caller.
// The concrete Git implementation is deliberately separate from the generic
// workflow engine and is injected by deployment composition.
type StandardAuthoringSourceCapturer interface {
	CaptureStandardAuthoringSource(context.Context) (StandardAuthoringSourceSnapshot, error)
}

// StandardAuthoringRunDefinitionSubject is the closed identity supplied to a
// deployment-owned definition provider. The provider may use it only to bind
// its static catalog selections to this source/session subject; it must not
// derive a repository, profile, model, prompt, or secret from CLI input.
type StandardAuthoringRunDefinitionSubject struct {
	SourceID             string
	AuthoringSessionID   string
	SourceSnapshotDigest workflowkit.SubjectDigest
	SourceSnapshotSchema string
	TargetTaskID         string
	RepositoryURL        string
	CommitSHA            string
}

// StandardAuthoringRunDefinition is the complete frozen execution definition
// for one source/session launch. The profile and specification are produced by
// a deployment-owned provider, never supplied through CLI flags. Optional
// catalog receipt/lock fields are verified against the lifecycle composition
// before they are persisted with the Run.
type StandardAuthoringRunDefinition struct {
	Profile                       workflowadapter.ExecutionProfile
	ExecutionSpec                 workflowadapter.RunExecutionSpec
	DeploymentCatalogReceipt      []byte
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity
}

// StandardAuthoringRunDefinitionProvider owns the deployment-specific
// profile/spec construction. Keeping this port narrow lets the app service
// remain independent of a provider implementation while refusing any missing
// definition instead of guessing a profile or a stage binding.
type StandardAuthoringRunDefinitionProvider interface {
	StandardAuthoringRunDefinition(context.Context, StandardAuthoringRunDefinitionSubject) (StandardAuthoringRunDefinition, error)
}

// StandardAuthoringLaunchCommand is the one public mutation that creates the
// pre-materialization source/session workflow. The Task is intentionally only
// a revision-free draft ownership record; it is not used as a workflow subject
// and no TaskRevision is manufactured here.
type StandardAuthoringLaunchCommand struct {
	LifecycleMutationCommandBase
	Slug         string
	Title        string
	MetadataJSON string
}

// StandardAuthoringLaunchService composes source capture, immutable source
// persistence, draft Task ownership, session freezing, and generic Run
// admission. It owns no provider side effect beyond source capture and never
// starts the later CodeEdge Phase-1 child workflow.
type StandardAuthoringLaunchService struct {
	core        *lifecycleServiceCore
	capturer    StandardAuthoringSourceCapturer
	definitions StandardAuthoringRunDefinitionProvider
}

func newStandardAuthoringLaunchService(core *lifecycleServiceCore, capturer StandardAuthoringSourceCapturer, definitions StandardAuthoringRunDefinitionProvider) *StandardAuthoringLaunchService {
	return &StandardAuthoringLaunchService{core: core, capturer: capturer, definitions: definitions}
}

// Available reports whether this lifecycle composition has both deployment-
// owned halves required to start the closed Standard authoring flow. It is a
// read-only capability probe for CLI/TUI projection and does not capture a
// source, create durable state, or infer missing authoring configuration.
func (service *StandardAuthoringLaunchService) Available() bool {
	return service != nil && service.core != nil && service.core.store != nil && service.core.objects != nil && service.capturer != nil && service.definitions != nil
}

// Start captures the approved Tower HTTP source exactly once for a completed
// idempotency key, then freezes the complete source/session contract and
// queues its Standard Run. The entity IDs are deterministic UUIDv7 derivatives
// of the caller-issued UUIDv7 key, making interrupted pre-operation work
// recoverable without reusing identities across entity types.
func (service *StandardAuthoringLaunchService) Start(ctx context.Context, command StandardAuthoringLaunchCommand) (LifecycleMutationReceipt, error) {
	if service == nil || service.core == nil || service.core.store == nil || service.core.objects == nil || service.capturer == nil || service.definitions == nil {
		return LifecycleMutationReceipt{}, ErrStandardAuthoringLaunchUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateStandardAuthoringLaunchCommand(command); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	mutations := &LifecycleMutationService{core: service.core}
	if receipt, replayed, err := mutations.completedOperationReplay(ctx, standardAuthoringLaunchAction, command.LifecycleMutationCommandBase); err != nil {
		return LifecycleMutationReceipt{}, err
	} else if replayed {
		return receipt, nil
	}

	metadata, err := canonicalStandardAuthoringMetadata(command.MetadataJSON)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	ids, err := standardAuthoringLaunchIdentities(command.IdempotencyKey)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}

	source, err := service.ensureAuthoringSource(ctx, ids.SourceID, command)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	task, err := service.ensureAuthoringDraftTask(ctx, ids.TaskID, command, metadata, source)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}

	subject := StandardAuthoringRunDefinitionSubject{
		SourceID:             source.ID,
		AuthoringSessionID:   ids.SessionID,
		SourceSnapshotDigest: workflowkit.SubjectDigest(source.SnapshotContentDigest),
		SourceSnapshotSchema: source.SnapshotSchemaVersion,
		TargetTaskID:         task.ID,
		RepositoryURL:        source.RepositoryURL,
		CommitSHA:            source.CommitSHA,
	}
	frozen, err := service.freezeStandardAuthoringDefinition(ctx, subject)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	sessionManifest, err := standardAuthoringSessionManifestJSON(source, task, subject.AuthoringSessionID, frozen)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	session, err := service.core.store.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		ID:                      ids.SessionID,
		SourceID:                source.ID,
		TargetTaskID:            task.ID,
		WorkflowTemplateID:      workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringWorkflowTemplateVersion,
		SessionManifestJSON:     sessionManifest,
		IdempotencyKey:          standardAuthoringLaunchChildKey(command.IdempotencyKey, "session"),
		Actor:                   command.Actor,
		Reason:                  command.Reason,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if err := verifyStandardAuthoringLaunchSession(session, source, task, frozen); err != nil {
		return LifecycleMutationReceipt{}, err
	}

	payload := standardAuthoringLaunchPayload{
		Format:                      standardAuthoringLaunchSessionManifestFormat,
		RepositoryURL:               source.RepositoryURL,
		CommitSHA:                   source.CommitSHA,
		SourceID:                    source.ID,
		AuthoringSessionID:          session.ID,
		TargetTaskID:                task.ID,
		SourceSnapshotDigest:        source.SnapshotContentDigest,
		SourceSnapshotSchemaVersion: source.SnapshotSchemaVersion,
		Slug:                        task.Slug,
		Title:                       task.Title,
		MetadataJSON:                task.MetadataJSON,
		DefinitionFingerprint:       frozen.Fingerprint,
	}
	op, replay, err := mutations.begin(ctx, standardAuthoringLaunchAction, command.LifecycleMutationCommandBase, payload, lifecycleOperationTargets{TaskID: task.ID, RunID: ids.RunID})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if op.TaskID != task.ID || op.RunID != ids.RunID || op.Actor != strings.TrimSpace(command.Actor) || op.Reason != strings.TrimSpace(command.Reason) {
		return LifecycleMutationReceipt{}, fmt.Errorf("%w: Standard authoring lifecycle operation %s", store.ErrIdempotencyConflict, op.ID)
	}

	run, err := (&RunService{core: service.core}).StartAuthoringRun(ctx, StartAuthoringRunRequest{
		ID:                            op.RunID,
		AuthoringSessionID:            session.ID,
		Profile:                       frozen.Profile,
		ExecutionSpec:                 frozen.ExecutionSpec,
		ProfileFingerprint:            frozen.ProfileFingerprint,
		ExecutionSpecFingerprint:      frozen.ExecutionSpecFingerprint,
		DeploymentCatalogReceipt:      append([]byte(nil), frozen.DeploymentCatalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(frozen.DeploymentCatalogLockIdentity),
		Trigger:                       standardAuthoringLaunchTrigger,
		Actor:                         op.Actor,
		Reason:                        op.Reason,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return mutations.complete(ctx, op, LifecycleMutationReceipt{
		Action:               standardAuthoringLaunchAction,
		TaskID:               task.ID,
		TaskVersion:          task.Version,
		RunID:                run.ID,
		RunVersion:           run.Version,
		AuthoringSourceID:    source.ID,
		AuthoringSessionID:   session.ID,
		SourceSnapshotDigest: source.SnapshotContentDigest,
		Summary:              "已捕获固定 Tower HTTP 源码并启动 Standard 创题 Run",
	})
}

type standardAuthoringLaunchIDs struct {
	SourceID  string
	TaskID    string
	SessionID string
	RunID     string
}

func standardAuthoringLaunchIdentities(idempotencyKey string) (standardAuthoringLaunchIDs, error) {
	return standardAuthoringLaunchIDs{
		SourceID:  standardAuthoringLaunchIdentity(idempotencyKey, "source"),
		TaskID:    standardAuthoringLaunchIdentity(idempotencyKey, "task"),
		SessionID: standardAuthoringLaunchIdentity(idempotencyKey, "session"),
		RunID:     standardAuthoringLaunchIdentity(idempotencyKey, "run"),
	}, nil
}

func standardAuthoringLaunchIdentity(idempotencyKey, entity string) string {
	parsed := uuid.MustParse(strings.TrimSpace(idempotencyKey))
	digest := sha256.Sum256([]byte(standardAuthoringLaunchIdentityDomain + "\x00" + entity + "\x00" + parsed.String()))
	derived := parsed
	derived[6] = 0x70 | (digest[0] & 0x0f)
	derived[7] = digest[1]
	derived[8] = 0x80 | (digest[2] & 0x3f)
	copy(derived[9:], digest[3:10])
	return derived.String()
}

func standardAuthoringLaunchChildKey(idempotencyKey, child string) string {
	return "standard-authoring-launch:" + strings.TrimSpace(idempotencyKey) + ":" + child
}

func validateStandardAuthoringLaunchCommand(command StandardAuthoringLaunchCommand) error {
	if err := store.ValidateUUIDv7(strings.TrimSpace(command.IdempotencyKey)); err != nil {
		return fmt.Errorf("Standard authoring idempotency key: %w", err)
	}
	if strings.TrimSpace(command.Actor) == "" || strings.TrimSpace(command.Reason) == "" {
		return fmt.Errorf("Standard authoring actor and reason are required")
	}
	if !command.Expected.empty() {
		return fmt.Errorf("Standard authoring creation cannot accept an existing lifecycle checkpoint")
	}
	if strings.TrimSpace(command.Slug) == "" {
		return fmt.Errorf("slug is required")
	}
	if strings.TrimSpace(command.Title) == "" {
		return fmt.Errorf("title is required")
	}
	return nil
}

func canonicalStandardAuthoringMetadata(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "{}", nil
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf("decode Standard authoring task metadata: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", fmt.Errorf("decode Standard authoring task metadata: unexpected trailing JSON")
		}
		return "", fmt.Errorf("decode Standard authoring task metadata trailing data: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("canonicalize Standard authoring task metadata: %w", err)
	}
	return string(encoded), nil
}

func (service *StandardAuthoringLaunchService) ensureAuthoringSource(ctx context.Context, sourceID string, command StandardAuthoringLaunchCommand) (store.AuthoringSource, error) {
	existing, err := service.core.store.GetAuthoringSource(ctx, sourceID)
	if err != nil {
		return store.AuthoringSource{}, err
	}
	if existing != nil {
		if existing.IdempotencyKey != standardAuthoringLaunchChildKey(command.IdempotencyKey, "source") {
			return store.AuthoringSource{}, fmt.Errorf("%w: Standard authoring source identity %s", store.ErrIdentityCollision, existing.ID)
		}
		if err := verifyStandardAuthoringLaunchSourceObject(ctx, service.core.objects, *existing); err != nil {
			return store.AuthoringSource{}, err
		}
		if err := validateStandardAuthoringLaunchSource(*existing); err != nil {
			return store.AuthoringSource{}, err
		}
		return *existing, nil
	}

	captured, err := service.capturer.CaptureStandardAuthoringSource(ctx)
	if err != nil {
		return store.AuthoringSource{}, fmt.Errorf("capture approved Standard authoring source: %w", err)
	}
	if err := validateStandardAuthoringSourceSnapshot(captured); err != nil {
		return store.AuthoringSource{}, err
	}
	object, err := service.core.objects.PutBytes(ctx, captured.Content)
	if err != nil {
		return store.AuthoringSource{}, fmt.Errorf("store Standard authoring source snapshot: %w", err)
	}
	source, err := service.core.store.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		ID:                    sourceID,
		RepositoryURL:         captured.RepositoryURL,
		CommitSHA:             captured.CommitSHA,
		SnapshotArtifactRef:   string(object.Digest),
		SnapshotContentDigest: string(object.Digest),
		SnapshotSchemaVersion: captured.SchemaVersion,
		IdempotencyKey:        standardAuthoringLaunchChildKey(command.IdempotencyKey, "source"),
		Actor:                 command.Actor,
		Reason:                command.Reason,
	})
	if err != nil {
		return store.AuthoringSource{}, err
	}
	if err := validateStandardAuthoringLaunchSource(source); err != nil {
		return store.AuthoringSource{}, err
	}
	return source, nil
}

func validateStandardAuthoringSourceSnapshot(snapshot StandardAuthoringSourceSnapshot) error {
	if snapshot.RepositoryURL != StandardAuthoringSourceRepositoryURL || snapshot.CommitSHA != StandardAuthoringSourceCommit || snapshot.SchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		return fmt.Errorf("Standard authoring source capture does not match the approved Tower HTTP source identity")
	}
	if err := validateStandardAuthoringSourceArchive(snapshot.Content); err != nil {
		return fmt.Errorf("validate Standard authoring source archive: %w", err)
	}
	return nil
}

func validateStandardAuthoringLaunchSource(source store.AuthoringSource) error {
	if source.RepositoryURL != StandardAuthoringSourceRepositoryURL || source.CommitSHA != StandardAuthoringSourceCommit ||
		source.SnapshotArtifactRef != source.SnapshotContentDigest || source.SnapshotSchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		return fmt.Errorf("%w: persisted Standard authoring source does not match the approved source contract", store.ErrIdempotencyConflict)
	}
	return nil
}

func verifyStandardAuthoringLaunchSourceObject(ctx context.Context, objects *workflowruntime.ArtifactObjectStore, source store.AuthoringSource) error {
	if objects == nil {
		return ErrStandardAuthoringLaunchUnavailable
	}
	file, err := objects.Open(ctx, workflowruntime.ObjectRef{Digest: workflowkit.Fingerprint(source.SnapshotContentDigest)})
	if err != nil {
		return fmt.Errorf("open persisted Standard authoring source object: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("read persisted Standard authoring source object: %w", err)
	}
	if actual := "sha256:" + hex.EncodeToString(hash.Sum(nil)); actual != source.SnapshotContentDigest {
		return fmt.Errorf("persisted Standard authoring source object digest differs from AuthoringSource")
	}
	return nil
}

func (service *StandardAuthoringLaunchService) ensureAuthoringDraftTask(ctx context.Context, taskID string, command StandardAuthoringLaunchCommand, metadata string, source store.AuthoringSource) (store.TaskV2, error) {
	existing, err := service.core.store.GetTaskV2(ctx, taskID)
	if err != nil {
		return store.TaskV2{}, err
	}
	if existing != nil {
		if err := validateStandardAuthoringLaunchTask(*existing, command, metadata, source); err != nil {
			return store.TaskV2{}, err
		}
		return *existing, nil
	}
	created, err := service.core.store.CreateTaskV2(ctx, store.CreateTaskV2Request{
		ID:             taskID,
		Slug:           command.Slug,
		Title:          command.Title,
		MetadataJSON:   metadata,
		SourceRepo:     source.RepositoryURL,
		SourceCommit:   source.CommitSHA,
		LifecycleState: store.TaskLifecycleDraft,
		Actor:          command.Actor,
		Reason:         command.Reason,
	})
	if err != nil {
		return store.TaskV2{}, err
	}
	if err := validateStandardAuthoringLaunchTask(created, command, metadata, source); err != nil {
		return store.TaskV2{}, err
	}
	return created, nil
}

func validateStandardAuthoringLaunchTask(task store.TaskV2, command StandardAuthoringLaunchCommand, metadata string, source store.AuthoringSource) error {
	if task.Slug != strings.TrimSpace(command.Slug) || task.Title != strings.TrimSpace(command.Title) || task.MetadataJSON != metadata ||
		task.SourceRepo != source.RepositoryURL || task.SourceCommit != source.CommitSHA || task.LifecycleState != store.TaskLifecycleDraft || task.CurrentRevisionID != "" {
		return fmt.Errorf("%w: Standard authoring draft Task %s", store.ErrIdempotencyConflict, task.ID)
	}
	return nil
}

type standardAuthoringFrozenDefinition struct {
	Profile                       workflowadapter.ExecutionProfile
	ExecutionSpec                 workflowadapter.RunExecutionSpec
	ProfileFingerprint            workflowkit.Fingerprint
	ExecutionSpecFingerprint      workflowkit.Fingerprint
	DeploymentCatalogReceipt      []byte
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity
	Fingerprint                   workflowkit.Fingerprint
}

func (service *StandardAuthoringLaunchService) freezeStandardAuthoringDefinition(ctx context.Context, subject StandardAuthoringRunDefinitionSubject) (standardAuthoringFrozenDefinition, error) {
	definition, err := service.definitions.StandardAuthoringRunDefinition(ctx, subject)
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("resolve Standard authoring deployment definition: %w", err)
	}
	if !definition.Profile.Template.Equal(workflowadapter.StandardAuthoringTemplateReference()) || !definition.ExecutionSpec.Template.Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("Standard authoring deployment definition has the wrong workflow template")
	}
	selection := definition.ExecutionSpec.Selection
	if selection.Kind != workflowadapter.RunSelectionAuthoringSession || selection.AuthoringSourceID != subject.SourceID || selection.AuthoringSessionID != subject.AuthoringSessionID || selection.AuthoringSourceDigest != subject.SourceSnapshotDigest {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("Standard authoring deployment definition does not bind the frozen source/session subject")
	}
	if err := definition.ExecutionSpec.ValidateWithOperationResolver(service.core.operationResolver); err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("validate Standard authoring deployment execution specification: %w", err)
	}
	if err := service.core.validateDeploymentCatalogExecutionSpec(definition.ExecutionSpec); err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("validate Standard authoring deployment catalog: %w", err)
	}
	profileCanonical, err := definition.Profile.CanonicalJSON()
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("canonicalize Standard authoring profile: %w", err)
	}
	profileFingerprint, err := definition.Profile.Fingerprint()
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("fingerprint Standard authoring profile: %w", err)
	}
	specificationCanonical, err := definition.ExecutionSpec.CanonicalJSON()
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("canonicalize Standard authoring execution specification: %w", err)
	}
	specificationFingerprint, err := definition.ExecutionSpec.Fingerprint()
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("fingerprint Standard authoring execution specification: %w", err)
	}
	catalogReceipt, err := service.core.resolveStartRunDeploymentCatalogReceipt(definition.ExecutionSpec.Template, definition.DeploymentCatalogReceipt)
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("freeze Standard authoring deployment catalog receipt: %w", err)
	}
	lockIdentity, err := service.core.resolveStartRunDeploymentCatalogLockIdentity(definition.ExecutionSpec.Template, definition.DeploymentCatalogLockIdentity)
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("freeze Standard authoring deployment catalog lock identity: %w", err)
	}
	definitionFingerprint, err := standardAuthoringDefinitionFingerprint(profileCanonical, specificationCanonical, catalogReceipt, lockIdentity)
	if err != nil {
		return standardAuthoringFrozenDefinition{}, err
	}
	return standardAuthoringFrozenDefinition{
		Profile:                       definition.Profile.Clone(),
		ExecutionSpec:                 definition.ExecutionSpec.Clone(),
		ProfileFingerprint:            profileFingerprint,
		ExecutionSpecFingerprint:      specificationFingerprint,
		DeploymentCatalogReceipt:      append([]byte(nil), catalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(lockIdentity),
		Fingerprint:                   definitionFingerprint,
	}, nil
}

func standardAuthoringDefinitionFingerprint(profile, specification, receipt []byte, lock *stageprovider.DeploymentOperationCatalogLockIdentity) (workflowkit.Fingerprint, error) {
	lockJSON := []byte("null")
	if lock != nil {
		var err error
		lockJSON, err = canonicalDeploymentCatalogLockIdentity(*lock)
		if err != nil {
			return "", err
		}
	}
	return workflowkit.FingerprintParts(standardAuthoringLaunchDefinitionDomain, []workflowkit.FingerprintPart{
		{Name: "deployment_catalog_lock_identity", Value: lockJSON},
		{Name: "deployment_catalog_receipt", Value: append([]byte(nil), receipt...)},
		{Name: "execution_profile", Value: append([]byte(nil), profile...)},
		{Name: "execution_specification", Value: append([]byte(nil), specification...)},
	})
}

type standardAuthoringSessionManifest struct {
	Format                        string                                                `json:"format"`
	Version                       string                                                `json:"version"`
	RepositoryURL                 string                                                `json:"repository_url"`
	CommitSHA                     string                                                `json:"commit_sha"`
	SourceID                      string                                                `json:"source_id"`
	AuthoringSessionID            string                                                `json:"authoring_session_id"`
	TargetTaskID                  string                                                `json:"target_task_id"`
	SourceSnapshotDigest          string                                                `json:"source_snapshot_digest"`
	SourceSnapshotSchemaVersion   string                                                `json:"source_snapshot_schema_version"`
	ProfileFingerprint            workflowkit.Fingerprint                               `json:"profile_fingerprint"`
	ExecutionSpecFingerprint      workflowkit.Fingerprint                               `json:"execution_spec_fingerprint"`
	DefinitionFingerprint         workflowkit.Fingerprint                               `json:"definition_fingerprint"`
	DeploymentCatalogReceipt      json.RawMessage                                       `json:"deployment_catalog_receipt,omitempty"`
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity `json:"deployment_catalog_lock_identity,omitempty"`
}

func standardAuthoringSessionManifestJSON(source store.AuthoringSource, task store.TaskV2, sessionID string, frozen standardAuthoringFrozenDefinition) (string, error) {
	manifest := standardAuthoringSessionManifest{
		Format:                        standardAuthoringLaunchSessionManifestFormat,
		Version:                       standardAuthoringLaunchSessionManifestVersion,
		RepositoryURL:                 source.RepositoryURL,
		CommitSHA:                     source.CommitSHA,
		SourceID:                      source.ID,
		AuthoringSessionID:            sessionID,
		TargetTaskID:                  task.ID,
		SourceSnapshotDigest:          source.SnapshotContentDigest,
		SourceSnapshotSchemaVersion:   source.SnapshotSchemaVersion,
		ProfileFingerprint:            frozen.ProfileFingerprint,
		ExecutionSpecFingerprint:      frozen.ExecutionSpecFingerprint,
		DefinitionFingerprint:         frozen.Fingerprint,
		DeploymentCatalogReceipt:      append(json.RawMessage(nil), frozen.DeploymentCatalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(frozen.DeploymentCatalogLockIdentity),
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func verifyStandardAuthoringLaunchSession(session store.AuthoringSession, source store.AuthoringSource, task store.TaskV2, frozen standardAuthoringFrozenDefinition) error {
	if session.SourceID != source.ID || session.TargetTaskID != task.ID || session.WorkflowTemplateID != workflowadapter.StandardAuthoringWorkflowTemplateID || session.WorkflowTemplateVersion != workflowadapter.StandardAuthoringWorkflowTemplateVersion {
		return fmt.Errorf("%w: persisted Standard authoring session binding", store.ErrIdempotencyConflict)
	}
	var manifest standardAuthoringSessionManifest
	if err := decodeStrictJSON(session.SessionManifestJSON, &manifest); err != nil {
		return fmt.Errorf("decode persisted Standard authoring session manifest: %w", err)
	}
	if manifest.Format != standardAuthoringLaunchSessionManifestFormat || manifest.Version != standardAuthoringLaunchSessionManifestVersion ||
		manifest.RepositoryURL != source.RepositoryURL || manifest.CommitSHA != source.CommitSHA || manifest.SourceID != source.ID ||
		manifest.AuthoringSessionID != session.ID || manifest.TargetTaskID != task.ID || manifest.SourceSnapshotDigest != source.SnapshotContentDigest ||
		manifest.SourceSnapshotSchemaVersion != source.SnapshotSchemaVersion || manifest.ProfileFingerprint != frozen.ProfileFingerprint ||
		manifest.ExecutionSpecFingerprint != frozen.ExecutionSpecFingerprint || manifest.DefinitionFingerprint != frozen.Fingerprint ||
		string(manifest.DeploymentCatalogReceipt) != string(frozen.DeploymentCatalogReceipt) ||
		!sameDeploymentCatalogLockIdentity(manifest.DeploymentCatalogLockIdentity, frozen.DeploymentCatalogLockIdentity) {
		return fmt.Errorf("%w: persisted Standard authoring session definition", store.ErrIdempotencyConflict)
	}
	return nil
}

func sameDeploymentCatalogLockIdentity(left, right *stageprovider.DeploymentOperationCatalogLockIdentity) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

type standardAuthoringLaunchPayload struct {
	Format                      string                  `json:"format"`
	RepositoryURL               string                  `json:"repository_url"`
	CommitSHA                   string                  `json:"commit_sha"`
	SourceID                    string                  `json:"source_id"`
	AuthoringSessionID          string                  `json:"authoring_session_id"`
	TargetTaskID                string                  `json:"target_task_id"`
	SourceSnapshotDigest        string                  `json:"source_snapshot_digest"`
	SourceSnapshotSchemaVersion string                  `json:"source_snapshot_schema_version"`
	Slug                        string                  `json:"slug"`
	Title                       string                  `json:"title"`
	MetadataJSON                string                  `json:"metadata_json"`
	DefinitionFingerprint       workflowkit.Fingerprint `json:"definition_fingerprint"`
}

// CatalogStandardAuthoringRunDefinitionProvider derives the full source-session
// execution spec from a static deployment catalog and an already explicit
// profile. It is intentionally not a generic catalog compiler: it only emits
// harbor.standard-authoring@1.0.0 and has no caller-selectable operation,
// checkout, runtime, secret, or model fields.
type CatalogStandardAuthoringRunDefinitionProvider struct {
	catalog *stageprovider.DeploymentOperationCatalogResolver
	profile workflowadapter.ExecutionProfile
}

func NewCatalogStandardAuthoringRunDefinitionProvider(catalog *stageprovider.DeploymentOperationCatalogResolver, profile workflowadapter.ExecutionProfile) (*CatalogStandardAuthoringRunDefinitionProvider, error) {
	if catalog == nil || !catalog.Template().Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return nil, fmt.Errorf("Standard authoring deployment catalog is required")
	}
	if !profile.Template.Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return nil, fmt.Errorf("Standard authoring execution profile is required")
	}
	if err := profile.Validate(); err != nil {
		return nil, fmt.Errorf("validate Standard authoring execution profile: %w", err)
	}
	return &CatalogStandardAuthoringRunDefinitionProvider{catalog: catalog, profile: profile.Clone()}, nil
}

func (provider *CatalogStandardAuthoringRunDefinitionProvider) StandardAuthoringRunDefinition(_ context.Context, subject StandardAuthoringRunDefinitionSubject) (StandardAuthoringRunDefinition, error) {
	if provider == nil || provider.catalog == nil || !provider.catalog.Template().Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return StandardAuthoringRunDefinition{}, ErrStandardAuthoringLaunchUnavailable
	}
	if err := store.ValidateUUIDv7(subject.SourceID); err != nil {
		return StandardAuthoringRunDefinition{}, fmt.Errorf("Standard authoring definition source ID: %w", err)
	}
	if err := store.ValidateUUIDv7(subject.AuthoringSessionID); err != nil {
		return StandardAuthoringRunDefinition{}, fmt.Errorf("Standard authoring definition session ID: %w", err)
	}
	if err := store.ValidateUUIDv7(subject.TargetTaskID); err != nil {
		return StandardAuthoringRunDefinition{}, fmt.Errorf("Standard authoring definition Task ID: %w", err)
	}
	if err := subject.SourceSnapshotDigest.Validate(); err != nil {
		return StandardAuthoringRunDefinition{}, fmt.Errorf("Standard authoring definition source digest: %w", err)
	}
	if subject.RepositoryURL != StandardAuthoringSourceRepositoryURL || subject.CommitSHA != StandardAuthoringSourceCommit || subject.SourceSnapshotSchema != StandardAuthoringSourceSnapshotSchemaVersion {
		return StandardAuthoringRunDefinition{}, fmt.Errorf("Standard authoring definition source identity is not approved")
	}
	catalog := provider.catalog.Catalog()
	specification, err := buildCatalogStandardAuthoringExecutionSpec(catalog, subject)
	if err != nil {
		return StandardAuthoringRunDefinition{}, err
	}
	return StandardAuthoringRunDefinition{Profile: provider.profile.Clone(), ExecutionSpec: specification}, nil
}

func buildCatalogStandardAuthoringExecutionSpec(catalog stageprovider.DeploymentOperationCatalog, subject StandardAuthoringRunDefinitionSubject) (workflowadapter.RunExecutionSpec, error) {
	if !catalog.Template.Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("catalog does not bind Standard authoring")
	}
	specification := workflowadapter.RunExecutionSpec{
		Format: workflowadapter.RunExecutionSpecFormat, Version: workflowadapter.RunExecutionSpecVersion,
		Template: workflowadapter.StandardAuthoringTemplateReference(),
		Selection: workflowadapter.RunSelectionReference{
			Kind: workflowadapter.RunSelectionAuthoringSession, AuthoringSourceID: subject.SourceID,
			AuthoringSessionID: subject.AuthoringSessionID, AuthoringSourceDigest: subject.SourceSnapshotDigest,
		},
		References: workflowadapter.ExecutionReferenceSet{},
	}
	checkouts := make(map[string]workflowadapter.CheckoutReference)
	runtimes := make(map[string]workflowadapter.RuntimeReference)
	providers := make(map[string]workflowadapter.ProviderReference)
	secrets := make(map[string]workflowadapter.SecretReference)
	for _, registration := range catalog.Operations {
		checkout := workflowadapter.CheckoutReference{ID: registration.Checkout.ID, RevisionID: subject.AuthoringSessionID, RevisionDigest: subject.SourceSnapshotDigest}
		if err := appendStandardAuthoringCheckout(checkouts, checkout); err != nil {
			return workflowadapter.RunExecutionSpec{}, err
		}
		if err := appendStandardAuthoringRuntime(runtimes, registration.Runtime); err != nil {
			return workflowadapter.RunExecutionSpec{}, err
		}
		if err := appendStandardAuthoringProvider(providers, registration.Provider); err != nil {
			return workflowadapter.RunExecutionSpec{}, err
		}
		secretIDs := make([]string, 0, len(registration.Secrets))
		for _, secret := range registration.Secrets {
			if err := appendStandardAuthoringSecret(secrets, secret); err != nil {
				return workflowadapter.RunExecutionSpec{}, err
			}
			secretIDs = append(secretIDs, secret.ID)
		}
		binding, err := standardAuthoringCatalogStageBinding(registration, secretIDs)
		if err != nil {
			return workflowadapter.RunExecutionSpec{}, err
		}
		specification.Stages = append(specification.Stages, binding)
	}
	for _, value := range checkouts {
		specification.References.Checkouts = append(specification.References.Checkouts, value)
	}
	for _, value := range runtimes {
		specification.References.Runtimes = append(specification.References.Runtimes, value)
	}
	for _, value := range providers {
		specification.References.Providers = append(specification.References.Providers, value)
	}
	for _, value := range secrets {
		specification.References.Secrets = append(specification.References.Secrets, value)
	}
	if err := specification.Validate(); err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("build catalog Standard authoring execution specification: %w", err)
	}
	return specification, nil
}

func appendStandardAuthoringCheckout(values map[string]workflowadapter.CheckoutReference, value workflowadapter.CheckoutReference) error {
	if existing, found := values[value.ID]; found && existing != value {
		return fmt.Errorf("Standard authoring catalog has conflicting checkout %q", value.ID)
	}
	values[value.ID] = value
	return nil
}

func appendStandardAuthoringRuntime(values map[string]workflowadapter.RuntimeReference, value workflowadapter.RuntimeReference) error {
	if existing, found := values[value.ID]; found && existing != value {
		return fmt.Errorf("Standard authoring catalog has conflicting runtime %q", value.ID)
	}
	values[value.ID] = value
	return nil
}

func appendStandardAuthoringProvider(values map[string]workflowadapter.ProviderReference, value workflowadapter.ProviderReference) error {
	if existing, found := values[value.ID]; found && existing != value {
		return fmt.Errorf("Standard authoring catalog has conflicting provider %q", value.ID)
	}
	values[value.ID] = value
	return nil
}

func appendStandardAuthoringSecret(values map[string]workflowadapter.SecretReference, value workflowadapter.SecretReference) error {
	if existing, found := values[value.ID]; found && existing != value {
		return fmt.Errorf("Standard authoring catalog has conflicting secret %q", value.ID)
	}
	values[value.ID] = value
	return nil
}

func standardAuthoringCatalogStageBinding(registration stageprovider.DeploymentOperationRegistration, secretIDs []string) (workflowadapter.StageExecutionBinding, error) {
	base := workflowadapter.StageBindingBase{
		Type: registration.Stage.Type, StageKey: registration.Stage.Key, Plugin: registration.Stage.Plugin,
		ArtifactInputs: []workflowadapter.ArtifactInputReference{}, CheckoutID: registration.Checkout.ID,
		RuntimeID: registration.Runtime.ID, Operation: registration.Operation.Clone(), SecretIDs: append([]string(nil), secretIDs...),
	}
	switch base.Type {
	case workflowadapter.StageBindingRepoPrepare:
		return workflowadapter.RepoPrepareBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingRepoAnalyze:
		return workflowadapter.RepoAnalyzeBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTaskDesign:
		return workflowadapter.TaskDesignBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTaskReview:
		return workflowadapter.TaskReviewBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingGenerateTaskFiles:
		return workflowadapter.GenerateTaskFilesBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingInstructionGen:
		return workflowadapter.InstructionGenBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTaskTOMLGen:
		return workflowadapter.TaskTOMLGenBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingDockerfileGen:
		return workflowadapter.DockerfileGenBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingContentReview:
		return workflowadapter.ContentReviewBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingSolveGen:
		return workflowadapter.SolveGenBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTestGen:
		return workflowadapter.TestGenBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTestsAnalysis:
		return workflowadapter.TestsAnalysisBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingSolutionReview:
		return workflowadapter.SolutionReviewBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingMaterializeTask:
		return workflowadapter.MaterializeTaskBinding{StageBindingBase: base}, nil
	default:
		return nil, fmt.Errorf("Standard authoring catalog has unsupported stage binding type %q", base.Type)
	}
}

var _ StandardAuthoringRunDefinitionProvider = (*CatalogStandardAuthoringRunDefinitionProvider)(nil)
