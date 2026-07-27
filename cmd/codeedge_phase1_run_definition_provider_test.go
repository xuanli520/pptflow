package cmd

import (
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

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
