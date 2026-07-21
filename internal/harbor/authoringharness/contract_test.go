package authoringharness

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestReadCandidateUsesClosedModeSpecificFilesAndCanonicalDigests(t *testing.T) {
	taskRoot := writeCandidateFixture(t)
	build, err := ReadCandidate(taskRoot, ModeDockerfileBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(build.Dockerfile) == 0 || build.SolveScript != nil || build.TestScript != nil {
		t.Fatalf("build candidate = %+v", build)
	}
	full, err := ReadCandidate(taskRoot, ModeInitialOracle)
	if err != nil {
		t.Fatal(err)
	}
	if full.EnvironmentDigest != build.EnvironmentDigest || full.CandidateDigest == build.CandidateDigest {
		t.Fatalf("candidate digests build=%q full=%q environment=%q/%q", build.CandidateDigest, full.CandidateDigest, build.EnvironmentDigest, full.EnvironmentDigest)
	}
	if err := os.WriteFile(filepath.Join(taskRoot, "solution", "solve.sh"), []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	changed, err := ReadCandidate(taskRoot, ModeInitialOracle)
	if err != nil {
		t.Fatal(err)
	}
	if changed.EnvironmentDigest != full.EnvironmentDigest || changed.CandidateDigest == full.CandidateDigest {
		t.Fatalf("solve change digests before=%q after=%q environment=%q/%q", full.CandidateDigest, changed.CandidateDigest, full.EnvironmentDigest, changed.EnvironmentDigest)
	}
}

func TestReadCandidateRejectsSymlinkAndHardlink(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		taskRoot := writeCandidateFixture(t)
		target := filepath.Join(taskRoot, "outside")
		if err := os.WriteFile(target, []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dockerfile := filepath.Join(taskRoot, "environment", "Dockerfile")
		if err := os.Remove(dockerfile); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, dockerfile); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := ReadCandidate(taskRoot, ModeDockerfileBuild); err == nil {
			t.Fatal("symlinked Dockerfile was accepted")
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		taskRoot := writeCandidateFixture(t)
		dockerfile := filepath.Join(taskRoot, "environment", "Dockerfile")
		if err := os.Link(dockerfile, filepath.Join(taskRoot, "Dockerfile-link")); err != nil {
			t.Skipf("hardlinks unavailable: %v", err)
		}
		if _, err := ReadCandidate(taskRoot, ModeDockerfileBuild); err == nil {
			t.Fatal("hardlinked Dockerfile was accepted")
		}
	})
}

func TestFinalizeBindsCanonicalReportJSON(t *testing.T) {
	fingerprint := workflowkit.SHA256Fingerprint([]byte("candidate"))
	result, err := Finalize(Result{
		Mode: ModeDockerfileBuild, RunID: "run", StageKey: "dockerfile_build_validate", StageAttemptID: "attempt",
		Passed: true, Step: "docker_build", ExitCode: 0, CandidateDigest: fingerprint, EnvironmentDigest: fingerprint,
		Steps: []StepResult{{Step: "docker_build", Passed: true, ExitCode: 0, Findings: []string{}, OutputFingerprint: fingerprint}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateReportJSON(); err != nil {
		t.Fatal(err)
	}
	tampered := result
	tampered.ReportJSON = append([]byte(nil), result.ReportJSON...)
	tampered.ReportJSON = bytes.Replace(tampered.ReportJSON, []byte(`"passed":true`), []byte(`"passed":false`), 1)
	if err := tampered.ValidateReportJSON(); err == nil {
		t.Fatal("tampered canonical report was accepted")
	}
}

func TestValidateReportJSONRejectsSummaryThatDisagreesWithFinalStep(t *testing.T) {
	fingerprint := workflowkit.SHA256Fingerprint([]byte("candidate"))
	result, err := Finalize(Result{
		Mode: ModeDockerfileBuild, RunID: "run", StageKey: "dockerfile_build_validate", StageAttemptID: "attempt",
		Passed: true, Step: "docker_build", ExitCode: 0, CandidateDigest: fingerprint, EnvironmentDigest: fingerprint,
		Steps: []StepResult{{Step: "docker_build", Passed: true, ExitCode: 0, Findings: []string{}, OutputFingerprint: fingerprint}},
	})
	if err != nil {
		t.Fatal(err)
	}

	mismatched := result
	mismatched.Passed = false
	mismatched.Findings = []string{"forced summary failure"}
	mismatched.ReportJSON = nil
	report, err := canonicalReportJSON(mismatched)
	if err != nil {
		t.Fatal(err)
	}
	mismatched.ReportJSON = report
	if err := mismatched.ValidateReportJSON(); err == nil {
		t.Fatal("report with a final-step/summary mismatch was accepted")
	}
}

func writeCandidateFixture(t *testing.T) string {
	t.Helper()
	taskRoot := filepath.Join(t.TempDir(), "task")
	for _, directory := range []string{"environment", "solution", "tests"} {
		if err := os.MkdirAll(filepath.Join(taskRoot, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		DockerfileRelativePath:  "FROM scratch\n",
		SolveScriptRelativePath: "#!/bin/sh\nexit 0\n",
		TestScriptRelativePath:  "#!/bin/sh\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(taskRoot, filepath.FromSlash(path)), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return taskRoot
}
