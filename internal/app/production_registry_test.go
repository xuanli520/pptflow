package app

import (
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestProductionRegistryCoversFullWorkflow(t *testing.T) {
	registry, err := buildProductionRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := buildWorkflowDefinition(RunnerOptions{
		Generate: true, RepoURL: "https://github.com/org/repo", Commit: "abc1234", Workspace: t.TempDir(),
		TaskOutputDir: t.TempDir(), VerifyDocker: true, QualityCheck: true, SimilarityCheck: true,
		SimilarityHistoryDirs: []string{t.TempDir()}, RunHarbor: true, HarborModels: "qwen,opus", Package: true,
		OutputDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range definition.Nodes {
		plugin, lookupErr := registry.Lookup(spec)
		if lookupErr != nil {
			t.Fatalf("production node %s has no plugin: %v", spec.ID, lookupErr)
		}
		if validateErr := plugin.Validate(spec); validateErr != nil {
			t.Fatalf("production node %s has invalid static config: %v", spec.ID, validateErr)
		}
	}
}

func TestProductionRegistryCoversEvidenceImportBranches(t *testing.T) {
	registry, err := buildProductionRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []workflow.NodeSpec{
		{ID: nodes.HarborVerify, Kind: "harborfactory.verify_report_import", Config: map[string]any{"task_dir": "/task", "report_path": "/workspace/verify_report.json"}},
		{ID: nodes.SimilarityCheck, Kind: "harborfactory.similarity_report_import", Config: map[string]any{"task_dir": "/task", "report_path": "/workspace/similarity_report.json"}},
	} {
		plugin, lookupErr := registry.Lookup(spec)
		if lookupErr != nil {
			t.Fatalf("production import node %s has no plugin: %v", spec.ID, lookupErr)
		}
		if validateErr := plugin.Validate(spec); validateErr != nil {
			t.Fatalf("production import node %s has invalid config: %v", spec.ID, validateErr)
		}
	}
}
