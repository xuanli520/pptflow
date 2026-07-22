package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunDoctorReportsMissingRequiredTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	report := runDoctor()
	if report.Passed {
		t.Fatalf("doctor should fail when required tools are missing: %+v", report)
	}
	if len(report.Tools) == 0 || len(report.Issues) == 0 {
		t.Fatalf("doctor report missing tool diagnostics: %+v", report)
	}
}

func TestRunDoctorFindsToolsInPath(t *testing.T) {
	binDir := t.TempDir()
	writeDoctorFake(t, binDir, "git", "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'git version 2.47.3'; exit 0; fi\nexit 1\n")
	writeDoctorFake(t, binDir, "docker", "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'Docker version 29.5.2'; exit 0; fi\nif [ \"$1\" = \"info\" ]; then echo '\"29.5.2\"'; exit 0; fi\nexit 1\n")
	writeDoctorFake(t, binDir, "harbor", doctorFakeHarborHelp("run --path TASK --agent AGENT --model MODEL --n-attempts 4 --n-concurrent 1 --max-retries 0 --env-file FILE --mounts JSON"))
	writeDoctorFake(t, binDir, "go", "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo 'go version go1.26.3 linux/amd64'; exit 0; fi\nexit 1\n")
	t.Setenv("PATH", binDir)
	report := runDoctor()
	if !report.Passed {
		t.Fatalf("doctor should pass with all required tools in PATH: %+v", report)
	}
	for _, tool := range report.Tools {
		if !tool.Found || tool.Path == "" {
			t.Fatalf("tool was not reported as found: %+v", tool)
		}
		if !tool.Healthy || len(tool.Probes) == 0 {
			t.Fatalf("tool was not reported healthy: %+v", tool)
		}
	}
}

func TestRunDoctorReportsDockerDaemonFailure(t *testing.T) {
	binDir := t.TempDir()
	writeDoctorFake(t, binDir, "git", "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'git version 2.47.3'; exit 0; fi\nexit 1\n")
	writeDoctorFake(t, binDir, "harbor", doctorFakeHarborHelp("run --path TASK --agent AGENT --model MODEL --n-attempts 4 --n-concurrent 1 --max-retries 0 --env-file FILE --mounts JSON"))
	writeDoctorFake(t, binDir, "go", "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo 'go version go1.26.3 linux/amd64'; exit 0; fi\nexit 1\n")
	writeDoctorFake(t, binDir, "docker", "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'Docker version 29.5.2'; exit 0; fi\nif [ \"$1\" = \"info\" ]; then echo daemon unavailable >&2; exit 1; fi\nexit 1\n")
	t.Setenv("PATH", binDir)
	report := runDoctor()
	if report.Passed {
		t.Fatalf("doctor should fail when docker daemon is unavailable: %+v", report)
	}
	if !hasDoctorIssue(report, "docker daemon unavailable") {
		t.Fatalf("doctor report missing docker daemon issue: %+v", report)
	}
}

func TestRunDoctorReportsGoVersionTooOld(t *testing.T) {
	binDir := t.TempDir()
	writeDoctorFake(t, binDir, "git", "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'git version 2.47.3'; exit 0; fi\nexit 1\n")
	writeDoctorFake(t, binDir, "docker", "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'Docker version 29.5.2'; exit 0; fi\nif [ \"$1\" = \"info\" ]; then echo '\"29.5.2\"'; exit 0; fi\nexit 1\n")
	writeDoctorFake(t, binDir, "harbor", doctorFakeHarborHelp("run --path TASK --agent AGENT --model MODEL --n-attempts 4 --n-concurrent 1 --max-retries 0 --env-file FILE --mounts JSON"))
	writeDoctorFake(t, binDir, "go", "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo 'go version go1.25.9 linux/amd64'; exit 0; fi\nexit 1\n")
	t.Setenv("PATH", binDir)
	report := runDoctor()
	if report.Passed {
		t.Fatalf("doctor should fail when Go is below go.mod requirement: %+v", report)
	}
	if !hasDoctorIssue(report, "go CLI health check failed") {
		t.Fatalf("doctor report missing go version issue: %+v", report)
	}
}

func TestRunDoctorRejectsHarborWithoutAgentFlag(t *testing.T) {
	binDir := t.TempDir()
	writeDoctorFake(t, binDir, "git", "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'git version 2.47.3'; exit 0; fi\nexit 1\n")
	writeDoctorFake(t, binDir, "docker", "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'Docker version 29.5.2'; exit 0; fi\nif [ \"$1\" = \"info\" ]; then echo '\"29.5.2\"'; exit 0; fi\nexit 1\n")
	writeDoctorFake(t, binDir, "harbor", doctorFakeHarborHelp("run --path TASK --model MODEL --n-attempts 4 --n-concurrent 1 --max-retries 0 --env-file FILE"))
	writeDoctorFake(t, binDir, "go", "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo 'go version go1.26.3 linux/amd64'; exit 0; fi\nexit 1\n")
	t.Setenv("PATH", binDir)
	report := runDoctor()
	if report.Passed {
		t.Fatalf("doctor should fail when harbor run lacks -a capability: %+v", report)
	}
	if !hasDoctorIssue(report, "harbor CLI health check failed") {
		t.Fatalf("doctor report missing harbor health issue: %+v", report)
	}
}

func writeDoctorFake(t *testing.T, binDir, name, script string) {
	t.Helper()
	path := filepath.Join(binDir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func doctorFakeHarborHelp(help string) string {
	return "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'harbor 1.0.0'; exit 0; fi\nif [ \"$1\" = \"run\" ] && [ \"$2\" = \"--help\" ]; then echo '" + help + "'; exit 0; fi\nexit 1\n"
}

func hasDoctorIssue(report doctorReport, want string) bool {
	for _, issue := range report.Issues {
		if issue == want {
			return true
		}
	}
	return false
}
