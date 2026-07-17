package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// StartAuthoringRunRequest starts the closed pre-materialization Standard
// workflow.  It deliberately accepts an already-created AuthoringSession:
// source capture and draft Task ownership are separate, auditable lifecycle
// actions, while this operation freezes only one executable session contract.
//
// The request has no TaskID or RevisionID.  Supplying either through the
// execution specification is rejected rather than silently using the draft
// Task as a fabricated revision.
type StartAuthoringRunRequest struct {
	ID                            string
	AuthoringSessionID            string
	Profile                       workflowadapter.ExecutionProfile
	ExecutionSpec                 workflowadapter.RunExecutionSpec
	ProfileFingerprint            workflowkit.Fingerprint
	ExecutionSpecFingerprint      workflowkit.Fingerprint
	DeploymentCatalogReceipt      []byte
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity
	Trigger                       string
	ExecutionEpoch                int
	Actor                         string
	Reason                        string
}

// StartAuthoringRun freezes the same profile/spec/catalog/lock/worker
// contract as StartRun, but stores an AuthoringSource/AuthoringSession subject
// and no synthetic TaskRevision.  The generic FrozenExecutionRuntime is the
// only executor for both paths.
func (service *RunService) StartAuthoringRun(ctx context.Context, request StartAuthoringRunRequest) (store.WorkflowRun, error) {
	if service == nil || service.core == nil {
		return store.WorkflowRun{}, fmt.Errorf("run service is not configured")
	}
	if err := store.ValidateUUIDv7(request.AuthoringSessionID); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("authoring session ID: %w", err)
	}
	if strings.TrimSpace(request.Trigger) == "" {
		return store.WorkflowRun{}, fmt.Errorf("run trigger is required")
	}
	if request.ExecutionEpoch < 0 {
		return store.WorkflowRun{}, fmt.Errorf("execution epoch cannot be negative")
	}
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(request.Profile.Template) ||
		!request.ExecutionSpec.Template.Equal(request.Profile.Template) {
		return store.WorkflowRun{}, fmt.Errorf("authoring Run requires one installed Standard authoring template")
	}
	if _, err := resolveFrozenRunTemplate(request.Profile, request.ExecutionSpec); err != nil {
		return store.WorkflowRun{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := service.core.store.GetAuthoringSession(ctx, request.AuthoringSessionID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if session == nil {
		return store.WorkflowRun{}, fmt.Errorf("%w: authoring session %s", ErrLifecycleNotFound, request.AuthoringSessionID)
	}
	source, err := service.core.store.GetAuthoringSource(ctx, session.SourceID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if source == nil {
		return store.WorkflowRun{}, fmt.Errorf("%w: authoring source %s", ErrLifecycleNotFound, session.SourceID)
	}
	if session.WorkflowTemplateID != request.Profile.Template.ID || session.WorkflowTemplateVersion != request.Profile.Template.Version {
		return store.WorkflowRun{}, fmt.Errorf("authoring session template does not match the closed Standard authoring template")
	}
	if err := validateAuthoringRunExecutionSpec(request.ExecutionSpec, *source, *session, service.core.operationResolver, service.core); err != nil {
		return store.WorkflowRun{}, err
	}

	requestedCanonical, requestedFingerprint, err := canonicalExecutionSpec(request.ExecutionSpec)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if request.ExecutionSpecFingerprint != "" && request.ExecutionSpecFingerprint != requestedFingerprint {
		return store.WorkflowRun{}, fmt.Errorf("%w: supplied execution specification fingerprint does not match canonical specification", store.ErrIdempotencyConflict)
	}
	catalogReceipt, err := service.core.resolveStartRunDeploymentCatalogReceipt(request.ExecutionSpec.Template, request.DeploymentCatalogReceipt)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("freeze deployment catalog receipt for authoring Run: %w", err)
	}
	lockIdentity, err := service.core.resolveStartRunDeploymentCatalogLockIdentity(request.ExecutionSpec.Template, request.DeploymentCatalogLockIdentity)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("freeze deployment catalog lock identity for authoring Run: %w", err)
	}
	template, err := workflowadapter.ResolveWorkflowTemplate(request.Profile.Template)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	resolved, err := template.Compile(request.Profile)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("compile explicit authoring execution profile: %w", err)
	}
	profileCanonical, err := request.Profile.CanonicalJSON()
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("canonicalize authoring execution profile: %w", err)
	}
	profileFingerprint, err := request.Profile.Fingerprint()
	if err != nil || profileFingerprint != resolved.ExecutionProfileFingerprint {
		return store.WorkflowRun{}, fmt.Errorf("authoring execution profile fingerprint does not match compiled workflow")
	}
	if request.ProfileFingerprint != "" && request.ProfileFingerprint != resolved.ExecutionProfileFingerprint {
		return store.WorkflowRun{}, fmt.Errorf("%w: supplied authoring execution profile fingerprint does not match compiled profile", store.ErrIdempotencyConflict)
	}
	plan, err := workflowkit.CompileDependencyExecutionPlan(resolved.Descriptor)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("compile initial authoring dependency execution plan: %w", err)
	}

	runID := strings.TrimSpace(request.ID)
	if runID == "" {
		runID, err = store.NewUUIDv7()
		if err != nil {
			return store.WorkflowRun{}, fmt.Errorf("allocate authoring workflow run ID: %w", err)
		}
	}
	if err := store.ValidateUUIDv7(runID); err != nil {
		return store.WorkflowRun{}, err
	}
	if existing, err := service.core.store.GetWorkflowRun(ctx, runID); err != nil {
		return store.WorkflowRun{}, err
	} else if existing != nil {
		if err := service.validateReplayedAuthoringWorkflowRun(ctx, *existing, request, *source, *session, resolved, profileCanonical, requestedCanonical, requestedFingerprint, plan, catalogReceipt, lockIdentity); err != nil {
			return store.WorkflowRun{}, err
		}
		manifest, err := decodeRunManifest(*existing)
		if err != nil {
			return store.WorkflowRun{}, fmt.Errorf("%w: authoring workflow Run %s manifest: %v", store.ErrIdempotencyConflict, existing.ID, err)
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
		return store.WorkflowRun{}, fmt.Errorf("create authoring Run parent: %w", err)
	}
	if err := os.Mkdir(runDirectory, 0o750); err != nil {
		if errors.Is(err, os.ErrExist) {
			return store.WorkflowRun{}, fmt.Errorf("authoring Run directory already exists without durable Run %s", runID)
		}
		return store.WorkflowRun{}, fmt.Errorf("create authoring Run directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(runDirectory)
		}
	}()
	manifest := runManifest{
		Format:               "harbor.workflow-run-manifest.v2",
		RunID:                runID,
		SubjectKind:          store.WorkflowRunSubjectAuthoringSession,
		SubjectID:            source.ID,
		SubjectRevisionID:    session.ID,
		SubjectDigest:        source.SnapshotContentDigest,
		AuthoringSessionID:   session.ID,
		Resolved:             resolved.Clone(),
		InitialExecutionPlan: plan.Clone(),
		Inputs: &runManifestInputs{
			Format:                            runManifestInputsFormat,
			ProfileFingerprint:                resolved.ExecutionProfileFingerprint,
			RequestedExecutionSpecFingerprint: requestedFingerprint,
			ExecutionSpecFingerprint:          requestedFingerprint,
		},
		ExecutionSpec:                 append(json.RawMessage(nil), requestedCanonical...),
		DeploymentCatalogReceipt:      append(json.RawMessage(nil), catalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(lockIdentity),
		Created:                       service.core.now().UTC(),
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("encode authoring Run manifest: %w", err)
	}
	dispatch, _, err := initialWorkflowRunDispatch(runID, string(resolved.DefinitionFingerprint), manifest)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if err := writeNewBytes(filepath.Join(runDirectory, runExecutionProfileFileName), profileCanonical); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("write authoring execution profile: %w", err)
	}
	if err := writeNewBytes(filepath.Join(runDirectory, runExecutionSpecFileName), requestedCanonical); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("write authoring execution specification: %w", err)
	}
	if len(catalogReceipt) != 0 {
		if err := writeNewBytes(filepath.Join(runDirectory, deploymentCatalogReceiptFileName), catalogReceipt); err != nil {
			return store.WorkflowRun{}, fmt.Errorf("write authoring deployment catalog receipt: %w", err)
		}
	}
	if lockIdentity != nil {
		canonicalLock, err := canonicalDeploymentCatalogLockIdentity(*lockIdentity)
		if err != nil {
			return store.WorkflowRun{}, err
		}
		if err := writeNewBytes(filepath.Join(runDirectory, deploymentCatalogLockIdentityFileName), canonicalLock); err != nil {
			return store.WorkflowRun{}, fmt.Errorf("write authoring deployment catalog lock identity: %w", err)
		}
	}
	if err := writeNewJSON(filepath.Join(runDirectory, "run-manifest.json"), manifest); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("write authoring Run manifest: %w", err)
	}
	run, err := service.core.store.CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		ID: runID, AuthoringSessionID: session.ID, WorkflowTemplateID: resolved.TemplateID, WorkflowTemplateVersion: resolved.TemplateVersion,
		ResolvedProfileHash: string(resolved.ExecutionProfileFingerprint), DefinitionHash: string(resolved.DefinitionFingerprint),
		RunManifestJSON: string(encoded), Trigger: request.Trigger, ExecutionEpoch: request.ExecutionEpoch,
		Actor: request.Actor, Reason: request.Reason, Dispatch: &dispatch,
	})
	if err != nil {
		return store.WorkflowRun{}, err
	}
	committed = true
	return run, nil
}

func canonicalExecutionSpec(specification workflowadapter.RunExecutionSpec) ([]byte, workflowkit.Fingerprint, error) {
	canonical, err := specification.CanonicalJSON()
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize authoring execution specification: %w", err)
	}
	fingerprint, err := specification.Fingerprint()
	if err != nil {
		return nil, "", fmt.Errorf("fingerprint authoring execution specification: %w", err)
	}
	return canonical, fingerprint, nil
}

func validateAuthoringRunExecutionSpec(specification workflowadapter.RunExecutionSpec, source store.AuthoringSource, session store.AuthoringSession, resolver workflowadapter.StageOperationResolver, core *lifecycleServiceCore) error {
	selection := specification.Selection
	if selection.Kind != workflowadapter.RunSelectionAuthoringSession || selection.AuthoringSourceID != source.ID || selection.AuthoringSessionID != session.ID || string(selection.AuthoringSourceDigest) != source.SnapshotContentDigest {
		return fmt.Errorf("%w: authoring execution specification selection does not match AuthoringSource/AuthoringSession", store.ErrOptimisticLock)
	}
	if err := validateRunExecutionSpecOperationResolver(specification, resolver); err != nil {
		return err
	}
	environmentPolicy, err := standardAuthoringEnvironmentPolicyInputFromSession(session)
	if err != nil {
		return fmt.Errorf("authoring session environment policy: %w", err)
	}
	if err := validateStandardAuthoringEnvironmentPolicyBindings(specification, environmentPolicy); err != nil {
		return err
	}
	if core == nil {
		return fmt.Errorf("authoring execution specification deployment catalog is not configured")
	}
	if err := core.validateDeploymentCatalogExecutionSpec(specification); err != nil {
		return err
	}
	return nil
}

func (service *RunService) validateReplayedAuthoringWorkflowRun(ctx context.Context, run store.WorkflowRun, request StartAuthoringRunRequest, source store.AuthoringSource, session store.AuthoringSession, resolved workflowadapter.ResolvedWorkflow, profileCanonical, specificationCanonical []byte, specificationFingerprint workflowkit.Fingerprint, plan workflowkit.ExecutionPlan, catalogReceipt []byte, lockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity) error {
	if run.SubjectKind != store.WorkflowRunSubjectAuthoringSession || run.SubjectID != source.ID || run.SubjectRevisionID != session.ID || run.SubjectDigest != source.SnapshotContentDigest || run.AuthoringSessionID != session.ID ||
		run.WorkflowTemplateID != resolved.TemplateID || run.WorkflowTemplateVersion != resolved.TemplateVersion || run.ResolvedProfileHash != string(resolved.ExecutionProfileFingerprint) || run.DefinitionHash != string(resolved.DefinitionFingerprint) || run.Trigger != request.Trigger || run.ExecutionEpoch != request.ExecutionEpoch {
		return fmt.Errorf("%w: authoring workflow Run %s does not match requested immutable definition", store.ErrIdempotencyConflict, run.ID)
	}
	manifest, err := decodeRunManifest(run)
	if err != nil || !manifestMatchesAuthoringExecutionSpec(manifest, specificationCanonical, specificationFingerprint) ||
		!manifestMatchesInitialExecutionPlan(manifest, resolved.Descriptor, plan) || !manifestMatchesDeploymentCatalogReceipt(manifest, catalogReceipt) || !manifestMatchesDeploymentCatalogLockIdentity(manifest, lockIdentity) {
		return fmt.Errorf("%w: authoring workflow Run %s execution specification", store.ErrIdempotencyConflict, run.ID)
	}
	profile, _, err := service.core.verifyRunManagedExecutionInputs(ctx, run)
	if err != nil {
		return fmt.Errorf("%w: authoring workflow Run %s managed execution inputs: %v", store.ErrIdempotencyConflict, run.ID, err)
	}
	canonicalProfile, err := profile.CanonicalJSON()
	if err != nil || string(canonicalProfile) != string(profileCanonical) {
		return fmt.Errorf("%w: authoring workflow Run %s execution profile", store.ErrIdempotencyConflict, run.ID)
	}
	return service.core.verifyRunDeploymentCatalogReceipt(run)
}

func manifestMatchesAuthoringExecutionSpec(manifest runManifest, expectedCanonical []byte, expectedFingerprint workflowkit.Fingerprint) bool {
	if manifest.Inputs == nil || manifest.Inputs.Format != runManifestInputsFormat || manifest.Inputs.ExecutionSpecFingerprint != expectedFingerprint ||
		manifest.Inputs.RequestedExecutionSpecFingerprint != expectedFingerprint || len(manifest.ExecutionSpec) == 0 {
		return false
	}
	return string(manifest.ExecutionSpec) == string(expectedCanonical)
}
