package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestProductionBuildLDFlagsBindsThreeVerifiedBundlesInStableOrder(t *testing.T) {
	fixture := newProductionBuildFixture(t)
	flags, err := productionBuildLDFlags(fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := productionBuildLDFlags(fixture.config); err != nil {
		t.Fatal(err)
	} else if again != flags {
		t.Fatalf("linker flags changed across equivalent inputs:\n%s\nwant:\n%s", again, flags)
	}

	want := fixture.expectedFlags(t)
	if flags != want {
		t.Fatalf("linker flags =\n%s\nwant:\n%s", flags, want)
	}
	if strings.Count(flags, "-X ") != 24 {
		t.Fatalf("linker flag count = %d, want 24: %s", strings.Count(flags, "-X "), flags)
	}
}

func TestProductionBuildLDFlagsRejectsMissingSwappedDuplicateAndDriftedInputs(t *testing.T) {
	t.Run("missing named pair", func(t *testing.T) {
		fixture := newProductionBuildFixture(t)
		config := fixture.config
		config.StandardAuthoringCatalog = ""
		assertProductionBuildError(t, config, "standard authoring catalog is required")
	})

	t.Run("swapped bundle templates", func(t *testing.T) {
		fixture := newProductionBuildFixture(t)
		config := fixture.config
		config.StandardAuthoringCatalog, config.CodeEdgePhase1Catalog = config.CodeEdgePhase1Catalog, config.StandardAuthoringCatalog
		config.StandardAuthoringLock, config.CodeEdgePhase1Lock = config.CodeEdgePhase1Lock, config.StandardAuthoringLock
		assertProductionBuildError(t, config, "standard authoring catalog template")
	})

	t.Run("duplicate deployment file", func(t *testing.T) {
		fixture := newProductionBuildFixture(t)
		config := fixture.config
		config.CodeEdgeEvaluatorCatalog = config.StandardAuthoringCatalog
		assertProductionBuildError(t, config, "duplicate deployment input")
	})

	t.Run("mismatched build identity", func(t *testing.T) {
		fixture := newProductionBuildFixture(t)
		parent := fixture.bundles["parent"]
		changed := parent.lock.Clone()
		changed.HarborFlowBuild.Commit = strings.Repeat("f", 40)
		rewriteFixtureLock(t, parent.lockPath, changed)
		assertProductionBuildError(t, fixture.config, "CodeEdge Phase-1 build identity does not match Standard authoring build identity")
	})

	t.Run("mismatched source manifest", func(t *testing.T) {
		fixture := newProductionBuildFixture(t)
		config := fixture.config
		config.SourceManifest = string(workflowkit.SHA256Fingerprint([]byte("different frozen source manifest")))
		assertProductionBuildError(t, config, "Standard authoring lock source manifest does not match frozen source")
	})

	t.Run("catalog lock receipt drift", func(t *testing.T) {
		fixture := newProductionBuildFixture(t)
		config := fixture.config
		parentLockRaw, err := os.ReadFile(fixture.bundles["parent"].lockPath)
		if err != nil {
			t.Fatal(err)
		}
		config.CodeEdgeEvaluatorLock = filepath.Join(t.TempDir(), "foreign-parent.lock.json")
		writeFixtureFile(t, config.CodeEdgeEvaluatorLock, parentLockRaw)
		assertProductionBuildError(t, config, "CodeEdge evaluator child lock template")
	})
}

func TestValidateLinkerValueRejectsShellAndFlagInjectionCharacters(t *testing.T) {
	for _, value := range []string{"contains space", "quote'", "semicolon;", "line\nbreak", "equals=value"} {
		if err := validateLinkerValue("test", value); err == nil {
			t.Fatalf("unsafe linker value %q was accepted", value)
		}
	}
	if err := validateLinkerValue("test", "github.com/example/harbor:v1.0.0+build@sha256"); err != nil {
		t.Fatalf("safe linker value was rejected: %v", err)
	}
}

func assertProductionBuildError(t *testing.T, config productionBuildConfig, want string) {
	t.Helper()
	if _, err := productionBuildLDFlags(config); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("production build error = %v, want substring %q", err, want)
	}
}

type productionBuildFixture struct {
	config  productionBuildConfig
	bundles map[string]productionBuildFixtureBundle
	build   stageprovider.HarborFlowBuildIdentity
}

type productionBuildFixtureBundle struct {
	catalogPath string
	lockPath    string
	lock        stageprovider.DeploymentOperationCatalogLock
}

func newProductionBuildFixture(t *testing.T) productionBuildFixture {
	t.Helper()
	root := t.TempDir()
	build := stageprovider.HarborFlowBuildIdentity{
		Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0",
		Commit: strings.Repeat("a", 40), ContentSHA256: workflowkit.SHA256Fingerprint([]byte("frozen-source-manifest")),
	}
	standard := writeProductionBuildFixtureBundle(t, root, "standard", workflowadapter.StandardAuthoringWorkflowTemplate(), build)
	parent := writeProductionBuildFixtureBundle(t, root, "parent", workflowadapter.CodeEdgePhase1WorkflowTemplate(), build)
	evaluator := writeProductionBuildFixtureBundle(t, root, "evaluator", workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplate(), build)
	return productionBuildFixture{
		config: productionBuildConfig{
			StandardAuthoringCatalog: standard.catalogPath,
			StandardAuthoringLock:    standard.lockPath,
			CodeEdgePhase1Catalog:    parent.catalogPath,
			CodeEdgePhase1Lock:       parent.lockPath,
			CodeEdgeEvaluatorCatalog: evaluator.catalogPath,
			CodeEdgeEvaluatorLock:    evaluator.lockPath,
			SourceManifest:           string(build.ContentSHA256),
		},
		bundles: map[string]productionBuildFixtureBundle{
			"standard":  standard,
			"parent":    parent,
			"evaluator": evaluator,
		},
		build: build,
	}
}

func (fixture productionBuildFixture) expectedFlags(t *testing.T) string {
	t.Helper()
	ordered := []struct {
		bundle    productionBuildFixtureBundle
		variables buildVariables
	}{
		{
			bundle: fixture.bundles["standard"], variables: buildVariables{
				module: standardAuthoringBuildModuleVariable, version: standardAuthoringBuildVersionVariable,
				commit: standardAuthoringBuildCommitVariable, digest: standardAuthoringBuildDigestVariable,
				catalogReceiptFingerprint: standardAuthoringBuildCatalogReceiptFingerprintVariable,
				lockID:                    standardAuthoringBuildLockIDVariable, lockVersion: standardAuthoringBuildLockVersionVariable,
				lockFingerprint: standardAuthoringBuildLockFingerprintVariable,
			},
		},
		{
			bundle: fixture.bundles["parent"], variables: buildVariables{
				module: codeEdgePhase1BuildModuleVariable, version: codeEdgePhase1BuildVersionVariable,
				commit: codeEdgePhase1BuildCommitVariable, digest: codeEdgePhase1BuildDigestVariable,
				catalogReceiptFingerprint: codeEdgePhase1BuildCatalogReceiptFingerprintVariable,
				lockID:                    codeEdgePhase1BuildLockIDVariable, lockVersion: codeEdgePhase1BuildLockVersionVariable,
				lockFingerprint: codeEdgePhase1BuildLockFingerprintVariable,
			},
		},
		{
			bundle: fixture.bundles["evaluator"], variables: buildVariables{
				module: codeEdgeEvaluatorBuildModuleVariable, version: codeEdgeEvaluatorBuildVersionVariable,
				commit: codeEdgeEvaluatorBuildCommitVariable, digest: codeEdgeEvaluatorBuildDigestVariable,
				catalogReceiptFingerprint: codeEdgeEvaluatorBuildCatalogReceiptFingerprintVariable,
				lockID:                    codeEdgeEvaluatorBuildLockIDVariable, lockVersion: codeEdgeEvaluatorBuildLockVersionVariable,
				lockFingerprint: codeEdgeEvaluatorBuildLockFingerprintVariable,
			},
		},
	}
	flags := make([]string, 0, 24)
	for _, item := range ordered {
		receipt, err := item.bundle.lock.CatalogReceipt.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		lock, err := item.bundle.lock.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		flags = append(flags,
			"-X "+item.variables.module+"="+fixture.build.Module,
			"-X "+item.variables.version+"="+fixture.build.Version,
			"-X "+item.variables.commit+"="+fixture.build.Commit,
			"-X "+item.variables.digest+"="+string(fixture.build.ContentSHA256),
			"-X "+item.variables.catalogReceiptFingerprint+"="+string(receipt),
			"-X "+item.variables.lockID+"="+item.bundle.lock.LockID,
			"-X "+item.variables.lockVersion+"="+item.bundle.lock.LockVersion,
			"-X "+item.variables.lockFingerprint+"="+string(lock),
		)
	}
	return strings.Join(flags, " ")
}

func writeProductionBuildFixtureBundle(t *testing.T, root, name string, template workflowadapter.WorkflowTemplate, build stageprovider.HarborFlowBuildIdentity) productionBuildFixtureBundle {
	t.Helper()
	catalogDocument := stageprovider.DeploymentOperationCatalog{
		Format: stageprovider.DeploymentOperationCatalogFormat, Version: stageprovider.DeploymentOperationCatalogVersion,
		CatalogID: "production-build-" + name, CatalogVersion: "1.0.0",
		Template: template.Catalog.Template, Operations: []stageprovider.DeploymentOperationRegistration{},
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatalf("create %s catalog: %v", name, err)
	}
	lock := stageprovider.DeploymentOperationCatalogLock{
		Format: stageprovider.DeploymentOperationCatalogLockFormat, Version: stageprovider.DeploymentOperationCatalogLockVersion,
		LockID: "production-build-" + name + "-lock", LockVersion: "1.0.0",
		CatalogReceipt: catalog.Receipt(), HarborFlowBuild: build, Operations: []stageprovider.DeploymentOperationCatalogLockRecord{},
	}
	profile := fixtureExecutionProfile(t, template, "production-build-"+name+"-profile")
	switch {
	case workflowadapter.IsStandardAuthoringWorkflowTemplate(template.Catalog.Template):
		lock.StandardAuthoringExecutionProfile = &stageprovider.StandardAuthoringExecutionProfileLock{Profile: profile}
		lock.StandardAuthoringSSHTransport = fixtureStandardAuthoringSSHTransport()
	case template.Catalog.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()):
		lock.CodeEdgePhase1ExecutionProfile = &stageprovider.CodeEdgePhase1ExecutionProfileLock{Profile: profile}
		lock.CodeEdgePhase1PreflightProfile = &stageprovider.CodeEdgePhase1PreflightProfileLock{Profile: fixtureCodeEdgePhase1PreflightProfile()}
		lock.CodeEdgePhase1FinalCompliancePolicy = &stageprovider.CodeEdgePhase1FinalCompliancePolicyLock{Policy: fixtureFinalCompliancePolicy()}
	case template.Catalog.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()):
		lock.CodeEdgeEvaluatorChildExecutionProfile = &stageprovider.CodeEdgeEvaluatorChildExecutionProfileLock{Profile: profile}
	default:
		t.Fatalf("unsupported fixture template %s@%s", template.Catalog.Template.ID, template.Catalog.Template.Version)
	}
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock); err != nil {
		t.Fatalf("create %s lock: %v", name, err)
	}
	catalogRaw, err := catalogDocument.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	lockRaw, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(root, name, "operation-catalog.v1.json")
	lockPath := filepath.Join(root, name, "operation-catalog.lock.json")
	writeFixtureFile(t, catalogPath, catalogRaw)
	writeFixtureFile(t, lockPath, lockRaw)
	return productionBuildFixtureBundle{catalogPath: catalogPath, lockPath: lockPath, lock: lock}
}

func fixtureExecutionProfile(t *testing.T, template workflowadapter.WorkflowTemplate, id string) workflowadapter.ExecutionProfile {
	t.Helper()
	profile := workflowadapter.ExecutionProfile{
		Template: template.Catalog.Template, ID: id, Version: "1.0.0",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod:  time.Minute,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{
			AttemptTimeout: 5 * time.Minute,
		},
		Stages: make([]workflowadapter.StageBudget, 0, len(template.Catalog.Stages)),
	}
	for _, stage := range template.Catalog.Stages {
		turns := max(stage.RequiredTurns, 1)
		attempt := time.Duration(turns) * time.Minute
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{
			StageKey: stage.Key,
			Budget: workflowkit.ExecutionBudget{
				TurnTimeout: time.Minute, MaxTurns: turns, AttemptTimeout: attempt, MaxAttempts: 1, MaxElapsed: attempt,
				Backoff: workflowkit.BackoffPolicy{RetryDelays: []time.Duration{}},
			},
		})
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("create fixture profile for %s@%s: %v", template.ID, template.Version, err)
	}
	return profile
}

func fixtureFinalCompliancePolicy() codeedge.FinalCompliancePolicy {
	maximumPassingTrials := 1
	qwen := codeedge.EvaluationPolicy{
		ID: "production-build.qwen", Version: "1.0.0", HarborEvidenceFormat: codeedge.HarborRunBundleV018Format,
		Evaluator: codeedge.EvaluatorIdentity{
			ProfileID: "production-build-qwen", ProfileVersion: "1.0.0", AgentName: "fixture-agent", AgentVersion: "1.0.0", ModelName: "fixture-qwen", ModelProvider: "controlled",
		},
		LogicalTrialCount: 4, PassRewardKey: "reward", PassRewardAtLeast: 1, MaxPassingTrials: &maximumPassingTrials,
		MinimumAverageTurns: 20, ScreenshotMediaType: "image/png", FailureClassifierID: "fixture-infra", FailureClassifierVersion: "1.0.0",
		InfraExceptionTypes: []string{"NetworkError"},
	}
	opus := qwen.Clone()
	opus.ID = "production-build.opus"
	opus.Evaluator.ProfileID = "production-build-opus"
	opus.Evaluator.ModelName = "fixture-opus"
	opus.MaxPassingTrials = nil
	return codeedge.FinalCompliancePolicy{
		ID: "production-build.final-compliance", Version: "1.0.0", QwenPolicy: qwen, OpusPolicy: opus,
		SubmissionCheckerID: "production-build.submission-check", SubmissionCheckerVersion: "1.0.0",
		SubmissionReportSchemaVersion: workflowadapter.CodeEdgeSubmissionReportSchemaVersion,
	}
}

func fixtureCodeEdgePhase1PreflightProfile() codeedge.Profile {
	return codeedge.Profile{
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
			"OPUS_HARBOR_BASE_URL",
			"QWEN_HARBOR_BASE_URL",
		},
	}
}

func rewriteFixtureLock(t *testing.T, path string, lock stageprovider.DeploymentOperationCatalogLock) {
	t.Helper()
	raw, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeFixtureFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixtureStandardAuthoringSSHTransport() *stageprovider.StandardAuthoringSSHTransportLock {
	sshContent := workflowkit.SHA256Fingerprint([]byte("locked ssh"))
	shellContent := workflowkit.SHA256Fingerprint([]byte("locked shell"))
	return &stageprovider.StandardAuthoringSSHTransportLock{
		Format:  stageprovider.StandardAuthoringSSHTransportLockFormat,
		Version: stageprovider.StandardAuthoringSSHTransportLockVersion,
		SSHExecutable: stageprovider.LocalExecutableLock{
			CommandID: stageprovider.StandardAuthoringSSHTransportCommandID, AbsolutePath: "/opt/standard-authoring/ssh", Version: "OpenSSH_10.0p2", ContentSHA256: sshContent,
		},
		WrapperShell: stageprovider.LocalExecutableLock{
			CommandID: stageprovider.StandardAuthoringSSHWrapperShellCommandID, AbsolutePath: "/opt/standard-authoring/dash", Version: string(shellContent), ContentSHA256: shellContent,
		},
		KnownHosts: stageprovider.StandardAuthoringSSHKnownHostsLock{
			Format:        stageprovider.StandardAuthoringSSHKnownHostsLockFormat,
			Version:       stageprovider.StandardAuthoringSSHKnownHostsLockVersion,
			RelativePath:  stageprovider.StandardAuthoringSSHKnownHostsRelativePath,
			ContentSHA256: workflowkit.SHA256Fingerprint([]byte("test known hosts")),
		},
		AgentSocketEnvironmentName: stageprovider.StandardAuthoringSSHAgentSocketEnvironment,
	}
}
