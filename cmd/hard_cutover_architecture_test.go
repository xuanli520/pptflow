package cmd

import (
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const hardCutoverModulePath = "github.com/purplevoid/harbor-factory"

func TestHardCutoverSourcesDoNotImportV1ExecutionGraph(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := []string{
		hardCutoverModulePath + "/internal/workflow",
		hardCutoverModulePath + "/internal/plugins",
		hardCutoverModulePath + "/internal/runtime/command",
		hardCutoverModulePath + "/internal/runtime/harborruntime",
		hardCutoverModulePath + "/internal/harbor/gen",
		hardCutoverModulePath + "/internal/harbor/quality",
		hardCutoverModulePath + "/internal/harbor/repair",
		hardCutoverModulePath + "/internal/harbor/nodes",
		hardCutoverModulePath + "/internal/harbor/status",
		hardCutoverModulePath + "/internal/harbor/runlock",
		hardCutoverModulePath + "/internal/harbor/domain",
		hardCutoverModulePath + "/internal/harbor/sanitize",
		hardCutoverModulePath + "/internal/harbor/harborrun",
		hardCutoverModulePath + "/internal/harbor/evidence",
		hardCutoverModulePath + "/internal/harbor/lint",
		hardCutoverModulePath + "/internal/harbor/packager",
		hardCutoverModulePath + "/internal/harbor/repoprep",
		hardCutoverModulePath + "/internal/harbor/repourl",
		hardCutoverModulePath + "/internal/harbor/similarity",
		hardCutoverModulePath + "/internal/harbor/verify",
		hardCutoverModulePath + "/internal/runmodel",
		hardCutoverModulePath + "/internal/templates",
		hardCutoverModulePath + "/internal/harbor/stageexecutor",
	}
	legacySourcePrefixes := []string{
		"internal/workflow/",
		"internal/plugins/",
		"internal/runtime/command/",
		"internal/runtime/harborruntime/",
		"internal/harbor/gen/",
		"internal/harbor/nodes/",
		"internal/harbor/status/",
		"internal/harbor/runlock/",
		"internal/harbor/domain/",
		"internal/harbor/sanitize/",
		"internal/harbor/harborrun/",
		"internal/harbor/quality/",
		"internal/harbor/repair/",
		"internal/harbor/evidence/",
		"internal/harbor/lint/",
		"internal/harbor/packager/",
		"internal/harbor/repoprep/",
		"internal/harbor/repourl/",
		"internal/harbor/similarity/",
		"internal/harbor/verify/",
		"internal/runmodel/",
		"internal/templates/",
		"internal/harbor/stageexecutor/",
	}
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".harbor-factory", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		relative, relativeErr := filepath.Rel(root, path)
		if relativeErr != nil {
			return relativeErr
		}
		relative = filepath.ToSlash(relative)
		for _, prefix := range legacySourcePrefixes {
			if strings.HasPrefix(relative, prefix) {
				violations = append(violations, relative+" remains under a retired V1 source path")
				return nil
			}
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				return err
			}
			for _, banned := range forbidden {
				if importPath == banned || strings.HasPrefix(importPath, banned+"/") {
					violations = append(violations, relative+" imports "+importPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk source tree: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("hard cutover retained V1 execution imports:\n%s", strings.Join(violations, "\n"))
	}
}

func TestHardCutoverRemovesLegacyExecutionSources(t *testing.T) {
	root := repositoryRoot(t)
	for _, relativePath := range []string{
		"internal/app/runner.go",
		"internal/app/runner_engine.go",
		"internal/app/task_scheduler.go",
		"internal/app/workflow_definition.go",
		"internal/app/production_registry.go",
		"internal/app/runtime_options.go",
		"internal/app/legacy_import.go",
		"internal/harbor/store/task_store.go",
		"internal/harbor/store/run_store.go",
		"internal/harbor/store/sync.go",
		"internal/workflow/engine.go",
		"internal/plugins/gen/plugins.go",
		"internal/harbor/legacyv1/reader.go",
		"internal/harbor/harborrun/pyshim/harbor_factory_retrying_claude.py",
		"internal/templates/engine.go",
		"internal/templates/phase1/repo_analyze.md",
		"internal/templates/phase1/task_design.md",
		"internal/templates/phase1/task_files.md",
		"internal/templates/phase2/quality_check.md",
		"internal/templates/phase2/runtime_self_check.md",
		"internal/templates/phase2/task_repair.md",
		"internal/harbor/stageexecutor/registry.go",
	} {
		_, err := os.Stat(filepath.Join(root, relativePath))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy execution source %s remains available: %v", relativePath, err)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve hard-cutover test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
}
