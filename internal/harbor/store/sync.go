package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/status"
)

// WorkspaceInfo is the result of scanning one workspace for sync.
type WorkspaceInfo struct {
	Status    status.WorkspaceStatus
	SizeBytes int64
}

// ScanWorkspaces scans rootDirs for workspace directories that contain state.json.
func ScanWorkspaces(ctx context.Context, rootDirs []string) ([]WorkspaceInfo, error) {
	var infos []WorkspaceInfo
	seen := map[string]bool{}

	for _, root := range rootDirs {
		select {
		case <-ctx.Done():
			return infos, ctx.Err()
		default:
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		candidates, err := workspaceCandidates(absRoot)
		if err != nil {
			continue
		}
		for _, wsPath := range candidates {
			select {
			case <-ctx.Done():
				return infos, ctx.Err()
			default:
			}
			if seen[wsPath] {
				continue
			}
			seen[wsPath] = true

			ws, err := status.ReadWorkspace(wsPath)
			if err != nil || !ws.StatePresent {
				continue
			}
			infos = append(infos, WorkspaceInfo{
				Status:    ws,
				SizeBytes: dirSize(wsPath),
			})
		}
	}
	return infos, nil
}

func workspaceCandidates(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, err
	}
	candidates := []string{root}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		candidates = append(candidates, path)
		if strings.EqualFold(entry.Name(), "workspaces") {
			children, childErr := os.ReadDir(path)
			if childErr != nil {
				continue
			}
			for _, child := range children {
				if child.IsDir() {
					candidates = append(candidates, filepath.Join(path, child.Name()))
				}
			}
		}
	}
	return candidates, nil
}

// SyncFromFilesystem scans workspaces and upserts them into the store.
func (s *Store) SyncFromFilesystem(ctx context.Context, rootDirs []string) error {
	infos, err := ScanWorkspaces(ctx, rootDirs)
	if err != nil {
		return err
	}

	for _, info := range infos {
		ws := info.Status
		var taskID int64

		// Determine task identity from workspace info
		taskDir := ""
		taskName := ""
		codeLang := ""
		taskType := ""
		application := ""
		repoURL := ""
		commitSHA := ""
		isGenerated := false

		if ws.RunOptions != nil {
			taskDir = ws.RunOptions.TaskDir
			taskName = ws.RunOptions.TaskName
			codeLang = ws.RunOptions.CodeLang
			taskType = ws.RunOptions.TaskType
			application = ws.RunOptions.Application
			repoURL = ws.RunOptions.RepoURL
			commitSHA = ws.RunOptions.Commit
			isGenerated = ws.RunOptions.Generate
		}

		// Fallback: use workspace path as task identity
		if taskDir == "" {
			taskDir = ws.Workspace
		}
		if taskName == "" {
			taskName = filepath.Base(ws.Workspace)
		}

		taskID, err = s.UpsertTask(Task{
			TaskDir:     taskDir,
			TaskName:    taskName,
			CodeLang:    codeLang,
			TaskType:    taskType,
			Application: application,
			RepoURL:     repoURL,
			CommitSHA:   commitSHA,
			IsGenerated: isGenerated,
		})
		if err != nil {
			return err
		}

		if err := s.UpsertRun(Run{
			TaskID:        taskID,
			WorkspacePath: ws.Workspace,
			RunID:         ws.RunID,
			Status:        ws.Status,
			Passed:        ws.Passed,
			StartedAt:     ws.StartedAt,
			FinishedAt:    ws.FinishedAt,
			SizeBytes:     info.SizeBytes,
			IsActive:      ws.Active,
			IsResumable:   ws.Resumable,
		}); err != nil {
			return err
		}
	}

	// Remove DB entries for workspaces that no longer exist
	existing, err := s.AllWorkspacePaths()
	if err != nil {
		return err
	}
	for _, p := range existing {
		if _, statErr := os.Stat(p); os.IsNotExist(statErr) {
			s.DeleteRunByWorkspace(p)
		}
	}
	return s.CleanOrphanTasks()
}

// RefreshRunning updates volatile status fields for indexed non-terminal runs
// without rescanning directory sizes or unrelated completed workspaces.
func (s *Store) RefreshRunning(ctx context.Context) error {
	paths, err := s.runningWorkspacePaths()
	if err != nil {
		return err
	}
	for _, path := range paths {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		indexed, err := s.GetRunByWorkspace(path)
		if err != nil {
			return err
		}
		if indexed == nil {
			continue
		}
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			if err := s.DeleteRunByWorkspace(path); err != nil {
				return err
			}
			continue
		}
		workspace, err := status.ReadWorkspace(path)
		if err != nil {
			continue
		}
		if err := s.UpsertRun(Run{
			TaskID:        indexed.TaskID,
			WorkspacePath: workspace.Workspace,
			RunID:         workspace.RunID,
			Status:        workspace.Status,
			Passed:        workspace.Passed,
			StartedAt:     workspace.StartedAt,
			FinishedAt:    workspace.FinishedAt,
			IsActive:      workspace.Active,
			IsResumable:   workspace.Resumable,
		}); err != nil {
			return err
		}
	}
	return s.CleanOrphanTasks()
}

// DefaultRootDirs returns the default workspace scan roots.
func DefaultRootDirs() []string {
	return []string{".harbor-factory"}
}

func dirSize(path string) int64 {
	var size int64
	filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return size
}
