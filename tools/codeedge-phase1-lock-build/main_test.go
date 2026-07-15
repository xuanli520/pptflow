package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

func TestSourceBuildIdentityExcludesAllGeneratedProductionLocks(t *testing.T) {
	git := parentLockTestGitExecutable(t)
	root := t.TempDir()
	parentLockWriteFile(t, filepath.Join(root, "tracked.txt"), "stable\n")
	for _, lock := range generatedLockRelatives {
		parentLockWriteFile(t, filepath.Join(root, lock), "first generated lock\n")
	}
	parentLockGit(t, root, git, "init")
	parentLockGit(t, root, git, "config", "user.email", "parent-lock-test@example.invalid")
	parentLockGit(t, root, git, "config", "user.name", "Parent Lock Test")
	parentLockGit(t, root, git, "add", ".")
	parentLockGit(t, root, git, "commit", "-m", "initial")

	_, first, err := sourceBuildIdentity(root, git)
	if err != nil {
		t.Fatalf("first source build identity: %v", err)
	}
	for _, lock := range generatedLockRelatives {
		parentLockWriteFile(t, filepath.Join(root, lock), "second generated lock\n")
	}
	parentLockGit(t, root, git, "add", "deployments")
	parentLockGit(t, root, git, "commit", "-m", "generated locks only")
	_, second, err := sourceBuildIdentity(root, git)
	if err != nil {
		t.Fatalf("second source build identity: %v", err)
	}
	if first != second {
		t.Fatalf("source manifest changed only because generated locks changed: %s != %s", first, second)
	}
}

func TestBuildCreatesClosedParentLock(t *testing.T) {
	fixture := newParentLockGeneratorFixture(t)
	lock, err := build(fixture.config)
	if err != nil {
		t.Fatalf("build parent lock: %v", err)
	}
	if !lock.CatalogReceipt.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		t.Fatalf("lock template = %s@%s, want CodeEdge Phase-1", lock.CatalogReceipt.Template.ID, lock.CatalogReceipt.Template.Version)
	}
	if lock.CodeEdgePhase1ExecutionProfile == nil || lock.CodeEdgePhase1PreflightProfile == nil || lock.CodeEdgePhase1FinalCompliancePolicy == nil {
		t.Fatal("parent lock did not carry every required lock-owned parent policy")
	}
	if lock.StandardAuthoringExecutionProfile != nil || lock.CodeEdgeEvaluatorChildExecutionProfile != nil {
		t.Fatal("parent lock carried a profile belonging to another bundle")
	}
	if lock.HarborFlowBuild.Module != modulePath || lock.HarborFlowBuild.Version != fixture.config.buildVersion {
		t.Fatalf("unexpected parent build identity: %+v", lock.HarborFlowBuild)
	}

	localCount := 0
	builtinCount := 0
	reviewCount := 0
	for _, record := range lock.Operations {
		switch payload := record.Operation.Payload.(type) {
		case workflowadapter.LocalCommandOperationPayload:
			localCount++
			if record.LocalExecutable == nil {
				t.Fatalf("local stage %q lacks Docker lock", record.Stage.Key)
			}
			if record.LocalExecutable.CommandID != payload.CommandID || record.LocalExecutable.AbsolutePath != fixture.docker || record.LocalExecutable.Version != "29.5.2" {
				t.Fatalf("local stage %q did not pin the fixture Docker executable: %+v", record.Stage.Key, record.LocalExecutable)
			}
			if record.LocalExecutable.ContentSHA256 == "" {
				t.Fatalf("local stage %q lacks Docker content fingerprint", record.Stage.Key)
			}
			if record.HarborFlowBuiltin != nil || record.DurableReviewPolicy != nil || record.AgentModel != nil || record.HarborEvaluator != nil {
				t.Fatalf("local stage %q has an unrelated execution attestation", record.Stage.Key)
			}
		case workflowadapter.HarborBuiltinOperationPayload:
			builtinCount++
			if record.HarborFlowBuiltin == nil || record.HarborFlowBuiltin.HandlerID != payload.HandlerID {
				t.Fatalf("built-in stage %q does not pin its handler", record.Stage.Key)
			}
			if record.LocalExecutable != nil || record.DurableReviewPolicy != nil || record.AgentModel != nil || record.HarborEvaluator != nil {
				t.Fatalf("built-in stage %q has an unrelated execution attestation", record.Stage.Key)
			}
		case workflowadapter.DurableReviewOperationPayload:
			reviewCount++
			if record.DurableReviewPolicy == nil || record.DurableReviewPolicy.PolicyID != payload.PolicyID {
				t.Fatalf("review stage %q does not pin its policy", record.Stage.Key)
			}
			if record.LocalExecutable != nil || record.HarborFlowBuiltin != nil || record.AgentModel != nil || record.HarborEvaluator != nil {
				t.Fatalf("review stage %q has an unrelated execution attestation", record.Stage.Key)
			}
		default:
			t.Fatalf("parent lock emitted unsupported payload %T", payload)
		}
	}
	if localCount != 3 || builtinCount != 8 || reviewCount != 4 {
		t.Fatalf("parent lock operation inventory local=%d builtin=%d review=%d, want 3/8/4", localCount, builtinCount, reviewCount)
	}
	if len(lock.Operations) != len(workflowadapter.CodeEdgePhase1StageOrder()) {
		t.Fatalf("parent lock operation count = %d, want %d", len(lock.Operations), len(workflowadapter.CodeEdgePhase1StageOrder()))
	}

	canonical, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical parent lock: %v", err)
	}
	parsed, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(canonical)
	if err != nil {
		t.Fatalf("parse canonical parent lock: %v", err)
	}
	if parsed.HarborFlowBuild != lock.HarborFlowBuild {
		t.Fatalf("canonical lock changed build identity: %+v != %+v", parsed.HarborFlowBuild, lock.HarborFlowBuild)
	}
}

func TestBuildRejectsDirtyOrSubstitutedSource(t *testing.T) {
	fixture := newParentLockGeneratorFixture(t)
	parentLockWriteFile(t, filepath.Join(fixture.root, "untracked.txt"), "not part of the source snapshot\n")
	if _, err := build(fixture.config); err == nil || !strings.Contains(err.Error(), "clean committed Git worktree") {
		t.Fatalf("dirty source tree was accepted: %v", err)
	}

	clean := newParentLockGeneratorFixture(t)
	clean.config.catalogPath = filepath.Join(clean.root, "catalog-substitute.json")
	if _, err := build(clean.config); err == nil || !strings.Contains(err.Error(), "fixed managed asset") {
		t.Fatalf("substituted catalog path was accepted: %v", err)
	}

	relative := newParentLockGeneratorFixture(t)
	relative.config.dockerPath = "docker"
	if err := validateConfig(&relative.config); err == nil || !strings.Contains(err.Error(), "clean non-root absolute path") {
		t.Fatalf("relative Docker executable path was accepted: %v", err)
	}

	output := newParentLockGeneratorFixture(t)
	parentLockWriteFile(t, output.config.outputPath, "previous lock\n")
	if err := validateConfig(&output.config); err == nil || !strings.Contains(err.Error(), "must not already exist") {
		t.Fatalf("existing output lock was accepted: %v", err)
	}
}

func TestWriteNewRegularFileDoesNotReplaceExistingLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "operation-catalog.lock.json")
	parentLockWriteFile(t, path, "first lock\n")
	if err := writeNewRegularFile(path, []byte("replacement lock\n")); err == nil {
		t.Fatal("lock writer replaced an existing lock")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "first lock\n" {
		t.Fatalf("existing lock changed to %q", contents)
	}
}

func TestProductionParentAssetsAreAccepted(t *testing.T) {
	root := parentLockProductionRoot(t)
	profile, _, err := readParentExecutionProfile(filepath.Join(root, parentProfileRelative))
	if err != nil {
		t.Fatalf("read production parent execution profile: %v", err)
	}
	if !profile.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		t.Fatalf("production parent execution profile template = %s@%s", profile.Template.ID, profile.Template.Version)
	}
	preflight, _, err := readParentPreflightProfile(filepath.Join(root, parentPreflightRelative))
	if err != nil {
		t.Fatalf("read production parent preflight profile: %v", err)
	}
	if len(preflight.ProtectedEnvironmentVariables) != 4 {
		t.Fatalf("production preflight protected variable count = %d, want 4", len(preflight.ProtectedEnvironmentVariables))
	}
	policy, _, err := readParentFinalCompliancePolicy(filepath.Join(root, parentPolicyRelative))
	if err != nil {
		t.Fatalf("read production final compliance policy: %v", err)
	}
	if policy.QwenPolicy.LogicalTrialCount != 4 || policy.OpusPolicy.LogicalTrialCount != 4 {
		t.Fatalf("production policy did not pin pass@4: qwen=%d opus=%d", policy.QwenPolicy.LogicalTrialCount, policy.OpusPolicy.LogicalTrialCount)
	}
}

func TestLockParentOperationsRejectsUnapprovedLocalCommand(t *testing.T) {
	fixture := newParentLockGeneratorFixture(t)
	raw, err := os.ReadFile(fixture.config.catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := stageprovider.ParseDeploymentOperationCatalogJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	registrations := document.Operations
	for index := range registrations {
		if registrations[index].Stage.Key == workflowadapter.DockerBuild {
			registrations[index].Operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "unapproved-parent-command", Arguments: []string{}}
			break
		}
	}
	docker, err := discoverDockerLock(fixture.docker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockParentOperations(registrations, docker, []byte(`{"profile":"fixture"}`), []byte(`{"preflight":"fixture"}`), []byte(`{"policy":"fixture"}`)); err == nil || !strings.Contains(err.Error(), "unapproved local command") {
		t.Fatalf("unapproved parent local command was accepted: %v", err)
	}
}

type parentLockGeneratorFixture struct {
	root   string
	docker string
	config buildConfig
}

func newParentLockGeneratorFixture(t *testing.T) parentLockGeneratorFixture {
	t.Helper()
	git := parentLockTestGitExecutable(t)
	root := t.TempDir()
	for _, relative := range []string{parentCatalogRelative, parentProfileRelative, parentPreflightRelative, parentPolicyRelative} {
		production := filepath.Join(parentLockProductionRoot(t), filepath.FromSlash(relative))
		contents, err := os.ReadFile(production)
		if err != nil {
			t.Fatalf("read production fixture %s: %v", relative, err)
		}
		parentLockWriteFile(t, filepath.Join(root, filepath.FromSlash(relative)), string(contents))
	}
	parentLockGit(t, root, git, "init")
	parentLockGit(t, root, git, "config", "user.email", "parent-lock-test@example.invalid")
	parentLockGit(t, root, git, "config", "user.name", "Parent Lock Test")
	parentLockGit(t, root, git, "add", "deployments")
	parentLockGit(t, root, git, "commit", "-m", "parent deployment assets")

	dockerDirectory := t.TempDir()
	docker := filepath.Join(dockerDirectory, "docker")
	parentLockWriteExecutable(t, docker, "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  printf '%s\\n' 'Docker version 29.5.2, build fixture'\n  exit 0\nfi\nexit 1\n")
	return parentLockGeneratorFixture{
		root:   root,
		docker: docker,
		config: buildConfig{
			sourceRoot:    root,
			catalogPath:   filepath.Join(root, filepath.FromSlash(parentCatalogRelative)),
			profilePath:   filepath.Join(root, filepath.FromSlash(parentProfileRelative)),
			preflightPath: filepath.Join(root, filepath.FromSlash(parentPreflightRelative)),
			policyPath:    filepath.Join(root, filepath.FromSlash(parentPolicyRelative)),
			outputPath:    filepath.Join(root, filepath.FromSlash(parentLockRelative)),
			buildVersion:  "v2.0.0",
			lockID:        "codeedge-phase1-parent-test-lock",
			lockVersion:   "2026.07.15-test",
			gitExecutable: git,
			dockerPath:    docker,
		},
	}
}

func parentLockProductionRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate parent lock generator test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func parentLockTestGitExecutable(t *testing.T) string {
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

func parentLockGit(t *testing.T, root, git string, arguments ...string) {
	t.Helper()
	command := exec.Command(git, arguments...)
	command.Dir = root
	command.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"GIT_AUTHOR_NAME=Parent Lock Test",
		"GIT_AUTHOR_EMAIL=parent-lock-test@example.invalid",
		"GIT_COMMITTER_NAME=Parent Lock Test",
		"GIT_COMMITTER_EMAIL=parent-lock-test@example.invalid",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
}

func parentLockWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func parentLockWriteExecutable(t *testing.T, path, contents string) {
	t.Helper()
	parentLockWriteFile(t, path, contents)
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
