package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestProductionBuildLDFlagsBindsVerifiedStandardAuthoringBundleInStableOrder(t *testing.T) {
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
	if strings.Count(flags, "-X ") != 8 {
		t.Fatalf("linker flag count = %d, want 8: %s", strings.Count(flags, "-X "), flags)
	}
}

func TestProductionBuildLDFlagsRejectsMissingSwappedDuplicateAndDriftedInputs(t *testing.T) {
	t.Run("missing named pair", func(t *testing.T) {
		fixture := newProductionBuildFixture(t)
		config := fixture.config
		config.StandardAuthoringCatalog = ""
		assertProductionBuildError(t, config, "standard authoring catalog is required")
	})

	t.Run("mismatched source manifest", func(t *testing.T) {
		fixture := newProductionBuildFixture(t)
		config := fixture.config
		config.SourceManifest = string(workflowkit.SHA256Fingerprint([]byte("different frozen source manifest")))
		assertProductionBuildError(t, config, "Standard authoring lock source manifest does not match frozen source")
	})

	t.Run("foreign template lock", func(t *testing.T) {
		fixture := newProductionBuildFixture(t)
		config := fixture.config
		foreignCatalogRaw := []byte(`{"format":"harbor.deployment-operation-catalog.v1","version":"1","catalog_id":"foreign","catalog_version":"1.0.0","template":{"id":"harbor.task-lifecycle","version":"2.2.0"},"operations":[]}`)
		foreignLockRaw := []byte(`{"format":"harbor.operation-catalog.lock.v1","version":"1","lock_id":"foreign-lock","lock_version":"1.0.0","catalog_receipt":{"format":"harbor.operation-catalog-receipt.v1","version":"1","catalog_id":"foreign","catalog_version":"1.0.0","template":{"id":"harbor.foreign","version":"9.9.9"},"fingerprint":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"harbor_flow_build":{"module":"github.com/purplevoid/harbor-factory","version":"v2.0.0","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","content_sha256":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"operations":[]}`)
		config.StandardAuthoringCatalog = filepath.Join(t.TempDir(), "foreign-catalog.json")
		config.StandardAuthoringLock = filepath.Join(t.TempDir(), "foreign-lock.json")
		writeFixtureFile(t, config.StandardAuthoringCatalog, foreignCatalogRaw)
		writeFixtureFile(t, config.StandardAuthoringLock, foreignLockRaw)
		assertProductionBuildError(t, config, "standard authoring catalog template")
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
	standard := writeProductionBuildFixtureBundle(t, root, "standard", workflowadapter.StandardAuthoringCurrentWorkflowTemplate(), build)
	return productionBuildFixture{
		config: productionBuildConfig{
			StandardAuthoringCatalog: standard.catalogPath,
			StandardAuthoringLock:    standard.lockPath,
			SourceManifest:           string(build.ContentSHA256),
		},
		bundles: map[string]productionBuildFixtureBundle{
			"standard": standard,
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
	}
	flags := make([]string, 0, 8)
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
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(template.Catalog.Template) {
		t.Fatalf("unsupported fixture template %s@%s", template.Catalog.Template.ID, template.Catalog.Template.Version)
	}
	lock.StandardAuthoringExecutionProfile = &stageprovider.StandardAuthoringExecutionProfileLock{Profile: profile}
	lock.StandardAuthoringSSHTransport = fixtureStandardAuthoringSSHTransport()
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
