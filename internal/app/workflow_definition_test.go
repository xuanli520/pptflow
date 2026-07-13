package app

import (
	"path/filepath"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestBuildWorkflowDefinitionUsesFiveExplicitGates(t *testing.T) {
	workspace := t.TempDir()
	definition, err := buildWorkflowDefinition(RunnerOptions{
		Generate: true, RepoURL: "https://github.com/org/repo", Commit: "abc1234", Workspace: workspace,
		TaskOutputDir: t.TempDir(), VerifyDocker: true, QualityCheck: true, SimilarityCheck: true,
		SimilarityHistoryDirs: []string{t.TempDir()}, RunHarbor: true, HarborModels: "qwen,opus", StrictSubmission: true, Package: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantGates := map[string]bool{
		nodes.TaskReview: false, nodes.ContentReview: false, nodes.SolutionReview: false,
		nodes.FinalReview: false, nodes.ResultReview: false,
	}
	seen := map[string]bool{}
	declaredAttempts := map[string]int{
		nodes.RepoPrepare:       1,
		nodes.RepoAnalyze:       3,
		nodes.TaskDesign:        3,
		nodes.GenerateTaskFiles: 3,
		nodes.InstructionGen:    1,
		nodes.DockerBuild:       3,
		nodes.InitialVerify:     1,
		nodes.OracleVerify:      1,
		nodes.QualityCheck:      3,
		nodes.SimilarityCheck:   2,
		nodes.HarborRunQwen:     1,
		nodes.HarborRunOpus:     1,
	}
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
	for _, nodeID := range []string{nodes.RepoPrepare, nodes.RepoAnalyze, nodes.TaskDesign, nodes.GenerateTaskFiles, nodes.DockerBuild, nodes.InitialVerify, nodes.OracleVerify, nodes.CodeEdgeLint, nodes.HarborRunQwen, nodes.HarborRunOpus, nodes.SubmissionLint} {
		if !seen[nodeID] {
			t.Fatalf("workflow missing production node %s", nodeID)
		}
	}
	internalTaskDir := filepath.Join(workspace, "phase2", "task", "generated-task")
	for _, spec := range definition.Nodes {
		if spec.Kind == humanGatePluginKind {
			if spec.Policy.MaxAttempts != 1 || len(spec.Policy.Retryable) != 0 {
				t.Fatalf("human gate %s must not retry automatically: %+v", spec.ID, spec.Policy)
			}
		} else if want, tracked := declaredAttempts[spec.ID]; tracked {
			if spec.Policy.MaxAttempts != want {
				t.Fatalf("node %s max attempts=%d, want declared %d", spec.ID, spec.Policy.MaxAttempts, want)
			}
			if want > 1 {
				for _, kind := range defaultNodeRetryableFailures {
					if !containsFailureKind(spec.Policy.Retryable, kind) {
						t.Fatalf("node %s retry policy missing %s: %+v", spec.ID, kind, spec.Policy.Retryable)
					}
				}
				if containsFailureKind(spec.Policy.Retryable, workflow.FailureUnknown) {
					t.Fatalf("node %s must not blindly retry unknown failures: %+v", spec.ID, spec.Policy.Retryable)
				}
			}
		}
		if spec.ID == nodes.RepoPrepare && spec.Config["max_network_attempts"] != 3 {
			t.Fatalf("repo_prepare lost its configured network budget: %+v", spec.Config)
		}
		if spec.ID == nodes.MaterializeTask && spec.Config["task_dir"] != internalTaskDir {
			t.Fatalf("materialize task_dir=%v, want ArtifactStore-local %s", spec.Config["task_dir"], internalTaskDir)
		}
		if spec.ID == nodes.PublishTask || spec.ID == nodes.Package || spec.Kind == "harborfactory.publish_task" || spec.Kind == "harborfactory.package" {
			t.Fatalf("legacy external delivery route remains in workflow definition: %+v", spec)
		}
	}
}

func TestBuildWorkflowDefinitionExpandsMultiTurnAgentBudget(t *testing.T) {
	definition, err := buildWorkflowDefinition(RunnerOptions{
		Generate: true, RepoURL: "https://github.com/org/repo", Commit: "abc1234", Workspace: t.TempDir(),
		TaskOutputDir: t.TempDir(), AgentTimeout: 1800,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{nodes.RepoAnalyze, nodes.TaskDesign, nodes.GenerateTaskFiles} {
		var policy workflow.NodePolicy
		for _, spec := range definition.Nodes {
			if spec.ID == nodeID {
				policy = spec.Policy
				break
			}
		}
		if policy.MaxTurns != 3 || policy.TurnTimeoutSeconds != 1800 {
			t.Fatalf("%s turn budget=%+v", nodeID, policy)
		}
		if policy.TimeoutSeconds < 3*1800+defaultStartupGraceSeconds+defaultShutdownGraceSeconds {
			t.Fatalf("%s parent attempt remains too short: %+v", nodeID, policy)
		}
		if policy.MaxElapsedSeconds < policy.MaxAttempts*policy.TimeoutSeconds {
			t.Fatalf("%s max elapsed omits attempts: %+v", nodeID, policy)
		}
	}
}

func containsFailureKind(values []workflow.FailureKind, target workflow.FailureKind) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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
