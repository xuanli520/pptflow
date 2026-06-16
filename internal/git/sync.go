package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/projectlayout"
)

const cloneDoneMarker = ".qa-clone-done"

var (
	chmodUserWritableTreeFunc = chmodUserWritableTree
	renamePathFunc            = os.Rename
)

type Syncer struct {
	BasePath string
	cfg      config.GitConfig
}

func NewSyncer(basePath string, cfg config.GitConfig) *Syncer {
	if strings.TrimSpace(basePath) == "" {
		basePath = "."
	}
	return &Syncer{
		BasePath: absClean(basePath),
		cfg:      cfg,
	}
}

func (s *Syncer) RepoPath(batchID, taskID string) string {
	batch := safePathSegment(batchID, "unknown-batch")
	task := safePathSegment(taskID, "unknown-task")
	return filepath.Join(s.basePath(), batch, task, task)
}

func (s *Syncer) Sync(ctx context.Context, taskID, batchID, gitURL string, onProgress SyncCallback) (*SyncResult, error) {
	task, err := validatePathSegment("taskID", taskID)
	if err != nil {
		return nil, err
	}
	batch, err := validatePathSegment("batchID", batchID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(gitURL) == "" {
		return nil, errors.New("gitURL is required")
	}

	if s.cfg.CloneTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.CloneTimeout)
		defer cancel()
	}

	taskPath := filepath.Join(s.basePath(), batch, task)
	repoPath := filepath.Join(taskPath, task)
	for _, path := range []string{taskPath, repoPath} {
		if err := s.ensureUnderBase(path); err != nil {
			return nil, err
		}
	}

	markerPath := filepath.Join(taskPath, cloneDoneMarker)
	if _, err := os.Stat(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat clone marker: %w", err)
	}
	if s.existingClone(repoPath) {
		return s.forcePull(ctx, taskPath, repoPath, repoPath, markerPath, gitURL, onProgress)
	}
	if err := s.discardIncompleteClone(taskPath, onProgress); err != nil {
		return nil, err
	}
	return s.clone(ctx, repoPath, repoPath, markerPath, gitURL, onProgress)
}

func (s *Syncer) existingClone(repoPath string) bool {
	if info, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil || !info.IsDir() {
		return false
	}
	if info, err := os.Stat(repoPath); err != nil || !info.IsDir() {
		return false
	}
	return true
}

func (s *Syncer) discardIncompleteClone(clonePath string, onProgress SyncCallback) error {
	info, err := os.Stat(clonePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat incomplete clone target: %w", err)
	}
	if info.IsDir() {
		empty, err := dirIsEmpty(clonePath)
		if err != nil {
			return err
		}
		if empty {
			return nil
		}
	}
	if err := s.ensureRemovableCloneTarget(clonePath); err != nil {
		return err
	}
	emit(onProgress, "cleanup", -1, "removing incomplete clone")
	if err := os.RemoveAll(clonePath); err != nil {
		return fmt.Errorf("remove incomplete clone target: %w", err)
	}
	return nil
}

func (s *Syncer) ensureRemovableCloneTarget(path string) error {
	if err := s.ensureUnderBase(path); err != nil {
		return err
	}
	base := s.basePath()
	target := absClean(path)
	if target == base {
		return fmt.Errorf("refuse to remove clone base path %s", base)
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return fmt.Errorf("validate removable clone target %s under %s: %w", target, base, err)
	}
	if len(strings.Split(rel, string(filepath.Separator))) < 2 {
		return fmt.Errorf("refuse to remove clone target outside batch/task scope: %s", target)
	}
	return nil
}

func (s *Syncer) clone(ctx context.Context, clonePath, repoPath, markerPath, gitURL string, onProgress SyncCallback) (*SyncResult, error) {
	emit(onProgress, "clone", 0, "cloning repository")
	if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
		return nil, fmt.Errorf("create clone parent: %w", err)
	}
	if exists, err := pathExists(clonePath); err != nil {
		return nil, err
	} else if exists {
		empty, err := dirIsEmpty(clonePath)
		if err != nil {
			return nil, err
		}
		if !empty {
			return nil, fmt.Errorf("clone target %s exists without %s marker", clonePath, cloneDoneMarker)
		}
	}

	args := []string{"clone"}
	if s.cfg.ShallowClone {
		args = append(args, "--depth", "1")
	}
	args = append(args, gitURL, clonePath)
	if err := s.runGit(ctx, "", args...); err != nil {
		return nil, err
	}
	if err := s.verifyGitRepo(clonePath); err != nil {
		return nil, err
	}
	if err := s.afterSync(ctx, clonePath); err != nil {
		return nil, err
	}
	if err := s.verifyDeliveryPackage(repoPath); err != nil {
		return nil, err
	}
	commit, err := s.currentCommit(ctx, clonePath)
	if err != nil {
		return nil, err
	}
	if err := writeMarker(markerPath, commit); err != nil {
		return nil, err
	}
	emit(onProgress, "clone", 100, "repository cloned")
	return &SyncResult{Operation: "clone", Commit: commit, RepoPath: repoPath, ClonePath: clonePath}, nil
}

func (s *Syncer) forcePull(ctx context.Context, taskPath, clonePath, repoPath, markerPath, gitURL string, onProgress SyncCallback) (*SyncResult, error) {
	emit(onProgress, "fetch", -1, "fetching updates")
	if err := s.verifyGitRepo(clonePath); err != nil {
		return nil, err
	}
	stashMessage := "auto-stash-before-force-pull-" + time.Now().UTC().Format("20060102T150405Z")
	if err := s.runGit(ctx, clonePath, "stash", "push", "-u", "-m", stashMessage); err != nil {
		return nil, err
	}
	if err := s.runGit(ctx, clonePath, "fetch", "origin", "--force", "--prune"); err != nil {
		return nil, err
	}

	emit(onProgress, "reset", -1, "resetting to remote HEAD")
	target, err := s.remoteHead(ctx, clonePath)
	if err != nil {
		return nil, err
	}
	if err := s.runGit(ctx, clonePath, "reset", "--hard", target); err != nil {
		return nil, err
	}

	emit(onProgress, "clean", -1, "cleaning working tree")
	if err := s.runGit(ctx, clonePath, "clean", "-fdx"); err != nil {
		recloned, result, recoverErr := s.recoverCleanFailure(ctx, taskPath, clonePath, repoPath, markerPath, gitURL, err, onProgress)
		if recoverErr != nil {
			return nil, recoverErr
		}
		if recloned {
			return result, nil
		}
	}
	if err := s.afterSync(ctx, clonePath); err != nil {
		return nil, err
	}
	if err := s.verifyClone(clonePath, repoPath); err != nil {
		return nil, err
	}
	commit, err := s.currentCommit(ctx, clonePath)
	if err != nil {
		return nil, err
	}
	if err := writeMarker(markerPath, commit); err != nil {
		return nil, err
	}
	emit(onProgress, "force-pull", 100, "repository synchronized")
	return &SyncResult{Operation: "force-pull", Commit: commit, RepoPath: repoPath, ClonePath: clonePath}, nil
}

func (s *Syncer) recoverCleanFailure(ctx context.Context, taskPath, clonePath, repoPath, markerPath, gitURL string, cleanErr error, onProgress SyncCallback) (bool, *SyncResult, error) {
	if !isCleanPermissionError(cleanErr) {
		return false, nil, cleanErr
	}
	if err := s.ensureRemovableCloneTarget(taskPath); err != nil {
		return false, nil, fmt.Errorf("%w; refusing git clean recovery for unsafe task path %s: %v", cleanErr, taskPath, err)
	}
	if err := s.ensureTaskCloneBoundary(taskPath); err != nil {
		return false, nil, fmt.Errorf("%w; refusing git clean recovery for unsafe task path %s: %v", cleanErr, taskPath, err)
	}

	recoveryCause := cleanErr
	emit(onProgress, "clean", -1, "repairing working tree permissions")
	if chmodErr := chmodUserWritableTreeFunc(clonePath); chmodErr == nil {
		emit(onProgress, "clean", -1, "retrying clean after permission repair")
		if retryErr := s.runGit(ctx, clonePath, "clean", "-fdx"); retryErr == nil {
			return false, nil, nil
		} else {
			recoveryCause = fmt.Errorf("retry clean after chmod: %w", retryErr)
		}
	} else {
		recoveryCause = fmt.Errorf("chmod recovery: %w", chmodErr)
	}

	emit(onProgress, "cleanup", -1, "quarantining contaminated working tree")
	quarantinePath, err := s.quarantineTaskPath(taskPath)
	if err != nil {
		return false, nil, cleanRecoveryError(cleanErr, taskPath, "", fmt.Errorf("%v; %w", recoveryCause, err))
	}
	emit(onProgress, "clone", -1, "fresh clone after quarantining contaminated working tree")
	result, cloneErr := s.clone(ctx, clonePath, repoPath, markerPath, gitURL, onProgress)
	if cloneErr != nil {
		return false, nil, cleanRecoveryError(cleanErr, taskPath, quarantinePath, fmt.Errorf("%v; %w", recoveryCause, cloneErr))
	}
	result.Operation = "reclone"
	return true, result, nil
}

func (s *Syncer) quarantineTaskPath(taskPath string) (string, error) {
	if err := s.ensureTaskCloneBoundary(taskPath); err != nil {
		return "", err
	}
	root := filepath.Join(s.basePath(), ".qa-control", "git-sync-quarantine")
	if err := s.ensureUnderBase(root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create git sync quarantine root: %w", err)
	}
	name := safePathSegment(filepath.Base(taskPath), "task") + "-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	target := filepath.Join(root, name)
	if err := renamePathFunc(taskPath, target); err != nil {
		return "", fmt.Errorf("quarantine git sync task path %s to %s: %w", taskPath, target, err)
	}
	return target, nil
}

func (s *Syncer) ensureTaskCloneBoundary(path string) error {
	if err := s.ensureUnderBase(path); err != nil {
		return err
	}
	rel, err := filepath.Rel(s.basePath(), absClean(path))
	if err != nil {
		return fmt.Errorf("validate task clone boundary: %w", err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		return fmt.Errorf("refuse to recover git clone outside exact batch/task scope: %s", path)
	}
	if _, err := validatePathSegment("batchID", parts[0]); err != nil {
		return err
	}
	if strings.HasPrefix(parts[0], ".") {
		return fmt.Errorf("refuse to recover git clone inside control directory: %s", path)
	}
	if _, err := validatePathSegment("taskID", parts[1]); err != nil {
		return err
	}
	return nil
}

func (s *Syncer) afterSync(ctx context.Context, clonePath string) error {
	if s.cfg.LFSEnabled {
		if err := s.runGit(ctx, clonePath, "lfs", "pull"); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(clonePath, ".gitmodules")); err == nil {
		return s.runGit(ctx, clonePath, "submodule", "update", "--init", "--recursive")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat .gitmodules: %w", err)
	}
	return nil
}

func (s *Syncer) verifyClone(clonePath, repoPath string) error {
	if err := s.verifyGitRepo(clonePath); err != nil {
		return err
	}
	return s.verifyDeliveryPackage(repoPath)
}

func (s *Syncer) verifyGitRepo(clonePath string) error {
	if info, err := os.Stat(filepath.Join(clonePath, ".git")); err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New(".git is not a directory")
		}
		return fmt.Errorf("verify clone: %w", err)
	}
	return nil
}

func (s *Syncer) verifyDeliveryPackage(repoPath string) error {
	if info, err := os.Stat(repoPath); err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("delivery package is not a directory")
		}
		return &DeliveryPackageError{RepoPath: repoPath, Err: err}
	}
	validation := projectlayout.ValidatePackageRoot(repoPath)
	if !validation.Valid {
		return &DeliveryPackageError{RepoPath: repoPath, Missing: validation.Missing}
	}
	return nil
}

func (s *Syncer) remoteHead(ctx context.Context, dir string) (string, error) {
	output, err := s.runGitOutput(ctx, dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		target := strings.TrimSpace(output)
		if target != "" {
			return target, nil
		}
	}
	for _, target := range []string{"origin/main", "origin/master"} {
		if err := s.runGit(ctx, dir, "rev-parse", "--verify", target); err == nil {
			return target, nil
		}
	}
	if strings.TrimSpace(output) != "" {
		return "", fmt.Errorf("resolve remote HEAD: %s", strings.TrimSpace(output))
	}
	return "", errors.New("resolve remote HEAD: origin/HEAD, origin/main, and origin/master are unavailable")
}

func (s *Syncer) currentCommit(ctx context.Context, dir string) (string, error) {
	output, err := s.runGitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(output)
	if commit == "" {
		return "", errors.New("empty git commit")
	}
	return commit, nil
}

func (s *Syncer) runGit(ctx context.Context, dir string, args ...string) error {
	_, err := s.runGitOutput(ctx, dir, args...)
	return err
}

func (s *Syncer) runGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = s.gitEnv()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.String(), ctxErr
	}
	if err != nil {
		return stdout.String(), &CommandError{
			Dir:    dir,
			Args:   append([]string(nil), args...),
			Stdout: stdout.String(),
			Stderr: stderr.String(),
			Err:    err,
		}
	}
	return stdout.String(), nil
}

func (s *Syncer) gitEnv() []string {
	env := os.Environ()
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	if !s.cfg.LFSEnabled {
		env = append(env, "GIT_LFS_SKIP_SMUDGE=1")
	}
	return env
}

func (s *Syncer) basePath() string {
	if strings.TrimSpace(s.BasePath) == "" {
		return absClean(".")
	}
	return absClean(s.BasePath)
}

func (s *Syncer) ensureUnderBase(path string) error {
	base := s.basePath()
	target := absClean(path)
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return fmt.Errorf("validate path %s under %s: %w", target, base, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path %s escapes base path %s", target, base)
	}
	return nil
}

func isCleanPermissionError(err error) bool {
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	if len(commandErr.Args) != 2 || commandErr.Args[0] != "clean" || commandErr.Args[1] != "-fdx" {
		return false
	}
	text := strings.ToLower(strings.Join([]string{commandErr.Stdout, commandErr.Stderr, commandErr.Error()}, "\n"))
	for _, marker := range []string{"permission denied", "failed to remove", "unable to unlink"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func chmodUserWritableTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		next := mode | 0o600
		if entry.IsDir() || mode&0o111 != 0 {
			next |= 0o100
		}
		if next.Perm() == mode.Perm() {
			return nil
		}
		return os.Chmod(path, next.Perm())
	})
}

func cleanRecoveryError(cleanErr error, taskPath, quarantinePath string, cause error) error {
	quarantineMessage := ""
	if strings.TrimSpace(quarantinePath) != "" {
		quarantineMessage = "; quarantined at " + quarantinePath
	}
	return fmt.Errorf("%w; failed to recover git clean permission error for %s%s: %v; manual cleanup: fix ownership or remove the path, then retry", cleanErr, taskPath, quarantineMessage, cause)
}

func emit(callback SyncCallback, phase string, percent int, message string) {
	if callback == nil {
		return
	}
	callback(SyncProgress{Phase: phase, Percent: percent, Message: message})
}

func writeMarker(path, commit string) error {
	content := []byte("commit=" + strings.TrimSpace(commit) + "\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write clone marker: %w", err)
	}
	return nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

func dirIsEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Errorf("read clone target: %w", err)
	}
	return len(entries) == 0, nil
}

func validatePathSegment(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return "", fmt.Errorf("%s is not a safe path segment", name)
	}
	if filepath.IsAbs(value) || strings.ContainsAny(value, `/\`) {
		return "", fmt.Errorf("%s is not a safe path segment", name)
	}
	if filepath.Clean(value) != value {
		return "", fmt.Errorf("%s is not a safe path segment", name)
	}
	return value, nil
}

func safePathSegment(value, fallback string) string {
	if segment, err := validatePathSegment("path segment", value); err == nil {
		return segment
	}
	return fallback
}

func absClean(path string) string {
	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(cleaned); err == nil {
		return abs
	}
	return cleaned
}
