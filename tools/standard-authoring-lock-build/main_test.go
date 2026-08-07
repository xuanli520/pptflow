package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestSourceBuildIdentityExcludesAllGeneratedProductionLocks(t *testing.T) {
	git := testGitExecutable(t)
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "tracked.txt"), "stable\n")
	for lock := range generatedProductionLocks {
		writeTestFile(t, filepath.Join(root, lock), "first lock\n")
	}
	testGit(t, root, git, "init")
	testGit(t, root, git, "config", "user.email", "lock-test@example.invalid")
	testGit(t, root, git, "config", "user.name", "Lock Test")
	testGit(t, root, git, "add", ".")
	testGit(t, root, git, "commit", "-m", "first")
	_, first, err := sourceBuildIdentity(root, git)
	if err != nil {
		t.Fatal(err)
	}
	for lock := range generatedProductionLocks {
		writeTestFile(t, filepath.Join(root, lock), "second lock\n")
	}
	testGit(t, root, git, "add", "deployments")
	testGit(t, root, git, "commit", "-m", "lock only")
	_, second, err := sourceBuildIdentity(root, git)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("source manifest changed only because generated locks changed: %s != %s", first, second)
	}
}

func TestReadRegularFileRejectsSymlinkAndDetectsPathShape(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	writeTestFile(t, target, "content")
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	if _, err := readRegularFile(link, maxAssetBytes); err == nil {
		t.Fatal("symlink asset was accepted")
	}
	if _, err := contractAssetPath(root, "../escape"); err == nil {
		t.Fatal("asset traversal was accepted")
	}
}

func TestRequireCleanGitWorktreeRejectsUntrackedContent(t *testing.T) {
	git := testGitExecutable(t)
	root := t.TempDir()
	testGit(t, root, git, "init")
	testGit(t, root, git, "config", "user.email", "lock-test@example.invalid")
	testGit(t, root, git, "config", "user.name", "Lock Test")
	writeTestFile(t, filepath.Join(root, "tracked.txt"), "tracked\n")
	testGit(t, root, git, "add", ".")
	testGit(t, root, git, "commit", "-m", "initial")
	if err := requireCleanGitWorktree(root, git); err != nil {
		t.Fatalf("clean worktree rejected: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "untracked.txt"), "untracked\n")
	if err := requireCleanGitWorktree(root, git); err == nil {
		t.Fatal("dirty worktree accepted")
	}
}

func TestReadStandardAuthoringExecutionProfileRequiresExactCompleteProfile(t *testing.T) {
	root := t.TempDir()
	profile := standardAuthoringGeneratorTestProfile(t)
	raw, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "execution-profile.v1.json")
	writeTestFile(t, path, string(raw))
	loaded, err := readStandardAuthoringExecutionProfile(path)
	if err != nil {
		t.Fatalf("read valid Standard authoring execution profile: %v", err)
	}
	wantFingerprint, err := profile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	gotFingerprint, err := loaded.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if gotFingerprint != wantFingerprint {
		t.Fatalf("loaded execution profile fingerprint = %s, want %s", gotFingerprint, wantFingerprint)
	}

	wrongTemplate := filepath.Join(root, "wrong-template.json")
	writeTestFile(t, wrongTemplate, strings.Replace(string(raw), "harbor.standard-authoring", "harbor.standard", 1))
	if _, err := readStandardAuthoringExecutionProfile(wrongTemplate); err == nil {
		t.Fatal("non-Standard execution profile was accepted by the lock generator")
	}
}

func TestProductionStandardAuthoringExecutionProfileAssetIsAccepted(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Standard authoring lock generator test")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "deployments", "standard-authoring", "execution-profile.v1.json"))
	profile, err := readStandardAuthoringExecutionProfile(path)
	if err != nil {
		t.Fatalf("read production Standard authoring execution profile asset: %v", err)
	}
	if !profile.Template.Equal(workflowadapter.StandardAuthoringCurrentTemplateReference()) {
		t.Fatalf("production execution profile template = %s@%s", profile.Template.ID, profile.Template.Version)
	}
}

func TestValidateStandardAuthoringTemplateBundleRequiresExactVersion(t *testing.T) {
	catalog := workflowadapter.StandardAuthoringCurrentTemplateReference()
	profile := catalog
	manifest := catalog
	if err := validateStandardAuthoringTemplateBundle(catalog, profile, manifest); err != nil {
		t.Fatalf("matching Standard authoring template bundle rejected: %v", err)
	}
	for name, candidate := range map[string]workflowadapter.TemplateReference{
		"profile":  workflowadapter.TemplateReference{ID: catalog.ID, Version: "1.9.9"},
		"manifest": workflowadapter.TemplateReference{ID: catalog.ID, Version: "1.9.9"},
	} {
		t.Run(name, func(t *testing.T) {
			candidateProfile, candidateManifest := profile, manifest
			if name == "profile" {
				candidateProfile = candidate
			} else {
				candidateManifest = candidate
			}
			if err := validateStandardAuthoringTemplateBundle(catalog, candidateProfile, candidateManifest); err == nil {
				t.Fatalf("template mismatch for %s was accepted", name)
			}
		})
	}
}

func TestProductionCodexStageAssetsRequireFrozenModelAndReasoningEffort(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Standard authoring lock generator test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	deploymentRoot := filepath.Join(root, "deployments", "standard-authoring")
	catalogRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "operation-catalog.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := stageprovider.ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := stageprovider.ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	assets := make(map[workflowkit.StageKey]stageprovider.StandardAuthoringContractAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		assets[entry.StageKey] = entry
	}
	for _, registration := range catalog.Operations {
		payload, isAgentTurn := registration.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
		if !isAgentTurn {
			continue
		}
		entry, found := assets[registration.Stage.Key]
		if !found {
			t.Fatalf("agent stage %q has no contract assets", registration.Stage.Key)
		}
		if err := validateCodexStageAssets(deploymentRoot, catalog.Template, registration.Stage.Key, entry, payload); err != nil {
			t.Fatalf("validate production Codex stage %q: %v", registration.Stage.Key, err)
		}
		promptRaw, err := os.ReadFile(filepath.Join(deploymentRoot, filepath.FromSlash(entry.Prompt.RelativePath)))
		if err != nil {
			t.Fatalf("read production Codex prompt %q: %v", registration.Stage.Key, err)
		}
		program, err := stageprovider.ParseStandardAuthoringCodexTurnProgramAsset(promptRaw)
		if err != nil {
			t.Fatalf("parse production Codex prompt %q: %v", registration.Stage.Key, err)
		}
		if payload.MaxTurns == 1 && !strings.Contains(strings.Join(program.TurnPrompts, "\n"), "harbor_submit_stage_output") {
			t.Fatalf("production Codex prompt %q does not require the stage output submission tool", registration.Stage.Key)
		}
		for name, mutate := range map[string]func(*workflowadapter.AgentTurnOperationPayload){
			"model drift": func(candidate *workflowadapter.AgentTurnOperationPayload) { candidate.ModelID = "other-model" },
			"reasoning effort drift": func(candidate *workflowadapter.AgentTurnOperationPayload) {
				candidate.ReasoningEffort = workflowadapter.AgentReasoningEffortMax
			},
		} {
			t.Run(string(registration.Stage.Key)+"/"+name, func(t *testing.T) {
				candidate := payload
				mutate(&candidate)
				if err := validateCodexStageAssets(deploymentRoot, catalog.Template, registration.Stage.Key, entry, candidate); err == nil {
					t.Fatalf("Codex stage assets accepted %s: %+v", name, candidate)
				}
			})
		}
	}
}

func TestProductionCodexLockSandboxMatchesStageWorkspace(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Standard authoring lock generator test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	lockRaw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "operation-catalog.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range lock.Operations {
		if record.CodexAppServer == nil {
			continue
		}
		stage, found := workflowadapter.StandardAuthoringCurrentWorkflowTemplate().Catalog.Stage(record.Stage.Key)
		if !found || stage.AgentRole == nil {
			t.Fatalf("Codex lock record %q is not an installed Agent stage", record.Stage.Key)
		}
		wantMode, wantPolicy, err := stageprovider.StandardAuthoringCodexSandboxForWorkspace(stage.AgentRole.Workspace.Mode)
		if err != nil {
			t.Fatalf("sandbox for %q: %v", record.Stage.Key, err)
		}
		if record.CodexAppServer.SandboxMode != wantMode || record.CodexAppServer.SandboxPolicy != wantPolicy {
			t.Fatalf("Codex lock sandbox for %q = %q/%q, want %q/%q", record.Stage.Key, record.CodexAppServer.SandboxMode, record.CodexAppServer.SandboxPolicy, wantMode, wantPolicy)
		}
	}
}

func TestProbeMultilineAcceptsBoundedCodexStyleHelp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-help")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '--listen <URL>' '-c, --config <key=value>'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := probeMultiline(path, nil, "app-server", "--help")
	if err != nil {
		t.Fatalf("probe multiline capability help: %v", err)
	}
	if !strings.Contains(output, "--listen") || !strings.Contains(output, "--config") {
		t.Fatalf("capability help = %q", output)
	}
}

func TestDiscoverStandardAuthoringSSHTransportPinsClientShellAndKnownHosts(t *testing.T) {
	root := t.TempDir()
	ssh := filepath.Join(root, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' OpenSSH_9.9p1 >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(root, "dash")
	if err := os.WriteFile(shell, []byte("test wrapper shell\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	contractRoot := filepath.Join(root, "contract")
	knownHosts := filepath.Join(contractRoot, filepath.FromSlash(stageprovider.StandardAuthoringSSHKnownHostsRelativePath))
	writeTestFile(t, knownHosts, "github.com ssh-ed25519 AQID\n")
	transport, err := discoverStandardAuthoringSSHTransport(buildConfig{
		sshExecutable: ssh, sshWrapperShell: shell, sshKnownHosts: knownHosts,
	})
	if err != nil {
		t.Fatalf("discover SSH transport: %v", err)
	}
	if err := transport.Validate(); err != nil {
		t.Fatal(err)
	}
	if transport.SSHExecutable.Version != "OpenSSH_9.9p1" || transport.WrapperShell.Version != string(transport.WrapperShell.ContentSHA256) ||
		transport.KnownHosts.RelativePath != stageprovider.StandardAuthoringSSHKnownHostsRelativePath ||
		transport.AgentSocketEnvironmentName != stageprovider.StandardAuthoringSSHAgentSocketEnvironment {
		t.Fatalf("discovered SSH transport = %+v", transport)
	}
	if _, err := discoverStandardAuthoringSSHTransport(buildConfig{sshExecutable: ssh, sshWrapperShell: shell, sshKnownHosts: filepath.Join(root, "other-known-hosts")}); err == nil {
		t.Fatal("missing/non-contract known_hosts input was accepted")
	}
}

func standardAuthoringGeneratorTestProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	profile := workflowadapter.ExecutionProfile{
		Template:                template.Reference(),
		ID:                      "standard-authoring-generator-test",
		Version:                 "1.0.0",
		ContinuationPlanTTL:     workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod:      time.Minute,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: 5 * time.Minute},
		Stages:                  make([]workflowadapter.StageBudget, 0, len(template.Catalog.Stages)),
	}
	for _, stage := range template.Catalog.Stages {
		turns := stage.RequiredTurns
		if turns < 1 {
			turns = 1
		}
		attempt := time.Duration(turns) * time.Minute
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{
			StageKey: stage.Key,
			Budget: workflowkit.ExecutionBudget{
				TurnTimeout:    time.Minute,
				MaxTurns:       turns,
				AttemptTimeout: attempt,
				MaxAttempts:    1,
				MaxElapsed:     attempt,
			},
		})
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("build Standard authoring generator test profile: %v", err)
	}
	return profile
}

func testGitExecutable(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	abs, err := filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func testGit(t *testing.T, root, git string, arguments ...string) {
	t.Helper()
	command := exec.Command(git, arguments...)
	command.Dir = root
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "GIT_AUTHOR_NAME=Lock Test", "GIT_AUTHOR_EMAIL=lock-test@example.invalid", "GIT_COMMITTER_NAME=Lock Test", "GIT_COMMITTER_EMAIL=lock-test@example.invalid"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
