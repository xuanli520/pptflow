package nodes

import (
	"path/filepath"
	"testing"
)

func TestOrderHasUniqueNodeIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range Order() {
		if id == "" {
			t.Fatal("node order contains empty id")
		}
		if seen[id] {
			t.Fatalf("node order contains duplicate id %q", id)
		}
		seen[id] = true
	}
	for _, id := range []string{TaskReview, RuntimeSelfCheck, InstructionGen, DockerBuild, InitialVerify, OracleVerify, HarborRunQwen, HarborRunOpus, FinalReview, Package} {
		if !seen[id] {
			t.Fatalf("node order missing %s", id)
		}
	}
	for _, redundant := range []string{ContentReview, ResultReview} {
		if seen[redundant] {
			t.Fatalf("redundant review node remains in active order: %s", redundant)
		}
	}
}

func TestArtifactPathsMatchDevPlanLayout(t *testing.T) {
	workspace := "/tmp/workspace"
	checks := map[string]string{
		PrimaryArtifactPath(workspace, InstructionGen): filepath.Join(workspace, "phase1", "artifacts", "instruction_generate", "instruction.md"),
		PrimaryArtifactPath(workspace, TaskTOMLGen):    filepath.Join(workspace, "phase1", "artifacts", "task_toml_generate", "task.toml"),
		PrimaryArtifactPath(workspace, DockerfileGen):  filepath.Join(workspace, "phase1", "artifacts", "dockerfile_generate", "Dockerfile"),
		PrimaryArtifactPath(workspace, SolveGen):       filepath.Join(workspace, "phase2", "artifacts", "solve_generate", "solve.sh"),
		PrimaryArtifactPath(workspace, TestGen):        filepath.Join(workspace, "phase2", "artifacts", "test_generate", "test.sh"),
		PrimaryArtifactPath(workspace, DockerBuild):    filepath.Join(workspace, "phase2", "artifacts", "docker_build", "build_result.json"),
		PrimaryArtifactPath(workspace, InitialVerify):  filepath.Join(workspace, "phase2", "artifacts", "initial_verify", "initial_result.json"),
		PrimaryArtifactPath(workspace, OracleVerify):   filepath.Join(workspace, "phase2", "artifacts", "oracle_verify", "oracle_result.json"),
		PrimaryArtifactPath(workspace, HarborRunQwen):  filepath.Join(workspace, "phase3", "artifacts", "harbor_run_qwen", "qwen_result.json"),
		PrimaryArtifactPath(workspace, HarborRunOpus):  filepath.Join(workspace, "phase3", "artifacts", "harbor_run_opus", "opus_result.json"),
	}
	for got, want := range checks {
		if got != want {
			t.Fatalf("artifact path mismatch: got %s want %s", got, want)
		}
	}
}
