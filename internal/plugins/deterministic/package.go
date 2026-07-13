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
	if err := removeStalePackageOutputs(pluginutil.String(req, "output_dir"), pluginutil.String(req, "task_name")); err != nil {
		return workflow.NodeResult{}, fmt.Errorf("remove stale package outputs: %w", err)
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
	report.TaskZipArtifact = zipRef.Path
	artifacts := []workflow.ArtifactRef{zipRef}
	if report.DeliveryZip != "" {
		delivery, err := os.Open(report.DeliveryZip)
		if err != nil {
			return workflow.NodeResult{Artifacts: artifacts}, fmt.Errorf("open delivery ZIP: %w", err)
		}
		defer delivery.Close()
		deliveryName := pluginutil.String(req, "delivery_artifact_name")
		if deliveryName == "" {
			deliveryName = filepath.ToSlash(filepath.Join("phase3", "artifacts", req.Spec.ID, filepath.Base(report.DeliveryZip)))
		}
		deliveryRef, err := req.Store.Put(ctx, workflow.PutArtifactRequest{Name: deliveryName, Type: "submission_delivery_zip", Producer: req.Spec.ID, Content: delivery})
		if err != nil {
			return workflow.NodeResult{Artifacts: artifacts}, fmt.Errorf("store delivery ZIP: %w", err)
		}
		report.DeliveryZipArtifact = deliveryRef.Path
		artifacts = append(artifacts, deliveryRef)
	}
	reportRef, err := req.Store.PutJSON(ctx, pluginutil.ArtifactName(req, "phase3/artifacts/"+req.Spec.ID+"/package_report.json"), "package_report", req.Spec.ID, report)
	if err != nil {
		return workflow.NodeResult{Artifacts: artifacts}, fmt.Errorf("store package report: %w", err)
	}
	artifacts = append([]workflow.ArtifactRef{reportRef}, artifacts...)
	return workflow.NodeResult{Artifacts: artifacts}, nil
}

func removeStalePackageOutputs(outputDir, taskName string) error {
	outputDir = filepath.Clean(outputDir)
	if outputDir == "" || outputDir == "." {
		return nil
	}
	normalized, err := packager.NormalizeTaskName(taskName)
	if err != nil {
		return nil
	}
	for _, name := range []string{normalized + ".zip", normalized + "-delivery.zip", "submission_report.json"} {
		if err := os.Remove(filepath.Join(outputDir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
