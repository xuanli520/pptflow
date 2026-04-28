package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func TestSelectedStagesStaticOnly(t *testing.T) {
	selected := selectedStages(RunOptions{StaticOnly: true}, true)
	for _, stage := range []string{"A", "D", "E", "F"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
	for _, stage := range []string{"B", "C"} {
		if selected[stage] {
			t.Fatalf("expected %s skipped", stage)
		}
	}
}

func TestSelectedStagesFrom(t *testing.T) {
	selected := selectedStages(RunOptions{From: "C"}, false)
	for _, stage := range []string{"A", "B"} {
		if selected[stage] {
			t.Fatalf("expected %s not selected", stage)
		}
	}
	for _, stage := range []string{"C", "D", "E", "F"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
}

func TestSelectedStagesSingleStageStillRunsSummary(t *testing.T) {
	selected := selectedStages(RunOptions{Stage: "D"}, false)
	if !selected["D"] || !selected["F"] {
		t.Fatalf("expected D and F selected, got %#v", selected)
	}
	for _, stage := range []string{"A", "B", "C", "E"} {
		if selected[stage] {
			t.Fatalf("expected %s not selected", stage)
		}
	}
}

func TestSelectedStagesExplicitDependencyChain(t *testing.T) {
	selected := selectedStages(RunOptions{Stages: []string{"A", "B", "C"}}, false)
	for _, stage := range []string{"A", "B", "C", "F"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
	if selected["D"] || selected["E"] {
		t.Fatalf("D/E should not be selected for A dependency rerun")
	}
}

func TestAssignFindingIDs(t *testing.T) {
	findings := assignFindingIDs("E", []model.Finding{
		{Severity: "Blocker", Title: "one"},
		{Severity: "High", Title: "two"},
		{Severity: "High", Title: "three"},
	})
	want := []string{"P2R-E-BLK-001", "P2R-E-HIGH-001", "P2R-E-HIGH-002"}
	for i, id := range want {
		if findings[i].ID != id {
			t.Fatalf("finding %d id = %s, want %s", i, findings[i].ID, id)
		}
	}
}

func TestShortCommentKeepsManualVerdictUnchecked(t *testing.T) {
	text := shortComment(map[string]string{"B": "skipped", "C": "skipped"}, nil)
	if want := "<[ ] PASS  [ ] REWORK  [ ] FAIL>"; !strings.Contains(text, want) {
		t.Fatalf("short comment missing manual verdict line: %s", text)
	}
}

func TestParseComposePSExtractsMappings(t *testing.T) {
	raw := `{"Service":"web","Publishers":[{"URL":"0.0.0.0","TargetPort":3000,"PublishedPort":34152,"Protocol":"tcp"}]}`
	mappings, services := parseComposePS(raw)
	if len(services) != 1 || services[0] != "web" {
		t.Fatalf("unexpected services: %#v", services)
	}
	if got := mappings["web"][0].Host; got != 34152 {
		t.Fatalf("host port = %d, want 34152", got)
	}
}

func TestReadmeDockerComposeCommandAcceptsStandaloneSpelling(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "```sh\ndocker-compose up --build\n```\n")
	fields := readmeComposeCommand(dir)
	if strings.Join(fields[:2], " ") != "docker compose" {
		t.Fatalf("expected docker-compose normalized to docker compose, got %#v", fields)
	}
}

func TestParseDockerPortFallback(t *testing.T) {
	mappings := parseDockerPort("web", "80/tcp -> 0.0.0.0:34152\n443/tcp -> [::]:34153\n")
	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings, got %#v", mappings)
	}
	if mappings[0].Container != 80 || mappings[0].Host != 34152 {
		t.Fatalf("unexpected first mapping: %#v", mappings[0])
	}
}

func TestExtractFindingsFromReport(t *testing.T) {
	report := "# Verdict\n\n- Blocker: missing auth guard\n- High: run_tests does not hit API\nNo blocker in unrelated sentence\n"
	findings := extractFindingsFromReport("E", report, "report.md")
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %#v", len(findings), findings)
	}
	if findings[0].Severity != "Blocker" || findings[1].Severity != "High" {
		t.Fatalf("unexpected severities: %#v", findings)
	}
}
