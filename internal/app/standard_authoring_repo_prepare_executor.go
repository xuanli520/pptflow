package app

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringWorkspaceDirectory          = "authoring-workspaces"
	standardAuthoringWorkspaceManifestFile       = "source-manifest.json"
	standardAuthoringRepoPreparedEvidenceFormat  = "harbor.standard-authoring-repo-prepared.v1"
	standardAuthoringRepoPreparedEvidenceVersion = "1"
	standardAuthoringRepoPrepareTimeout          = 30 * time.Second
)

// StandardAuthoringRepoPrepareExecutor is the one controlled implementation
// of standard-authoring.git-snapshot. It never accepts a checkout path, a Git
// argument, a source URL, a ref, or an environment from a Run. Instead it
// re-proves the locked Git executable, reads the already persisted immutable
// AuthoringSource archive, and materializes that archive under the Run's
// private managed workspace for later locked Codex turns.
//
// Source capture and repo_prepare intentionally remain distinct: capture is
// the only network operation before an AuthoringSession exists; repo_prepare
// is the durable stage that verifies and projects that captured source into a
// fenced Run-local checkout.
type StandardAuthoringRepoPrepareExecutor struct {
	core          *lifecycleServiceCore
	workspaceRoot string
	lockedGit     stageprovider.LocalExecutableLock
}

// StandardAuthoringRepoPrepareExecutorConfig contains only composition-owned
// local facts. WorkspaceRoot is optional only to make the derived managed root
// explicit in tests; when supplied it must equal the fixed directory below
// ManagedRoot. No caller-facing API can select it.
type StandardAuthoringRepoPrepareExecutorConfig struct {
	ManagedRoot   string
	Store         *store.Store
	LockedGit     stageprovider.LocalExecutableLock
	WorkspaceRoot string
}

// StandardAuthoringCodexWorkspaceRoot returns the one managed root shared by
// repo_prepare and the run-scoped Codex executor. It does not create it.
func StandardAuthoringCodexWorkspaceRoot(managedRoot string) (string, error) {
	layout, err := newManagedLayout(managedRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(layout.root, standardAuthoringWorkspaceDirectory), nil
}

// NewStandardAuthoringRepoPrepareExecutor constructs the lock-bound Git
// adapter. Construction deliberately creates only the private managed
// workspace parent; it does not contact Git, read a source object, or create
// a Run checkout. Those effects happen only under an admitted StageAttempt.
func NewStandardAuthoringRepoPrepareExecutor(config StandardAuthoringRepoPrepareExecutorConfig) (*StandardAuthoringRepoPrepareExecutor, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("Standard authoring repo_prepare Store is required")
	}
	layout, err := newManagedLayout(config.ManagedRoot)
	if err != nil {
		return nil, err
	}
	if err := layout.ensureRoot(); err != nil {
		return nil, err
	}
	workspaceRoot := filepath.Join(layout.root, standardAuthoringWorkspaceDirectory)
	if provided := strings.TrimSpace(config.WorkspaceRoot); provided != "" {
		absolute, absErr := filepath.Abs(provided)
		if absErr != nil || filepath.Clean(absolute) != workspaceRoot {
			return nil, fmt.Errorf("Standard authoring repo_prepare workspace root must be the managed run-scoped root")
		}
	}
	if err := ensureStandardAuthoringWorkspaceRoot(workspaceRoot); err != nil {
		return nil, err
	}
	if config.LockedGit.CommandID != stageprovider.StandardAuthoringGitSnapshotCommandID || strings.TrimSpace(config.LockedGit.Version) == "" {
		return nil, fmt.Errorf("Standard authoring repo_prepare Git lock is invalid")
	}
	if err := config.LockedGit.ContentSHA256.Validate(); err != nil {
		return nil, fmt.Errorf("Standard authoring repo_prepare Git lock fingerprint: %w", err)
	}
	gitExecutable, err := standardAuthoringRegularExecutable(config.LockedGit.AbsolutePath)
	if err != nil || gitExecutable != config.LockedGit.AbsolutePath {
		return nil, fmt.Errorf("Standard authoring repo_prepare locked Git executable is unavailable")
	}
	objects, err := workflowruntime.NewArtifactObjectStore(filepath.Join(layout.root, "objects"))
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring repo_prepare object store: %w", err)
	}
	lockedGit := config.LockedGit
	return &StandardAuthoringRepoPrepareExecutor{
		core:          &lifecycleServiceCore{store: config.Store, layout: layout, objects: objects, now: time.Now},
		workspaceRoot: workspaceRoot,
		lockedGit:     lockedGit,
	}, nil
}

// ExecuteLocalCommand implements the exact local.command frozen in the
// Standard catalog. It does not run a shell and it rejects every command ID
// or argv other than the zero-argument Git snapshot operation.
func (executor *StandardAuthoringRepoPrepareExecutor) ExecuteLocalCommand(ctx context.Context, invocation stageprovider.StageOperationInvocation, payload workflowadapter.LocalCommandOperationPayload) (workflowkit.StageExecutionResult, error) {
	if executor == nil || executor.core == nil || executor.core.store == nil || executor.core.objects == nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring repo_prepare executor is not configured")
	}
	if ctx == nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring repo_prepare context is required")
	}
	if err := ctx.Err(); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if payload.CommandID != stageprovider.StandardAuthoringGitSnapshotCommandID || len(payload.Arguments) != 0 ||
		invocation.Resolution.StageKey != workflowkit.StageKey(workflowadapter.RepoPrepare) ||
		invocation.Request.Stage.Key != workflowkit.StageKey(workflowadapter.RepoPrepare) {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring repo_prepare received an unbound local command")
	}
	if resolved, ok := invocation.Resolution.Operation.Payload.(workflowadapter.LocalCommandOperationPayload); !ok || resolved.CommandID != payload.CommandID || len(resolved.Arguments) != 0 {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring repo_prepare operation payload is not frozen")
	}
	request := invocation.Request
	if request.Execution.ID == "" || request.Claim.Stage == nil || request.Claim.Stage.StageAttempt.ID == "" {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring repo_prepare requires a frozen Run and stage attempt")
	}

	run, err := executor.core.store.GetWorkflowRun(ctx, request.Execution.ID)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if run == nil || run.Status != store.WorkflowRunRunning || run.WorkflowTemplateID != workflowadapter.StandardAuthoringWorkflowTemplateID ||
		run.WorkflowTemplateVersion != workflowadapter.StandardAuthoringWorkflowTemplateVersion {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring repo_prepare Run is not active under the closed template")
	}
	subject, err := executor.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if !subject.isAuthoringSession() || request.Execution.Subject != subject.Binding {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring repo_prepare Run has an invalid source/session subject")
	}
	attempt, err := executor.core.store.GetStageAttempt(ctx, string(request.Claim.Stage.StageAttempt.ID))
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if attempt == nil || attempt.RunID != run.ID || attempt.StageKey != workflowadapter.RepoPrepare || attempt.ExecutionStatus != store.StageExecutionRunning {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring repo_prepare stage attempt is not active")
	}
	if len(request.Inputs) != 0 {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring repo_prepare cannot accept caller stage inputs")
	}

	snapshot, err := executor.readFrozenAuthoringSource(ctx, *run, subject)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	gitVersion, err := executor.lockedGitVersion(ctx)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if err := executor.ensurePreparedWorkspace(ctx, *run, subject, snapshot); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	evidence, err := standardAuthoringRepoPreparedEvidence(subject, gitVersion)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	return workflowkit.StageExecutionResult{
		Outcome:   workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass},
		Artifacts: []workflowkit.StageArtifact{{Name: "repo_prepared", SchemaVersion: "harbor.artifact.v1", Content: evidence}},
	}, nil
}

func (executor *StandardAuthoringRepoPrepareExecutor) readFrozenAuthoringSource(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject) ([]byte, error) {
	if !subject.isAuthoringSession() || subject.AuthoringSource == nil || subject.AuthoringSession == nil {
		return nil, fmt.Errorf("Standard authoring repo_prepare source/session subject is unavailable")
	}
	input, err := executor.core.store.GetAuthoringRunInputArtifactForPort(ctx, run.ID, "source_snapshot")
	if err != nil {
		return nil, err
	}
	source := subject.AuthoringSource
	if err := validateStandardAuthoringLaunchSource(*source); err != nil {
		return nil, fmt.Errorf("validate frozen Standard authoring source: %w", err)
	}
	if input == nil || input.RunID != run.ID || input.SessionID != subject.AuthoringSession.ID || input.SourceID != source.ID ||
		input.SourceFingerprint != source.SourceFingerprint || input.SnapshotArtifactRef != source.SnapshotArtifactRef ||
		input.ContentDigest != source.SnapshotContentDigest || input.SchemaVersion != source.SnapshotSchemaVersion ||
		source.SnapshotArtifactRef != source.SnapshotContentDigest || source.SnapshotSchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		return nil, fmt.Errorf("Standard authoring repo_prepare source snapshot does not match the frozen Run")
	}
	reference := workflowruntime.ObjectRef{Digest: workflowkit.Fingerprint(source.SnapshotContentDigest)}
	file, err := executor.core.objects.Open(ctx, reference)
	if err != nil {
		return nil, fmt.Errorf("open Standard authoring source snapshot: %w", err)
	}
	defer file.Close()
	limited := io.LimitReader(file, int64(standardAuthoringSourceArchiveMaxBytes)+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read Standard authoring source snapshot: %w", err)
	}
	if len(content) > standardAuthoringSourceArchiveMaxBytes || workflowkit.SHA256Fingerprint(content) != workflowkit.Fingerprint(source.SnapshotContentDigest) {
		return nil, fmt.Errorf("Standard authoring source snapshot object does not match its immutable digest")
	}
	if err := validateStandardAuthoringSourceArchive(content); err != nil {
		return nil, fmt.Errorf("validate frozen Standard authoring source snapshot: %w", err)
	}
	return content, nil
}

func (executor *StandardAuthoringRepoPrepareExecutor) lockedGitVersion(ctx context.Context) (string, error) {
	if executor == nil {
		return "", fmt.Errorf("Standard authoring repo_prepare executor is not configured")
	}
	path, err := standardAuthoringRegularExecutable(executor.lockedGit.AbsolutePath)
	if err != nil || path != executor.lockedGit.AbsolutePath {
		return "", fmt.Errorf("Standard authoring repo_prepare locked Git executable is unavailable")
	}
	contents, err := os.ReadFile(path)
	if err != nil || workflowkit.SHA256Fingerprint(contents) != executor.lockedGit.ContentSHA256 {
		return "", fmt.Errorf("Standard authoring repo_prepare locked Git executable bytes do not match")
	}
	commandCtx, cancel := context.WithTimeout(ctx, standardAuthoringRepoPrepareTimeout)
	defer cancel()
	output := newStandardAuthoringLimitedBuffer(standardAuthoringGitCommandOutputMax)
	stderr := newStandardAuthoringLimitedBuffer(standardAuthoringGitCommandOutputMax)
	command := exec.CommandContext(commandCtx, path, "--version")
	command.Dir = executor.workspaceRoot
	command.Env = standardAuthoringGitEnvironment(executor.workspaceRoot)
	command.Stdout = output
	command.Stderr = stderr
	if err := command.Run(); err != nil || commandCtx.Err() != nil || output.exceeded || stderr.exceeded {
		return "", fmt.Errorf("controlled Standard authoring Git version command failed")
	}
	version := strings.TrimSpace(string(output.Bytes()))
	expected := "git version " + executor.lockedGit.Version
	if version != expected {
		return "", fmt.Errorf("Standard authoring repo_prepare locked Git version does not match")
	}
	return version, nil
}

func (executor *StandardAuthoringRepoPrepareExecutor) ensurePreparedWorkspace(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, snapshot []byte) error {
	if err := store.ValidateUUIDv7(run.ID); err != nil {
		return fmt.Errorf("Standard authoring repo_prepare Run identity: %w", err)
	}
	if err := ensureStandardAuthoringWorkspaceRoot(executor.workspaceRoot); err != nil {
		return err
	}
	workspace := filepath.Join(executor.workspaceRoot, run.ID)
	sourceRoot := filepath.Join(workspace, stageprovider.StandardAuthoringCodexRunSourceDirectory)
	if !standardAuthoringWorkspacePathWithin(executor.workspaceRoot, workspace) || !standardAuthoringWorkspacePathWithin(executor.workspaceRoot, sourceRoot) {
		return fmt.Errorf("Standard authoring repo_prepare workspace path escapes managed root")
	}
	if _, err := os.Lstat(workspace); err == nil {
		return verifyStandardAuthoringPreparedWorkspace(ctx, workspace, sourceRoot, subject, snapshot)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Standard authoring workspace: %w", err)
	}

	staging, err := os.MkdirTemp(executor.workspaceRoot, ".prepare-"+run.ID+"-")
	if err != nil {
		return fmt.Errorf("create Standard authoring workspace staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	stagedSource := filepath.Join(staging, stageprovider.StandardAuthoringCodexRunSourceDirectory)
	if err := extractStandardAuthoringSourceSnapshot(ctx, snapshot, staging); err != nil {
		return err
	}
	manifest, err := standardAuthoringWorkspaceManifestFor(subject)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := writeNewBytes(filepath.Join(staging, standardAuthoringWorkspaceManifestFile), encoded); err != nil {
		return fmt.Errorf("write Standard authoring workspace manifest: %w", err)
	}
	if err := markStandardAuthoringSourceReadOnly(stagedSource); err != nil {
		return err
	}
	if err := os.Chmod(staging, 0o750); err != nil {
		return fmt.Errorf("seal Standard authoring workspace staging directory: %w", err)
	}
	if err := os.Rename(staging, workspace); err != nil {
		if _, statErr := os.Lstat(workspace); statErr == nil {
			return verifyStandardAuthoringPreparedWorkspace(ctx, workspace, sourceRoot, subject, snapshot)
		}
		return fmt.Errorf("publish Standard authoring workspace: %w", err)
	}
	return verifyStandardAuthoringPreparedWorkspace(ctx, workspace, sourceRoot, subject, snapshot)
}

type standardAuthoringWorkspaceManifest struct {
	Format               string `json:"format"`
	Version              string `json:"version"`
	AuthoringSourceID    string `json:"authoring_source_id"`
	AuthoringSessionID   string `json:"authoring_session_id"`
	RepositoryURL        string `json:"repository_url"`
	CommitSHA            string `json:"commit_sha"`
	SourceSnapshotDigest string `json:"source_snapshot_digest"`
	SourceFingerprint    string `json:"source_fingerprint"`
}

func standardAuthoringWorkspaceManifestFor(subject workflowRunSubject) (standardAuthoringWorkspaceManifest, error) {
	if !subject.isAuthoringSession() || subject.AuthoringSource == nil || subject.AuthoringSession == nil {
		return standardAuthoringWorkspaceManifest{}, fmt.Errorf("Standard authoring workspace subject is unavailable")
	}
	source := subject.AuthoringSource
	return standardAuthoringWorkspaceManifest{
		Format: "harbor.standard-authoring-workspace.v1", Version: "1", AuthoringSourceID: source.ID,
		AuthoringSessionID: subject.AuthoringSession.ID, RepositoryURL: source.RepositoryURL, CommitSHA: source.CommitSHA,
		SourceSnapshotDigest: source.SnapshotContentDigest, SourceFingerprint: source.SourceFingerprint,
	}, nil
}

func verifyStandardAuthoringPreparedWorkspace(ctx context.Context, workspace, sourceRoot string, subject workflowRunSubject, snapshot []byte) error {
	for _, directory := range []string{workspace, sourceRoot} {
		info, err := os.Lstat(directory)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("Standard authoring workspace is unavailable or unsafe")
		}
	}
	wanted, err := standardAuthoringWorkspaceManifestFor(subject)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(workspace, standardAuthoringWorkspaceManifestFile))
	if err != nil {
		return fmt.Errorf("read Standard authoring workspace manifest: %w", err)
	}
	var actual standardAuthoringWorkspaceManifest
	if err := decodeStrictJSON(string(raw), &actual); err != nil || actual != wanted {
		return fmt.Errorf("Standard authoring workspace manifest does not match frozen source/session")
	}
	canonical, err := json.Marshal(wanted)
	if err != nil || !bytes.Equal(raw, canonical) {
		return fmt.Errorf("Standard authoring workspace manifest is not canonical")
	}
	return verifyStandardAuthoringExtractedSnapshot(ctx, snapshot, sourceRoot)
}

func extractStandardAuthoringSourceSnapshot(ctx context.Context, snapshot []byte, workspace string) error {
	if err := validateStandardAuthoringSourceArchive(snapshot); err != nil {
		return fmt.Errorf("validate Standard authoring workspace source snapshot: %w", err)
	}
	reader := tar.NewReader(bytes.NewReader(snapshot))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read Standard authoring source archive: %w", err)
		}
		name := filepath.FromSlash(header.Name)
		path := filepath.Join(workspace, name)
		if !standardAuthoringWorkspacePathWithin(workspace, path) {
			return fmt.Errorf("Standard authoring source archive escapes workspace")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return fmt.Errorf("create Standard authoring source directory: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create Standard authoring source parent: %w", err)
			}
			file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if openErr != nil {
				return fmt.Errorf("create Standard authoring source file: %w", openErr)
			}
			copyErr := copyContext(ctx, file, io.LimitReader(reader, header.Size))
			if copyErr == nil {
				copyErr = file.Sync()
			}
			closeErr := file.Close()
			if copyErr != nil {
				_ = os.Remove(path)
				return fmt.Errorf("write Standard authoring source file: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close Standard authoring source file: %w", closeErr)
			}
		default:
			return fmt.Errorf("Standard authoring source archive has unsupported entry")
		}
	}
	return nil
}

func verifyStandardAuthoringExtractedSnapshot(ctx context.Context, snapshot []byte, sourceRoot string) error {
	if err := validateStandardAuthoringSourceArchive(snapshot); err != nil {
		return err
	}
	expected := make(map[string][]byte)
	reader := tar.NewReader(bytes.NewReader(snapshot))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if !strings.HasPrefix(header.Name, stageprovider.StandardAuthoringCodexRunSourceDirectory+"/") {
			return fmt.Errorf("Standard authoring source archive has unexpected root")
		}
		relative := strings.TrimPrefix(header.Name, stageprovider.StandardAuthoringCodexRunSourceDirectory+"/")
		contents, readErr := io.ReadAll(io.LimitReader(reader, header.Size))
		if readErr != nil || int64(len(contents)) != header.Size {
			return fmt.Errorf("read Standard authoring source archive entry")
		}
		expected[filepath.ToSlash(relative)] = contents
	}
	actual := make(map[string][]byte, len(expected))
	err := filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == sourceRoot {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe Standard authoring source workspace entry")
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular Standard authoring source workspace entry")
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil || filepath.IsAbs(relative) {
			return fmt.Errorf("Standard authoring source workspace path escapes root")
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		actual[filepath.ToSlash(relative)] = contents
		return nil
	})
	if err != nil || len(actual) != len(expected) {
		return fmt.Errorf("Standard authoring source workspace does not match its immutable snapshot")
	}
	for path, wanted := range expected {
		got, present := actual[path]
		if !present || !bytes.Equal(got, wanted) {
			return fmt.Errorf("Standard authoring source workspace does not match its immutable snapshot")
		}
	}
	return nil
}

func markStandardAuthoringSourceReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe Standard authoring source workspace entry")
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o555)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular Standard authoring source workspace entry")
		}
		return os.Chmod(path, 0o444)
	})
}

func ensureStandardAuthoringWorkspaceRoot(root string) error {
	if err := os.Mkdir(root, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Standard authoring workspace root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("Standard authoring workspace root is unavailable or unsafe")
	}
	return nil
}

func standardAuthoringWorkspacePathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func standardAuthoringRepoPreparedEvidence(subject workflowRunSubject, gitVersion string) ([]byte, error) {
	if !subject.isAuthoringSession() || subject.AuthoringSource == nil || subject.AuthoringSession == nil || strings.TrimSpace(gitVersion) == "" {
		return nil, fmt.Errorf("Standard authoring repo_prepare evidence subject is invalid")
	}
	source := subject.AuthoringSource
	return json.Marshal(struct {
		Format             string `json:"format"`
		Version            string `json:"version"`
		AuthoringSourceID  string `json:"authoring_source_id"`
		AuthoringSessionID string `json:"authoring_session_id"`
		SourceURL          string `json:"source_url"`
		SourceCommit       string `json:"source_commit"`
		SnapshotDigest     string `json:"snapshot_digest"`
		SourceFingerprint  string `json:"source_fingerprint"`
		GitVersion         string `json:"git_version"`
	}{
		Format: standardAuthoringRepoPreparedEvidenceFormat, Version: standardAuthoringRepoPreparedEvidenceVersion,
		AuthoringSourceID: source.ID, AuthoringSessionID: subject.AuthoringSession.ID, SourceURL: source.RepositoryURL,
		SourceCommit: source.CommitSHA, SnapshotDigest: source.SnapshotContentDigest, SourceFingerprint: source.SourceFingerprint,
		GitVersion: gitVersion,
	})
}

var _ stageprovider.LocalCommandOperationExecutor = (*StandardAuthoringRepoPrepareExecutor)(nil)
