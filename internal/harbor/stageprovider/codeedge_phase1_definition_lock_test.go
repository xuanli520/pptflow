package stageprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCodeEdgePhase1DeploymentLockRequiresTypedParentDefinitionMaterials(t *testing.T) {
	_, lock := codeEdgePhase1DefinitionLockFixture(t)
	if err := lock.Validate(); err != nil {
		t.Fatalf("validate complete parent lock: %v", err)
	}

	t.Run("absent profile", func(t *testing.T) {
		missing := lock.Clone()
		missing.CodeEdgePhase1ExecutionProfile = nil
		if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "execution profile is required") {
			t.Fatalf("missing parent profile error = %v", err)
		}
	})

	t.Run("absent policy", func(t *testing.T) {
		missing := lock.Clone()
		missing.CodeEdgePhase1FinalCompliancePolicy = nil
		if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "final compliance policy is required") {
			t.Fatalf("missing parent policy error = %v", err)
		}
	})

	t.Run("wrong profile template", func(t *testing.T) {
		wrong := lock.Clone()
		wrong.CodeEdgePhase1ExecutionProfile.Profile.Template = workflowadapter.StandardTemplateReference()
		if err := wrong.Validate(); err == nil || !strings.Contains(err.Error(), "must bind") {
			t.Fatalf("wrong parent profile template error = %v", err)
		}
	})

	t.Run("parent materials forbidden on evaluator child", func(t *testing.T) {
		child := lock.Clone()
		child.CatalogReceipt.Template = workflowadapter.CodeEdgeEvaluatorChildTemplateReference()
		if err := child.Validate(); err == nil || !strings.Contains(err.Error(), "only valid for") {
			t.Fatalf("parent materials on evaluator child error = %v", err)
		}
	})
}

func TestCodeEdgePhase1DeploymentLockParentDefinitionMaterialsAreCanonicalAndDefensive(t *testing.T) {
	_, lock := codeEdgePhase1DefinitionLockFixture(t)
	baseline, err := lock.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	profileDrift := lock.Clone()
	profileDrift.CodeEdgePhase1ExecutionProfile.Profile.ControlGracePeriod += time.Second
	if err := profileDrift.Validate(); err != nil {
		t.Fatalf("validate profile drift fixture: %v", err)
	}
	profileFingerprint, err := profileDrift.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if profileFingerprint == baseline {
		t.Fatal("parent execution profile is absent from deployment lock fingerprint")
	}

	policyDrift := lock.Clone()
	policyDrift.CodeEdgePhase1FinalCompliancePolicy.Policy.Version = "2"
	if err := policyDrift.Validate(); err != nil {
		t.Fatalf("validate policy drift fixture: %v", err)
	}
	policyFingerprint, err := policyDrift.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if policyFingerprint == baseline {
		t.Fatal("parent final compliance policy is absent from deployment lock fingerprint")
	}

	canonical, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDeploymentOperationCatalogLockJSON(canonical)
	if err != nil {
		t.Fatalf("parse canonical parent lock: %v", err)
	}
	if parsedFingerprint, fingerprintErr := parsed.Fingerprint(); fingerprintErr != nil || parsedFingerprint != baseline {
		t.Fatalf("round-trip parent lock fingerprint = %q, %v; want %q", parsedFingerprint, fingerprintErr, baseline)
	}

	profile, err := lock.CodeEdgePhase1Profile()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := lock.CodeEdgePhase1FinalCompliance()
	if err != nil {
		t.Fatal(err)
	}
	profile.ControlGracePeriod += time.Second
	policy.Version = "changed"
	againProfile, profileErr := lock.CodeEdgePhase1Profile()
	againPolicy, policyErr := lock.CodeEdgePhase1FinalCompliance()
	if profileErr != nil || policyErr != nil {
		t.Fatalf("read defensive parent definition copies: %v / %v", profileErr, policyErr)
	}
	if againProfile.ControlGracePeriod == profile.ControlGracePeriod || againPolicy.Version == policy.Version {
		t.Fatal("parent definition accessor leaked mutable lock material")
	}
	if !reflect.DeepEqual(againProfile, lock.CodeEdgePhase1ExecutionProfile.Profile) || !reflect.DeepEqual(againPolicy, lock.CodeEdgePhase1FinalCompliancePolicy.Policy) {
		t.Fatal("parent definition accessors did not return the locked definition")
	}
}

func codeEdgePhase1DefinitionLockFixture(t *testing.T) (*DeploymentOperationCatalogResolver, DeploymentOperationCatalogLock) {
	t.Helper()
	catalogDocument := codeEdgePhase1DefinitionCatalog(t)
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	profile := CodeEdgePhase1ExecutionProfileLock{Profile: codeEdgePhase1DefinitionProfile(t)}
	policy := CodeEdgePhase1FinalCompliancePolicyLock{Policy: codeEdgePhase1DefinitionPolicy()}
	lock := DeploymentOperationCatalogLock{
		Format: DeploymentOperationCatalogLockFormat, Version: DeploymentOperationCatalogLockVersion,
		LockID: "codeedge-phase1-parent-definition-test", LockVersion: "test-v1",
		CatalogReceipt: catalog.Receipt(),
		HarborFlowBuild: HarborFlowBuildIdentity{
			Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0",
			Commit: strings.Repeat("c", 40), ContentSHA256: workflowkit.SHA256Fingerprint([]byte("codeedge-phase1-parent-definition-test")),
		},
		CodeEdgePhase1ExecutionProfile:      &profile,
		CodeEdgePhase1FinalCompliancePolicy: &policy,
		Operations:                          make([]DeploymentOperationCatalogLockRecord, 0, len(catalogDocument.Operations)),
	}
	for _, registration := range catalogDocument.Operations {
		lock.Operations = append(lock.Operations, codeEdgePhase1DefinitionLockRecord(registration))
	}
	return catalog, lock
}

func codeEdgePhase1DefinitionCatalog(t *testing.T) DeploymentOperationCatalog {
	t.Helper()
	workflow := workflowadapter.CodeEdgePhase1WorkflowTemplate()
	operations := make([]DeploymentOperationRegistration, 0, len(workflow.Catalog.Stages))
	provider := workflowadapter.ProviderReference{ID: CodeEdgePhase1ProviderID, Kind: CodeEdgePhase1ProviderKind, Version: CodeEdgePhase1ProviderVersion}
	runtime := workflowadapter.RuntimeReference{ID: "codeedge-phase1-test-runtime", Kind: "controlled", Version: "1"}
	for _, stage := range workflow.Catalog.Stages {
		commandID := "codeedge-phase1-test-" + string(stage.Key)
		operations = append(operations, DeploymentOperationRegistration{
			Stage:    DeploymentStageContract{Key: stage.Key, Type: stageBindingTypeForCodeEdgePhase1Test(t, stage.Key), Group: stage.Group, Plugin: workflowkit.PluginBinding{ID: stage.Plugin.ID, Version: stage.Plugin.Version}},
			Provider: provider,
			Operation: workflowadapter.StageOperationBinding{
				ProviderID: provider.ID, OperationID: "codeedge.phase1.test." + string(stage.Key), Version: "1",
				Payload: workflowadapter.LocalCommandOperationPayload{CommandID: commandID, Arguments: []string{}},
			},
			Runtime:  runtime,
			Checkout: DeploymentCheckoutContract{ID: "codeedge-phase1-test-checkout", Purpose: "test-parent"},
			Secrets:  []workflowadapter.SecretReference{},
		})
	}
	return DeploymentOperationCatalog{
		Format: DeploymentOperationCatalogFormat, Version: DeploymentOperationCatalogVersion,
		CatalogID: "codeedge-phase1-parent-definition-test", CatalogVersion: "test-v1",
		Template: workflowadapter.CodeEdgePhase1TemplateReference(), Operations: operations,
	}
}

func codeEdgePhase1DefinitionLockRecord(registration DeploymentOperationRegistration) DeploymentOperationCatalogLockRecord {
	payload := registration.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
	return DeploymentOperationCatalogLockRecord{
		Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
		Checkout: registration.Checkout, Secrets: []workflowadapter.SecretReference{},
		PromptContentFingerprint: workflowkit.SHA256Fingerprint([]byte("prompt:" + string(registration.Stage.Key))),
		SchemaContentFingerprint: workflowkit.SHA256Fingerprint([]byte("schema:" + string(registration.Stage.Key))),
		ExecutionKind:            payload.Kind(),
		LocalExecutable: &LocalExecutableLock{
			CommandID: payload.CommandID, AbsolutePath: "/opt/harbor/codeedge-phase1/" + payload.CommandID,
			Version: "1", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("binary:" + payload.CommandID)),
		},
	}
}

func codeEdgePhase1DefinitionProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	workflow := workflowadapter.CodeEdgePhase1WorkflowTemplate()
	profile := workflowadapter.ExecutionProfile{
		Template: workflowadapter.CodeEdgePhase1TemplateReference(), ID: "codeedge-phase1-parent-definition-test", Version: "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, ControlGracePeriod: time.Minute,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: 5 * time.Minute},
		Stages:                  make([]workflowadapter.StageBudget, 0, len(workflow.Catalog.Stages)),
	}
	for _, stage := range workflow.Catalog.Stages {
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{StageKey: stage.Key, Budget: workflowkit.ExecutionBudget{
			TurnTimeout: time.Minute, MaxTurns: max(1, stage.RequiredTurns), AttemptTimeout: time.Minute,
			MaxAttempts: 1, MaxElapsed: time.Minute,
		}})
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("build complete parent profile: %v", err)
	}
	return profile
}

func codeEdgePhase1DefinitionPolicy() codeedge.FinalCompliancePolicy {
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

func stageBindingTypeForCodeEdgePhase1Test(t *testing.T, key workflowkit.StageKey) workflowadapter.StageBindingType {
	t.Helper()
	switch key {
	case workflowadapter.RepoPrepare:
		return workflowadapter.StageBindingRepoPrepare
	case workflowadapter.RepoAnalyze:
		return workflowadapter.StageBindingRepoAnalyze
	case workflowadapter.CodeEdgeLint:
		return workflowadapter.StageBindingCodeEdgeLint
	case workflowadapter.DockerBuild:
		return workflowadapter.StageBindingDockerBuild
	case workflowadapter.InitialVerify:
		return workflowadapter.StageBindingInitialVerify
	case workflowadapter.OracleVerify:
		return workflowadapter.StageBindingOracleVerify
	case workflowadapter.TestsAnalysis:
		return workflowadapter.StageBindingTestsAnalysis
	case workflowadapter.SolutionReview:
		return workflowadapter.StageBindingSolutionReview
	case workflowadapter.QualityCheck:
		return workflowadapter.StageBindingQualityCheck
	case workflowadapter.SimilarityCheck:
		return workflowadapter.StageBindingSimilarityCheck
	case workflowadapter.FinalReview:
		return workflowadapter.StageBindingFinalReview
	case workflowadapter.EvaluatorEvidenceHandoff:
		return workflowadapter.StageBindingEvaluatorEvidenceHandoff
	case workflowadapter.SubmissionLint:
		return workflowadapter.StageBindingSubmissionLint
	case workflowadapter.ResultReview:
		return workflowadapter.StageBindingResultReview
	case workflowadapter.Package:
		return workflowadapter.StageBindingPackage
	default:
		t.Fatalf("unknown CodeEdge Phase-1 stage %q", key)
		return ""
	}
}
