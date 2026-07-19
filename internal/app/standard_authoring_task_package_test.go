package app

import (
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
	analysis := string(files["tests_analysis.md"].Data)
	for _, heading := range []string{"## 1. instruction 和 environment 已提供的信息", "## 2. 模型的理论通过路径", "## 3. 模型具备通过条件的依据"} {
		if !strings.Contains(analysis, heading) {
			t.Fatalf("rendered analysis omitted %q:\n%s", heading, analysis)
		}
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
