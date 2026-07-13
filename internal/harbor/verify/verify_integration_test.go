package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDockerIntegration(t *testing.T) {
	taskDir := os.Getenv("HARBOR_VERIFY_INTEGRATION_TASK")
	if taskDir == "" {
		if os.Getenv("HARBOR_RUN_DOCKER_INTEGRATION") != "1" {
			t.Skip("set HARBOR_RUN_DOCKER_INTEGRATION=1 for the self-contained fixture or HARBOR_VERIFY_INTEGRATION_TASK for a real task")
		}
		taskDir = writeDockerIntegrationTask(t)
	}
	reportPath := os.Getenv("HARBOR_VERIFY_INTEGRATION_REPORT")
	if reportPath == "" {
		reportPath = filepath.Join(t.TempDir(), "verify_report.json")
	}
	report, err := Run(context.Background(), Options{
		TaskDir:        taskDir,
		ImageTag:       "harbor-verify-integration-" + time.Now().UTC().Format("20060102150405"),
		TimeoutSeconds: 600,
		WriteReport:    reportPath,
	})
	if err != nil {
		t.Fatalf("real Docker verification failed: %v", err)
	}
	if !report.Passed || !report.InitialExposesIssue {
		t.Fatalf("verification contract not satisfied: %+v", report)
	}
	if report.DockerBuild == nil || !report.DockerBuild.Passed {
		t.Fatalf("Docker build evidence missing: %+v", report.DockerBuild)
	}
	if report.InitialVerify == nil || !report.InitialVerify.Passed {
		t.Fatalf("initial issue evidence missing: %+v", report.InitialVerify)
	}
	if report.OracleVerify == nil || !report.OracleVerify.Passed {
		t.Fatalf("oracle evidence missing: %+v", report.OracleVerify)
	}
	if report.Cleanup == nil || !report.Cleanup.Passed {
		t.Fatalf("cleanup evidence missing: %+v", report.Cleanup)
	}
	if len(report.CommandLogs) < 4 {
		t.Fatalf("command evidence is incomplete: %+v", report.CommandLogs)
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("verification report was not written: %v", err)
	}
}

// writeDockerIntegrationTask creates a strict task snapshot whose initial
// verifier fails without the solution mount and whose oracle verifier passes
// with it. The environment uses a locally available Ubuntu base image so this
// test exercises the Docker boundary without depending on a remote repository
// or provider.
func writeDockerIntegrationTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]struct {
		contents string
		mode     os.FileMode
	}{
		"instruction.md": {
			contents: "Verify that the supplied solution is mounted for the oracle run.\n",
			mode:     0o644,
		},
		"task.toml": {
			contents: "[task]\nname = \"docker-integration\"\n",
			mode:     0o644,
		},
		"tests_analysis.md": {
			contents: "The initial verifier intentionally lacks the solution mount; the oracle receives it.\n",
			mode:     0o644,
		},
		"environment/Dockerfile": {
			contents: "FROM ubuntu:24.04\n",
			mode:     0o644,
		},
		"solution/solve.sh": {
			contents: "#!/bin/sh\nexit 0\n",
			mode:     0o755,
		},
		"tests/test.sh": {
			contents: "#!/bin/sh\nif [ -x /solution/solve.sh ]; then\n  exit 0\nfi\nprintf '%s\\n' 'solution mount missing' >&2\nexit 1\n",
			mode:     0o755,
		},
	}
	for relative, file := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.contents), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
