package app

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringRepoPrepareExecutesLockedGitAndMaterializesFrozenRunSource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	snapshot := standardAuthoringRepoPrepareArchive(t)
	objects, err := workflowruntime.NewArtifactObjectStore(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	object, err := objects.PutBytes(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	source, task, session, run, attempt := standardAuthoringRepoPrepareFixture(t, ctx, database, object)
	lockedGit := standardAuthoringRepoPrepareLockedGit(t)
	workspaceRoot, err := StandardAuthoringCodexWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	preparedRoot := filepath.Join(workspaceRoot, run.ID, stageprovider.StandardAuthoringCodexRunSourceDirectory)
	t.Cleanup(func() {
		_ = filepath.WalkDir(preparedRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	executor, err := NewStandardAuthoringRepoPrepareExecutor(StandardAuthoringRepoPrepareExecutorConfig{
		ManagedRoot: root, Store: database, LockedGit: lockedGit, WorkspaceRoot: workspaceRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := standardAuthoringRepoPrepareRequest(run, source, session, attempt)
	invocation := stageprovider.StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			StageKey: workflowkit.StageKey(workflowadapter.RepoPrepare),
			Operation: workflowadapter.StageOperationBinding{Payload: workflowadapter.LocalCommandOperationPayload{
				CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, Arguments: []string{},
			}},
		},
	}
	result, err := executor.ExecuteLocalCommand(ctx, invocation, workflowadapter.LocalCommandOperationPayload{
		CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, Arguments: []string{},
	})
	if err != nil {
		t.Fatalf("execute locked Standard repo_prepare: %v", err)
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass || len(result.Artifacts) != 1 {
		t.Fatalf("repo_prepare result = %+v", result)
	}
	artifact := result.Artifacts[0]
	if artifact.Name != "repo_prepared" || artifact.SchemaVersion != "harbor.artifact.v1" {
		t.Fatalf("repo_prepare artifact = %+v", artifact)
	}
	var evidence struct {
		Format             string `json:"format"`
		Version            string `json:"version"`
		AuthoringSourceID  string `json:"authoring_source_id"`
		AuthoringSessionID string `json:"authoring_session_id"`
		SourceURL          string `json:"source_url"`
		SourceCommit       string `json:"source_commit"`
		SnapshotDigest     string `json:"snapshot_digest"`
		GitVersion         string `json:"git_version"`
	}
	if err := json.Unmarshal(artifact.Content, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Format != standardAuthoringRepoPreparedEvidenceFormat || evidence.Version != standardAuthoringRepoPreparedEvidenceVersion ||
		evidence.AuthoringSourceID != source.ID || evidence.AuthoringSessionID != session.ID || evidence.SourceURL != source.RepositoryURL ||
		evidence.SourceCommit != source.CommitSHA || evidence.SnapshotDigest != source.SnapshotContentDigest || evidence.GitVersion != "git version "+lockedGit.Version {
		t.Fatalf("repo_prepare evidence = %+v", evidence)
	}
	for name, want := range map[string]string{
		"Cargo.toml":     "[package]\nname = \"tower-http\"\n",
		"src/lib.rs":     "pub fn source_fixture() {}\n",
		"src/request.rs": "pub fn request_fixture() {}\n",
	} {
		raw, readErr := os.ReadFile(filepath.Join(preparedRoot, filepath.FromSlash(name)))
		if readErr != nil || string(raw) != want {
			t.Fatalf("prepared source %s = %q, %v; want %q", name, raw, readErr, want)
		}
		info, statErr := os.Stat(filepath.Join(preparedRoot, filepath.FromSlash(name)))
		if statErr != nil || info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("prepared source %s is not read-only: info=%v err=%v", name, info, statErr)
		}
	}

	// A fenced retry verifies the already materialized immutable workspace and
	// produces the same evidence rather than checking out or contacting Git a
	// second time through an unconstrained route.
	replayed, err := executor.ExecuteLocalCommand(ctx, invocation, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, Arguments: []string{}})
	if err != nil || !bytes.Equal(replayed.Artifacts[0].Content, artifact.Content) {
		t.Fatalf("repo_prepare replay = %+v, %v", replayed, err)
	}
	identity, err := executor.VerifyStandardAuthoringCodexFrozenSource(ctx, request.Execution, preparedRoot)
	if err != nil || identity != workflowkit.Fingerprint(source.SnapshotContentDigest) {
		t.Fatalf("verify prepared frozen source identity = %q, %v", identity, err)
	}
	for _, modeTamper := range []struct {
		name string
		path string
	}{
		{name: "source root", path: preparedRoot},
		{name: "source directory", path: filepath.Join(preparedRoot, "src")},
		{name: "source file", path: filepath.Join(preparedRoot, "Cargo.toml")},
	} {
		info, statErr := os.Stat(modeTamper.path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		originalMode := info.Mode().Perm()
		if err := os.Chmod(modeTamper.path, originalMode|0o200); err != nil {
			t.Fatal(err)
		}
		_, replayErr := executor.ExecuteLocalCommand(ctx, invocation, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, Arguments: []string{}})
		if err := os.Chmod(modeTamper.path, originalMode); err != nil {
			t.Fatal(err)
		}
		if replayErr == nil {
			t.Fatalf("Standard authoring workspace replay accepted mode-only tampering of %s", modeTamper.name)
		}
	}

	// An existing workspace is evidence, not a mutable cache. Restoring only
	// the read-only mode after writing different bytes must not hide tampering.
	tampered := filepath.Join(preparedRoot, "src", "lib.rs")
	if err := os.Chmod(tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tampered, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tampered, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.VerifyStandardAuthoringCodexFrozenSource(ctx, request.Execution, preparedRoot); err == nil {
		t.Fatal("frozen source verifier accepted content tampering after mode restoration")
	}
	if _, err := executor.ExecuteLocalCommand(ctx, invocation, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, Arguments: []string{}}); err == nil {
		t.Fatal("tampered Standard authoring workspace was accepted")
	}

	if task.CurrentRevisionID != "" {
		t.Fatalf("repo_prepare must not fabricate a TaskRevision: %+v", task)
	}
}

func TestStandardAuthoringRepoPrepareRejectsCommandOrSubjectDriftBeforeSideEffect(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	lockedGit := standardAuthoringRepoPrepareLockedGit(t)
	executor, err := NewStandardAuthoringRepoPrepareExecutor(StandardAuthoringRepoPrepareExecutorConfig{ManagedRoot: root, Store: database, LockedGit: lockedGit})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.ExecuteLocalCommand(ctx, stageprovider.StageOperationInvocation{}, workflowadapter.LocalCommandOperationPayload{CommandID: "not-approved"})
	if err == nil || result.Outcome.Status != "" {
		t.Fatalf("unbound repo_prepare command = result=%+v err=%v", result, err)
	}
}

func TestVerifyStandardAuthoringExtractedSnapshotRejectsPathTypeModeAndContentDrift(t *testing.T) {
	ctx := context.Background()
	snapshot := standardAuthoringRepoPrepareArchive(t)
	prepare := func(t *testing.T) string {
		t.Helper()
		workspace := t.TempDir()
		if err := extractStandardAuthoringSourceSnapshot(ctx, snapshot, workspace, standardAuthoringLaunchTestCoordinate); err != nil {
			t.Fatal(err)
		}
		sourceRoot := filepath.Join(workspace, stageprovider.StandardAuthoringCodexRunSourceDirectory)
		t.Cleanup(func() {
			_ = filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr == nil && entry.IsDir() {
					_ = os.Chmod(path, 0o755)
				}
				return nil
			})
		})
		if err := markStandardAuthoringSourceReadOnly(sourceRoot); err != nil {
			t.Fatal(err)
		}
		if err := verifyStandardAuthoringExtractedSnapshot(ctx, snapshot, sourceRoot, standardAuthoringLaunchTestCoordinate); err != nil {
			t.Fatalf("verify normal extracted snapshot: %v", err)
		}
		return sourceRoot
	}

	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "unexpected path",
			mutate: func(t *testing.T, root string) {
				if err := os.Chmod(root, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "unexpected.txt"), []byte("unexpected\n"), 0o444); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(root, 0o555); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "file replaced by directory",
			mutate: func(t *testing.T, root string) {
				parent := filepath.Join(root, "src")
				target := filepath.Join(parent, "lib.rs")
				if err := os.Chmod(parent, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, 0o555); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(parent, 0o555); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "mode changed",
			mutate: func(t *testing.T, root string) {
				if err := os.Chmod(filepath.Join(root, "Cargo.toml"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "content changed with mode restored",
			mutate: func(t *testing.T, root string) {
				target := filepath.Join(root, "Cargo.toml")
				if err := os.Chmod(target, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("tampered\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, 0o444); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sourceRoot := prepare(t)
			testCase.mutate(t, sourceRoot)
			if err := verifyStandardAuthoringExtractedSnapshot(ctx, snapshot, sourceRoot, standardAuthoringLaunchTestCoordinate); err == nil {
				t.Fatal("mutated extracted snapshot was accepted")
			}
		})
	}
}

func standardAuthoringRepoPrepareFixture(t *testing.T, ctx context.Context, database *store.Store, object workflowruntime.ObjectRef) (store.AuthoringSource, store.TaskV2, store.AuthoringSession, store.WorkflowRun, store.StageAttempt) {
	t.Helper()
	source, err := database.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		SnapshotArtifactRef: string(object.Digest), SnapshotContentDigest: string(object.Digest), SnapshotSchemaVersion: StandardAuthoringSourceSnapshotSchemaVersion,
		IdempotencyKey: "repo-prepare-source", Actor: "author", Reason: "freeze source fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: "repo-prepare-task", Title: "Repo prepare task", MetadataJSON: `{}`,
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author", Reason: "reserve source task",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringRepairFeedbackTemplateVersion, SessionManifestJSON: `{"format":"fixture"}`,
		IdempotencyKey: "repo-prepare-session", Actor: "author", Reason: "freeze source session",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:repo-prepare-profile", DefinitionHash: "sha256:repo-prepare-definition", RunManifestJSON: `{}`,
		Trigger: "authoring.standard.create", Actor: "author", Reason: "run source prepare fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "worker", Reason: "run repo_prepare fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.RepoPrepare, StageGroup: string(workflowadapter.StageSourcePrepare), Ordinal: 1,
		InputFingerprint: "sha256:repo-prepare-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "worker", Reason: "create repo_prepare stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: "worker", Reason: "execute repo_prepare stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, task, session, run, attempt
}

func standardAuthoringRepoPrepareRequest(run store.WorkflowRun, source store.AuthoringSource, session store.AuthoringSession, attempt store.StageAttempt) workflowkit.StageExecutionRequest {
	return workflowkit.StageExecutionRequest{
		Execution: workflowkit.FrozenExecution{ID: run.ID, Subject: workflowkit.SubjectBinding{SubjectID: source.ID, RevisionID: session.ID, Digest: workflowkit.SubjectDigest(source.SnapshotContentDigest)}},
		Claim:     workflowkit.JobClaim{Stage: &workflowkit.StageClaim{StageAttempt: workflowkit.AttemptIdentity{ID: workflowkit.AttemptID(attempt.ID)}}},
		Stage: workflowkit.StageDescriptor{
			Key: workflowkit.StageKey(workflowadapter.RepoPrepare), Plugin: workflowkit.PluginBinding{ID: "harborfactory.repo_prepare", Version: "1.0.0"},
			Outputs: []workflowkit.ArtifactSpec{{Name: "repo_prepared", SchemaVersion: "harbor.artifact.v1", Required: true}},
		},
	}
}

func standardAuthoringRepoPrepareArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: standardAuthoringGitPAXGlobalHeaderName, Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": standardAuthoringLaunchTestCoordinate.CommitSHA}}); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"source/Cargo.toml":     "[package]\nname = \"tower-http\"\n",
		"source/src/lib.rs":     "pub fn source_fixture() {}\n",
		"source/src/request.rs": "pub fn request_fixture() {}\n",
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func standardAuthoringRepoPrepareLockedGit(t *testing.T) stageprovider.LocalExecutableLock {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is unavailable")
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(strings.TrimPrefix(string(output), "git version "))
	if version == "" || version == string(output) {
		t.Fatalf("unexpected Git version output %q", output)
	}
	return stageprovider.LocalExecutableLock{
		CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, AbsolutePath: path, Version: version,
		ContentSHA256: workflowkit.SHA256Fingerprint(contents),
	}
}
