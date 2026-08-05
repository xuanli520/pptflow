package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

const (
	standardAuthoringInstructionArtifactFormat = "harbor.standard-authoring-instruction.v1"
	standardAuthoringModelArtifactVersion      = "1"
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
	// Contract is the only root source for task, source, environment, and
	// delivery facts in a v2 authoring package.
	Contract  workflowadapter.AuthoringContract
	Admission codeedge.TaskAdmissionContract
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
	if err := input.Contract.Validate(); err != nil {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("validate frozen Standard authoring root contract: %w", err)
	}
	if input.Contract.Source.RepositoryURL != input.Source.RepositoryURL || input.Contract.Source.CommitSHA != input.Source.CommitSHA ||
		input.Contract.Source.SnapshotDigest != input.Source.SnapshotContentDigest {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("root contract does not match frozen Authoring source")
	}
	environment, err := input.Contract.EnvironmentPolicy()
	if err != nil {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("derive frozen Standard authoring environment policy: %w", err)
	}
	if err := input.Admission.Validate(); err != nil {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("validate frozen CodeEdge admission contract: %w", err)
	}
	facts := newStandardAuthoringTaskContractFacts(input.Contract)
	repositoryURL, err := store.NormalizeAuthoringRepositoryURL(input.Source.RepositoryURL)
	if err != nil {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("validate frozen Authoring source repository: %w", err)
	}
	commitSHA, err := store.NormalizeAuthoringCommitSHA(input.Source.CommitSHA)
	if err != nil {
		return StandardAuthoringTaskPackageResult{}, fmt.Errorf("validate frozen Authoring source commit: %w", err)
	}
	contentViolations := make([]codeedge.Violation, 0)
	if err := environment.ValidateDockerfile(input.Dockerfile); err != nil {
		contentViolations = append(contentViolations, invalidStandardAuthoringDockerfileViolation())
	}
	instruction, err := decodeStandardAuthoringInstructionArtifact(input.Instruction)
	if err != nil || !standardAuthoringModelBytes(instruction) {
		instruction = clonePackageBytes(input.Instruction)
		contentViolations = append(contentViolations, invalidStandardAuthoringInstructionViolation())
	}
	taskTOMLDraft := clonePackageBytes(input.TaskTOMLDraft)
	taskTOML := clonePackageBytes(taskTOMLDraft)
	if err := taskpolicy.ValidateStandardAuthoringTaskTOML(taskTOMLDraft); err != nil {
		contentViolations = append(contentViolations, invalidStandardAuthoringTaskTOMLViolation(err))
	} else {
		var canonicalizationViolations []codeedge.Violation
		taskTOML, canonicalizationViolations, err = canonicalizeStandardAuthoringTaskTOML(taskTOMLDraft, input.Admission.Profile.Metadata, repositoryURL, commitSHA, facts)
		if err != nil {
			taskTOML = clonePackageBytes(taskTOMLDraft)
			canonicalizationViolations = []codeedge.Violation{invalidStandardAuthoringTaskTOMLViolation(nil)}
		}
		contentViolations = append(contentViolations, canonicalizationViolations...)
	}
	testsAnalysis, err := renderStandardAuthoringTestsAnalysis(input.TestsAnalysis)
	if err != nil {
		testsAnalysis = clonePackageBytes(input.TestsAnalysis)
		contentViolations = append(contentViolations, invalidStandardAuthoringTestsAnalysisViolation())
	}
	if !standardAuthoringShellScript(input.SolveScript) {
		contentViolations = append(contentViolations, invalidStandardAuthoringSolveScriptViolation())
	}
	if !standardAuthoringShellScript(input.TestScript) {
		contentViolations = append(contentViolations, invalidStandardAuthoringTestScriptViolation())
	}
	files := []codeedge.TaskPackageFile{
		{Path: "instruction.md", Mode: 0o644, Data: instruction},
		{Path: "task.toml", Mode: 0o644, Data: taskTOML},
		{Path: "tests_analysis.md", Mode: 0o644, Data: testsAnalysis},
		{Path: "environment/Dockerfile", Mode: 0o644, Data: clonePackageBytes(input.Dockerfile)},
		{Path: "solution/solve.sh", Mode: 0o755, Data: clonePackageBytes(input.SolveScript)},
		{Path: "tests/test.sh", Mode: 0o755, Data: clonePackageBytes(input.TestScript)},
	}
	report, err := codeedge.ValidateTaskPackage(input.Admission, standardAuthoringAdmissionValidationFiles(files, contentViolations))
	if err != nil {
		return StandardAuthoringTaskPackageResult{}, err
	}
	if len(contentViolations) != 0 {
		report.Passed = false
		report.Violations = append(report.Violations, contentViolations...)
		sortViolations(report.Violations)
	}
	return StandardAuthoringTaskPackageResult{CanonicalFiles: files, Report: report}, nil
}

func clonePackageBytes(value []byte) []byte { return append([]byte(nil), value...) }

type standardAuthoringInstructionArtifact struct {
	Format  string `json:"format"`
	Version string `json:"version"`
	Content string `json:"content"`
}

func decodeStandardAuthoringInstructionArtifact(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("generated instruction is empty")
	}
	if !standardAuthoringJSONArtifactCandidate(raw) {
		return clonePackageBytes(raw), nil
	}
	var artifact standardAuthoringInstructionArtifact
	if err := decodeStrictStandardAuthoringJSONArtifact(raw, &artifact); err != nil {
		return nil, err
	}
	if artifact.Format != standardAuthoringInstructionArtifactFormat || artifact.Version != standardAuthoringModelArtifactVersion || !standardAuthoringModelText(artifact.Content) {
		return nil, fmt.Errorf("generated instruction wrapper does not match the supported contract")
	}
	if standardAuthoringJSONArtifactCandidate([]byte(artifact.Content)) {
		return nil, fmt.Errorf("generated instruction wrapper payload must be raw final-file content")
	}
	return []byte(artifact.Content), nil
}

// A raw instruction may itself be JSON. Reserve only a top-level format field
// as the discriminator for the observed structured artifact form.
func standardAuthoringJSONArtifactCandidate(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&fields); err != nil {
		return bytes.Contains(trimmed, []byte(`"format"`))
	}
	_, found := fields["format"]
	return found
}

func decodeStrictStandardAuthoringJSONArtifact(raw []byte, destination any) error {
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return fmt.Errorf("generated JSON artifact is not valid UTF-8 text")
	}
	if err := rejectDuplicateStandardAuthoringJSONKeys(raw); err != nil {
		return err
	}
	if err := decodeStrictJSON(string(raw), destination); err != nil {
		return err
	}
	return nil
}

func rejectDuplicateStandardAuthoringJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := walkStandardAuthoringJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func walkStandardAuthoringJSONValue(decoder *json.Decoder, location string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key at %s is not a string", location)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key %q at %s", key, location)
			}
			seen[key] = struct{}{}
			if err := walkStandardAuthoringJSONValue(decoder, location+"."+key); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object at %s is not closed", location)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := walkStandardAuthoringJSONValue(decoder, fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
			index++
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array at %s is not closed", location)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, location)
	}
	return nil
}

func invalidStandardAuthoringInstructionViolation() codeedge.Violation {
	return codeedge.Violation{Code: "task_instruction", Path: "instruction.md", Message: "generated instruction must be raw content or a strict harbor.standard-authoring-instruction.v1 wrapper with non-empty content"}
}

func invalidStandardAuthoringDockerfileViolation() codeedge.Violation {
	return codeedge.Violation{Code: "environment_isolation", Path: "environment/Dockerfile", Message: "generated Dockerfile does not match the frozen environment policy"}
}

func invalidStandardAuthoringTaskTOMLViolation(validationErr error) codeedge.Violation {
	message := "generated task.toml must be valid TOML and contain the complete Harbor TaskConfig contract: [metadata], [task], [environment], and [verifier]"
	if validationErr != nil {
		message += "; " + validationErr.Error()
	}
	return codeedge.Violation{Code: "task_metadata", Path: "task.toml", Message: message}
}

func invalidStandardAuthoringTestsAnalysisViolation() codeedge.Violation {
	return codeedge.Violation{Code: "tests_analysis", Path: "tests_analysis.md", Message: "tests_analysis.md is rendered from tests_analysis.json; generated tests analysis must be a strict harbor.standard-authoring-tests-analysis.v2 JSON object (requirement_ids plus provided_information, theoretical_path, and passing_evidence sections) with stable requirement IDs"}
}

func invalidStandardAuthoringSolveScriptViolation() codeedge.Violation {
	return codeedge.Violation{Code: "task_layout", Path: "solution/solve.sh", Message: "generated solution must be a non-empty executable shell script with a shebang"}
}

func invalidStandardAuthoringTestScriptViolation() codeedge.Violation {
	return codeedge.Violation{Code: "task_layout", Path: "tests/test.sh", Message: "generated tests must be a non-empty executable shell script with a shebang"}
}

func standardAuthoringModelBytes(value []byte) bool {
	return len(bytes.TrimSpace(value)) != 0 && utf8.Valid(value) && bytes.IndexByte(value, 0) < 0
}

func standardAuthoringModelText(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func standardAuthoringShellScript(content []byte) bool {
	if !standardAuthoringModelBytes(content) || !bytes.HasPrefix(content, []byte("#!")) {
		return false
	}
	lineEnd := bytes.IndexByte(content, '\n')
	return lineEnd >= 3 && bytes.IndexByte(content[:lineEnd], '\r') < 0 && strings.TrimSpace(string(content[lineEnd+1:])) != ""
}

func standardAuthoringAdmissionValidationFiles(files []codeedge.TaskPackageFile, violations []codeedge.Violation) []codeedge.TaskPackageFile {
	validationFiles := make([]codeedge.TaskPackageFile, len(files))
	copy(validationFiles, files)
	invalidPaths := make(map[string]struct{}, len(violations))
	for _, violation := range violations {
		invalidPaths[violation.Path] = struct{}{}
	}
	for index := range validationFiles {
		validationFiles[index].Data = clonePackageBytes(validationFiles[index].Data)
		if len(validationFiles[index].Data) == 0 {
			if _, invalid := invalidPaths[validationFiles[index].Path]; invalid {
				validationFiles[index].Data = []byte("\n")
			}
		}
	}
	return validationFiles
}

type standardAuthoringTaskTOMLFact struct {
	name  string
	path  codeedge.TOMLPath
	value string
}

// standardAuthoringTaskContractFacts is deliberately a private projection.
// The root contract remains the single persisted source of these values.
type standardAuthoringTaskContractFacts struct {
	codeLang    string
	taskType    string
	application string
	is0To1      bool
}

func newStandardAuthoringTaskContractFacts(contract workflowadapter.AuthoringContract) standardAuthoringTaskContractFacts {
	return standardAuthoringTaskContractFacts{
		codeLang: contract.Task.CodeLang, taskType: contract.Task.TaskType,
		application: contract.Task.Application, is0To1: contract.Task.Is0To1,
	}
}

func canonicalizeStandardAuthoringTaskTOML(raw []byte, mapping codeedge.MetadataFieldMapping, repositoryURL, commitSHA string, facts standardAuthoringTaskContractFacts) ([]byte, []codeedge.Violation, error) {
	document, err := parseStandardAuthoringTaskTOMLPayload(raw)
	if err != nil {
		return nil, nil, err
	}
	violations := make([]codeedge.Violation, 0)
	sourceFacts := []standardAuthoringTaskTOMLFact{
		{"metadata.github_url", mapping.GitHubURL, repositoryURL},
		{"metadata.commit_id", mapping.CommitID, commitSHA},
	}
	for _, item := range sourceFacts {
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
	// The current Harbor task contract models source as one top-level frozen
	// coordinate. The immutable URL and commit remain separately attested in
	// metadata, while this coordinate keeps the package readable by the external
	// evaluator's local task parser.
	sourceCoordinate := repositoryURL + "@" + commitSHA
	if source, present := document["source"]; present {
		sourceText, isString := source.(string)
		if !isString || sourceText != sourceCoordinate {
			violations = append(violations, codeedge.Violation{Code: "task_metadata", Path: "task.toml", Message: "generated value conflicts with frozen source at source"})
		}
	}
	document["source"] = sourceCoordinate
	metadataFacts, err := standardAuthoringTaskTOMLMetadataFacts(mapping, facts)
	if err != nil {
		return nil, nil, err
	}
	violations = append(violations, standardAuthoringTaskTOMLFactViolations(document, metadataFacts)...)
	for _, fact := range metadataFacts {
		if err := setTaskTOMLString(document, fact.path, fact.value); err != nil {
			return nil, nil, fmt.Errorf("canonicalize generated task.toml %s: %w", fact.name, err)
		}
	}
	if len(mapping.IsZeroToOne) == 0 {
		return nil, nil, fmt.Errorf("CodeEdge task admission contract omits metadata.is_zero_to_one mapping")
	}
	if existing, present := readTaskTOMLBool(document, mapping.IsZeroToOne); !present || existing != facts.is0To1 {
		violations = append(violations, codeedge.Violation{Code: "task_metadata", Path: "task.toml", Message: "generated value does not exactly match the root contract at metadata.is_zero_to_one"})
	}
	if err := setTaskTOMLBool(document, mapping.IsZeroToOne, facts.is0To1); err != nil {
		return nil, nil, fmt.Errorf("canonicalize generated task.toml metadata.is_zero_to_one: %w", err)
	}
	encoded, err := toml.Marshal(document)
	if err != nil {
		return nil, nil, fmt.Errorf("encode canonical task.toml: %w", err)
	}
	return encoded, violations, nil
}

func parseStandardAuthoringTaskTOML(raw []byte) (map[string]any, error) {
	return parseStandardAuthoringTaskTOMLPayload(raw)
}

func parseStandardAuthoringTaskTOMLPayload(payload []byte) (map[string]any, error) {
	var document map[string]any
	decoder := toml.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse generated task.toml: %w", err)
	}
	if document == nil {
		return nil, fmt.Errorf("parse generated task.toml: document is required")
	}
	return document, nil
}

func validateStandardAuthoringTaskTOMLContract(raw []byte, mapping codeedge.MetadataFieldMapping, contract workflowadapter.AuthoringContract) ([]codeedge.Violation, error) {
	document, err := parseStandardAuthoringTaskTOMLPayload(raw)
	if err != nil {
		return []codeedge.Violation{invalidStandardAuthoringTaskTOMLViolation(nil)}, nil
	}
	facts, err := standardAuthoringTaskTOMLMetadataFacts(mapping, newStandardAuthoringTaskContractFacts(contract))
	if err != nil {
		return nil, err
	}
	violations := standardAuthoringTaskTOMLFactViolations(document, facts)
	if len(mapping.IsZeroToOne) == 0 {
		return nil, fmt.Errorf("CodeEdge task admission contract omits metadata.is_zero_to_one mapping")
	}
	if existing, present := readTaskTOMLBool(document, mapping.IsZeroToOne); !present || existing != contract.Task.Is0To1 {
		violations = append(violations, codeedge.Violation{Code: "task_metadata", Path: "task.toml", Message: "generated value does not exactly match the root contract at metadata.is_zero_to_one"})
	}
	return violations, nil
}

func standardAuthoringTaskTOMLMetadataFacts(mapping codeedge.MetadataFieldMapping, contractFacts standardAuthoringTaskContractFacts) ([]standardAuthoringTaskTOMLFact, error) {
	metadataFacts := []standardAuthoringTaskTOMLFact{
		{"metadata.code_lang", mapping.CodeLang, contractFacts.codeLang},
		{"metadata.task_type", mapping.TaskType, contractFacts.taskType},
		{"metadata.application", mapping.Application, contractFacts.application},
	}
	for _, fact := range metadataFacts {
		if len(fact.path) == 0 {
			return nil, fmt.Errorf("CodeEdge task admission contract omits %s mapping", fact.name)
		}
	}
	return metadataFacts, nil
}

func standardAuthoringTaskTOMLFactViolations(document map[string]any, facts []standardAuthoringTaskTOMLFact) []codeedge.Violation {
	violations := make([]codeedge.Violation, 0)
	for _, fact := range facts {
		if existing, present := readTaskTOMLString(document, fact.path); !present || existing != fact.value {
			violations = append(violations, codeedge.Violation{Code: "task_metadata", Path: "task.toml", Message: "generated value does not exactly match the root contract at " + fact.name})
		}
	}
	return violations
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

func readTaskTOMLBool(document map[string]any, path codeedge.TOMLPath) (bool, bool) {
	var current any = document
	for _, segment := range path {
		table, ok := current.(map[string]any)
		if !ok {
			return false, false
		}
		current, ok = table[segment]
		if !ok {
			return false, false
		}
	}
	value, ok := current.(bool)
	return value, ok
}

func setTaskTOMLBool(document map[string]any, path codeedge.TOMLPath, value bool) error {
	if len(path) == 0 {
		return fmt.Errorf("path is required")
	}
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
	Format              string   `json:"format"`
	Version             string   `json:"version"`
	RequirementIDs      []string `json:"requirement_ids"`
	ProvidedInformation string   `json:"provided_information"`
	TheoreticalPath     string   `json:"theoretical_path"`
	PassingEvidence     string   `json:"passing_evidence"`
}

// standardAuthoringTestsAnalysisRequirement is one entry of the
// requirements-array shape some authoring agents emit. The package compiler
// accepts that shape as a v2-compatible equivalent and renders the same
// tests_analysis.md sections from it.
type standardAuthoringTestsAnalysisRequirement struct {
	RequirementID       string `json:"requirement_id"`
	ProvidedInformation string `json:"provided_information"`
	TheoreticalPath     string `json:"theoretical_path"`
	PassingEvidence     string `json:"passing_evidence"`
}

type standardAuthoringTestsAnalysisRequirementsShape struct {
	Format       string                                      `json:"format"`
	Version      string                                      `json:"version"`
	Requirements []standardAuthoringTestsAnalysisRequirement `json:"requirements"`
}

func renderStandardAuthoringTestsAnalysis(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return nil, fmt.Errorf("decode typed tests analysis: document is not valid UTF-8 text")
	}
	if err := rejectDuplicateStandardAuthoringJSONKeys(raw); err != nil {
		return nil, fmt.Errorf("decode typed tests analysis: %w", err)
	}
	analysis, err := decodeStandardAuthoringTestsAnalysis(raw)
	if err != nil {
		return nil, fmt.Errorf("decode typed tests analysis: %w", err)
	}
	sections := []struct{ title, body string }{
		{"0. Requirement IDs", strings.Join(analysis.RequirementIDs, ", ")},
		{"1. instruction 和 environment 已提供的信息", analysis.ProvidedInformation},
		{"2. 模型的理论通过路径", analysis.TheoreticalPath},
		{"3. 模型具备通过条件的依据", analysis.PassingEvidence},
	}
	var output strings.Builder
	for index, section := range sections {
		if !standardAuthoringModelText(section.body) {
			return nil, fmt.Errorf("typed tests analysis section %q is required", section.title)
		}
		body := strings.TrimSpace(section.body)
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

// decodeStandardAuthoringTestsAnalysis accepts both the canonical flat
// harbor.standard-authoring-tests-analysis.v2 shape (requirement_ids plus one
// section per requirement family) and the requirements-array shape emitted by
// some authoring agents (one entry per requirement with the same section
// fields). Both decode strictly: duplicate JSON keys, unknown fields, invalid
// or duplicate requirement IDs, and missing section bodies are rejected.
func decodeStandardAuthoringTestsAnalysis(raw []byte) (standardAuthoringTestsAnalysis, error) {
	if analysis, err := decodeStandardAuthoringTestsAnalysisFlat(raw); err == nil {
		return analysis, nil
	}
	return decodeStandardAuthoringTestsAnalysisRequirementsShape(raw)
}

func decodeStandardAuthoringTestsAnalysisFlat(raw []byte) (standardAuthoringTestsAnalysis, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var analysis standardAuthoringTestsAnalysis
	if err := decoder.Decode(&analysis); err != nil {
		return standardAuthoringTestsAnalysis{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return standardAuthoringTestsAnalysis{}, fmt.Errorf("trailing JSON value")
	}
	if analysis.Format != "harbor.standard-authoring-tests-analysis.v2" || analysis.Version != "2" || len(analysis.RequirementIDs) == 0 {
		return standardAuthoringTestsAnalysis{}, fmt.Errorf("format, version, and requirement IDs are required")
	}
	requirementIDs := make(map[string]struct{}, len(analysis.RequirementIDs))
	for _, id := range analysis.RequirementIDs {
		if !standardAuthoringRequirementID(id) {
			return standardAuthoringTestsAnalysis{}, fmt.Errorf("requirement ID is invalid")
		}
		if _, duplicate := requirementIDs[id]; duplicate {
			return standardAuthoringTestsAnalysis{}, fmt.Errorf("requirement IDs must be unique")
		}
		requirementIDs[id] = struct{}{}
	}
	return analysis, nil
}

func decodeStandardAuthoringTestsAnalysisRequirementsShape(raw []byte) (standardAuthoringTestsAnalysis, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var shape standardAuthoringTestsAnalysisRequirementsShape
	if err := decoder.Decode(&shape); err != nil {
		return standardAuthoringTestsAnalysis{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return standardAuthoringTestsAnalysis{}, fmt.Errorf("trailing JSON value")
	}
	if shape.Format != "harbor.standard-authoring-tests-analysis.v2" || shape.Version != "2" || len(shape.Requirements) == 0 {
		return standardAuthoringTestsAnalysis{}, fmt.Errorf("format, version, and requirements are required")
	}
	analysis := standardAuthoringTestsAnalysis{
		Format: shape.Format, Version: shape.Version,
		RequirementIDs: make([]string, 0, len(shape.Requirements)),
	}
	requirementIDs := make(map[string]struct{}, len(shape.Requirements))
	provided := make([]string, 0, len(shape.Requirements))
	theoretical := make([]string, 0, len(shape.Requirements))
	evidence := make([]string, 0, len(shape.Requirements))
	for _, requirement := range shape.Requirements {
		if !standardAuthoringRequirementID(requirement.RequirementID) {
			return standardAuthoringTestsAnalysis{}, fmt.Errorf("requirement ID is invalid")
		}
		if _, duplicate := requirementIDs[requirement.RequirementID]; duplicate {
			return standardAuthoringTestsAnalysis{}, fmt.Errorf("requirement IDs must be unique")
		}
		requirementIDs[requirement.RequirementID] = struct{}{}
		analysis.RequirementIDs = append(analysis.RequirementIDs, requirement.RequirementID)
		provided = append(provided, strings.TrimSpace(requirement.ProvidedInformation))
		theoretical = append(theoretical, strings.TrimSpace(requirement.TheoreticalPath))
		evidence = append(evidence, strings.TrimSpace(requirement.PassingEvidence))
	}
	analysis.ProvidedInformation = strings.Join(provided, "\n\n")
	analysis.TheoreticalPath = strings.Join(theoretical, "\n\n")
	analysis.PassingEvidence = strings.Join(evidence, "\n\n")
	return analysis, nil
}

func standardAuthoringRequirementID(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for _, character := range value[1:] {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

// sortViolations is kept local so future compiler-owned diagnostics can be
// merged with consumer diagnostics without changing receipt determinism.
func sortViolations(violations []codeedge.Violation) {
	sort.Slice(violations, func(i, j int) bool { return violations[i].String() < violations[j].String() })
}
