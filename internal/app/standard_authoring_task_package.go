package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

// StandardAuthoringTaskPackageInput contains frozen Authoring outputs only.
// The compiler neither reads a checkout nor has access to a mutable workspace.
type StandardAuthoringTaskPackageInput struct {
	Instruction   []byte
	TaskTOMLDraft []byte
	Dockerfile    []byte
	SolveScript   []byte
	TestScript    []byte
	TestsAnalysis []byte
	Source        store.AuthoringSource
	Environment   workflowadapter.StandardAuthoringEnvironmentPolicy
	Admission     codeedge.TaskAdmissionContract
}

// StandardAuthoringTaskPackageResult is the canonical package and its
// deterministic consumer admission evidence. CanonicalFiles is populated even
// for a repairable rejection so it can be inspected without recreating bytes.
type StandardAuthoringTaskPackageResult struct {
	CanonicalFiles []codeedge.TaskPackageFile
	Report         codeedge.AdmissionReport
}

// CompileStandardAuthoringTaskPackage canonicalizes model-owned artifacts and
// evaluates the frozen CodeEdge contract. Content violations are returned in
// Report; non-nil errors are contract corruption or an unavailable compiler.
func CompileStandardAuthoringTaskPackage(input StandardAuthoringTaskPackageInput) (StandardAuthoringTaskPackageResult, error) {
	if err := input.Environment.Validate(); err != nil {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("validate frozen Standard authoring environment policy: %w", err)
	}
	if err := input.Admission.Validate(); err != nil {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("validate frozen CodeEdge admission contract: %w", err)
	}
	repositoryURL, err := store.NormalizeAuthoringRepositoryURL(input.Source.RepositoryURL)
	if err != nil {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("validate frozen Authoring source repository: %w", err)
	}
	commitSHA, err := store.NormalizeAuthoringCommitSHA(input.Source.CommitSHA)
	if err != nil {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("validate frozen Authoring source commit: %w", err)
	}
	if err := input.Environment.ValidateDockerfile(input.Dockerfile); err != nil {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("validate frozen Standard authoring Dockerfile base image: %w", err)
	}

	taskTOML, canonicalizationViolations, err := canonicalizeStandardAuthoringTaskTOML(input.TaskTOMLDraft, input.Admission.Profile.Metadata, repositoryURL, commitSHA)
	if err != nil {
		return StandardAuthoringTaskPackageResult{}, err
	}
	testsAnalysis, err := renderStandardAuthoringTestsAnalysis(input.TestsAnalysis)
	if err != nil {
		return StandardAuthoringTaskPackageResult{}, err
	}
	files := []codeedge.TaskPackageFile{
		{Path: "instruction.md", Mode: 0o644, Data: clonePackageBytes(input.Instruction)},
		{Path: "task.toml", Mode: 0o644, Data: taskTOML},
		{Path: "tests_analysis.md", Mode: 0o644, Data: testsAnalysis},
		{Path: "environment/Dockerfile", Mode: 0o644, Data: clonePackageBytes(input.Dockerfile)},
		{Path: "solution/solve.sh", Mode: 0o755, Data: clonePackageBytes(input.SolveScript)},
		{Path: "tests/test.sh", Mode: 0o755, Data: clonePackageBytes(input.TestScript)},
	}
	report, err := codeedge.ValidateTaskPackage(input.Admission, files)
	if err != nil {
		return StandardAuthoringTaskPackageResult{}, err
	}
	if len(canonicalizationViolations) != 0 {
		report.Passed = false
		report.Violations = append(report.Violations, canonicalizationViolations...)
		sortViolations(report.Violations)
	}
	return StandardAuthoringTaskPackageResult{CanonicalFiles: files, Report: report}, nil
}

func clonePackageBytes(value []byte) []byte { return append([]byte(nil), value...) }

func canonicalizeStandardAuthoringTaskTOML(raw []byte, mapping codeedge.MetadataFieldMapping, repositoryURL, commitSHA string) ([]byte, []codeedge.Violation, error) {
	var document map[string]any
	decoder := toml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&document); err != nil {
		return nil, nil, fmt.Errorf("parse generated task.toml: %w", err)
	}
	if document == nil {
		return nil, nil, fmt.Errorf("parse generated task.toml: document is required")
	}
	paths := []struct {
		name  string
		path  codeedge.TOMLPath
		value string
	}{
		{"metadata.github_url", mapping.GitHubURL, repositoryURL},
		{"metadata.commit_id", mapping.CommitID, commitSHA},
		{"source.repository_url", codeedge.TOMLPath{"source", "repository_url"}, repositoryURL},
		{"source.commit_sha", codeedge.TOMLPath{"source", "commit_sha"}, commitSHA},
	}
	violations := make([]codeedge.Violation, 0)
	for _, item := range paths {
		if len(item.path) == 0 {
			return nil, nil, fmt.Errorf("CodeEdge task admission contract omits %s mapping", item.name)
		}
		if existing, present := readTaskTOMLString(document, item.path); present && existing != item.value {
			violations = append(violations, codeedge.Violation{Code: "task_metadata", Path: "task.toml", Message: "generated value conflicts with frozen source at " + item.name})
		}
		if err := setTaskTOMLString(document, item.path, item.value); err != nil {
			return nil, nil, fmt.Errorf("canonicalize generated task.toml %s: %w", item.name, err)
		}
	}
	encoded, err := toml.Marshal(document)
	if err != nil {
		return nil, nil, fmt.Errorf("encode canonical task.toml: %w", err)
	}
	return encoded, violations, nil
}

func readTaskTOMLString(document map[string]any, path codeedge.TOMLPath) (string, bool) {
	var current any = document
	for _, segment := range path {
		table, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = table[segment]
		if !ok {
			return "", false
		}
	}
	value, ok := current.(string)
	return value, ok
}

func setTaskTOMLString(document map[string]any, path codeedge.TOMLPath, value string) error {
	current := document
	for _, segment := range path[:len(path)-1] {
		if nested, found := current[segment]; found {
			table, ok := nested.(map[string]any)
			if !ok {
				return fmt.Errorf("table %q is not a table", segment)
			}
			current = table
			continue
		}
		next := make(map[string]any)
		current[segment] = next
		current = next
	}
	current[path[len(path)-1]] = value
	return nil
}

type standardAuthoringTestsAnalysis struct {
	ProvidedInformation string `json:"provided_information"`
	TheoreticalPath     string `json:"theoretical_path"`
	PassingEvidence     string `json:"passing_evidence"`
}

func renderStandardAuthoringTestsAnalysis(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var analysis standardAuthoringTestsAnalysis
	if err := decoder.Decode(&analysis); err != nil {
		return nil, fmt.Errorf("decode typed tests analysis: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode typed tests analysis: trailing JSON value")
	}
	sections := []struct{ title, body string }{
		{"1. instruction 和 environment 已提供的信息", analysis.ProvidedInformation},
		{"2. 模型的理论通过路径", analysis.TheoreticalPath},
		{"3. 模型具备通过条件的依据", analysis.PassingEvidence},
	}
	var output strings.Builder
	for index, section := range sections {
		body := strings.TrimSpace(section.body)
		if body == "" {
			return nil, fmt.Errorf("typed tests analysis section %q is required", section.title)
		}
		if index > 0 {
			output.WriteByte('\n')
		}
		output.WriteString("## ")
		output.WriteString(section.title)
		output.WriteString("\n")
		output.WriteString(body)
		output.WriteString("\n")
	}
	return []byte(output.String()), nil
}

// sortViolations is kept local so future compiler-owned diagnostics can be
// merged with consumer diagnostics without changing receipt determinism.
func sortViolations(violations []codeedge.Violation) {
	sort.Slice(violations, func(i, j int) bool { return violations[i].String() < violations[j].String() })
}
