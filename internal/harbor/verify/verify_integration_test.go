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
		t.Skip("set HARBOR_VERIFY_INTEGRATION_TASK to run the real Docker verification flow")
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
