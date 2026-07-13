package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// LegacyImportRequest deliberately separates an exact canonical identity from
// a source path. A path is evidence to retain, never an identity to merge.
type LegacyImportRequest struct {
	SourceDirectory string
	Slug            string
	Title           string
	MetadataJSON    string
	SourceRepo      string
	SourceCommit    string
	LegacyIdentity  string
	ExactIdentity   bool
	Actor           string
	Reason          string
}

type LegacyImportResult struct {
	Task     store.TaskV2
	Revision *store.TaskRevision
	Merged   bool
	Orphan   bool
}

// ImportLegacy follows the confirmed migration policy: only a supplied,
// complete exact identity can merge. Ambiguous, missing, or non-V2-complete
// sources become distinct legacy_orphan tasks with their original bytes held
// under a managed directory for later human review.
func (service *TaskService) ImportLegacy(ctx context.Context, request LegacyImportRequest) (LegacyImportResult, error) {
	if service == nil || service.core == nil {
		return LegacyImportResult{}, fmt.Errorf("task service is not configured")
	}
	if strings.TrimSpace(request.SourceDirectory) == "" {
		return LegacyImportResult{}, fmt.Errorf("legacy source directory is required")
	}
	identity := strings.TrimSpace(request.LegacyIdentity)
	exact := request.ExactIdentity && identity != "" && strings.TrimSpace(request.SourceRepo) != "" && strings.TrimSpace(request.SourceCommit) != ""
	strictSnapshot := harborrun.ValidateManagedTaskSnapshotV2(request.SourceDirectory) == nil
	if exact {
		existing, err := service.core.store.FindTaskByCanonicalIdentity(ctx, store.CanonicalIdentityLookup{
			LegacyIdentity: identity,
			SourceRepo:     request.SourceRepo,
			SourceCommit:   request.SourceCommit,
		})
		if err != nil {
			return LegacyImportResult{}, err
		}
		if existing != nil {
			result := LegacyImportResult{Task: *existing, Merged: true}
			if !strictSnapshot {
				return result, nil
			}
			digest, err := harborrun.ComputeManagedTaskDigestV2(request.SourceDirectory)
			if err != nil {
				return LegacyImportResult{}, err
			}
			revisions, err := service.core.store.ListTaskRevisions(ctx, existing.ID)
			if err != nil {
				return LegacyImportResult{}, err
			}
			for _, revision := range revisions {
				if revision.TaskDigest == digest {
					matched := revision
					result.Revision = &matched
					return result, nil
				}
			}
			revision, err := (&RevisionService{core: service.core}).CreateFromSnapshot(ctx, CreateRevisionFromSnapshotRequest{
				TaskID:           existing.ID,
				ParentRevisionID: existing.CurrentRevisionID,
				Origin:           store.RevisionOriginImported,
				SourceDirectory:  request.SourceDirectory,
				ChangeSummary:    "legacy import with exact canonical identity",
				MetadataJSON:     request.MetadataJSON,
				Actor:            request.Actor,
				Reason:           request.Reason,
			})
			if err != nil {
				return LegacyImportResult{}, err
			}
			result.Revision = &revision
			return result, nil
		}
		if strictSnapshot {
			task, revision, err := service.createTaskWithInitialSnapshot(ctx, CreateDraftTaskRequest{
				Slug:           legacySlug(request.Slug),
				Title:          request.Title,
				MetadataJSON:   request.MetadataJSON,
				SourceRepo:     request.SourceRepo,
				SourceCommit:   request.SourceCommit,
				LegacyIdentity: identity,
				Actor:          request.Actor,
				Reason:         request.Reason,
			}, CreateRevisionFromSnapshotRequest{
				Origin:          store.RevisionOriginImported,
				SourceDirectory: request.SourceDirectory,
				ChangeSummary:   "legacy import with exact canonical identity",
				MetadataJSON:    request.MetadataJSON,
				Actor:           request.Actor,
				Reason:          request.Reason,
			})
			if err != nil {
				return LegacyImportResult{}, err
			}
			return LegacyImportResult{Task: task, Revision: &revision}, nil
		}
	}
	return service.importLegacyOrphan(ctx, request, identity)
}

func (service *TaskService) importLegacyOrphan(ctx context.Context, request LegacyImportRequest, identity string) (LegacyImportResult, error) {
	if identity == "" {
		absolute, err := filepath.Abs(request.SourceDirectory)
		if err != nil {
			return LegacyImportResult{}, err
		}
		identity = "unresolved legacy source: " + filepath.Clean(absolute)
	}
	if harborrun.ValidateManagedTaskSnapshotV2(request.SourceDirectory) == nil {
		task, revision, err := service.createTaskWithInitialSnapshotIdentity(ctx, CreateDraftTaskRequest{
			Slug:           legacySlug(request.Slug),
			Title:          request.Title,
			MetadataJSON:   request.MetadataJSON,
			SourceRepo:     request.SourceRepo,
			SourceCommit:   request.SourceCommit,
			LegacyIdentity: identity,
			Actor:          request.Actor,
			Reason:         request.Reason,
		}, CreateRevisionFromSnapshotRequest{
			Origin:          store.RevisionOriginImported,
			SourceDirectory: request.SourceDirectory,
			ChangeSummary:   "legacy orphan import",
			MetadataJSON:    request.MetadataJSON,
			Actor:           request.Actor,
			Reason:          request.Reason,
		}, store.TaskIdentityLegacyOrphan)
		if err != nil {
			return LegacyImportResult{}, err
		}
		return LegacyImportResult{Task: task, Revision: &revision, Orphan: true}, nil
	}
	task, err := service.core.store.CreateLegacyOrphan(ctx, store.CreateTaskV2Request{
		Slug:           legacySlug(request.Slug),
		Title:          request.Title,
		MetadataJSON:   request.MetadataJSON,
		SourceRepo:     request.SourceRepo,
		SourceCommit:   request.SourceCommit,
		LegacyIdentity: identity,
		Actor:          request.Actor,
		Reason:         request.Reason,
	})
	if err != nil {
		return LegacyImportResult{}, err
	}
	if err := service.core.layout.ensureRoot(); err != nil {
		return LegacyImportResult{}, err
	}
	destination := filepath.Join(service.core.layout.taskDirectory(task.ID), "legacy-snapshot")
	if err := copyLegacySource(ctx, request.SourceDirectory, destination); err != nil {
		return LegacyImportResult{}, err
	}
	return LegacyImportResult{Task: task, Orphan: true}, nil
}

func legacySlug(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "legacy-orphan"
	}
	return value
}

func copyLegacySource(ctx context.Context, source, destination string) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	source, err := filepath.Abs(strings.TrimSpace(source))
	if err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("legacy source is not a real directory")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0o750); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(destination)
		}
	}()
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("legacy source path escapes root")
		}
		destinationPath := filepath.Join(destination, relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("legacy source symlink is not allowed: %s", relative)
		}
		if entry.IsDir() {
			return os.Mkdir(destinationPath, 0o750)
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			return fmt.Errorf("legacy source non-regular file is not allowed: %s", relative)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = input.Close()
			return err
		}
		_, err = io.Copy(output, input)
		inputErr := input.Close()
		outputErr := output.Close()
		if err != nil {
			return err
		}
		if inputErr != nil {
			return inputErr
		}
		return outputErr
	})
	if err != nil {
		return err
	}
	committed = true
	return nil
}
