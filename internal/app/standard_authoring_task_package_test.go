package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

func TestCompileStandardAuthoringTaskPackageCanonicalizesFrozenSourceAndAnalysis(t *testing.T) {
	input := standardAuthoringTaskPackageFixture(t)
	result, err := CompileStandardAuthoringTaskPackage(input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Report.Passed || len(result.Report.Violations) != 0 {
		t.Fatalf("admission report = %+v", result.Report)
	}
	files := taskPackageFilesByPath(result.CanonicalFiles)
	if !strings.Contains(string(files["task.toml"].Data), "commit_id = '"+input.Source.CommitSHA+"'") ||
		!strings.Contains(string(files["task.toml"].Data), "github_url = '"+input.Source.RepositoryURL+"'") {
		t.Fatalf("canonical task.toml did not use frozen provenance:\n%s", files["task.toml"].Data)
	}
	document, err := parseStandardAuthoringTaskTOMLPayload(files["task.toml"].Data)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := document["source"].(string); !ok || got != input.Source.RepositoryURL+"@"+input.Source.CommitSHA {
		t.Fatalf("canonical Harbor source = %#v, want string coordinate", document["source"])
	}
	analysis := string(files["tests_analysis.md"].Data)
	for _, heading := range []string{"## 1. instruction 和 environment 已提供的信息", "## 2. 模型的理论通过路径", "## 3. 模型具备通过条件的依据"} {
		if !strings.Contains(analysis, heading) {
			t.Fatalf("rendered analysis omitted %q:\n%s", heading, analysis)
		}
	}
}

func TestCompileStandardAuthoringTaskPackageMigratesMatchingLegacySourceTable(t *testing.T) {
	input := standardAuthoringTaskPackageFixture(t)
	input.TaskTOMLDraft = append(input.TaskTOMLDraft, []byte("\n[source]\nrepository_url = \""+input.Source.RepositoryURL+"\"\ncommit_sha = \""+input.Source.CommitSHA+"\"\n")...)

	result, err := CompileStandardAuthoringTaskPackage(input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Report.Passed {
		t.Fatalf("matching legacy source table should be migrated: %+v", result.Report)
	}
	files := taskPackageFilesByPath(result.CanonicalFiles)
	document, err := parseStandardAuthoringTaskTOMLPayload(files["task.toml"].Data)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := document["source"].(string); !ok || got != input.Source.RepositoryURL+"@"+input.Source.CommitSHA {
		t.Fatalf("migrated Harbor source = %#v, want string coordinate", document["source"])
	}
}

func TestCompileStandardAuthoringTaskPackageReportsIncidentClasses(t *testing.T) {
	input := standardAuthoringTaskPackageFixture(t)
	input.TaskTOMLDraft = []byte(strings.Replace(string(input.TaskTOMLDraft), input.Source.CommitSHA, input.Source.CommitSHA+"b", 1))
	input.Dockerfile = []byte(strings.Replace(string(input.Dockerfile), "COPY package.json /tmp/package.json", "COPY . .", 1))
	result, err := CompileStandardAuthoringTaskPackage(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Report.Passed {
		t.Fatalf("incident fixture unexpectedly passed: %+v", result.Report)
	}
	assertAdmissionCode(t, result.Report, "environment_isolation")
	assertAdmissionCode(t, result.Report, "task_metadata")
}

func TestCompileStandardAuthoringTaskPackageReportsFrozenBriefMetadataMismatch(t *testing.T) {
	for _, test := range []struct {
		name        string
		old         string
		replacement string
		field       string
	}{
		{name: "task type", old: `task_type = "bugfix"`, replacement: `task_type = "feature"`, field: "metadata.task_type"},
		{name: "application", old: `application = "widget"`, replacement: `application = "backend"`, field: "metadata.application"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := standardAuthoringTaskPackageFixture(t)
			input.TaskTOMLDraft = []byte(strings.Replace(string(input.TaskTOMLDraft), test.old, test.replacement, 1))
			result, err := CompileStandardAuthoringTaskPackage(input)
			if err != nil {
				t.Fatal(err)
			}
			if result.Report.Passed {
				t.Fatalf("mismatched frozen brief metadata unexpectedly passed: %+v", result.Report)
			}
			assertAdmissionViolationMessage(t, result.Report, "task_metadata", test.field)
		})
	}
}

func TestCompileStandardAuthoringTaskPackageAcceptsObservedArtifactWrappers(t *testing.T) {
	input := standardAuthoringTaskPackageFixture(t)
	wantInstruction := append([]byte(nil), input.Instruction...)
	input.Instruction = standardAuthoringInstructionWrapperFixture(t, input.Instruction)
	input.TaskTOMLDraft = standardAuthoringTaskTOMLWrapperFixture(t, input.TaskTOMLDraft, *input.Brief)

	result, err := CompileStandardAuthoringTaskPackage(input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Report.Passed {
		t.Fatalf("wrapped package admission report = %+v", result.Report)
	}
	files := taskPackageFilesByPath(result.CanonicalFiles)
	if string(files["instruction.md"].Data) != string(wantInstruction) {
		t.Fatalf("materialized instruction = %q, want decoded content %q", files["instruction.md"].Data, wantInstruction)
	}
	if !strings.Contains(string(files["task.toml"].Data), `task_type = 'bugfix'`) || strings.Contains(string(files["task.toml"].Data), `"task_toml"`) {
		t.Fatalf("materialized task.toml was not decoded and canonicalized:\n%s", files["task.toml"].Data)
	}
}

func TestCompileStandardAuthoringTaskPackageRejectsWrapperScopeMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*standardAuthoringTaskTOMLArtifactMetadata)
		field  string
	}{
		{name: "task type", mutate: func(metadata *standardAuthoringTaskTOMLArtifactMetadata) { metadata.TaskType = "feature" }, field: "metadata.task_type"},
		{name: "application", mutate: func(metadata *standardAuthoringTaskTOMLArtifactMetadata) { metadata.Application = "backend" }, field: "metadata.application"},
		{name: "objective", mutate: func(metadata *standardAuthoringTaskTOMLArtifactMetadata) { metadata.Objective = "Different objective" }, field: "metadata.objective"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := standardAuthoringTaskPackageFixture(t)
			metadata := standardAuthoringTaskTOMLArtifactMetadata{TaskType: input.Brief.TaskType, Application: input.Brief.Application, Objective: input.Brief.Objective}
			test.mutate(&metadata)
			encoded, err := json.Marshal(standardAuthoringTaskTOMLArtifact{
				Format: standardAuthoringTaskTOMLArtifactFormat, Version: standardAuthoringModelArtifactVersion,
				Metadata: metadata, TaskTOML: string(input.TaskTOMLDraft),
			})
			if err != nil {
				t.Fatal(err)
			}
			input.TaskTOMLDraft = encoded
			result, err := CompileStandardAuthoringTaskPackage(input)
			if err != nil {
				t.Fatal(err)
			}
			if result.Report.Passed {
				t.Fatalf("mismatched wrapper scope unexpectedly passed: %+v", result.Report)
			}
			assertAdmissionViolationMessage(t, result.Report, "task_metadata", test.field)
		})
	}
}

func TestCompileStandardAuthoringTaskPackageReportsMalformedModelContent(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*StandardAuthoringTaskPackageInput)
		code        string
		messagePart string
	}{
		{
			name: "empty instruction",
			mutate: func(input *StandardAuthoringTaskPackageInput) {
				input.Instruction = nil
			},
			code: "task_instruction", messagePart: "generated instruction",
		},
		{
			name: "invalid task TOML",
			mutate: func(input *StandardAuthoringTaskPackageInput) {
				input.TaskTOMLDraft = []byte("{not valid TOML or a wrapper")
			},
			code: "task_metadata", messagePart: "valid TOML",
		},
		{
			name: "invalid Dockerfile",
			mutate: func(input *StandardAuthoringTaskPackageInput) {
				input.Dockerfile = []byte("FROM docker.io/library/alpine:3.20\n")
			},
			code: "environment_isolation", messagePart: "frozen environment policy",
		},
		{
			name: "invalid solution script",
			mutate: func(input *StandardAuthoringTaskPackageInput) {
				input.SolveScript = nil
			},
			code: "task_layout", messagePart: "generated solution",
		},
		{
			name: "invalid test script",
			mutate: func(input *StandardAuthoringTaskPackageInput) {
				input.TestScript = []byte("exit 0\n")
			},
			code: "task_layout", messagePart: "generated tests",
		},
		{
			name: "invalid tests analysis",
			mutate: func(input *StandardAuthoringTaskPackageInput) {
				input.TestsAnalysis = []byte(`{"provided_information":"\u0000","theoretical_path":"path","passing_evidence":"evidence"}`)
			},
			code: "tests_analysis", messagePart: "strict JSON object",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := standardAuthoringTaskPackageFixture(t)
			test.mutate(&input)
			result, err := CompileStandardAuthoringTaskPackage(input)
			if err != nil {
				t.Fatalf("model-owned content returned infrastructure error: %v", err)
			}
			if result.Report.Passed || len(result.CanonicalFiles) != 6 {
				t.Fatalf("malformed model content result = %+v", result)
			}
			assertAdmissionViolationMessage(t, result.Report, test.code, test.messagePart)
		})
	}
}

func TestStandardAuthoringObservedArtifactWrappersAreStrict(t *testing.T) {
	validTaskTOML := standardAuthoringTaskPackageFixture(t).TaskTOMLDraft
	tests := []struct {
		name string
		raw  []byte
		read func([]byte) ([]byte, error)
	}{
		{name: "instruction unknown field", raw: []byte(`{"format":"harbor.standard-authoring-instruction.v1","version":"1","content":"task","extra":true}`), read: decodeStandardAuthoringInstructionArtifact},
		{name: "instruction duplicate field", raw: []byte(`{"format":"harbor.standard-authoring-instruction.v1","version":"1","content":"first","content":"second"}`), read: decodeStandardAuthoringInstructionArtifact},
		{name: "instruction trailing value", raw: []byte(`{"format":"harbor.standard-authoring-instruction.v1","version":"1","content":"task"}{}`), read: decodeStandardAuthoringInstructionArtifact},
		{name: "instruction empty payload", raw: []byte(`{"format":"harbor.standard-authoring-instruction.v1","version":"1","content":"  "}`), read: decodeStandardAuthoringInstructionArtifact},
		{name: "instruction invalid UTF-8", raw: append([]byte(`{"format":"harbor.standard-authoring-instruction.v1","version":"1","content":"`), 0xff, '"', '}'), read: decodeStandardAuthoringInstructionArtifact},
		{name: "instruction nested wrapper", raw: []byte(`{"format":"harbor.standard-authoring-instruction.v1","version":"1","content":"{\"format\":\"harbor.standard-authoring-instruction.v1\",\"version\":\"1\",\"content\":\"task\"}"}`), read: decodeStandardAuthoringInstructionArtifact},
		{name: "task wrong version", raw: []byte(`{"format":"harbor.artifact.v1","version":"2","metadata":{"task_type":"bugfix","application":"widget","objective":"Repair"},"task_toml":"[metadata]\\n"}`), read: decodeStandardAuthoringTaskTOMLArtifact},
		{name: "task unknown metadata field", raw: []byte(`{"format":"harbor.artifact.v1","version":"1","metadata":{"task_type":"bugfix","application":"widget","objective":"Repair","extra":true},"task_toml":"[metadata]\\n"}`), read: decodeStandardAuthoringTaskTOMLArtifact},
		{name: "task duplicate payload", raw: []byte(`{"format":"harbor.artifact.v1","version":"1","metadata":{"task_type":"bugfix","application":"widget","objective":"Repair"},"task_toml":"[metadata]\\n","task_toml":"[other]\\n"}`), read: decodeStandardAuthoringTaskTOMLArtifact},
		{name: "task empty payload", raw: []byte(`{"format":"harbor.artifact.v1","version":"1","metadata":{"task_type":"bugfix","application":"widget","objective":"Repair"},"task_toml":""}`), read: decodeStandardAuthoringTaskTOMLArtifact},
		{name: "task nested wrapper", raw: []byte(`{"format":"harbor.artifact.v1","version":"1","metadata":{"task_type":"bugfix","application":"widget","objective":"Repair"},"task_toml":"{\"format\":\"harbor.artifact.v1\",\"version\":\"1\",\"metadata\":{\"task_type\":\"feature\",\"application\":\"backend\",\"objective\":\"Other\"},\"task_toml\":\"[metadata]\\ntask_type = \\\"bugfix\\\"\\napplication = \\\"widget\\\"\\n\"}"}`), read: decodeStandardAuthoringTaskTOMLArtifact},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.read(test.raw); err == nil {
				t.Fatal("malformed wrapper unexpectedly accepted")
			}
		})
	}
	if content, err := decodeStandardAuthoringInstructionArtifact([]byte(`{"requirement":"raw JSON instructions remain raw"}`)); err != nil || string(content) != `{"requirement":"raw JSON instructions remain raw"}` {
		t.Fatalf("raw JSON instruction = %q, %v", content, err)
	}
	if content, err := decodeStandardAuthoringTaskTOMLArtifact(validTaskTOML); err != nil || string(content) != string(validTaskTOML) {
		t.Fatalf("raw task TOML = %q, %v", content, err)
	}
}

func standardAuthoringTaskPackageFixture(t *testing.T) StandardAuthoringTaskPackageInput {
	t.Helper()
	base, err := workflowadapter.NewStandardAuthoringEnvironmentPolicy("docker.io/library/alpine:3.21@sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("b", 40)
	repository := "https://github.com/acme/widget.git"
	brief, err := workflowadapter.NewStandardAuthoringBrief("bugfix", "widget", "Repair the bounded widget behavior")
	if err != nil {
		t.Fatal(err)
	}
	return StandardAuthoringTaskPackageInput{
		Instruction:   []byte("# Repair widget\n"),
		TaskTOMLDraft: []byte("[metadata]\ncode_lang = \"go\"\ntask_type = \"bugfix\"\napplication = \"widget\"\nis_0_to_1 = false\ngithub_url = \"" + repository + "\"\ncommit_id = \"" + commit + "\"\n"),
		Dockerfile:    []byte("FROM " + base.BaseImage + "\nRUN git clone " + repository + " /app/repo && cd /app/repo && git checkout " + commit + "\nCOPY package.json /tmp/package.json\n"),
		SolveScript:   []byte("#!/bin/sh\nexit 0\n"), TestScript: []byte("#!/bin/sh\nexit 0\n"),
		TestsAnalysis: []byte(`{"provided_information":"The instruction and pinned environment define the task.","theoretical_path":"Inspect the repository, implement the requested behavior, and run tests.","passing_evidence":"The visible contract and tests provide an objective pass condition."}`),
		Source:        store.AuthoringSource{RepositoryURL: repository, CommitSHA: commit}, Environment: base, Brief: &brief,
		Admission: codeedge.TaskAdmissionContract{ID: "codeedge.phase1.task-admission", Version: "1", Profile: codeedge.Profile{
			Metadata:                      codeedge.MetadataFieldMapping{CodeLang: codeedge.TOMLPath{"metadata", "code_lang"}, TaskType: codeedge.TOMLPath{"metadata", "task_type"}, Application: codeedge.TOMLPath{"metadata", "application"}, IsZeroToOne: codeedge.TOMLPath{"metadata", "is_0_to_1"}, GitHubURL: codeedge.TOMLPath{"metadata", "github_url"}, CommitID: codeedge.TOMLPath{"metadata", "commit_id"}},
			ProtectedEnvironmentVariables: []string{"ANTHROPIC_AUTH_TOKEN"},
		}},
	}
}

func standardAuthoringInstructionWrapperFixture(t *testing.T, content []byte) []byte {
	t.Helper()
	encoded, err := json.Marshal(standardAuthoringInstructionArtifact{
		Format: standardAuthoringInstructionArtifactFormat, Version: standardAuthoringModelArtifactVersion, Content: string(content),
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func standardAuthoringTaskTOMLWrapperFixture(t *testing.T, content []byte, brief workflowadapter.StandardAuthoringBrief) []byte {
	t.Helper()
	encoded, err := json.Marshal(standardAuthoringTaskTOMLArtifact{
		Format: standardAuthoringTaskTOMLArtifactFormat, Version: standardAuthoringModelArtifactVersion,
		Metadata: standardAuthoringTaskTOMLArtifactMetadata{TaskType: brief.TaskType, Application: brief.Application, Objective: brief.Objective},
		TaskTOML: string(content),
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func assertAdmissionViolationMessage(t *testing.T, report codeedge.AdmissionReport, code, messagePart string) {
	t.Helper()
	for _, violation := range report.Violations {
		if violation.Code == code && strings.Contains(violation.Message, messagePart) {
			return
		}
	}
	t.Fatalf("report %+v omitted %q violation containing %q", report, code, messagePart)
}

func taskPackageFilesByPath(files []codeedge.TaskPackageFile) map[string]codeedge.TaskPackageFile {
	result := make(map[string]codeedge.TaskPackageFile, len(files))
	for _, file := range files {
		result[file.Path] = file
	}
	return result
}

func assertAdmissionCode(t *testing.T, report codeedge.AdmissionReport, want string) {
	t.Helper()
	for _, violation := range report.Violations {
		if violation.Code == want {
			return
		}
	}
	t.Fatalf("report %+v omitted %q", report, want)
}
