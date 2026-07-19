package workflowadapter

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringBriefCanonicalizesAndStrictlyParses(t *testing.T) {
	brief, err := NewStandardAuthoringBrief(" feature ", " tower-http ", " Add a backend feature. ")
	if err != nil {
		t.Fatal(err)
	}
	if brief.TaskType != "feature" || brief.Application != "tower-http" || brief.Objective != "Add a backend feature." {
		t.Fatalf("canonical brief = %+v", brief)
	}
	canonical, err := brief.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"format":"harbor.standard-authoring-brief.v1","version":"1","task_type":"feature","application":"tower-http","objective":"Add a backend feature."}`
	if string(canonical) != want {
		t.Fatalf("canonical brief = %s, want %s", canonical, want)
	}
	parsed, err := ParseStandardAuthoringBriefJSON([]byte(`{"format":"harbor.standard-authoring-brief.v1","version":"1","task_type":" feature ","application":" tower-http ","objective":" Add a backend feature. "}`))
	if err != nil || parsed != brief {
		t.Fatalf("parsed brief = %+v, %v", parsed, err)
	}
	var direct StandardAuthoringBrief
	if err := json.Unmarshal(canonical, &direct); err != nil || direct != brief {
		t.Fatalf("direct strict decode = %+v, %v", direct, err)
	}
	digest, err := brief.ContentDigest()
	if err != nil || digest != workflowkit.SHA256Fingerprint(canonical) {
		t.Fatalf("brief digest = %q, %v", digest, err)
	}

	for name, raw := range map[string][]byte{
		"unknown field":   bytes.Replace(canonical, []byte(`"objective":`), []byte(`"unknown":true,"objective":`), 1),
		"duplicate field": bytes.Replace(canonical, []byte(`"task_type":`), []byte(`"task_type":"feature","task_type":`), 1),
		"trailing value":  append(append([]byte(nil), canonical...), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseStandardAuthoringBriefJSON(raw); err == nil {
				t.Fatalf("invalid brief parsed: %s", raw)
			}
		})
	}
}

func TestStandardAuthoringBriefRejectsInvalidTokensAndObjectives(t *testing.T) {
	validObjective512 := strings.Repeat("a", StandardAuthoringBriefObjectiveMaxBytes)
	if _, err := NewStandardAuthoringBrief("f", "a", validObjective512); err != nil {
		t.Fatalf("512-byte objective rejected: %v", err)
	}
	for _, test := range []struct {
		name, taskType, application, objective string
	}{
		{name: "missing task type", application: "tower-http", objective: "objective"},
		{name: "uppercase task type", taskType: "Feature", application: "tower-http", objective: "objective"},
		{name: "underscore application", taskType: "feature", application: "tower_http", objective: "objective"},
		{name: "long token", taskType: "feature", application: "a" + strings.Repeat("b", 64), objective: "objective"},
		{name: "empty objective", taskType: "feature", application: "tower-http", objective: "   "},
		{name: "multiline objective", taskType: "feature", application: "tower-http", objective: "first\nsecond"},
		{name: "tab objective", taskType: "feature", application: "tower-http", objective: "first\tsecond"},
		{name: "long objective", taskType: "feature", application: "tower-http", objective: strings.Repeat("a", StandardAuthoringBriefObjectiveMaxBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewStandardAuthoringBrief(test.taskType, test.application, test.objective); err == nil {
				t.Fatal("invalid brief accepted")
			}
		})
	}
}

func TestStandardAuthoringBriefTemplateAddsOnlyVersionedIntrinsicConsumers(t *testing.T) {
	template := StandardAuthoringBriefWorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatalf("validate 1.4.0 template: %v", err)
	}
	if !template.Reference().Equal(StandardAuthoringBriefTemplateReference()) || template.Catalog.Version != StandardAuthoringBriefTemplateVersion || template.QuotaPolicy.Version != StandardAuthoringBriefQuotaPolicyVersion {
		t.Fatalf("brief template identities = template %s catalog %s quota %s", template.Version, template.Catalog.Version, template.QuotaPolicy.Version)
	}
	consumers := map[workflowkit.StageKey]bool{
		workflowkit.StageKey(RepoAnalyze): true, workflowkit.StageKey(TaskDesign): true,
		workflowkit.StageKey(GenerateTaskFiles): true, workflowkit.StageKey(TaskTOMLGen): true,
		workflowkit.StageKey(ContentReview): true, workflowkit.StageKey(CodeEdgePackageAdmission): true,
		workflowkit.StageKey(MaterializeTask): true,
	}
	for _, stage := range template.Catalog.Stages {
		_, hasBrief := artifactSpecNamed(stage.Inputs, StandardAuthoringBriefArtifact)
		if hasBrief != consumers[stage.Key] {
			t.Fatalf("stage %q brief input = %t, want %t", stage.Key, hasBrief, consumers[stage.Key])
		}
		if resourcePresent(stage.ReadSet, resourceAuthoringBrief) != consumers[stage.Key] {
			t.Fatalf("stage %q brief read resource differs from input contract", stage.Key)
		}
	}
	for _, legacy := range []WorkflowTemplate{StandardAuthoringWorkflowTemplate(), StandardAuthoringTaskAdmissionWorkflowTemplate()} {
		if err := legacy.Validate(); err != nil {
			t.Fatalf("validate historical template %s: %v", legacy.Version, err)
		}
		for _, stage := range legacy.Catalog.Stages {
			if _, present := artifactSpecNamed(stage.Inputs, StandardAuthoringBriefArtifact); present || resourcePresent(stage.ReadSet, resourceAuthoringBrief) {
				t.Fatalf("historical template %s stage %q acquired brief contract", legacy.Version, stage.Key)
			}
		}
	}
}
