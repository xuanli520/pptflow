package app

import (
	"path/filepath"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

func TestBuildWorkflowDefinitionUsesFiveExplicitGates(t *testing.T) {
	workspace := t.TempDir()
	publishDestination := t.TempDir()
	definition, err := buildWorkflowDefinition(RunnerOptions{
		Generate: true, RepoURL: "https://github.com/org/repo", Commit: "abc1234", Workspace: workspace,
		TaskOutputDir: publishDestination, VerifyDocker: true, QualityCheck: true, SimilarityCheck: true,
		SimilarityHistoryDirs: []string{t.TempDir()}, RunHarbor: true, HarborModels: "qwen,opus", StrictSubmission: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantGates := map[string]bool{
		nodes.TaskReview: false, nodes.ContentReview: false, nodes.SolutionReview: false,
		nodes.FinalReview: false, nodes.ResultReview: false,
	}
	seen := map[string]bool{}
	for _, spec := range definition.Nodes {
		if seen[spec.ID] {
			t.Fatalf("duplicate workflow node %s", spec.ID)
		}
		seen[spec.ID] = true
		if _, ok := wantGates[spec.ID]; ok {
			if spec.Kind != "harborfactory.human_gate" {
				t.Fatalf("gate %s uses kind %s", spec.ID, spec.Kind)
			}
			wantGates[spec.ID] = true
		}
	}
	for gateID, present := range wantGates {
		if !present {
			t.Fatalf("workflow missing gate %s", gateID)
		}
	}
	for _, nodeID := range []string{nodes.RepoPrepare, nodes.RepoAnalyze, nodes.TaskDesign, nodes.GenerateTaskFiles, nodes.DockerBuild, nodes.InitialVerify, nodes.OracleVerify, nodes.CodeEdgeLint, nodes.HarborRunQwen, nodes.HarborRunOpus, nodes.SubmissionLint, nodes.PublishTask} {
		if !seen[nodeID] {
			t.Fatalf("workflow missing production node %s", nodeID)
		}
	}
	internalTaskDir := filepath.Join(workspace, "phase2", "task", "generated-task")
	for _, spec := range definition.Nodes {
		if spec.ID == nodes.RepoPrepare && (spec.Policy.MaxAttempts != 1 || spec.Config["max_network_attempts"] != 3) {
			t.Fatalf("repo_prepare must use one Engine attempt with its configured network budget: policy=%+v config=%+v", spec.Policy, spec.Config)
		}
		if spec.ID == nodes.MaterializeTask && spec.Config["task_dir"] != internalTaskDir {
			t.Fatalf("materialize task_dir=%v, want ArtifactStore-local %s", spec.Config["task_dir"], internalTaskDir)
		}
		if spec.ID == nodes.PublishTask {
			if spec.Config["task_dir"] != internalTaskDir || spec.Config["destination_dir"] != publishDestination {
				t.Fatalf("unexpected publish boundary config: %+v", spec.Config)
			}
		}
	}
}

func TestBuildWorkflowDefinitionKeepsPassAtFourTrialCount(t *testing.T) {
	definition, err := buildWorkflowDefinition(RunnerOptions{TaskDir: t.TempDir(), RunHarbor: true, HarborModels: "qwen,opus"})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range definition.Nodes {
		if spec.ID != nodes.HarborRunQwen && spec.ID != nodes.HarborRunOpus {
			continue
		}
		if got := spec.Config["attempts"]; got != 4 {
			t.Fatalf("%s business trial count=%v, want 4", spec.ID, got)
		}
		if got := spec.Config["concurrency"]; got != 2 {
			t.Fatalf("%s default concurrency=%v, want bounded parallelism 2", spec.ID, got)
		}
	}
}

func TestBuildWorkflowDefinitionRejectsInvalidHarborPassSettings(t *testing.T) {
	for _, opts := range []RunnerOptions{
		{TaskDir: t.TempDir(), HarborConcurrency: 5, HarborAttempts: 4},
		{TaskDir: t.TempDir(), HarborConcurrency: 2, HarborAttempts: 3},
	} {
		if _, err := buildWorkflowDefinition(opts); err == nil {
			t.Fatalf("invalid Harbor settings unexpectedly built a workflow: %+v", opts)
		}
	}
}
