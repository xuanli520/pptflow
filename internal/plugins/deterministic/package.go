package deterministic

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/packager"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	PackagePluginID = "harborfactory.package"
	PackageKind     = "harborfactory.package"
)

type PackageFunc func(packager.Options) (domain.PackageReport, error)

type PackagePlugin struct {
	Package PackageFunc
}

func (PackagePlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: PackagePluginID, Version: "1.0.0", Kinds: []string{PackageKind}}
}

func (PackagePlugin) Validate(spec workflow.NodeSpec) error {
	for _, field := range []string{"task_dir", "output_dir", "tests_analysis", "verify_report", "similarity_report", "qwen_result", "opus_result"} {
		if err := pluginutil.RequiredString(spec, field); err != nil {
			return err
		}
	}
	return nil
}

func (p PackagePlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("package artifact store is required")
	}
	packageTask := p.Package
	if packageTask == nil {
		packageTask = packager.Package
	}
	report, err := packageTask(packager.Options{
		TaskDir:          pluginutil.String(req, "task_dir"),
		OutputDir:        pluginutil.String(req, "output_dir"),
		TaskName:         pluginutil.String(req, "task_name"),
		CodeLang:         pluginutil.String(req, "code_lang"),
		TaskType:         pluginutil.String(req, "task_type"),
		Application:      pluginutil.String(req, "application"),
		AHT:              pluginutil.String(req, "aht"),
		Description:      pluginutil.String(req, "description"),
		IsZeroToOne:      pluginutil.Bool(req, "is_zero_to_one"),
		GitHubURL:        pluginutil.String(req, "github_url"),
		CommitID:         pluginutil.String(req, "commit_id"),
		TestsAnalysis:    pluginutil.String(req, "tests_analysis"),
		VerifyReport:     pluginutil.String(req, "verify_report"),
		QualityReport:    pluginutil.String(req, "quality_report"),
		SimilarityReport: pluginutil.String(req, "similarity_report"),
		QwenResult:       pluginutil.String(req, "qwen_result"),
		OpusResult:       pluginutil.String(req, "opus_result"),
		QwenScreenshot:   pluginutil.String(req, "qwen_screenshot"),
		OpusScreenshot:   pluginutil.String(req, "opus_screenshot"),
	})
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("package Harbor task: %w", err)
	}
	if !report.Passed {
		return workflow.NodeResult{}, workflow.NewNodeError(workflow.FailurePermanent, false, "package", fmt.Errorf("report did not pass"))
	}
	reportRef, err := req.Store.PutJSON(ctx, pluginutil.ArtifactName(req, "phase3/artifacts/"+req.Spec.ID+"/package_report.json"), "package_report", req.Spec.ID, report)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("store package report: %w", err)
	}
	zip, err := os.Open(report.OutputZip)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("open packaged ZIP: %w", err)
	}
	defer zip.Close()
	zipName := pluginutil.String(req, "zip_artifact_name")
	if zipName == "" {
		zipName = filepath.ToSlash(filepath.Join("phase3", "artifacts", req.Spec.ID, filepath.Base(report.OutputZip)))
	}
	zipRef, err := req.Store.Put(ctx, workflow.PutArtifactRequest{Name: zipName, Type: "harbor_task_zip", Producer: req.Spec.ID, Content: zip})
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("store packaged ZIP: %w", err)
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{reportRef, zipRef}}, nil
}
