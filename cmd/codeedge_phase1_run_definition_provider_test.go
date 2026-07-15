package cmd

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	codeEdgePhase1DefinitionTestTaskID       = "018f0a73-3b49-7000-8000-000000000301"
	codeEdgePhase1DefinitionTestRevisionID   = "018f0a73-3b49-7000-8000-000000000302"
	codeEdgePhase1DefinitionTestAuthoringRun = "018f0a73-3b49-7000-8000-000000000303"
	codeEdgePhase1DefinitionTestSource       = "018f0a73-3b49-7000-8000-000000000304"
	codeEdgePhase1DefinitionTestSession      = "018f0a73-3b49-7000-8000-000000000305"
	codeEdgePhase1DefinitionTestSnapshot     = "018f0a73-3b49-7000-8000-000000000306"
	codeEdgePhase1DefinitionTestDigest       = "harbor.task.v2:sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestCodeEdgePhase1DefinitionProviderBuildsOnlyLockOwnedParentDefinition(t *testing.T) {
	verifier, lock := codeEdgePhase1DefinitionVerifierFixture(t)
	provider, err := newCodeEdgePhase1RunDefinitionProvider(verifier)
	if err != nil {
		t.Fatalf("construct parent definition provider: %v", err)
	}
	request := codeEdgePhase1DefinitionRequest()
	definition, err := provider.DefinitionForCodeEdgePhase1Run(context.Background(), request)
	if err != nil {
		t.Fatalf("build parent definition: %v", err)
	}
	if err := definition.Profile.Validate(); err != nil {
		t.Fatalf("validate lock-owned parent profile: %v", err)
	}
	if err := definition.ExecutionSpec.Validate(); err != nil {
		t.Fatalf("validate lock-owned parent specification: %v", err)
	}
	if !definition.Profile.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) || !definition.ExecutionSpec.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		t.Fatalf("parent definition templates = %#v/%#v", definition.Profile.Template, definition.ExecutionSpec.Template)
	}
	if !reflect.DeepEqual(definition.Profile, lock.CodeEdgePhase1ExecutionProfile.Profile) {
		t.Fatal("parent profile was not copied from the verified lock")
	}
	if definition.ExecutionSpec.CodeEdgeFinalCompliancePolicy == nil || !reflect.DeepEqual(*definition.ExecutionSpec.CodeEdgeFinalCompliancePolicy, lock.CodeEdgePhase1FinalCompliancePolicy.Policy) {
		t.Fatal("parent final compliance policy was not copied from the verified lock")
	}
	if len(definition.ExecutionSpec.Stages) != len(workflowadapter.CodeEdgePhase1StageOrder()) {
		t.Fatalf("parent stage count = %d, want %d", len(definition.ExecutionSpec.Stages), len(workflowadapter.CodeEdgePhase1StageOrder()))
	}
	if len(definition.ExecutionSpec.References.Artifacts) != 0 {
		t.Fatal("parent definition pre-bound an untrusted artifact instead of leaving task_snapshot for managed Run input materialization")
	}
	for _, stageKey := range workflowadapter.CodeEdgePhase1StageOrder() {
		resolution, resolveErr := definition.ExecutionSpec.ResolveStageOperation(stageKey)
		if resolveErr != nil {
			t.Fatalf("resolve parent stage %q: %v", stageKey, resolveErr)
		}
		if resolution.Template != workflowadapter.CodeEdgePhase1TemplateReference() || resolution.Checkout.RevisionID != request.RevisionID || resolution.Checkout.RevisionDigest != request.RevisionDigest {
			t.Fatalf("parent stage %q did not seal the selected TaskRevision: %#v", stageKey, resolution)
		}
		if _, err := verifier.VerifyStageOperation(resolution); err != nil {
			t.Fatalf("parent stage %q differs from verified lock: %v", stageKey, err)
		}
		if len(resolution.ArtifactInputs) != 0 {
			t.Fatalf("parent stage %q pre-bound mutable lineage inputs: %#v", stageKey, resolution.ArtifactInputs)
		}
	}

	// Authoring coordinates and the source artifact are lineage checks owned by
	// StandardAuthoringHandoffService. They cannot influence the parent profile,
	// operation, resource, or final-compliance policy selected here.
	changedLineage := request
	changedLineage.AuthoringRunID = "018f0a73-3b49-7000-8000-000000000307"
	changedLineage.AuthoringSourceID = "018f0a73-3b49-7000-8000-000000000308"
	changedLineage.AuthoringSessionID = "018f0a73-3b49-7000-8000-000000000309"
	changedLineage.TaskSnapshot.ID = "018f0a73-3b49-7000-8000-000000000310"
	changedLineage.TaskSnapshot.ContentDigest = workflowkit.SHA256Fingerprint([]byte("another-handoff-snapshot"))
	again, err := provider.DefinitionForCodeEdgePhase1Run(context.Background(), changedLineage)
	if err != nil {
		t.Fatalf("build parent definition with independently valid lineage: %v", err)
	}
	if !reflect.DeepEqual(again.Profile, definition.Profile) || !reflect.DeepEqual(again.ExecutionSpec, definition.ExecutionSpec) {
		t.Fatal("parent definition changed with Standard/evaluator lineage; it must be lock-owned")
	}

	// Returned values are defensive copies; a caller cannot mutate process-held
	// deployment materials before a later handoff.
	definition.Profile.ControlGracePeriod += time.Second
	definition.ExecutionSpec.CodeEdgeFinalCompliancePolicy.Version = "forged"
	definition.ExecutionSpec.Stages[0] = workflowadapter.RepoPrepareBinding{StageBindingBase: workflowadapter.StageBindingBase{}}
	replayed, err := provider.DefinitionForCodeEdgePhase1Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed.Profile, again.Profile) || !reflect.DeepEqual(replayed.ExecutionSpec, again.ExecutionSpec) {
		t.Fatal("caller mutation leaked into lock-owned parent definition")
	}
}

func TestCodeEdgePhase1DefinitionProviderRejectsInvalidRequestAndUnapprovedVerifier(t *testing.T) {
	if _, err := newCodeEdgePhase1RunDefinitionProvider(nil); err == nil {
		t.Fatal("accepted a missing verified parent catalog/lock resolver")
	}
	verifier, lock := codeEdgePhase1DefinitionVerifierFixture(t)
	provider, err := newCodeEdgePhase1RunDefinitionProvider(verifier)
	if err != nil {
		t.Fatal(err)
	}
	invalid := codeEdgePhase1DefinitionRequest()
	invalid.TaskID = "not-a-uuid"
	if _, err := provider.DefinitionForCodeEdgePhase1Run(context.Background(), invalid); err == nil {
		t.Fatal("accepted an invalid sealed TaskRevision identity")
	}
	invalid = codeEdgePhase1DefinitionRequest()
	invalid.TaskSnapshot.SchemaVersion = "other.snapshot.v1"
	if _, err := provider.DefinitionForCodeEdgePhase1Run(context.Background(), invalid); err == nil {
		t.Fatal("accepted an unapproved handoff task snapshot schema")
	}

	// The verified resolver is required before construction. A raw parent lock
	// missing either typed definition material is rejected by the lock layer,
	// leaving no way to pass an ad-hoc profile or policy to this provider.
	missingProfile := lock.Clone()
	missingProfile.CodeEdgePhase1ExecutionProfile = nil
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(codeEdgePhase1DefinitionCatalogFixture(t), missingProfile); err == nil {
		t.Fatal("verified resolver accepted a parent lock without its typed profile")
	}
	missingPolicy := lock.Clone()
	missingPolicy.CodeEdgePhase1FinalCompliancePolicy = nil
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(codeEdgePhase1DefinitionCatalogFixture(t), missingPolicy); err == nil {
		t.Fatal("verified resolver accepted a parent lock without its typed final compliance policy")
	}
	missingPreflight := lock.Clone()
	missingPreflight.CodeEdgePhase1PreflightProfile = nil
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(codeEdgePhase1DefinitionCatalogFixture(t), missingPreflight); err == nil {
		t.Fatal("verified resolver accepted a parent lock without its typed preflight profile")
	}
}

func codeEdgePhase1DefinitionRequest() app.CodeEdgePhase1RunDefinitionRequest {
	return app.CodeEdgePhase1RunDefinitionRequest{
		TaskID: codeEdgePhase1DefinitionTestTaskID, RevisionID: codeEdgePhase1DefinitionTestRevisionID,
		RevisionDigest: workflowkit.SubjectDigest(codeEdgePhase1DefinitionTestDigest),
		AuthoringRunID: codeEdgePhase1DefinitionTestAuthoringRun, AuthoringSourceID: codeEdgePhase1DefinitionTestSource,
		AuthoringSessionID: codeEdgePhase1DefinitionTestSession,
		TaskSnapshot: workflowadapter.ArtifactReference{
			ID: codeEdgePhase1DefinitionTestSnapshot, ContentDigest: workflowkit.SHA256Fingerprint([]byte("sealed-standard-handoff-snapshot")), SchemaVersion: "harbor.artifact.v1",
		},
	}
}

func codeEdgePhase1DefinitionVerifierFixture(t *testing.T) (*stageprovider.DeploymentOperationCatalogLockResolver, stageprovider.DeploymentOperationCatalogLock) {
	t.Helper()
	catalog := codeEdgePhase1DefinitionCatalogFixture(t)
	lock := stageprovider.DeploymentOperationCatalogLock{
		Format: stageprovider.DeploymentOperationCatalogLockFormat, Version: stageprovider.DeploymentOperationCatalogLockVersion,
		LockID: "codeedge-phase1-definition-provider-test", LockVersion: "test-v1", CatalogReceipt: catalog.Receipt(),
		HarborFlowBuild: stageprovider.HarborFlowBuildIdentity{
			Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0", Commit: strings.Repeat("d", 40),
			ContentSHA256: workflowkit.SHA256Fingerprint([]byte("codeedge-phase1-definition-provider-test")),
		},
		CodeEdgePhase1ExecutionProfile:      &stageprovider.CodeEdgePhase1ExecutionProfileLock{Profile: codeEdgePhase1DefinitionProviderProfile(t)},
		CodeEdgePhase1PreflightProfile:      &stageprovider.CodeEdgePhase1PreflightProfileLock{Profile: codeEdgePhase1DefinitionProviderPreflightProfile(t)},
		CodeEdgePhase1FinalCompliancePolicy: &stageprovider.CodeEdgePhase1FinalCompliancePolicyLock{Policy: codeEdgePhase1DefinitionProviderPolicy()},
		Operations:                          make([]stageprovider.DeploymentOperationCatalogLockRecord, 0, len(workflowadapter.CodeEdgePhase1StageOrder())),
	}
	for _, registration := range catalog.Catalog().Operations {
		lock.Operations = append(lock.Operations, codeEdgePhase1DefinitionProviderLockRecord(registration))
	}
	verifier, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatalf("construct verified parent catalog/lock: %v", err)
	}
	return verifier, verifier.Lock()
}

func codeEdgePhase1DefinitionCatalogFixture(t *testing.T) *stageprovider.DeploymentOperationCatalogResolver {
	t.Helper()
	workflow := workflowadapter.CodeEdgePhase1WorkflowTemplate()
	provider := workflowadapter.ProviderReference{ID: stageprovider.CodeEdgePhase1ProviderID, Kind: stageprovider.CodeEdgePhase1ProviderKind, Version: stageprovider.CodeEdgePhase1ProviderVersion}
	runtime := workflowadapter.RuntimeReference{ID: "codeedge-phase1-definition-runtime", Kind: "controlled", Version: "1"}
	operations := make([]stageprovider.DeploymentOperationRegistration, 0, len(workflow.Catalog.Stages))
	for _, stage := range workflow.Catalog.Stages {
		operation := workflowadapter.StageOperationBinding{ProviderID: provider.ID, OperationID: "codeedge.phase1.definition." + string(stage.Key), Version: "1"}
		if stage.Gate != nil {
			operation.Payload = workflowadapter.DurableReviewOperationPayload{PolicyID: "codeedge-phase1-review-" + string(stage.Key)}
		} else {
			operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "codeedge-phase1-command-" + string(stage.Key), Arguments: []string{}}
		}
		operations = append(operations, stageprovider.DeploymentOperationRegistration{
			Stage:    stageprovider.DeploymentStageContract{Key: stage.Key, Type: codeEdgePhase1BindingType(stage.Key), Group: stage.Group, Plugin: workflowkit.PluginBinding{ID: stage.Plugin.ID, Version: stage.Plugin.Version}},
			Provider: provider, Operation: operation, Runtime: runtime,
			Checkout: stageprovider.DeploymentCheckoutContract{ID: "codeedge-phase1-definition-checkout", Purpose: "parent-task-snapshot"},
			Secrets:  []workflowadapter.SecretReference{},
		})
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(stageprovider.DeploymentOperationCatalog{
		Format: stageprovider.DeploymentOperationCatalogFormat, Version: stageprovider.DeploymentOperationCatalogVersion,
		CatalogID: "codeedge-phase1-definition-provider-test", CatalogVersion: "test-v1",
		Template: workflowadapter.CodeEdgePhase1TemplateReference(), Operations: operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func codeEdgePhase1DefinitionProviderLockRecord(registration stageprovider.DeploymentOperationRegistration) stageprovider.DeploymentOperationCatalogLockRecord {
	record := stageprovider.DeploymentOperationCatalogLockRecord{
		Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
		Checkout: registration.Checkout, Secrets: []workflowadapter.SecretReference{},
		PromptContentFingerprint: workflowkit.SHA256Fingerprint([]byte("prompt:" + string(registration.Stage.Key))),
		SchemaContentFingerprint: workflowkit.SHA256Fingerprint([]byte("schema:" + string(registration.Stage.Key))),
		ExecutionKind:            registration.Operation.Payload.Kind(),
	}
	switch payload := registration.Operation.Payload.(type) {
	case workflowadapter.LocalCommandOperationPayload:
		record.LocalExecutable = &stageprovider.LocalExecutableLock{
			CommandID: payload.CommandID, AbsolutePath: "/opt/harbor/codeedge-phase1/" + payload.CommandID,
			Version: "1", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("binary:" + payload.CommandID)),
		}
	case workflowadapter.DurableReviewOperationPayload:
		record.DurableReviewPolicy = &stageprovider.DurableReviewPolicyLock{PolicyID: payload.PolicyID, Version: "1"}
	default:
		panic("unexpected parent definition test payload")
	}
	return record
}

func codeEdgePhase1DefinitionProviderProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	workflow := workflowadapter.CodeEdgePhase1WorkflowTemplate()
	profile := workflowadapter.ExecutionProfile{
		Template: workflowadapter.CodeEdgePhase1TemplateReference(), ID: "codeedge-phase1-definition-provider-test", Version: "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, ControlGracePeriod: time.Minute,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: 5 * time.Minute},
		Stages:                  make([]workflowadapter.StageBudget, 0, len(workflow.Catalog.Stages)),
	}
	for _, stage := range workflow.Catalog.Stages {
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{StageKey: stage.Key, Budget: workflowkit.ExecutionBudget{
			TurnTimeout: time.Minute, MaxTurns: max(1, stage.RequiredTurns), AttemptTimeout: time.Minute, MaxAttempts: 1, MaxElapsed: time.Minute,
		}})
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("build complete parent profile: %v", err)
	}
	return profile
}

func codeEdgePhase1DefinitionProviderPreflightProfile(t *testing.T) codeedge.Profile {
	t.Helper()
	profile := codeedge.Profile{
		Metadata: codeedge.MetadataFieldMapping{
			CodeLang:    codeedge.TOMLPath{"metadata", "code_lang"},
			TaskType:    codeedge.TOMLPath{"metadata", "task_type"},
			Application: codeedge.TOMLPath{"metadata", "application"},
			IsZeroToOne: codeedge.TOMLPath{"metadata", "is_0_to_1"},
			GitHubURL:   codeedge.TOMLPath{"metadata", "github_url"},
			CommitID:    codeedge.TOMLPath{"metadata", "commit_id"},
		},
		ProtectedEnvironmentVariables: []string{
			"ANTHROPIC_AUTH_TOKEN",
			"ANTHROPIC_BASE_URL",
			"QWEN_HARBOR_BASE_URL",
		},
	}
	if err := codeedge.ValidateProfile(profile); err != nil {
		t.Fatalf("build complete parent preflight profile: %v", err)
	}
	return profile
}

func codeEdgePhase1DefinitionProviderPolicy() codeedge.FinalCompliancePolicy {
	maximumPassingTrials := 1
	qwen := codeedge.EvaluationPolicy{
		ID: "codeedge.qwen.pass-at-four", Version: "1", HarborEvidenceFormat: codeedge.HarborRunBundleV018Format,
		Evaluator:         codeedge.EvaluatorIdentity{ProfileID: "codeedge-qwen-profile", ProfileVersion: "1", AgentName: "codeedge-agent", AgentVersion: "1", ModelName: "qwen-model", ModelProvider: "controlled"},
		LogicalTrialCount: 4, PassRewardKey: "reward", PassRewardAtLeast: 1, MaxPassingTrials: &maximumPassingTrials,
		MinimumAverageTurns: 20, ScreenshotMediaType: "image/png", FailureClassifierID: "codeedge-infra", FailureClassifierVersion: "1",
		InfraExceptionTypes: []string{"DockerBuildError", "NetworkError"},
	}
	opus := qwen.Clone()
	opus.ID = "codeedge.opus.reference"
	opus.Evaluator.ProfileID = "codeedge-opus-profile"
	opus.Evaluator.ModelName = "opus-model"
	opus.MaxPassingTrials = nil
	return codeedge.FinalCompliancePolicy{
		ID: "codeedge.phase1.final-compliance", Version: "1", QwenPolicy: qwen, OpusPolicy: opus,
		SubmissionCheckerID: "codeedge.submission-check", SubmissionCheckerVersion: "1",
		SubmissionReportSchemaVersion: workflowadapter.CodeEdgeSubmissionReportSchemaVersion,
	}
}
