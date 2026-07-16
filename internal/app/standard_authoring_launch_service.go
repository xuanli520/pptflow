package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringSourceSnapshotSchemaVersion identifies the safe,
	// content-addressed Git archive produced by the controlled source capturer.
	// The bytes are stored only in the managed object store; neither this value
	// nor AuthoringSource carries a caller filesystem path.
	StandardAuthoringSourceSnapshotSchemaVersion = "harbor.source-snapshot.v1"

	standardAuthoringLaunchAction                 LifecycleMutationAction = "authoring.start"
	standardAuthoringLaunchSessionManifestFormat                          = "harbor.standard-authoring-session.v1"
	standardAuthoringLaunchSessionManifestVersion                         = "2"
	standardAuthoringLaunchTrigger                                        = "authoring.standard.create"
	standardAuthoringLaunchIdentityDomain                                 = "harbor.standard-authoring.launch.identity.v1"
	standardAuthoringLaunchDefinitionDomain                               = "harbor.standard-authoring.launch-definition.v1"
	standardAuthoringLaunchStaticDefinitionDomain                         = "harbor.standard-authoring.launch-static-definition.v1"
	standardAuthoringLaunchPreparationFormat                              = "harbor.standard-authoring-launch-preparation.v1"
	standardAuthoringLaunchPreparationVersion                             = "3"
	standardAuthoringLaunchPreparationFileName                            = "deployment-definition.json"
	standardAuthoringLaunchPreparationMaxBytes    int64                   = 1 << 20
	standardAuthoringLaunchCaptureReceiptFormat                           = "harbor.standard-authoring-launch-capture-receipt.v1"
	standardAuthoringLaunchCaptureReceiptVersion                          = "1"
	standardAuthoringLaunchCaptureReceiptFileName                         = "capture-receipt.json"
	standardAuthoringLaunchCaptureReceiptMaxBytes int64                   = 1 << 20
)

var (
	// ErrStandardAuthoringLaunchUnavailable is intentionally stable and does
	// not expose a deployment path, provider configuration, or credential. A
	// normal lifecycle composition remains usable for read/control operations
	// when the Standard source capture capability has not been installed.
	ErrStandardAuthoringLaunchUnavailable = errors.New("standard authoring launch is not configured")
)

// StandardAuthoringSourceSnapshot is an immutable archive capture returned
// by a composition-owned capturer. Its coordinate must exactly match the
// canonical HTTPS/SSH coordinate selected at launch; Content is validated as
// a bounded, safe archive before it enters the managed object store.
type StandardAuthoringSourceSnapshot struct {
	RepositoryURL string
	CommitSHA     string
	SchemaVersion string
	Content       []byte
}

// StandardAuthoringSourceCapturer is the only boundary allowed to acquire an
// immutable, caller-selected HTTPS/SSH Git commit before an AuthoringSource
// exists. It never receives a branch, local checkout directory, model, or
// secret input. The concrete Git implementation remains deployment-owned and
// separate from the generic workflow engine.
type StandardAuthoringSourceCapturer interface {
	CaptureStandardAuthoringSource(context.Context, StandardAuthoringSourceCoordinate) (StandardAuthoringSourceSnapshot, error)
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
// a deployment-owned provider, never supplied through CLI flags. Its catalog
// receipt is explicit provider evidence and is verified against the lifecycle
// composition before it is persisted with the Run; the lock identity is
// independently resolved and verified when the installation requires it.
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

// StandardAuthoringStaticRunDefinition is the source-independent half of a
// Standard authoring definition. It is deliberately resolved before Git is
// contacted, then persisted with the prepared lifecycle operation. The
// catalog receipt is explicit evidence supplied by the deployment-owned
// provider. LifecycleServices verifies it against its independently installed
// template binding before a remote source is contacted; it never fills a
// missing provider receipt from the registry.
type StandardAuthoringStaticRunDefinition struct {
	Profile                       workflowadapter.ExecutionProfile
	DeploymentCatalogReceipt      []byte
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity
}

// StandardAuthoringStaticRunDefinitionProvider is required in addition to the
// subject-bound definition provider. A launch must not acquire a remote source
// before it has durably committed the deployment profile/catalog/lock identity
// that will govern any retry.
type StandardAuthoringStaticRunDefinitionProvider interface {
	StandardAuthoringStaticRunDefinition(context.Context) (StandardAuthoringStaticRunDefinition, error)
}

// StandardAuthoringLaunchCommand is the one public mutation that creates the
// pre-materialization source/session workflow. The Task is intentionally only
// a revision-free draft ownership record; it is not used as a workflow subject
// and no TaskRevision is manufactured here.
type StandardAuthoringLaunchCommand struct {
	LifecycleMutationCommandBase
	RepositoryURL string
	CommitSHA     string
	BaseImage     string
	Slug          string
	Title         string
	MetadataJSON  string
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
	if service == nil || service.core == nil || service.core.store == nil || service.core.objects == nil || service.capturer == nil || service.definitions == nil {
		return false
	}
	_, configured := service.definitions.(StandardAuthoringStaticRunDefinitionProvider)
	return configured
}

// Start freezes one caller-selected immutable source coordinate, captures it
// exactly once for its idempotency key, then queues the Standard Run. The
// entity IDs are deterministic UUIDv7 derivatives of the caller-issued key,
// making interrupted capture recoverable without reusing identities across
// entity types.
func (service *StandardAuthoringLaunchService) Start(ctx context.Context, command StandardAuthoringLaunchCommand) (LifecycleMutationReceipt, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return LifecycleMutationReceipt{}, ErrStandardAuthoringLaunchUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	coordinate, err := standardAuthoringLaunchCoordinate(command)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	command.RepositoryURL, command.CommitSHA = coordinate.RepositoryURL, coordinate.CommitSHA
	environmentPolicy, err := workflowadapter.NewStandardAuthoringEnvironmentPolicy(command.BaseImage)
	if err != nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("Standard authoring base image: %w", err)
	}
	command.BaseImage = environmentPolicy.BaseImage
	if err := validateStandardAuthoringLaunchCommand(command); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	mutations := &LifecycleMutationService{core: service.core}
	metadata, err := canonicalStandardAuthoringMetadata(command.MetadataJSON)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	ids, err := standardAuthoringLaunchIdentities(command.IdempotencyKey)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	environmentPolicyInput, err := newStandardAuthoringEnvironmentPolicyInput(ids.EnvironmentPolicyArtifactID, environmentPolicy)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	admission, err := service.lockStandardAuthoringLaunchOperation(ctx, standardAuthoringLaunchAdmissionOperationID(command.IdempotencyKey))
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	admissionOpen := true
	defer func() {
		if admissionOpen {
			_ = admission.Close()
		}
	}()
	prior, err := service.core.store.GetLifecycleOperationByIdempotencyKey(ctx, command.IdempotencyKey)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	preparedBeforeBegin := prior != nil && prior.Action == string(standardAuthoringLaunchAction) && prior.State == store.LifecycleOperationPrepared
	var freshDeploymentDefinition standardAuthoringLaunchDeploymentDefinition
	if prior == nil {
		// A fresh key has no durable lifecycle state yet. Resolve the static
		// deployment proof before Begin so an ordinary unavailable/mismatched
		// provider does not leave an unrecoverable prepared operation behind.
		if !service.Available() {
			return LifecycleMutationReceipt{}, ErrStandardAuthoringLaunchUnavailable
		}
		freshDeploymentDefinition, err = service.freezeStandardAuthoringLaunchDeploymentDefinition(ctx)
		if err != nil {
			return LifecycleMutationReceipt{}, err
		}
	}
	op, replay, err := mutations.begin(ctx, standardAuthoringLaunchAction, command.LifecycleMutationCommandBase, standardAuthoringLaunchRequest{
		RepositoryURL: command.RepositoryURL,
		CommitSHA:     command.CommitSHA,
		BaseImage:     command.BaseImage,
		Slug:          strings.TrimSpace(command.Slug),
		Title:         strings.TrimSpace(command.Title),
		MetadataJSON:  metadata,
	}, lifecycleOperationTargets{TaskID: ids.TaskID, RunID: ids.RunID})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if op.TaskID != ids.TaskID || op.RunID != ids.RunID || op.Actor != strings.TrimSpace(command.Actor) || op.Reason != strings.TrimSpace(command.Reason) {
		return LifecycleMutationReceipt{}, fmt.Errorf("%w: Standard authoring lifecycle operation %s", store.ErrIdempotencyConflict, op.ID)
	}
	lease, err := service.lockStandardAuthoringLaunchOperation(ctx, op.ID)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	defer lease.Close()
	// A peer may have completed this operation while this caller waited on the
	// operation lock. Replaying here avoids re-resolving a new definition or
	// re-running any capture after that durable completion.
	if receipt, found, replayErr := mutations.ReplayCompleted(ctx, standardAuthoringLaunchAction, command.IdempotencyKey); replayErr != nil {
		return LifecycleMutationReceipt{}, replayErr
	} else if found {
		return receipt, nil
	}
	var preparation standardAuthoringLaunchPreparation
	if preparedBeforeBegin {
		stored, found, readErr := readStandardAuthoringLaunchPreparationAt(lease.directory)
		if readErr != nil {
			return LifecycleMutationReceipt{}, readErr
		}
		if !found {
			return LifecycleMutationReceipt{}, fmt.Errorf("%w: prepared Standard authoring lifecycle operation %s has no immutable deployment preparation", store.ErrIdempotencyConflict, op.ID)
		}
		if !service.Available() {
			return LifecycleMutationReceipt{}, ErrStandardAuthoringLaunchUnavailable
		}
		deploymentDefinition, definitionErr := service.freezeStandardAuthoringLaunchDeploymentDefinition(ctx)
		if definitionErr != nil {
			return LifecycleMutationReceipt{}, definitionErr
		}
		expected := newStandardAuthoringLaunchPreparation(op, ids, deploymentDefinition, environmentPolicyInput)
		if verifyErr := verifyStandardAuthoringLaunchPreparation(stored, expected); verifyErr != nil {
			return LifecycleMutationReceipt{}, verifyErr
		}
		preparation = stored
	} else {
		preparation, err = service.ensureStandardAuthoringLaunchPreparation(ctx, lease.directory, op, ids, freshDeploymentDefinition, environmentPolicyInput)
		if err != nil {
			return LifecycleMutationReceipt{}, err
		}
	}
	if err := admission.Close(); err != nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("release Standard authoring launch admission lock: %w", err)
	}
	admissionOpen = false

	capture, err := service.ensureStandardAuthoringLaunchCaptureReceipt(ctx, lease.directory, op, ids, coordinate, preparation)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	source, err := service.ensureAuthoringSource(ctx, ids.SourceID, command, coordinate, capture)
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
	frozen, err := service.freezeStandardAuthoringDefinition(ctx, subject, preparation)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	sessionManifest, err := standardAuthoringSessionManifestJSON(source, task, subject.AuthoringSessionID, op.ID, preparation, frozen)
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
	if err := verifyStandardAuthoringLaunchSession(session, source, task, op.ID, preparation, frozen); err != nil {
		return LifecycleMutationReceipt{}, err
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
		Summary:              "已捕获冻结 Git 源码并启动 Standard 创题 Run",
	})
}

type standardAuthoringLaunchIDs struct {
	SourceID                    string
	TaskID                      string
	SessionID                   string
	RunID                       string
	EnvironmentPolicyArtifactID string
}

func standardAuthoringLaunchIdentities(idempotencyKey string) (standardAuthoringLaunchIDs, error) {
	return standardAuthoringLaunchIDs{
		SourceID:                    standardAuthoringLaunchIdentity(idempotencyKey, "source"),
		TaskID:                      standardAuthoringLaunchIdentity(idempotencyKey, "task"),
		SessionID:                   standardAuthoringLaunchIdentity(idempotencyKey, "session"),
		RunID:                       standardAuthoringLaunchIdentity(idempotencyKey, "run"),
		EnvironmentPolicyArtifactID: standardAuthoringLaunchIdentity(idempotencyKey, "environment-policy"),
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

// standardAuthoringEnvironmentPolicyInput is the small immutable bridge from
// a session manifest to a normal workflow artifact binding.  The policy bytes
// are already canonical and content-addressed, so they do not need a second
// mutable database row or a synthetic stage attempt merely to be visible to
// the frozen execution contract.
type standardAuthoringEnvironmentPolicyInput struct {
	ArtifactID    workflowkit.ArtifactID
	Policy        workflowadapter.StandardAuthoringEnvironmentPolicy
	CanonicalJSON []byte
	ContentDigest workflowkit.Fingerprint
}

func newStandardAuthoringEnvironmentPolicyInput(artifactID string, policy workflowadapter.StandardAuthoringEnvironmentPolicy) (standardAuthoringEnvironmentPolicyInput, error) {
	if err := store.ValidateUUIDv7(artifactID); err != nil {
		return standardAuthoringEnvironmentPolicyInput{}, fmt.Errorf("Standard authoring environment policy artifact ID: %w", err)
	}
	canonical, err := policy.CanonicalJSON()
	if err != nil {
		return standardAuthoringEnvironmentPolicyInput{}, fmt.Errorf("canonicalize Standard authoring environment policy: %w", err)
	}
	digest, err := policy.ContentDigest()
	if err != nil {
		return standardAuthoringEnvironmentPolicyInput{}, fmt.Errorf("fingerprint Standard authoring environment policy: %w", err)
	}
	return standardAuthoringEnvironmentPolicyInput{
		ArtifactID: workflowkit.ArtifactID(artifactID), Policy: policy, CanonicalJSON: canonical, ContentDigest: digest,
	}, nil
}

func (input standardAuthoringEnvironmentPolicyInput) artifactReference() workflowadapter.ArtifactReference {
	return workflowadapter.ArtifactReference{
		ID: input.ArtifactID, ContentDigest: input.ContentDigest, SchemaVersion: workflowadapter.StandardAuthoringEnvironmentPolicySchemaVersion,
	}
}

func (input standardAuthoringEnvironmentPolicyInput) artifactBinding() workflowkit.ArtifactBinding {
	return workflowkit.ArtifactBinding{
		Name: workflowadapter.StandardAuthoringEnvironmentPolicyArtifact, ArtifactID: input.ArtifactID,
		ContentDigest: input.ContentDigest, SchemaVersion: workflowadapter.StandardAuthoringEnvironmentPolicySchemaVersion,
	}
}

// standardAuthoringLaunchAdmissionOperationID is a deterministic lock-only
// directory identity. It serializes discovery/creation of the store-backed
// lifecycle operation long enough to distinguish a fresh operation from a
// previously prepared operation with missing immutable preparation evidence.
// It is not persisted as a lifecycle entity and cannot collide with the
// source/task/session/run derivations because its domain label is distinct.
func standardAuthoringLaunchAdmissionOperationID(idempotencyKey string) string {
	return standardAuthoringLaunchIdentity(idempotencyKey, "admission-lock")
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
	if _, err := workflowadapter.NewStandardAuthoringEnvironmentPolicy(command.BaseImage); err != nil {
		return fmt.Errorf("Standard authoring base image: %w", err)
	}
	return nil
}

func standardAuthoringLaunchCoordinate(command StandardAuthoringLaunchCommand) (StandardAuthoringSourceCoordinate, error) {
	coordinate, err := (StandardAuthoringSourceCoordinate{RepositoryURL: command.RepositoryURL, CommitSHA: command.CommitSHA}).Canonical()
	if err != nil {
		return StandardAuthoringSourceCoordinate{}, fmt.Errorf("Standard authoring source coordinate: %w", err)
	}
	return coordinate, nil
}

func standardAuthoringStoredSourceCoordinate(source store.AuthoringSource) (StandardAuthoringSourceCoordinate, error) {
	coordinate, err := (StandardAuthoringSourceCoordinate{RepositoryURL: source.RepositoryURL, CommitSHA: source.CommitSHA}).Canonical()
	if err != nil || source.RepositoryURL != coordinate.RepositoryURL || source.CommitSHA != coordinate.CommitSHA {
		return StandardAuthoringSourceCoordinate{}, fmt.Errorf("persisted Standard authoring source coordinate is invalid")
	}
	return coordinate, nil
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

func (service *StandardAuthoringLaunchService) ensureAuthoringSource(ctx context.Context, sourceID string, command StandardAuthoringLaunchCommand, coordinate StandardAuthoringSourceCoordinate, capture standardAuthoringLaunchCaptureReceipt) (store.AuthoringSource, error) {
	if err := capture.Validate(); err != nil {
		return store.AuthoringSource{}, fmt.Errorf("validate Standard authoring capture receipt: %w", err)
	}
	if capture.RepositoryURL != coordinate.RepositoryURL || capture.CommitSHA != coordinate.CommitSHA || capture.RequestedSourceID != sourceID {
		return store.AuthoringSource{}, fmt.Errorf("%w: Standard authoring capture receipt does not match the requested source", store.ErrIdempotencyConflict)
	}
	contentDigest := string(capture.SourceSnapshotObject.Digest)
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
		if err := validateStandardAuthoringLaunchSnapshotSource(*existing, coordinate, contentDigest); err != nil {
			return store.AuthoringSource{}, err
		}
		return *existing, nil
	}

	if reused, err := service.core.store.GetAuthoringSourceByCoordinateAndSnapshot(ctx, capture.RepositoryURL, capture.CommitSHA, contentDigest, capture.SourceSnapshotSchemaVersion); err != nil {
		return store.AuthoringSource{}, fmt.Errorf("find existing Standard authoring source snapshot: %w", err)
	} else if reused != nil {
		if err := verifyStandardAuthoringLaunchSourceObject(ctx, service.core.objects, *reused); err != nil {
			return store.AuthoringSource{}, err
		}
		if err := validateStandardAuthoringLaunchSnapshotSource(*reused, coordinate, contentDigest); err != nil {
			return store.AuthoringSource{}, err
		}
		return *reused, nil
	}
	source, err := service.core.store.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		ID:                    sourceID,
		RepositoryURL:         capture.RepositoryURL,
		CommitSHA:             capture.CommitSHA,
		SnapshotArtifactRef:   contentDigest,
		SnapshotContentDigest: contentDigest,
		SnapshotSchemaVersion: capture.SourceSnapshotSchemaVersion,
		IdempotencyKey:        standardAuthoringLaunchChildKey(command.IdempotencyKey, "source"),
		Actor:                 command.Actor,
		Reason:                command.Reason,
	})
	if err != nil {
		if !errors.Is(err, store.ErrIdentityCollision) {
			return store.AuthoringSource{}, err
		}
		reused, lookupErr := service.core.store.GetAuthoringSourceByCoordinateAndSnapshot(ctx, capture.RepositoryURL, capture.CommitSHA, contentDigest, capture.SourceSnapshotSchemaVersion)
		if lookupErr != nil {
			return store.AuthoringSource{}, fmt.Errorf("recheck existing Standard authoring source snapshot: %w", lookupErr)
		}
		if reused == nil {
			return store.AuthoringSource{}, err
		}
		if err := verifyStandardAuthoringLaunchSourceObject(ctx, service.core.objects, *reused); err != nil {
			return store.AuthoringSource{}, err
		}
		if err := validateStandardAuthoringLaunchSnapshotSource(*reused, coordinate, contentDigest); err != nil {
			return store.AuthoringSource{}, err
		}
		return *reused, nil
	}
	if err := verifyStandardAuthoringLaunchSourceObject(ctx, service.core.objects, source); err != nil {
		return store.AuthoringSource{}, err
	}
	if err := validateStandardAuthoringLaunchSnapshotSource(source, coordinate, contentDigest); err != nil {
		return store.AuthoringSource{}, err
	}
	return source, nil
}

func validateStandardAuthoringSourceSnapshot(snapshot StandardAuthoringSourceSnapshot, expected StandardAuthoringSourceCoordinate) error {
	coordinate, err := (StandardAuthoringSourceCoordinate{RepositoryURL: snapshot.RepositoryURL, CommitSHA: snapshot.CommitSHA}).Canonical()
	if err != nil || coordinate != expected || snapshot.RepositoryURL != coordinate.RepositoryURL || snapshot.CommitSHA != coordinate.CommitSHA || snapshot.SchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		return fmt.Errorf("Standard authoring source capture does not match the requested immutable source identity")
	}
	if err := validateStandardAuthoringSourceArchive(snapshot.Content, coordinate); err != nil {
		return fmt.Errorf("validate Standard authoring source archive: %w", err)
	}
	return nil
}

// standardAuthoringLaunchCaptureReceipt binds the archive object that was
// already validated and written to the immutable object store to one prepared
// lifecycle operation. It is deliberately published only after both steps;
// a retry that sees this receipt must never contact Git again.
type standardAuthoringLaunchCaptureReceipt struct {
	Format                      string                    `json:"format"`
	Version                     string                    `json:"version"`
	LifecycleOperationID        string                    `json:"lifecycle_operation_id"`
	PreparationFingerprint      workflowkit.Fingerprint   `json:"preparation_fingerprint"`
	RequestedSourceID           string                    `json:"requested_source_id"`
	RepositoryURL               string                    `json:"repository_url"`
	CommitSHA                   string                    `json:"commit_sha"`
	SourceSnapshotSchemaVersion string                    `json:"source_snapshot_schema_version"`
	SourceSnapshotObject        workflowruntime.ObjectRef `json:"source_snapshot_object"`
}

func newStandardAuthoringLaunchCaptureReceipt(operation store.LifecycleOperation, ids standardAuthoringLaunchIDs, coordinate StandardAuthoringSourceCoordinate, preparation standardAuthoringLaunchPreparation, object workflowruntime.ObjectRef) standardAuthoringLaunchCaptureReceipt {
	return standardAuthoringLaunchCaptureReceipt{
		Format:                      standardAuthoringLaunchCaptureReceiptFormat,
		Version:                     standardAuthoringLaunchCaptureReceiptVersion,
		LifecycleOperationID:        operation.ID,
		PreparationFingerprint:      preparation.PreparationFingerprint,
		RequestedSourceID:           ids.SourceID,
		RepositoryURL:               coordinate.RepositoryURL,
		CommitSHA:                   coordinate.CommitSHA,
		SourceSnapshotSchemaVersion: StandardAuthoringSourceSnapshotSchemaVersion,
		SourceSnapshotObject:        object.Clone(),
	}
}

func (receipt standardAuthoringLaunchCaptureReceipt) Validate() error {
	if receipt.Format != standardAuthoringLaunchCaptureReceiptFormat || receipt.Version != standardAuthoringLaunchCaptureReceiptVersion ||
		receipt.SourceSnapshotSchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		return errors.New("invalid Standard authoring capture receipt format")
	}
	for _, identity := range []struct {
		name  string
		value string
	}{
		{"lifecycle operation", receipt.LifecycleOperationID},
		{"requested source", receipt.RequestedSourceID},
	} {
		if err := store.ValidateUUIDv7(identity.value); err != nil {
			return fmt.Errorf("Standard authoring capture receipt %s ID: %w", identity.name, err)
		}
	}
	coordinate, err := (StandardAuthoringSourceCoordinate{RepositoryURL: receipt.RepositoryURL, CommitSHA: receipt.CommitSHA}).Canonical()
	if err != nil || coordinate.RepositoryURL != receipt.RepositoryURL || coordinate.CommitSHA != receipt.CommitSHA {
		return errors.New("Standard authoring capture receipt source coordinate is invalid")
	}
	if err := receipt.PreparationFingerprint.Validate(); err != nil {
		return fmt.Errorf("Standard authoring capture receipt preparation fingerprint: %w", err)
	}
	if err := receipt.SourceSnapshotObject.Validate(); err != nil || receipt.SourceSnapshotObject.SizeBytes < 1 || receipt.SourceSnapshotObject.SizeBytes > standardAuthoringSourceArchiveMaxBytes {
		if err != nil {
			return fmt.Errorf("Standard authoring capture receipt snapshot object: %w", err)
		}
		return errors.New("Standard authoring capture receipt snapshot object size is invalid")
	}
	return nil
}

func (receipt standardAuthoringLaunchCaptureReceipt) CanonicalJSON() ([]byte, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(receipt)
}

func verifyStandardAuthoringLaunchCaptureReceipt(receipt standardAuthoringLaunchCaptureReceipt, operation store.LifecycleOperation, ids standardAuthoringLaunchIDs, coordinate StandardAuthoringSourceCoordinate, preparation standardAuthoringLaunchPreparation) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if receipt.LifecycleOperationID != operation.ID || receipt.PreparationFingerprint != preparation.PreparationFingerprint || receipt.RequestedSourceID != ids.SourceID ||
		receipt.RepositoryURL != coordinate.RepositoryURL || receipt.CommitSHA != coordinate.CommitSHA {
		return fmt.Errorf("%w: Standard authoring capture receipt does not match prepared lifecycle operation %s", store.ErrIdempotencyConflict, operation.ID)
	}
	return nil
}

func readStandardAuthoringLaunchCaptureReceiptAt(directory *os.File) (standardAuthoringLaunchCaptureReceipt, bool, error) {
	raw, found, err := standardAuthoringReadNewImmutableFileAt(directory, standardAuthoringLaunchCaptureReceiptFileName, standardAuthoringLaunchCaptureReceiptMaxBytes)
	if err != nil {
		return standardAuthoringLaunchCaptureReceipt{}, false, fmt.Errorf("read Standard authoring capture receipt: %w", err)
	}
	if !found {
		return standardAuthoringLaunchCaptureReceipt{}, false, nil
	}
	var receipt standardAuthoringLaunchCaptureReceipt
	if err := decodeStrictJSON(string(raw), &receipt); err != nil {
		return standardAuthoringLaunchCaptureReceipt{}, false, fmt.Errorf("decode Standard authoring capture receipt: %w", err)
	}
	canonical, err := receipt.CanonicalJSON()
	if err != nil || !bytes.Equal(raw, canonical) {
		return standardAuthoringLaunchCaptureReceipt{}, false, errors.New("Standard authoring capture receipt is not canonical")
	}
	return receipt, true, nil
}

func (service *StandardAuthoringLaunchService) ensureStandardAuthoringLaunchCaptureReceipt(ctx context.Context, directory *os.File, operation store.LifecycleOperation, ids standardAuthoringLaunchIDs, coordinate StandardAuthoringSourceCoordinate, preparation standardAuthoringLaunchPreparation) (standardAuthoringLaunchCaptureReceipt, error) {
	if existing, found, err := readStandardAuthoringLaunchCaptureReceiptAt(directory); err != nil {
		return standardAuthoringLaunchCaptureReceipt{}, err
	} else if found {
		if err := service.verifyPersistedStandardAuthoringLaunchCaptureReceipt(ctx, existing, operation, ids, coordinate, preparation); err != nil {
			return standardAuthoringLaunchCaptureReceipt{}, err
		}
		return existing, nil
	}
	if err := ctx.Err(); err != nil {
		return standardAuthoringLaunchCaptureReceipt{}, err
	}
	captured, err := service.capturer.CaptureStandardAuthoringSource(ctx, coordinate)
	if err != nil {
		return standardAuthoringLaunchCaptureReceipt{}, fmt.Errorf("capture requested Standard authoring source: %w", err)
	}
	if err := validateStandardAuthoringSourceSnapshot(captured, coordinate); err != nil {
		return standardAuthoringLaunchCaptureReceipt{}, err
	}
	object, err := service.core.objects.PutBytes(ctx, captured.Content)
	if err != nil {
		return standardAuthoringLaunchCaptureReceipt{}, fmt.Errorf("store Standard authoring source snapshot: %w", err)
	}
	receipt := newStandardAuthoringLaunchCaptureReceipt(operation, ids, coordinate, preparation, object)
	canonical, err := receipt.CanonicalJSON()
	if err != nil {
		return standardAuthoringLaunchCaptureReceipt{}, err
	}
	// There is intentionally no context check between object publication and
	// receipt publication. Once the archive object exists, sealing its receipt
	// is the recovery boundary that prevents a cancelled caller from fetching it
	// again. A process crash or storage failure before this write can still cause
	// one later read-only Git recapture; the orphan object is content-addressed
	// and never becomes an AuthoringSource without this validated receipt.
	if err := standardAuthoringWriteNewImmutableFileAt(directory, standardAuthoringLaunchCaptureReceiptFileName, canonical, 0o640); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return standardAuthoringLaunchCaptureReceipt{}, fmt.Errorf("write Standard authoring capture receipt: %w", err)
		}
		stored, found, readErr := readStandardAuthoringLaunchCaptureReceiptAt(directory)
		if readErr != nil || !found {
			if readErr != nil {
				return standardAuthoringLaunchCaptureReceipt{}, readErr
			}
			return standardAuthoringLaunchCaptureReceipt{}, errors.New("Standard authoring capture receipt appeared then disappeared")
		}
		if err := service.verifyPersistedStandardAuthoringLaunchCaptureReceipt(ctx, stored, operation, ids, coordinate, preparation); err != nil {
			return standardAuthoringLaunchCaptureReceipt{}, err
		}
		return stored, nil
	}
	return receipt, nil
}

func (service *StandardAuthoringLaunchService) verifyPersistedStandardAuthoringLaunchCaptureReceipt(ctx context.Context, receipt standardAuthoringLaunchCaptureReceipt, operation store.LifecycleOperation, ids standardAuthoringLaunchIDs, coordinate StandardAuthoringSourceCoordinate, preparation standardAuthoringLaunchPreparation) error {
	if err := verifyStandardAuthoringLaunchCaptureReceipt(receipt, operation, ids, coordinate, preparation); err != nil {
		return err
	}
	archive, err := service.core.objects.ReadAll(ctx, receipt.SourceSnapshotObject)
	if err != nil {
		return fmt.Errorf("verify persisted Standard authoring capture object: %w", err)
	}
	if err := validateStandardAuthoringSourceArchive(archive, coordinate); err != nil {
		return fmt.Errorf("validate persisted Standard authoring capture object: %w", err)
	}
	return nil
}

func validateStandardAuthoringLaunchSource(source store.AuthoringSource, expected StandardAuthoringSourceCoordinate) error {
	coordinate, err := standardAuthoringStoredSourceCoordinate(source)
	if err != nil || coordinate != expected ||
		source.SnapshotArtifactRef != source.SnapshotContentDigest || source.SnapshotSchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		return fmt.Errorf("%w: persisted Standard authoring source does not match the requested source contract", store.ErrIdempotencyConflict)
	}
	return nil
}

func validateStandardAuthoringLaunchSnapshotSource(source store.AuthoringSource, expected StandardAuthoringSourceCoordinate, expectedContentDigest string) error {
	if err := validateStandardAuthoringLaunchSource(source, expected); err != nil {
		return err
	}
	if source.SnapshotArtifactRef != expectedContentDigest || source.SnapshotContentDigest != expectedContentDigest {
		return fmt.Errorf("%w: persisted Standard authoring source snapshot does not match the captured source object", store.ErrIdempotencyConflict)
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

// standardAuthoringLaunchDeploymentDefinition is the source-independent
// deployment identity committed before source capture. The source/session
// selection is intentionally absent because its snapshot digest does not exist
// until capture succeeds.
type standardAuthoringLaunchDeploymentDefinition struct {
	Profile                       workflowadapter.ExecutionProfile
	ProfileCanonical              []byte
	ProfileFingerprint            workflowkit.Fingerprint
	DeploymentCatalogReceipt      []byte
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity
	Fingerprint                   workflowkit.Fingerprint
}

// standardAuthoringLaunchPreparation is the immutable durable bridge between
// a prepared lifecycle operation and the later AuthoringSource. It resides in
// the managed control-plane directory because the consolidated V2 Store
// intentionally does not permit incremental DDL migrations. The operation's
// request fingerprint also commits PreparationFingerprint before this record
// can authorize a Git capture.
type standardAuthoringLaunchPreparation struct {
	Format               string `json:"format"`
	Version              string `json:"version"`
	LifecycleOperationID string `json:"lifecycle_operation_id"`
	// RequestedSourceID is the deterministic launch-local allocation. It is
	// intentionally not asserted to be the final AuthoringSource ID because an
	// identical captured snapshot may safely reuse an earlier immutable source.
	RequestedSourceID             string                                                `json:"requested_source_id"`
	TargetTaskID                  string                                                `json:"target_task_id"`
	AuthoringSessionID            string                                                `json:"authoring_session_id"`
	RunID                         string                                                `json:"run_id"`
	WorkflowTemplateID            string                                                `json:"workflow_template_id"`
	WorkflowTemplateVersion       string                                                `json:"workflow_template_version"`
	SourceSnapshotSchemaVersion   string                                                `json:"source_snapshot_schema_version"`
	EnvironmentPolicyArtifactID   string                                                `json:"environment_policy_artifact_id"`
	EnvironmentPolicy             json.RawMessage                                       `json:"environment_policy"`
	EnvironmentPolicyDigest       workflowkit.Fingerprint                               `json:"environment_policy_digest"`
	ExecutionProfile              json.RawMessage                                       `json:"execution_profile"`
	ProfileFingerprint            workflowkit.Fingerprint                               `json:"profile_fingerprint"`
	DeploymentCatalogReceipt      json.RawMessage                                       `json:"deployment_catalog_receipt,omitempty"`
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity `json:"deployment_catalog_lock_identity,omitempty"`
	PreparationFingerprint        workflowkit.Fingerprint                               `json:"preparation_fingerprint"`
}

func (service *StandardAuthoringLaunchService) freezeStandardAuthoringLaunchDeploymentDefinition(ctx context.Context) (standardAuthoringLaunchDeploymentDefinition, error) {
	provider, ok := service.definitions.(StandardAuthoringStaticRunDefinitionProvider)
	if !ok || provider == nil {
		return standardAuthoringLaunchDeploymentDefinition{}, ErrStandardAuthoringLaunchUnavailable
	}
	definition, err := provider.StandardAuthoringStaticRunDefinition(ctx)
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("resolve Standard authoring static deployment definition: %w", err)
	}
	return service.newStandardAuthoringLaunchDeploymentDefinition(definition.Profile, definition.DeploymentCatalogReceipt, definition.DeploymentCatalogLockIdentity)
}

func (service *StandardAuthoringLaunchService) newStandardAuthoringLaunchDeploymentDefinition(profile workflowadapter.ExecutionProfile, receipt []byte, lockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity) (standardAuthoringLaunchDeploymentDefinition, error) {
	if !profile.Template.Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("Standard authoring static deployment definition has the wrong workflow template")
	}
	profileCanonical, err := profile.CanonicalJSON()
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("canonicalize Standard authoring static profile: %w", err)
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("fingerprint Standard authoring static profile: %w", err)
	}
	if len(bytes.TrimSpace(receipt)) == 0 {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("%w: Standard authoring static definition must explicitly supply its deployment catalog receipt", stageprovider.ErrDeploymentOperationCatalogUnavailable)
	}
	catalogReceipt, err := service.core.resolveStartRunDeploymentCatalogReceipt(workflowadapter.StandardAuthoringTemplateReference(), receipt)
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("freeze Standard authoring static deployment catalog receipt: %w", err)
	}
	resolvedLockIdentity, err := service.core.resolveStartRunDeploymentCatalogLockIdentity(workflowadapter.StandardAuthoringTemplateReference(), lockIdentity)
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("freeze Standard authoring static deployment catalog lock identity: %w", err)
	}
	fingerprint, err := standardAuthoringStaticDefinitionFingerprint(profileCanonical, catalogReceipt, resolvedLockIdentity)
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, err
	}
	return standardAuthoringLaunchDeploymentDefinition{
		Profile:                       profile.Clone(),
		ProfileCanonical:              append([]byte(nil), profileCanonical...),
		ProfileFingerprint:            profileFingerprint,
		DeploymentCatalogReceipt:      append([]byte(nil), catalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(resolvedLockIdentity),
		Fingerprint:                   fingerprint,
	}, nil
}

func standardAuthoringStaticDefinitionFingerprint(profile, receipt []byte, lock *stageprovider.DeploymentOperationCatalogLockIdentity) (workflowkit.Fingerprint, error) {
	lockJSON := []byte("null")
	if lock != nil {
		var err error
		lockJSON, err = canonicalDeploymentCatalogLockIdentity(*lock)
		if err != nil {
			return "", err
		}
	}
	return workflowkit.FingerprintParts(standardAuthoringLaunchStaticDefinitionDomain, []workflowkit.FingerprintPart{
		{Name: "deployment_catalog_lock_identity", Value: lockJSON},
		{Name: "deployment_catalog_receipt", Value: append([]byte(nil), receipt...)},
		{Name: "execution_profile", Value: append([]byte(nil), profile...)},
	})
}

func newStandardAuthoringLaunchPreparation(operation store.LifecycleOperation, ids standardAuthoringLaunchIDs, definition standardAuthoringLaunchDeploymentDefinition, environmentPolicy standardAuthoringEnvironmentPolicyInput) standardAuthoringLaunchPreparation {
	return standardAuthoringLaunchPreparation{
		Format:                        standardAuthoringLaunchPreparationFormat,
		Version:                       standardAuthoringLaunchPreparationVersion,
		LifecycleOperationID:          operation.ID,
		RequestedSourceID:             ids.SourceID,
		TargetTaskID:                  ids.TaskID,
		AuthoringSessionID:            ids.SessionID,
		RunID:                         ids.RunID,
		WorkflowTemplateID:            workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion:       workflowadapter.StandardAuthoringWorkflowTemplateVersion,
		SourceSnapshotSchemaVersion:   StandardAuthoringSourceSnapshotSchemaVersion,
		EnvironmentPolicyArtifactID:   string(environmentPolicy.ArtifactID),
		EnvironmentPolicy:             append(json.RawMessage(nil), environmentPolicy.CanonicalJSON...),
		EnvironmentPolicyDigest:       environmentPolicy.ContentDigest,
		ExecutionProfile:              append(json.RawMessage(nil), definition.ProfileCanonical...),
		ProfileFingerprint:            definition.ProfileFingerprint,
		DeploymentCatalogReceipt:      append(json.RawMessage(nil), definition.DeploymentCatalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(definition.DeploymentCatalogLockIdentity),
		PreparationFingerprint:        definition.Fingerprint,
	}
}

func (preparation standardAuthoringLaunchPreparation) EnvironmentPolicyInput() (standardAuthoringEnvironmentPolicyInput, error) {
	policy, err := workflowadapter.ParseStandardAuthoringEnvironmentPolicyJSON(preparation.EnvironmentPolicy)
	if err != nil {
		return standardAuthoringEnvironmentPolicyInput{}, fmt.Errorf("decode Standard authoring launch preparation environment policy: %w", err)
	}
	input, err := newStandardAuthoringEnvironmentPolicyInput(preparation.EnvironmentPolicyArtifactID, policy)
	if err != nil {
		return standardAuthoringEnvironmentPolicyInput{}, err
	}
	if !bytes.Equal(preparation.EnvironmentPolicy, input.CanonicalJSON) || preparation.EnvironmentPolicyDigest != input.ContentDigest {
		return standardAuthoringEnvironmentPolicyInput{}, fmt.Errorf("Standard authoring launch preparation environment policy is not canonical")
	}
	return input, nil
}

func (preparation standardAuthoringLaunchPreparation) DeploymentDefinition() (standardAuthoringLaunchDeploymentDefinition, error) {
	if preparation.Format != standardAuthoringLaunchPreparationFormat || preparation.Version != standardAuthoringLaunchPreparationVersion ||
		preparation.WorkflowTemplateID != workflowadapter.StandardAuthoringWorkflowTemplateID || preparation.WorkflowTemplateVersion != workflowadapter.StandardAuthoringWorkflowTemplateVersion ||
		preparation.SourceSnapshotSchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("invalid Standard authoring launch preparation format or template")
	}
	for _, identity := range []struct {
		name  string
		value string
	}{
		{"lifecycle operation", preparation.LifecycleOperationID}, {"requested source", preparation.RequestedSourceID}, {"Task", preparation.TargetTaskID},
		{"authoring session", preparation.AuthoringSessionID}, {"Run", preparation.RunID}, {"environment policy artifact", preparation.EnvironmentPolicyArtifactID},
	} {
		if err := store.ValidateUUIDv7(identity.value); err != nil {
			return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("Standard authoring launch preparation %s ID: %w", identity.name, err)
		}
	}
	if _, err := preparation.EnvironmentPolicyInput(); err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, err
	}
	profile, err := workflowadapter.ParseExecutionProfileJSON(preparation.ExecutionProfile)
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("decode Standard authoring launch preparation profile: %w", err)
	}
	definition, err := newStandardAuthoringLaunchDeploymentDefinitionWithoutResolver(profile, preparation.DeploymentCatalogReceipt, preparation.DeploymentCatalogLockIdentity)
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, err
	}
	if !bytes.Equal(preparation.ExecutionProfile, definition.ProfileCanonical) || !bytes.Equal(preparation.DeploymentCatalogReceipt, definition.DeploymentCatalogReceipt) ||
		!sameDeploymentCatalogLockIdentity(preparation.DeploymentCatalogLockIdentity, definition.DeploymentCatalogLockIdentity) ||
		preparation.ProfileFingerprint != definition.ProfileFingerprint || preparation.PreparationFingerprint != definition.Fingerprint {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("Standard authoring launch preparation is not canonical")
	}
	return definition, nil
}

// newStandardAuthoringLaunchDeploymentDefinitionWithoutResolver validates a
// persisted static tuple without consulting a current deployment. It is used
// only while reading the immutable preparation record; retry admission then
// compares this result to a separately resolved current deployment tuple.
func newStandardAuthoringLaunchDeploymentDefinitionWithoutResolver(profile workflowadapter.ExecutionProfile, receipt []byte, lockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity) (standardAuthoringLaunchDeploymentDefinition, error) {
	if !profile.Template.Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("Standard authoring launch preparation profile has the wrong workflow template")
	}
	profileCanonical, err := profile.CanonicalJSON()
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("canonicalize Standard authoring launch preparation profile: %w", err)
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("fingerprint Standard authoring launch preparation profile: %w", err)
	}
	if len(bytes.TrimSpace(receipt)) == 0 {
		return standardAuthoringLaunchDeploymentDefinition{}, errors.New("Standard authoring launch preparation has no deployment catalog receipt")
	}
	parsed, canonical, err := canonicalDeploymentCatalogReceipt(receipt)
	if err != nil || !parsed.Template.Equal(workflowadapter.StandardAuthoringTemplateReference()) || !bytes.Equal(receipt, canonical) {
		return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("Standard authoring launch preparation catalog receipt is invalid")
	}
	catalogReceipt := append([]byte(nil), canonical...)
	resolvedLockIdentity := cloneDeploymentCatalogLockIdentity(lockIdentity)
	if resolvedLockIdentity != nil {
		if _, err := canonicalDeploymentCatalogLockIdentity(*resolvedLockIdentity); err != nil {
			return standardAuthoringLaunchDeploymentDefinition{}, fmt.Errorf("Standard authoring launch preparation lock identity: %w", err)
		}
	}
	fingerprint, err := standardAuthoringStaticDefinitionFingerprint(profileCanonical, catalogReceipt, resolvedLockIdentity)
	if err != nil {
		return standardAuthoringLaunchDeploymentDefinition{}, err
	}
	return standardAuthoringLaunchDeploymentDefinition{
		Profile:                       profile.Clone(),
		ProfileCanonical:              append([]byte(nil), profileCanonical...),
		ProfileFingerprint:            profileFingerprint,
		DeploymentCatalogReceipt:      catalogReceipt,
		DeploymentCatalogLockIdentity: resolvedLockIdentity,
		Fingerprint:                   fingerprint,
	}, nil
}

func sameStandardAuthoringLaunchDeploymentDefinition(left, right standardAuthoringLaunchDeploymentDefinition) bool {
	return left.Fingerprint == right.Fingerprint && left.ProfileFingerprint == right.ProfileFingerprint &&
		bytes.Equal(left.ProfileCanonical, right.ProfileCanonical) && bytes.Equal(left.DeploymentCatalogReceipt, right.DeploymentCatalogReceipt) &&
		sameDeploymentCatalogLockIdentity(left.DeploymentCatalogLockIdentity, right.DeploymentCatalogLockIdentity)
}

func (preparation standardAuthoringLaunchPreparation) CanonicalJSON() ([]byte, error) {
	if _, err := preparation.DeploymentDefinition(); err != nil {
		return nil, err
	}
	return json.Marshal(preparation)
}

func (service *StandardAuthoringLaunchService) ensureStandardAuthoringLaunchPreparation(ctx context.Context, directory *os.File, operation store.LifecycleOperation, ids standardAuthoringLaunchIDs, definition standardAuthoringLaunchDeploymentDefinition, environmentPolicy standardAuthoringEnvironmentPolicyInput) (standardAuthoringLaunchPreparation, error) {
	if err := ctx.Err(); err != nil {
		return standardAuthoringLaunchPreparation{}, err
	}
	if operation.Action != string(standardAuthoringLaunchAction) || operation.State != store.LifecycleOperationPrepared || operation.TaskID != ids.TaskID || operation.RunID != ids.RunID {
		return standardAuthoringLaunchPreparation{}, fmt.Errorf("%w: Standard authoring lifecycle operation %s", store.ErrIdempotencyConflict, operation.ID)
	}
	expected := newStandardAuthoringLaunchPreparation(operation, ids, definition, environmentPolicy)
	if stored, found, readErr := readStandardAuthoringLaunchPreparationAt(directory); readErr != nil {
		return standardAuthoringLaunchPreparation{}, readErr
	} else if found {
		if err := verifyStandardAuthoringLaunchPreparation(stored, expected); err != nil {
			return standardAuthoringLaunchPreparation{}, err
		}
		return stored, nil
	}
	canonical, err := expected.CanonicalJSON()
	if err != nil {
		return standardAuthoringLaunchPreparation{}, err
	}
	if err := standardAuthoringWriteNewImmutableFileAt(directory, standardAuthoringLaunchPreparationFileName, canonical, 0o640); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return standardAuthoringLaunchPreparation{}, fmt.Errorf("write Standard authoring launch preparation: %w", err)
		}
		stored, found, readErr := readStandardAuthoringLaunchPreparationAt(directory)
		if readErr != nil || !found {
			if readErr != nil {
				return standardAuthoringLaunchPreparation{}, readErr
			}
			return standardAuthoringLaunchPreparation{}, fmt.Errorf("Standard authoring launch preparation appeared then disappeared")
		}
		if err := verifyStandardAuthoringLaunchPreparation(stored, expected); err != nil {
			return standardAuthoringLaunchPreparation{}, err
		}
		return stored, nil
	}
	return expected, nil
}

func readStandardAuthoringLaunchPreparationAt(directory *os.File) (standardAuthoringLaunchPreparation, bool, error) {
	raw, found, err := standardAuthoringReadNewImmutableFileAt(directory, standardAuthoringLaunchPreparationFileName, standardAuthoringLaunchPreparationMaxBytes)
	if err != nil {
		return standardAuthoringLaunchPreparation{}, false, fmt.Errorf("read Standard authoring launch preparation: %w", err)
	}
	if !found {
		return standardAuthoringLaunchPreparation{}, false, nil
	}
	var preparation standardAuthoringLaunchPreparation
	if err := decodeStrictJSON(string(raw), &preparation); err != nil {
		return standardAuthoringLaunchPreparation{}, false, fmt.Errorf("decode Standard authoring launch preparation: %w", err)
	}
	canonical, err := preparation.CanonicalJSON()
	if err != nil || !bytes.Equal(raw, canonical) {
		return standardAuthoringLaunchPreparation{}, false, fmt.Errorf("Standard authoring launch preparation is not canonical")
	}
	return preparation, true, nil
}

func verifyStandardAuthoringLaunchPreparation(stored, expected standardAuthoringLaunchPreparation) error {
	storedDefinition, err := stored.DeploymentDefinition()
	if err != nil {
		return fmt.Errorf("validate persisted Standard authoring launch preparation: %w", err)
	}
	expectedDefinition, err := expected.DeploymentDefinition()
	if err != nil {
		return err
	}
	if stored.Format != expected.Format || stored.Version != expected.Version || stored.LifecycleOperationID != expected.LifecycleOperationID ||
		stored.RequestedSourceID != expected.RequestedSourceID || stored.TargetTaskID != expected.TargetTaskID || stored.AuthoringSessionID != expected.AuthoringSessionID || stored.RunID != expected.RunID ||
		stored.WorkflowTemplateID != expected.WorkflowTemplateID || stored.WorkflowTemplateVersion != expected.WorkflowTemplateVersion || stored.SourceSnapshotSchemaVersion != expected.SourceSnapshotSchemaVersion ||
		stored.EnvironmentPolicyArtifactID != expected.EnvironmentPolicyArtifactID || !bytes.Equal(stored.EnvironmentPolicy, expected.EnvironmentPolicy) || stored.EnvironmentPolicyDigest != expected.EnvironmentPolicyDigest ||
		!sameStandardAuthoringLaunchDeploymentDefinition(storedDefinition, expectedDefinition) {
		return fmt.Errorf("%w: Standard authoring deployment definition changed for prepared lifecycle operation %s", store.ErrIdempotencyConflict, expected.LifecycleOperationID)
	}
	return nil
}

func (service *StandardAuthoringLaunchService) freezeStandardAuthoringDefinition(ctx context.Context, subject StandardAuthoringRunDefinitionSubject, expected standardAuthoringLaunchPreparation) (standardAuthoringFrozenDefinition, error) {
	expectedDefinition, err := expected.DeploymentDefinition()
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("read Standard authoring prepared deployment definition: %w", err)
	}
	environmentPolicy, err := expected.EnvironmentPolicyInput()
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("read Standard authoring prepared environment policy: %w", err)
	}
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
	definition.ExecutionSpec, err = definition.ExecutionSpec.BindManagedArtifactInput(workflowadapter.StandardAuthoringEnvironmentPolicyArtifact, environmentPolicy.artifactReference())
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("bind Standard authoring environment policy to frozen execution specification: %w", err)
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
	if len(bytes.TrimSpace(definition.DeploymentCatalogReceipt)) == 0 {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("%w: Standard authoring source-bound definition must explicitly supply its deployment catalog receipt", stageprovider.ErrDeploymentOperationCatalogUnavailable)
	}
	catalogReceipt, err := service.core.resolveStartRunDeploymentCatalogReceipt(definition.ExecutionSpec.Template, definition.DeploymentCatalogReceipt)
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("freeze Standard authoring deployment catalog receipt: %w", err)
	}
	lockIdentity, err := service.core.resolveStartRunDeploymentCatalogLockIdentity(definition.ExecutionSpec.Template, definition.DeploymentCatalogLockIdentity)
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("freeze Standard authoring deployment catalog lock identity: %w", err)
	}
	currentStaticDefinition, err := service.newStandardAuthoringLaunchDeploymentDefinition(definition.Profile, catalogReceipt, lockIdentity)
	if err != nil {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("validate Standard authoring static deployment definition after source capture: %w", err)
	}
	if !sameStandardAuthoringLaunchDeploymentDefinition(expectedDefinition, currentStaticDefinition) {
		return standardAuthoringFrozenDefinition{}, fmt.Errorf("%w: Standard authoring deployment definition changed after preparation", store.ErrIdempotencyConflict)
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
	LifecycleOperationID          string                                                `json:"lifecycle_operation_id"`
	PreparationFingerprint        workflowkit.Fingerprint                               `json:"preparation_fingerprint"`
	RepositoryURL                 string                                                `json:"repository_url"`
	CommitSHA                     string                                                `json:"commit_sha"`
	RequestedSourceID             string                                                `json:"requested_source_id"`
	SourceID                      string                                                `json:"source_id"`
	AuthoringSessionID            string                                                `json:"authoring_session_id"`
	TargetTaskID                  string                                                `json:"target_task_id"`
	SourceSnapshotDigest          string                                                `json:"source_snapshot_digest"`
	SourceSnapshotSchemaVersion   string                                                `json:"source_snapshot_schema_version"`
	EnvironmentPolicyArtifactID   string                                                `json:"environment_policy_artifact_id"`
	EnvironmentPolicy             json.RawMessage                                       `json:"environment_policy"`
	EnvironmentPolicyDigest       workflowkit.Fingerprint                               `json:"environment_policy_digest"`
	ProfileFingerprint            workflowkit.Fingerprint                               `json:"profile_fingerprint"`
	ExecutionSpecFingerprint      workflowkit.Fingerprint                               `json:"execution_spec_fingerprint"`
	DefinitionFingerprint         workflowkit.Fingerprint                               `json:"definition_fingerprint"`
	DeploymentCatalogReceipt      json.RawMessage                                       `json:"deployment_catalog_receipt,omitempty"`
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity `json:"deployment_catalog_lock_identity,omitempty"`
}

func standardAuthoringSessionManifestJSON(source store.AuthoringSource, task store.TaskV2, sessionID, operationID string, preparation standardAuthoringLaunchPreparation, frozen standardAuthoringFrozenDefinition) (string, error) {
	environmentPolicy, err := preparation.EnvironmentPolicyInput()
	if err != nil {
		return "", fmt.Errorf("read Standard authoring session environment policy: %w", err)
	}
	manifest := standardAuthoringSessionManifest{
		Format:                        standardAuthoringLaunchSessionManifestFormat,
		Version:                       standardAuthoringLaunchSessionManifestVersion,
		LifecycleOperationID:          operationID,
		PreparationFingerprint:        preparation.PreparationFingerprint,
		RepositoryURL:                 source.RepositoryURL,
		CommitSHA:                     source.CommitSHA,
		RequestedSourceID:             preparation.RequestedSourceID,
		SourceID:                      source.ID,
		AuthoringSessionID:            sessionID,
		TargetTaskID:                  task.ID,
		SourceSnapshotDigest:          source.SnapshotContentDigest,
		SourceSnapshotSchemaVersion:   source.SnapshotSchemaVersion,
		EnvironmentPolicyArtifactID:   string(environmentPolicy.ArtifactID),
		EnvironmentPolicy:             append(json.RawMessage(nil), environmentPolicy.CanonicalJSON...),
		EnvironmentPolicyDigest:       environmentPolicy.ContentDigest,
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

func standardAuthoringEnvironmentPolicyInputFromSession(session store.AuthoringSession) (standardAuthoringEnvironmentPolicyInput, error) {
	var manifest standardAuthoringSessionManifest
	if err := decodeStrictJSON(session.SessionManifestJSON, &manifest); err != nil {
		return standardAuthoringEnvironmentPolicyInput{}, fmt.Errorf("decode Standard authoring session manifest: %w", err)
	}
	if manifest.Format != standardAuthoringLaunchSessionManifestFormat || manifest.Version != standardAuthoringLaunchSessionManifestVersion ||
		manifest.AuthoringSessionID != session.ID || manifest.SourceID != session.SourceID || manifest.TargetTaskID != session.TargetTaskID ||
		manifest.SourceSnapshotSchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		return standardAuthoringEnvironmentPolicyInput{}, fmt.Errorf("Standard authoring session manifest is not a current immutable environment contract")
	}
	policy, err := workflowadapter.ParseStandardAuthoringEnvironmentPolicyJSON(manifest.EnvironmentPolicy)
	if err != nil {
		return standardAuthoringEnvironmentPolicyInput{}, fmt.Errorf("decode Standard authoring session environment policy: %w", err)
	}
	input, err := newStandardAuthoringEnvironmentPolicyInput(manifest.EnvironmentPolicyArtifactID, policy)
	if err != nil {
		return standardAuthoringEnvironmentPolicyInput{}, err
	}
	if !bytes.Equal(manifest.EnvironmentPolicy, input.CanonicalJSON) || manifest.EnvironmentPolicyDigest != input.ContentDigest {
		return standardAuthoringEnvironmentPolicyInput{}, fmt.Errorf("Standard authoring session environment policy is not canonical")
	}
	return input, nil
}

// validateStandardAuthoringEnvironmentPolicyBindings proves the policy is not
// merely recorded in a session manifest: every catalog stage that declares
// the intrinsic port is bound to the exact canonical policy artifact in the
// frozen execution spec.  This is deliberately shared by launch admission and
// worker-time verification so an alternate StartAuthoringRun caller cannot
// omit or substitute the policy after the session exists.
func validateStandardAuthoringEnvironmentPolicyBindings(specification workflowadapter.RunExecutionSpec, policy standardAuthoringEnvironmentPolicyInput) error {
	if err := specification.Validate(); err != nil {
		return fmt.Errorf("validate Standard authoring execution specification: %w", err)
	}
	template, err := workflowadapter.ResolveWorkflowTemplate(specification.Template)
	if err != nil {
		return err
	}
	if !template.Reference().Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return fmt.Errorf("environment policy binding requires the Standard authoring template")
	}
	policyReference := policy.artifactReference()
	matchedReference := false
	for _, reference := range specification.References.Artifacts {
		if reference.ID != policyReference.ID {
			continue
		}
		if reference != policyReference {
			return fmt.Errorf("Standard authoring environment policy artifact reference differs from the session contract")
		}
		matchedReference = true
	}
	if !matchedReference {
		return fmt.Errorf("Standard authoring execution specification does not reference the session environment policy")
	}
	consumers := 0
	for _, stage := range template.Catalog.Stages {
		usesPolicy := false
		for _, input := range stage.Inputs {
			if input.Name == workflowadapter.StandardAuthoringEnvironmentPolicyArtifact {
				if input.SchemaVersion != workflowadapter.StandardAuthoringEnvironmentPolicySchemaVersion || !input.Required {
					return fmt.Errorf("Standard authoring stage %q has an invalid environment policy contract", stage.Key)
				}
				usesPolicy = true
			}
		}
		if !usesPolicy {
			continue
		}
		consumers++
		resolution, err := specification.ResolveStageOperation(stage.Key)
		if err != nil {
			return err
		}
		found := false
		for _, binding := range resolution.ArtifactInputs {
			if binding.Port != workflowadapter.StandardAuthoringEnvironmentPolicyArtifact {
				continue
			}
			if found || binding.ArtifactID != policyReference.ID {
				return fmt.Errorf("Standard authoring stage %q environment policy binding differs from the session contract", stage.Key)
			}
			found = true
		}
		if !found {
			return fmt.Errorf("Standard authoring stage %q does not bind the session environment policy", stage.Key)
		}
	}
	if consumers == 0 {
		return fmt.Errorf("Standard authoring template has no environment policy consumers")
	}
	return nil
}

func verifyStandardAuthoringLaunchSession(session store.AuthoringSession, source store.AuthoringSource, task store.TaskV2, operationID string, preparation standardAuthoringLaunchPreparation, frozen standardAuthoringFrozenDefinition) error {
	if session.SourceID != source.ID || session.TargetTaskID != task.ID || session.WorkflowTemplateID != workflowadapter.StandardAuthoringWorkflowTemplateID || session.WorkflowTemplateVersion != workflowadapter.StandardAuthoringWorkflowTemplateVersion {
		return fmt.Errorf("%w: persisted Standard authoring session binding", store.ErrIdempotencyConflict)
	}
	var manifest standardAuthoringSessionManifest
	if err := decodeStrictJSON(session.SessionManifestJSON, &manifest); err != nil {
		return fmt.Errorf("decode persisted Standard authoring session manifest: %w", err)
	}
	environmentPolicy, err := preparation.EnvironmentPolicyInput()
	if err != nil {
		return err
	}
	if manifest.Format != standardAuthoringLaunchSessionManifestFormat || manifest.Version != standardAuthoringLaunchSessionManifestVersion ||
		manifest.LifecycleOperationID != operationID || manifest.PreparationFingerprint != preparation.PreparationFingerprint ||
		manifest.RepositoryURL != source.RepositoryURL || manifest.CommitSHA != source.CommitSHA || manifest.RequestedSourceID != preparation.RequestedSourceID || manifest.SourceID != source.ID ||
		manifest.AuthoringSessionID != session.ID || manifest.TargetTaskID != task.ID || manifest.SourceSnapshotDigest != source.SnapshotContentDigest ||
		manifest.SourceSnapshotSchemaVersion != source.SnapshotSchemaVersion || manifest.ProfileFingerprint != frozen.ProfileFingerprint ||
		manifest.EnvironmentPolicyArtifactID != string(environmentPolicy.ArtifactID) || !bytes.Equal(manifest.EnvironmentPolicy, environmentPolicy.CanonicalJSON) || manifest.EnvironmentPolicyDigest != environmentPolicy.ContentDigest ||
		manifest.ExecutionSpecFingerprint != frozen.ExecutionSpecFingerprint || manifest.DefinitionFingerprint != frozen.Fingerprint ||
		string(manifest.DeploymentCatalogReceipt) != string(frozen.DeploymentCatalogReceipt) ||
		!sameDeploymentCatalogLockIdentity(manifest.DeploymentCatalogLockIdentity, frozen.DeploymentCatalogLockIdentity) {
		return fmt.Errorf("%w: persisted Standard authoring session definition", store.ErrIdempotencyConflict)
	}
	if _, err := standardAuthoringEnvironmentPolicyInputFromSession(session); err != nil {
		return fmt.Errorf("%w: persisted Standard authoring session environment policy: %v", store.ErrIdempotencyConflict, err)
	}
	if err := validateStandardAuthoringEnvironmentPolicyBindings(frozen.ExecutionSpec, environmentPolicy); err != nil {
		return fmt.Errorf("%w: persisted Standard authoring session execution policy binding: %v", store.ErrIdempotencyConflict, err)
	}
	return nil
}

func sameDeploymentCatalogLockIdentity(left, right *stageprovider.DeploymentOperationCatalogLockIdentity) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// standardAuthoringLaunchRequest is the complete caller-selected semantic
// input frozen before source capture. It intentionally excludes all derived
// IDs, snapshot bytes, provider selections, model configuration, and mutable
// deployment facts. A completed operation can therefore replay its durable
// receipt despite later deployment drift, while a prepared operation compares
// its separately persisted deployment preparation before it can capture.
type standardAuthoringLaunchRequest struct {
	RepositoryURL string `json:"repository_url"`
	CommitSHA     string `json:"commit_sha"`
	BaseImage     string `json:"base_image"`
	Slug          string `json:"slug"`
	Title         string `json:"title"`
	MetadataJSON  string `json:"metadata_json"`
}

// CatalogStandardAuthoringRunDefinitionProvider derives the full source-session
// execution spec from a static deployment catalog and an already explicit
// profile. It is intentionally not a generic catalog compiler: it only emits
// harbor.standard-authoring@1.1.0 and has no caller-selectable operation,
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

// StandardAuthoringStaticRunDefinition returns only deployment-owned facts
// that do not depend on a captured source snapshot. The app service resolves
// the template-scoped catalog receipt and lock around this profile before it
// permits a Git remote contact.
func (provider *CatalogStandardAuthoringRunDefinitionProvider) StandardAuthoringStaticRunDefinition(context.Context) (StandardAuthoringStaticRunDefinition, error) {
	if provider == nil || provider.catalog == nil || !provider.catalog.Template().Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return StandardAuthoringStaticRunDefinition{}, ErrStandardAuthoringLaunchUnavailable
	}
	receipt, err := provider.catalog.CanonicalReceiptJSON()
	if err != nil {
		return StandardAuthoringStaticRunDefinition{}, fmt.Errorf("canonicalize Standard authoring provider catalog receipt: %w", err)
	}
	return StandardAuthoringStaticRunDefinition{Profile: provider.profile.Clone(), DeploymentCatalogReceipt: receipt}, nil
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
	coordinate, err := (StandardAuthoringSourceCoordinate{RepositoryURL: subject.RepositoryURL, CommitSHA: subject.CommitSHA}).Canonical()
	if err != nil || coordinate.RepositoryURL != subject.RepositoryURL || coordinate.CommitSHA != subject.CommitSHA || subject.SourceSnapshotSchema != StandardAuthoringSourceSnapshotSchemaVersion {
		return StandardAuthoringRunDefinition{}, fmt.Errorf("Standard authoring definition source identity is invalid")
	}
	catalog := provider.catalog.Catalog()
	specification, err := buildCatalogStandardAuthoringExecutionSpec(catalog, subject)
	if err != nil {
		return StandardAuthoringRunDefinition{}, err
	}
	receipt, err := provider.catalog.CanonicalReceiptJSON()
	if err != nil {
		return StandardAuthoringRunDefinition{}, fmt.Errorf("canonicalize Standard authoring provider catalog receipt: %w", err)
	}
	return StandardAuthoringRunDefinition{Profile: provider.profile.Clone(), ExecutionSpec: specification, DeploymentCatalogReceipt: receipt}, nil
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
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingRepoAnalyze:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTaskDesign:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTaskReview:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingGenerateTaskFiles:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingInstructionGen:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTaskTOMLGen:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingDockerfileGen:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingContentReview:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingSolveGen:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTestGen:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTestsAnalysis:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingSolutionReview:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingMaterializeTask:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	default:
		return nil, fmt.Errorf("Standard authoring catalog has unsupported stage binding type %q", base.Type)
	}
}

var _ StandardAuthoringRunDefinitionProvider = (*CatalogStandardAuthoringRunDefinitionProvider)(nil)
var _ StandardAuthoringStaticRunDefinitionProvider = (*CatalogStandardAuthoringRunDefinitionProvider)(nil)
