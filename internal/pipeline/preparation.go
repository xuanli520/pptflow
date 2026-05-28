package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/projectlayout"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func (r Runner) normalizeRunOptions(ctx context.Context, project scanner.Project, opts RunOptions) (RunOptions, error) {
	var err error
	if opts, err = normalizeStageOptions(opts); err != nil {
		return opts, err
	}
	opts.Mode = strings.ToLower(strings.TrimSpace(opts.Mode))
	if opts.Mode == "" {
		opts.Mode = "initial"
	}
	if opts.StaticOnly || r.cfg.Pipeline.StaticOnly {
		if stage := explicitRuntimeStageSelection(opts); stage != "" {
			return opts, fmt.Errorf("static-only mode cannot run runtime stage %s", stage)
		}
	}
	if opts.Stage == "" && opts.From == "" && len(opts.Stages) == 0 {
		if stages := r.cfg.Pipeline.DefaultStages[opts.Mode]; len(stages) > 0 {
			opts.Stages = append([]string(nil), stages...)
		}
	}
	switch opts.Mode {
	case "initial":
		if strings.TrimSpace(opts.RefRun) != "" {
			return opts, fmt.Errorf("--ref-run is only valid with --mode recheck")
		}
		if len(opts.ExtraDocs) > 0 {
			return opts, fmt.Errorf("--extra-docs is only valid with --mode recheck")
		}
	case "recheck":
		opts.RefRun = strings.TrimSpace(opts.RefRun)
		if opts.RefRun == "" {
			return opts, fmt.Errorf("--mode recheck requires --ref-run")
		}
		ref, err := r.store.GetRun(ctx, opts.RefRun)
		if err != nil {
			return opts, fmt.Errorf("ref run %s does not exist: %w", opts.RefRun, err)
		}
		if ref.TaskID != project.TaskID {
			return opts, fmt.Errorf("ref run %s belongs to task %s, not %s", opts.RefRun, ref.TaskID, project.TaskID)
		}
		if !completedRefRunStatus(ref.Status) {
			return opts, fmt.Errorf("ref run %s status is %s; --mode recheck requires a completed reference run", opts.RefRun, ref.Status)
		}
		if !dirExists(ref.ArtifactRoot) {
			return opts, fmt.Errorf("ref run %s artifact root is missing: %s", opts.RefRun, ref.ArtifactRoot)
		}
	default:
		return opts, fmt.Errorf("invalid --mode %q; expected initial or recheck", opts.Mode)
	}
	return opts, nil
}

func explicitRuntimeStageSelection(opts RunOptions) string {
	if opts.Stage != "" && model.IsRuntimeStage(opts.Stage) {
		return opts.Stage
	}
	if opts.From != "" && model.IsRuntimeStage(opts.From) {
		return opts.From
	}
	for _, stage := range opts.Stages {
		if model.IsRuntimeStage(stage) {
			return stage
		}
	}
	return ""
}

func normalizeStageOptions(opts RunOptions) (RunOptions, error) {
	if opts.Stage != "" {
		stage, ok := model.NormalizeStage(opts.Stage)
		if !ok {
			return opts, fmt.Errorf("invalid stage %q; expected A..F", opts.Stage)
		}
		opts.Stage = stage
	}
	if opts.From != "" {
		from, ok := model.NormalizeStage(opts.From)
		if !ok {
			return opts, fmt.Errorf("invalid from stage %q; expected A..F", opts.From)
		}
		opts.From = from
	}
	if len(opts.Stages) > 0 {
		normalized := make([]string, 0, len(opts.Stages))
		seen := map[string]bool{}
		for _, raw := range opts.Stages {
			stage, ok := model.NormalizeStage(raw)
			if !ok {
				return opts, fmt.Errorf("invalid stage %q in stage list; expected A..F", raw)
			}
			if seen[stage] {
				continue
			}
			seen[stage] = true
			normalized = append(normalized, stage)
		}
		opts.Stages = normalized
	}
	return opts, nil
}

func completedRefRunStatus(status string) bool {
	return status == model.RunCompletedClean || status == model.RunCompletedWithFindings
}

func (r Runner) canonicalizeProjectForRun(project scanner.Project) (scanner.Project, []ProjectPathWarning, error) {
	dbPath := filepath.Clean(project.Path)
	expected := projectlayout.ExpectedProjectPath(r.cfg.ScanPath, project.Batch, project.TaskID)
	validation := projectlayout.ValidatePackageRoot(expected)
	if !validation.Valid {
		return project, nil, invalidIndexedProjectPathError(r.cfg.ScanPath, expected)
	}

	project.Path = filepath.Clean(expected)
	project.MetadataPromptMissing = projectlayout.MetadataPromptMissing(project.Path)
	if dbPath != project.Path {
		return project, []ProjectPathWarning{{
			Type:          "stale_project_path",
			DBPath:        dbPath,
			CanonicalPath: project.Path,
		}}, nil
	}
	return project, nil, nil
}

func invalidIndexedProjectPathError(scanRoot, expected string) error {
	return fmt.Errorf("indexed project path is invalid or stale:\nexpected package root %s\nbut it does not contain metadata.json, docs/, repo/, and an original session marker.\nPlease rerun p2r scan --path %s; if old artifact rows remain, run p2r scan --path %s --prune-artifacts.", expected, filepath.Clean(scanRoot), filepath.Clean(scanRoot))
}

func (warning ProjectPathWarning) Message() string {
	if warning.Type == "stale_project_path" {
		return fmt.Sprintf("DB project path was stale; runtime used canonical package root.\ndb_path=%s\ncanonical_path=%s", warning.DBPath, warning.CanonicalPath)
	}
	return fmt.Sprintf("project path warning: %s db_path=%s canonical_path=%s", warning.Type, warning.DBPath, warning.CanonicalPath)
}

func formatProjectPathWarnings(warnings []ProjectPathWarning) string {
	var builder strings.Builder
	builder.WriteString("=== project path warnings ===\n")
	for _, warning := range warnings {
		builder.WriteString(warning.Message())
		builder.WriteString("\n")
	}
	return builder.String()
}
