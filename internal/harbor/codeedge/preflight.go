// Package codeedge provides deterministic, side-effect-free preflight checks
// for CodeEdge Phase-1 Harbor task snapshots.
//
// It deliberately validates only the task package contract. It does not run
// Docker, invoke a provider, read credentials, resolve a Git revision, or
// mutate a managed snapshot. Those actions belong to separately controlled
// deployment operations after this preflight has admitted the immutable input.
package codeedge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"gopkg.in/yaml.v3"
)

// EnvironmentKind names the one environment definition selected by the
// CodeEdge Phase-1 profile.
type EnvironmentKind string

const (
	EnvironmentDockerfile EnvironmentKind = "dockerfile"
	EnvironmentCompose    EnvironmentKind = "docker-compose"
)

// Metadata is the subset of task.toml metadata that CodeEdge Phase-1 needs to
// make its package and source-provenance decision. It intentionally excludes
// secrets, execution settings, and arbitrary task-local configuration.
type Metadata struct {
	CodeLang    string
	TaskType    string
	Application string
	IsZeroToOne bool
	GitHubURL   string
	CommitID    string
}

// TOMLPath is an explicit, unambiguous path to one value in task.toml. A
// deployment selects these paths when it adopts the CodeEdge profile; this
// package deliberately does not assume a Harbor template's table or key names.
//
// For example, TOMLPath{"submission", "language"} selects
// [submission].language. The path is represented as segments rather than a
// dotted string so TOML keys containing punctuation cannot be silently parsed
// differently by a caller and this preflight.
type TOMLPath []string

func (path TOMLPath) String() string {
	return strings.Join(path, ".")
}

// MetadataFieldMapping explicitly binds the semantic CodeEdge Phase-1 fields
// to task.toml locations. The training document requires these facts, but does
// not prescribe a task.toml schema, so a controlled deployment must supply this
// mapping rather than this package guessing table/key names.
type MetadataFieldMapping struct {
	CodeLang    TOMLPath
	TaskType    TOMLPath
	Application TOMLPath
	IsZeroToOne TOMLPath
	GitHubURL   TOMLPath
	CommitID    TOMLPath
}

// Profile holds the explicit, version-controlled policy inputs needed by this
// otherwise schema-agnostic preflight. It contains identities and field paths,
// never provider configuration, credentials, or secret values.
type Profile struct {
	Metadata MetadataFieldMapping
	// ProtectedEnvironmentVariables is the explicit deployment-derived set of
	// host and child environment names that a task-owned Dockerfile or Compose
	// document may never interpolate or request for pass-through. It stores
	// names only, never endpoint or credential values.
	ProtectedEnvironmentVariables []string
}

// Report is the deterministic, non-secret result of a successful Phase-1
// preflight. Callers may persist it as evidence, but must bind the original
// managed snapshot digest separately.
type Report struct {
	Environment EnvironmentKind
	Metadata    Metadata
}

// Violation is one stable diagnostic emitted by the preflight. Code identifies
// the policy family, Path identifies the managed task-relative input when
// applicable, and Message is suitable for an operator-facing receipt.
type Violation struct {
	Code    string
	Path    string
	Message string
}

func (violation Violation) String() string {
	if violation.Path == "" {
		return violation.Code + ": " + violation.Message
	}
	return violation.Code + " (" + violation.Path + "): " + violation.Message
}

// ValidationError aggregates all independently observable deterministic
// failures. Violations are sorted before the error is returned, so a caller
// gets the same receipt order for the same snapshot bytes and layout.
type ValidationError struct {
	Violations []Violation
}

func (err *ValidationError) Error() string {
	if err == nil || len(err.Violations) == 0 {
		return "CodeEdge Phase-1 preflight failed"
	}
	parts := make([]string, 0, len(err.Violations))
	for _, violation := range err.Violations {
		parts = append(parts, violation.String())
	}
	return "CodeEdge Phase-1 preflight failed: " + strings.Join(parts, "; ")
}

// ValidatePhase1Task validates a prospective CodeEdge Phase-1 managed task
// snapshot against an explicit deployment profile. It is a convenience wrapper
// for callers which only need an admission decision; InspectPhase1Task returns
// the parsed non-secret report.
func ValidatePhase1Task(root string, profile Profile) error {
	_, err := InspectPhase1Task(root, profile)
	return err
}

// ValidateProtectedEnvironmentReferences checks whether task-owned Dockerfile,
// Compose, or Harbor [environment.env] content can request, declare, or
// interpolate one of the deployment-controlled environment variable names.
// It deliberately does not interpret task metadata. It does inspect
// [environment.env], whose schema is an observed Harbor runtime contract:
// Harbor resolves its values from its own process environment before it starts
// the task environment.
//
// The supplied list is names only and must be explicit, non-empty, valid, and
// duplicate-free. Values must never cross this API boundary.
func ValidateProtectedEnvironmentReferences(root string, protectedEnvironmentVariables []string) error {
	root = strings.TrimSpace(root)
	collector := newViolationCollector()
	if root == "" {
		collector.add("task_root", "", "task root is required")
		return collector.err()
	}
	valid, protected := validateProtectedEnvironmentVariables(protectedEnvironmentVariables, collector)
	if !valid {
		return collector.err()
	}
	validateTaskTOMLProtectedEnvironmentReferences(root, protected, collector)
	if raw, ok := readRegularTaskFile(root, "environment/Dockerfile", collector); ok {
		validateDockerfileProtectedEnvironmentReferences(string(raw), "environment/Dockerfile", protected, collector)
	}
	if raw, ok := readRegularTaskFile(root, "environment/docker-compose.yaml", collector); ok {
		validateComposeProtectedEnvironmentReferencesRaw(raw, "environment/docker-compose.yaml", protected, collector)
	}
	return collector.err()
}

// InspectPhase1Task validates a prospective CodeEdge Phase-1 managed task
// snapshot and returns its selected environment and required metadata only on
// success.
//
// The taskpolicy package remains the authority for the V2 canonical file set.
// CodeEdge adds profile-specific rules which taskpolicy intentionally does not
// impose: exactly one environment definition, metadata/provenance, the tests
// analysis template, and statically obvious environment isolation.
func InspectPhase1Task(root string, profile Profile) (Report, error) {
	root = strings.TrimSpace(root)
	collector := newViolationCollector()
	if root == "" {
		collector.add("task_layout", "", "managed task root is required")
		return Report{}, collector.err()
	}

	profileValid, protectedEnvironmentVariables := validateProfile(profile, collector)
	validateManagedLayout(root, collector)
	environment, environmentPath := detectSingleEnvironment(root, collector)
	metadata, provenance := Metadata{}, repositoryProvenance{}
	if profileValid {
		metadata, provenance = inspectTaskMetadata(root, profile.Metadata, collector)
		validateTaskTOMLProtectedEnvironmentReferences(root, protectedEnvironmentVariables, collector)
	}
	validateTestsAnalysis(root, collector)

	var environmentEvidence gitEvidence
	switch environment {
	case EnvironmentDockerfile:
		environmentEvidence = inspectDockerfilePath(root, environmentPath, protectedEnvironmentVariables, collector)
	case EnvironmentCompose:
		environmentEvidence = inspectComposePath(root, environmentPath, protectedEnvironmentVariables, collector)
	}
	if provenance.required && provenance.valid && environment != "" {
		validateRepositoryProvenance(environmentPath, provenance.repository, provenance.commit, environmentEvidence, collector)
	}

	if err := collector.err(); err != nil {
		return Report{}, err
	}
	return Report{Environment: environment, Metadata: metadata}, nil
}

type violationCollector struct {
	seen       map[Violation]struct{}
	violations []Violation
}

func newViolationCollector() *violationCollector {
	return &violationCollector{seen: make(map[Violation]struct{})}
}

func (collector *violationCollector) add(code, filePath, message string) {
	violation := Violation{Code: code, Path: filePath, Message: message}
	if _, exists := collector.seen[violation]; exists {
		return
	}
	collector.seen[violation] = struct{}{}
	collector.violations = append(collector.violations, violation)
}

func (collector *violationCollector) err() error {
	if len(collector.violations) == 0 {
		return nil
	}
	violations := append([]Violation(nil), collector.violations...)
	sort.Slice(violations, func(left, right int) bool {
		if violations[left].Code != violations[right].Code {
			return violations[left].Code < violations[right].Code
		}
		if violations[left].Path != violations[right].Path {
			return violations[left].Path < violations[right].Path
		}
		return violations[left].Message < violations[right].Message
	})
	return &ValidationError{Violations: violations}
}

func validateManagedLayout(root string, collector *violationCollector) {
	err := taskpolicy.ValidateManagedSnapshotV2(root)
	if err == nil {
		return
	}
	var snapshotErr *taskpolicy.SnapshotValidationError
	if errors.As(err, &snapshotErr) {
		for _, violation := range snapshotErr.Violations {
			collector.add("task_layout", "", violation)
		}
		return
	}
	collector.add("task_layout", "", err.Error())
}

func detectSingleEnvironment(root string, collector *violationCollector) (EnvironmentKind, string) {
	dockerPath := "environment/Dockerfile"
	composePath := "environment/docker-compose.yaml"
	dockerExists := regularFileExists(root, dockerPath)
	composeExists := regularFileExists(root, composePath)
	count := 0
	if dockerExists {
		count++
	}
	if composeExists {
		count++
	}
	if count != 1 {
		collector.add("environment_profile", "environment", "CodeEdge Phase-1 requires exactly one of environment/Dockerfile or environment/docker-compose.yaml")
		return "", ""
	}
	if dockerExists {
		return EnvironmentDockerfile, dockerPath
	}
	return EnvironmentCompose, composePath
}

func regularFileExists(root, relativePath string) bool {
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relativePath)))
	return err == nil && info.Mode().IsRegular()
}

func validateProfile(profile Profile, collector *violationCollector) (bool, map[string]struct{}) {
	fields := []struct {
		name string
		path TOMLPath
	}{
		{name: "code language", path: profile.Metadata.CodeLang},
		{name: "task type", path: profile.Metadata.TaskType},
		{name: "application", path: profile.Metadata.Application},
		{name: "zero-to-one flag", path: profile.Metadata.IsZeroToOne},
		{name: "GitHub URL", path: profile.Metadata.GitHubURL},
		{name: "commit ID", path: profile.Metadata.CommitID},
	}
	valid := true
	seen := make(map[string]string, len(fields))
	for _, field := range fields {
		if len(field.path) == 0 {
			collector.add("metadata_profile", "", "metadata field mapping is required for "+field.name)
			valid = false
			continue
		}
		for _, segment := range field.path {
			if strings.TrimSpace(segment) == "" || segment != strings.TrimSpace(segment) {
				collector.add("metadata_profile", "", "metadata field mapping for "+field.name+" contains an empty or whitespace-padded TOML path segment")
				valid = false
				break
			}
		}
		path := field.path.String()
		if other, exists := seen[path]; exists {
			collector.add("metadata_profile", "", "metadata field mappings for "+other+" and "+field.name+" must not use the same TOML path "+path)
			valid = false
			continue
		}
		seen[path] = field.name
	}
	protectedValid, protected := validateProtectedEnvironmentVariables(profile.ProtectedEnvironmentVariables, collector)
	valid = valid && protectedValid
	return valid, protected
}

func validateProtectedEnvironmentVariables(variables []string, collector *violationCollector) (bool, map[string]struct{}) {
	protected := make(map[string]struct{}, len(variables))
	if variables == nil || len(variables) == 0 {
		collector.add("deployment_profile", "", "protected deployment environment variables must be an explicit non-empty list")
		return false, protected
	}
	valid := true
	for _, variable := range variables {
		if !validEnvironmentVariableName(variable) {
			collector.add("deployment_profile", "", "protected deployment environment variable is invalid: "+variable)
			valid = false
			continue
		}
		if _, duplicate := protected[variable]; duplicate {
			collector.add("deployment_profile", "", "protected deployment environment variable is duplicated: "+variable)
			valid = false
			continue
		}
		protected[variable] = struct{}{}
	}
	return valid, protected
}

func validEnvironmentVariableName(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if (character >= 'A' && character <= 'Z') || character == '_' {
			continue
		}
		if index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

type repositoryProvenance struct {
	required   bool
	valid      bool
	repository githubRepository
	commit     string
}

func inspectTaskMetadata(root string, fields MetadataFieldMapping, collector *violationCollector) (Metadata, repositoryProvenance) {
	raw, ok := readRegularTaskFile(root, "task.toml", collector)
	if !ok {
		return Metadata{}, repositoryProvenance{}
	}
	var document map[string]any
	if err := toml.Unmarshal(raw, &document); err != nil {
		collector.add("task_metadata", "task.toml", "task.toml must be valid TOML: "+err.Error())
		return Metadata{}, repositoryProvenance{}
	}

	metadata := taskTOMLMetadataToReport(document, fields, collector)
	provenance := repositoryProvenance{}
	isZeroToOne, zeroToOneOK := mappedTOMLBool(document, fields.IsZeroToOne, collector)
	if !zeroToOneOK {
		return metadata, provenance
	}
	metadata.IsZeroToOne = isZeroToOne
	if isZeroToOne {
		return metadata, provenance
	}

	provenance.required = true
	githubURL, githubURLOK := mappedTOMLString(document, fields.GitHubURL, collector, false)
	commitID, commitIDOK := mappedTOMLString(document, fields.CommitID, collector, false)
	repository, repositoryOK := githubRepository{}, false
	if !githubURLOK {
		collector.add("task_metadata", "task.toml", fields.GitHubURL.String()+" is required for non-0-1 tasks")
	} else {
		repository, repositoryOK = validateGitHubURL(githubURL, fields.GitHubURL, collector)
	}
	commit, commitOK := "", false
	if !commitIDOK {
		collector.add("task_metadata", "task.toml", fields.CommitID.String()+" is required for non-0-1 tasks")
	} else {
		commit, commitOK = validateCommitID(commitID, fields.CommitID, collector)
	}
	if repositoryOK && commitOK {
		provenance.valid = true
		provenance.repository = repository
		provenance.commit = commit
	}
	return metadata, provenance
}

// validateTaskTOMLProtectedEnvironmentReferences validates only Harbor's
// [environment.env] runtime map. It intentionally shares the same
// name-only policy with Dockerfile and Compose inspection so an evaluator can
// call it after materializing an immutable snapshot without reading a secret.
func validateTaskTOMLProtectedEnvironmentReferences(root string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) {
	if len(protectedEnvironmentVariables) == 0 {
		return
	}
	raw, ok := readRegularTaskFile(root, "task.toml", collector)
	if !ok {
		return
	}
	var document map[string]any
	if err := toml.Unmarshal(raw, &document); err != nil {
		collector.add("environment_isolation", "task.toml", "task.toml must be valid TOML: "+err.Error())
		return
	}
	value, found := tomlValueAt(document, TOMLPath{"environment", "env"})
	if !found {
		return
	}
	variables, ok := value.(map[string]any)
	if !ok {
		collector.add("environment_isolation", "task.toml", "task.toml [environment.env] must be a string map")
		return
	}
	for name, rawValue := range variables {
		if _, protected := protectedEnvironmentVariables[name]; protected {
			collector.add("environment_isolation", "task.toml", "task.toml [environment.env] must not declare protected deployment environment variable "+name)
		}
		value, ok := rawValue.(string)
		if !ok {
			collector.add("environment_isolation", "task.toml", "task.toml [environment.env]."+name+" must be a string")
			continue
		}
		validateProtectedEnvironmentInterpolations(value, "task.toml", "task.toml [environment.env]", protectedEnvironmentVariables, collector)
	}
}

func taskTOMLMetadataToReport(document map[string]any, fields MetadataFieldMapping, collector *violationCollector) Metadata {
	codeLang, _ := mappedTOMLString(document, fields.CodeLang, collector, true)
	taskType, _ := mappedTOMLString(document, fields.TaskType, collector, true)
	application, _ := mappedTOMLString(document, fields.Application, collector, true)
	githubURL, _ := mappedTOMLString(document, fields.GitHubURL, collector, false)
	commitID, _ := mappedTOMLString(document, fields.CommitID, collector, false)
	return Metadata{
		CodeLang:    codeLang,
		TaskType:    taskType,
		Application: application,
		GitHubURL:   githubURL,
		CommitID:    commitID,
	}
}

func mappedTOMLString(document map[string]any, field TOMLPath, collector *violationCollector, required bool) (string, bool) {
	value, found := tomlValueAt(document, field)
	fieldName := field.String()
	if !found {
		if required {
			collector.add("task_metadata", "task.toml", fieldName+" is required")
		}
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		collector.add("task_metadata", "task.toml", fieldName+" must be a string")
		return "", false
	}
	return requiredMetadataValue(fieldName, text, collector, required)
}

func mappedTOMLBool(document map[string]any, field TOMLPath, collector *violationCollector) (bool, bool) {
	value, found := tomlValueAt(document, field)
	fieldName := field.String()
	if !found {
		collector.add("task_metadata", "task.toml", fieldName+" is required")
		return false, false
	}
	boolean, ok := value.(bool)
	if !ok {
		collector.add("task_metadata", "task.toml", fieldName+" must be a boolean")
		return false, false
	}
	return boolean, true
}

func tomlValueAt(document map[string]any, field TOMLPath) (any, bool) {
	var current any = document
	for _, segment := range field {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, found := mapping[segment]
		if !found {
			return nil, false
		}
		current = value
	}
	return current, true
}

func requiredMetadataValue(name, value string, collector *violationCollector, required bool) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		if required {
			collector.add("task_metadata", "task.toml", name+" is required")
		}
		return "", false
	}
	if trimmed != value {
		collector.add("task_metadata", "task.toml", name+" must not contain surrounding whitespace")
		return trimmed, false
	}
	return value, true
}

func validateGitHubURL(value string, field TOMLPath, collector *violationCollector) (githubRepository, bool) {
	repository, err := parseMetadataGitHubURL(value)
	if err != nil {
		collector.add("task_metadata", "task.toml", field.String()+" "+err.Error())
		return githubRepository{}, false
	}
	return repository, true
}

var commitIDPattern = regexp.MustCompile(`^[0-9A-Fa-f]{7,40}$`)

func validateCommitID(value string, field TOMLPath, collector *violationCollector) (string, bool) {
	if value == "" {
		collector.add("task_metadata", "task.toml", field.String()+" is required for non-0-1 tasks")
		return "", false
	}
	if value != strings.TrimSpace(value) {
		collector.add("task_metadata", "task.toml", field.String()+" must not contain surrounding whitespace")
		return "", false
	}
	if !commitIDPattern.MatchString(value) {
		collector.add("task_metadata", "task.toml", field.String()+" must be a 7-40 character hexadecimal commit")
		return "", false
	}
	return strings.ToLower(value), true
}

// githubRepository is a normalized public GitHub repository identity. It
// intentionally has no URL credentials, query, fragment, branch, or revision
// component, so it can be safely compared with an environment clone command.
type githubRepository struct {
	owner string
	repo  string
}

func (repository githubRepository) equal(other githubRepository) bool {
	return repository.owner == other.owner && repository.repo == other.repo
}

func (repository githubRepository) String() string {
	return "https://github.com/" + repository.owner + "/" + repository.repo
}

func parseMetadataGitHubURL(value string) (githubRepository, error) {
	if value == "" {
		return githubRepository{}, fmt.Errorf("is required for non-0-1 tasks")
	}
	if value != strings.TrimSpace(value) {
		return githubRepository{}, fmt.Errorf("must not contain surrounding whitespace")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return githubRepository{}, fmt.Errorf("must be an HTTPS public GitHub repository URL: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return githubRepository{}, fmt.Errorf("must be an HTTPS public GitHub repository URL")
	}
	if parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return githubRepository{}, fmt.Errorf("must not contain credentials, a port, query, or fragment")
	}
	return githubRepositoryFromPath(parsed.Path)
}

func parseGitCloneGitHubURL(value string) (githubRepository, bool) {
	value = strings.TrimSpace(strings.Trim(value, "\"'"))
	if value == "" || strings.ContainsAny(value, "${}") {
		return githubRepository{}, false
	}
	if strings.HasPrefix(strings.ToLower(value), "git@github.com:") {
		repository, err := githubRepositoryFromPath(strings.TrimPrefix(value[len("git@github.com:"):], "/"))
		return repository, err == nil
	}
	parsed, err := url.Parse(value)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return githubRepository{}, false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http", "git":
		if parsed.User != nil {
			return githubRepository{}, false
		}
	case "ssh":
		// ssh://git@github.com/owner/repo.git is a normal public clone
		// spelling. Password-bearing or non-git SSH user URLs are not an
		// auditable public-repository source for this profile.
		if parsed.User == nil || parsed.User.Username() != "git" {
			return githubRepository{}, false
		}
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return githubRepository{}, false
		}
		// These are public-repository clone syntaxes. The metadata link itself
		// remains HTTPS-only; clone syntax is normalized for equality.
	default:
		return githubRepository{}, false
	}
	repository, err := githubRepositoryFromPath(parsed.Path)
	return repository, err == nil
}

func githubRepositoryFromPath(rawPath string) (githubRepository, error) {
	parts := strings.Split(strings.Trim(rawPath, "/"), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return githubRepository{}, fmt.Errorf("must identify exactly one GitHub owner/repository")
	}
	owner := strings.ToLower(parts[0])
	repo := strings.ToLower(strings.TrimSuffix(parts[1], ".git"))
	if owner == "" || repo == "" || strings.ContainsAny(owner+repo, "?#@") {
		return githubRepository{}, fmt.Errorf("must identify exactly one GitHub owner/repository")
	}
	return githubRepository{owner: owner, repo: repo}, nil
}

func validateTestsAnalysis(root string, collector *violationCollector) {
	raw, ok := readRegularTaskFile(root, "tests_analysis.md", collector)
	if !ok {
		return
	}
	headings := markdownHeadings(string(raw))
	required := []string{
		"instruction 和 environment 已提供的信息",
		"模型的理论通过路径",
		"模型具备通过条件的依据",
	}
	positions := make([]int, len(required))
	for index := range positions {
		positions[index] = -1
	}
	counts := make([]int, len(required))
	for headingIndex, heading := range headings {
		for sectionIndex, title := range required {
			if normalizeAnalysisSectionTitle(heading.title) != title {
				continue
			}
			counts[sectionIndex]++
			if positions[sectionIndex] == -1 {
				positions[sectionIndex] = headingIndex
			}
		}
	}
	for index, title := range required {
		switch counts[index] {
		case 0:
			collector.add("tests_analysis", "tests_analysis.md", "missing required section: "+title)
		case 1:
			// Checked below for ordering and body content.
		default:
			collector.add("tests_analysis", "tests_analysis.md", "required section appears more than once: "+title)
		}
	}
	if positions[0] >= 0 && positions[1] >= 0 && positions[2] >= 0 && !(positions[0] < positions[1] && positions[1] < positions[2]) {
		collector.add("tests_analysis", "tests_analysis.md", "required sections must appear in the documented 1, 2, 3 order")
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	for index, position := range positions {
		if position < 0 || counts[index] != 1 {
			continue
		}
		if !sectionHasBody(lines, headings, position) {
			collector.add("tests_analysis", "tests_analysis.md", "required section must contain substantive content: "+required[index])
		}
	}
}

type markdownHeading struct {
	line  int
	level int
	title string
}

func markdownHeadings(raw string) []markdownHeading {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	headings := make([]markdownHeading, 0)
	for index, line := range lines {
		trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
		level := 0
		for level < len(trimmed) && trimmed[level] == '#' {
			level++
		}
		if level == 0 || level > 6 || len(trimmed) == level || !unicode.IsSpace(rune(trimmed[level])) {
			continue
		}
		title := strings.TrimSpace(trimmed[level:])
		title = strings.TrimSpace(strings.TrimRight(title, "#"))
		if title == "" {
			continue
		}
		headings = append(headings, markdownHeading{line: index, level: level, title: title})
	}
	return headings
}

var analysisSectionNumber = regexp.MustCompile(`^\s*[123]\s*[.、．:：-]\s*`)

func normalizeAnalysisSectionTitle(value string) string {
	value = analysisSectionNumber.ReplaceAllString(strings.TrimSpace(value), "")
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func sectionHasBody(lines []string, headings []markdownHeading, position int) bool {
	start := headings[position]
	end := len(lines)
	for index := position + 1; index < len(headings); index++ {
		if headings[index].level <= start.level {
			end = headings[index].line
			break
		}
	}
	for _, line := range lines[start.line+1 : end] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "---" || trimmed == "***" || trimmed == "___" {
			continue
		}
		if _, isHeading := markdownHeadingLine(trimmed); isHeading {
			continue
		}
		return true
	}
	return false
}

func markdownHeadingLine(line string) (markdownHeading, bool) {
	headings := markdownHeadings(line)
	if len(headings) != 1 {
		return markdownHeading{}, false
	}
	return headings[0], true
}

type dockerInstruction struct {
	verb string
	args string
}

type gitEvidence struct {
	repositories []githubRepository
	commits      []string
}

func (evidence *gitEvidence) add(other gitEvidence) {
	evidence.repositories = append(evidence.repositories, other.repositories...)
	evidence.commits = append(evidence.commits, other.commits...)
}

func inspectDockerfilePath(root, relativePath string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) gitEvidence {
	raw, ok := readRegularTaskFile(root, relativePath, collector)
	if !ok {
		return gitEvidence{}
	}
	return inspectDockerfile(string(raw), relativePath, protectedEnvironmentVariables, collector)
}

func inspectDockerfile(raw, filePath string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) gitEvidence {
	evidence := gitEvidence{}
	validateDockerfileProtectedEnvironmentReferences(raw, filePath, protectedEnvironmentVariables, collector)
	for _, instruction := range parseDockerInstructions(raw) {
		if containsRewardReference(instruction.args) {
			collector.add("environment_isolation", filePath, "environment must not prewrite or include verifier reward files")
		}
		switch instruction.verb {
		case "copy", "add":
			for _, source := range dockerTransferSources(instruction.args) {
				if isBroadDockerCopySource(source) {
					collector.add("environment_isolation", filePath, strings.ToUpper(instruction.verb)+" must not use a broad task-root or wildcard source: "+source)
				}
				for _, kind := range forbiddenTaskContent(source) {
					collector.add("environment_isolation", filePath, strings.ToUpper(instruction.verb)+" must not include "+kind)
				}
			}
		case "run":
			evidence.add(gitEvidenceFromTokens(dockerRunTokens(instruction.args)))
		}
	}
	return evidence
}

func parseDockerInstructions(raw string) []dockerInstruction {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	instructions := make([]dockerInstruction, 0)
	var logical string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if logical == "" && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		continued := dockerLineContinues(line)
		if continued {
			line = strings.TrimRightFunc(line, unicode.IsSpace)
			line = strings.TrimSuffix(line, "\\")
		}
		if logical == "" {
			logical = line
		} else {
			logical += " " + strings.TrimSpace(line)
		}
		if continued {
			continue
		}
		if instruction, ok := parseDockerInstruction(logical); ok {
			instructions = append(instructions, instruction)
		}
		logical = ""
	}
	if logical != "" {
		if instruction, ok := parseDockerInstruction(logical); ok {
			instructions = append(instructions, instruction)
		}
	}
	return instructions
}

func dockerLineContinues(line string) bool {
	line = strings.TrimRightFunc(line, unicode.IsSpace)
	backslashes := 0
	for index := len(line) - 1; index >= 0 && line[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func parseDockerInstruction(line string) (dockerInstruction, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return dockerInstruction{}, false
	}
	index := strings.IndexFunc(line, unicode.IsSpace)
	if index < 0 {
		return dockerInstruction{verb: strings.ToLower(line)}, true
	}
	return dockerInstruction{verb: strings.ToLower(line[:index]), args: strings.TrimSpace(line[index:])}, true
}

func dockerTransferSources(args string) []string {
	tokens := dockerArgumentTokens(args)
	for len(tokens) > 0 && strings.HasPrefix(tokens[0], "--") {
		tokens = tokens[1:]
	}
	if len(tokens) < 2 {
		return nil
	}
	return append([]string(nil), tokens[:len(tokens)-1]...)
}

func dockerArgumentTokens(args string) []string {
	trimmed := strings.TrimSpace(args)
	if strings.HasPrefix(trimmed, "[") {
		var tokens []string
		if err := json.Unmarshal([]byte(trimmed), &tokens); err == nil {
			return tokens
		}
	}
	return shellWords(trimmed)
}

func isBroadDockerCopySource(value string) bool {
	value = strings.TrimSpace(strings.Trim(value, "\"'"))
	return value == "." || value == "./" || value == "*" || value == "./*"
}

func dockerRunTokens(args string) []string {
	trimmed := strings.TrimSpace(args)
	if strings.HasPrefix(trimmed, "[") {
		var tokens []string
		if err := json.Unmarshal([]byte(trimmed), &tokens); err == nil {
			return tokens
		}
	}
	return shellWords(trimmed)
}

func shellWords(raw string) []string {
	words := make([]string, 0)
	var current strings.Builder
	var quote rune
	flush := func() {
		if current.Len() == 0 {
			return
		}
		words = append(words, current.String())
		current.Reset()
	}
	for index := 0; index < len(raw); index++ {
		character := rune(raw[index])
		if quote != 0 {
			if character == quote {
				quote = 0
				continue
			}
			if character == '\\' && quote == '"' && index+1 < len(raw) {
				index++
				current.WriteByte(raw[index])
				continue
			}
			current.WriteRune(character)
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '\\':
			if index+1 < len(raw) {
				index++
				current.WriteByte(raw[index])
			}
		case '&', '|', ';':
			flush()
			if (character == '&' || character == '|') && index+1 < len(raw) && rune(raw[index+1]) == character {
				index++
			}
			words = append(words, string(character))
		default:
			if unicode.IsSpace(character) {
				flush()
				continue
			}
			current.WriteRune(character)
		}
	}
	flush()
	return words
}

func gitEvidenceFromTokens(tokens []string) gitEvidence {
	evidence := gitEvidence{}
	for index := 0; index < len(tokens); index++ {
		if !isGitExecutable(tokens[index]) {
			continue
		}
		subcommandIndex := gitSubcommandIndex(tokens, index)
		if subcommandIndex >= len(tokens) {
			continue
		}
		switch strings.ToLower(tokens[subcommandIndex]) {
		case "clone":
			for _, argument := range gitArguments(tokens, subcommandIndex+1) {
				if repository, ok := parseGitCloneGitHubURL(argument); ok {
					evidence.repositories = append(evidence.repositories, repository)
					break
				}
			}
		case "checkout", "reset":
			for _, argument := range gitArguments(tokens, subcommandIndex+1) {
				if commitIDPattern.MatchString(argument) {
					evidence.commits = append(evidence.commits, strings.ToLower(argument))
					break
				}
			}
		}
	}
	return evidence
}

func isGitExecutable(value string) bool {
	return strings.EqualFold(path.Base(strings.ReplaceAll(value, "\\", "/")), "git")
}

func gitSubcommandIndex(tokens []string, gitIndex int) int {
	index := gitIndex + 1
	for index < len(tokens) {
		value := tokens[index]
		if value == "-C" || value == "--git-dir" || value == "--work-tree" {
			index += 2
			continue
		}
		if strings.HasPrefix(value, "--git-dir=") || strings.HasPrefix(value, "--work-tree=") {
			index++
			continue
		}
		break
	}
	return index
}

func gitArguments(tokens []string, start int) []string {
	end := start
	for end < len(tokens) && !isShellControlToken(tokens[end]) {
		end++
	}
	return tokens[start:end]
}

func isShellControlToken(value string) bool {
	return value == "&" || value == "|" || value == ";"
}

func validateRepositoryProvenance(filePath string, repository githubRepository, commit string, evidence gitEvidence, collector *violationCollector) {
	matchingRepository := false
	for _, candidate := range evidence.repositories {
		if candidate.equal(repository) {
			matchingRepository = true
			break
		}
	}
	if !matchingRepository {
		if len(evidence.repositories) == 0 {
			collector.add("repo_provenance", filePath, "non-0-1 task environment must git clone the mapped GitHub URL "+repository.String())
		} else {
			collector.add("repo_provenance", filePath, "git clone repository must match the mapped GitHub URL "+repository.String())
		}
		return
	}
	for _, candidate := range evidence.commits {
		if candidate == commit {
			return
		}
	}
	if len(evidence.commits) == 0 {
		collector.add("repo_provenance", filePath, "git clone of the mapped GitHub URL must checkout or reset the mapped commit ID "+commit)
		return
	}
	collector.add("repo_provenance", filePath, "git checkout/reset commit must match the mapped commit ID "+commit)
}

func inspectComposePath(root, relativePath string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) gitEvidence {
	raw, ok := readRegularTaskFile(root, relativePath, collector)
	if !ok {
		return gitEvidence{}
	}
	return inspectCompose(raw, relativePath, protectedEnvironmentVariables, collector)
}

func inspectCompose(raw []byte, filePath string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) gitEvidence {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		collector.add("environment_isolation", filePath, "docker-compose.yaml must be valid YAML: "+err.Error())
		return gitEvidence{}
	}
	root := yamlDocumentRoot(&document)
	if root == nil || root.Kind != yaml.MappingNode {
		collector.add("environment_isolation", filePath, "docker-compose.yaml must contain a top-level mapping")
		return gitEvidence{}
	}
	services := yamlMappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		collector.add("environment_isolation", filePath, "docker-compose.yaml must define a services mapping")
		return gitEvidence{}
	}
	mainService := yamlMappingValue(services, "main")
	if mainService == nil || mainService.Kind != yaml.MappingNode {
		collector.add("environment_isolation", filePath, "docker-compose.yaml must define a main service")
	}
	validateComposeProtectedEnvironmentReferences(root, filePath, protectedEnvironmentVariables, collector)

	evidence := gitEvidence{}
	for index := 0; index+1 < len(services.Content); index += 2 {
		serviceName := services.Content[index]
		service := services.Content[index+1]
		if service.Kind != yaml.MappingNode {
			collector.add("environment_isolation", filePath, "compose service "+serviceName.Value+" must be a mapping")
			continue
		}
		if build := yamlMappingValue(service, "build"); build != nil {
			evidence.add(inspectComposeBuild(build, filePath, protectedEnvironmentVariables, collector))
		}
		if volumes := yamlMappingValue(service, "volumes"); volumes != nil {
			validateComposeVolumes(volumes, filePath, collector)
		}
		if command := yamlMappingValue(service, "command"); command != nil {
			evidence.add(gitEvidenceFromComposeCommand(command))
			validateComposeCommandIsolation(command, filePath, collector)
		}
		if entrypoint := yamlMappingValue(service, "entrypoint"); entrypoint != nil {
			evidence.add(gitEvidenceFromComposeCommand(entrypoint))
			validateComposeCommandIsolation(entrypoint, filePath, collector)
		}
		validateComposeRewardReferences(service, filePath, collector)
	}
	return evidence
}

func yamlDocumentRoot(document *yaml.Node) *yaml.Node {
	if document == nil {
		return nil
	}
	if document.Kind == yaml.DocumentNode && len(document.Content) == 1 {
		return document.Content[0]
	}
	return document
}

func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func inspectComposeBuild(build *yaml.Node, filePath string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) gitEvidence {
	evidence := gitEvidence{}
	switch build.Kind {
	case yaml.ScalarNode:
		validateComposeBuildContext(build.Value, filePath, collector)
	case yaml.MappingNode:
		context := yamlMappingValue(build, "context")
		if context == nil || context.Kind != yaml.ScalarNode {
			collector.add("environment_isolation", filePath, "compose build.context must be a static isolated relative path")
		} else {
			validateComposeBuildContext(context.Value, filePath, collector)
		}
		if contexts := yamlMappingValue(build, "additional_contexts"); contexts != nil {
			for _, value := range yamlScalarValues(contexts) {
				validateComposeBuildContext(value, filePath, collector)
			}
		}
		if inline := yamlMappingValue(build, "dockerfile_inline"); inline != nil && inline.Kind == yaml.ScalarNode {
			evidence.add(inspectDockerfile(inline.Value, filePath, protectedEnvironmentVariables, collector))
		}
	default:
		collector.add("environment_isolation", filePath, "compose build must be a context string or mapping")
	}
	return evidence
}

func validateDockerEnvironmentDeclarations(args, filePath string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) {
	tokens := dockerArgumentTokens(args)
	if len(tokens) == 0 {
		return
	}
	for index, token := range tokens {
		if index > 0 && !strings.Contains(token, "=") {
			break
		}
		name := strings.SplitN(token, "=", 2)[0]
		if _, protected := protectedEnvironmentVariables[name]; protected {
			collector.add("environment_isolation", filePath, "Dockerfile must not declare protected deployment environment variable "+name)
		}
	}
}

func validateDockerfileProtectedEnvironmentReferences(raw, filePath string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) {
	if dockerUsesNonDefaultEscapeDirective(raw) {
		// The Dockerfile parser supports a backtick escape directive, but this
		// controlled scanner only joins the default backslash continuations.
		// Rejecting it prevents a protected name from being split across lines.
		collector.add("environment_isolation", filePath, "Dockerfile must not use a non-default escape directive while protected deployment environment validation is active")
		return
	}
	for _, instruction := range parseDockerInstructions(raw) {
		validateProtectedEnvironmentInterpolations(instruction.args, filePath, "Dockerfile", protectedEnvironmentVariables, collector)
		switch instruction.verb {
		case "arg", "env":
			validateDockerEnvironmentDeclarations(instruction.args, filePath, protectedEnvironmentVariables, collector)
		}
	}
}

func dockerUsesNonDefaultEscapeDirective(raw string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			return false
		}
		directive := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
		if !strings.HasPrefix(strings.ToLower(directive), "escape=") {
			continue
		}
		return strings.TrimSpace(directive[len("escape="):]) != "\\"
	}
	return false
}

func validateComposeProtectedEnvironmentReferencesRaw(raw []byte, filePath string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		collector.add("environment_isolation", filePath, "docker-compose.yaml must be valid YAML: "+err.Error())
		return
	}
	root := yamlDocumentRoot(&document)
	if root == nil || root.Kind != yaml.MappingNode {
		collector.add("environment_isolation", filePath, "docker-compose.yaml must contain a top-level mapping")
		return
	}
	validateComposeProtectedEnvironmentReferences(root, filePath, protectedEnvironmentVariables, collector)
}

func validateComposeProtectedEnvironmentReferences(node *yaml.Node, filePath string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) {
	if node == nil || len(protectedEnvironmentVariables) == 0 {
		return
	}
	var visit func(*yaml.Node)
	visit = func(current *yaml.Node) {
		if current == nil {
			return
		}
		if current.Kind == yaml.ScalarNode {
			validateProtectedEnvironmentInterpolations(current.Value, filePath, "Compose", protectedEnvironmentVariables, collector)
			return
		}
		if current.Kind == yaml.MappingNode {
			for index := 0; index+1 < len(current.Content); index += 2 {
				key := current.Content[index]
				value := current.Content[index+1]
				if key.Value == "environment" || key.Value == "env_file" || key.Value == "args" {
					validateComposeProtectedEnvironmentDeclarations(value, filePath, protectedEnvironmentVariables, collector)
				}
				visit(key)
				visit(value)
			}
			return
		}
		for _, child := range current.Content {
			visit(child)
		}
	}
	visit(node)
}

func validateComposeProtectedEnvironmentDeclarations(node *yaml.Node, filePath string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			name := node.Content[index].Value
			if _, protected := protectedEnvironmentVariables[name]; protected {
				collector.add("environment_isolation", filePath, "Compose must not declare protected deployment environment variable "+name)
			}
		}
	case yaml.SequenceNode:
		for _, entry := range node.Content {
			if entry.Kind != yaml.ScalarNode {
				continue
			}
			name := strings.SplitN(entry.Value, "=", 2)[0]
			if _, protected := protectedEnvironmentVariables[name]; protected {
				collector.add("environment_isolation", filePath, "Compose must not declare protected deployment environment variable "+name)
			}
		}
	case yaml.ScalarNode:
		name := strings.SplitN(node.Value, "=", 2)[0]
		if _, protected := protectedEnvironmentVariables[name]; protected {
			collector.add("environment_isolation", filePath, "Compose must not declare protected deployment environment variable "+name)
		}
	}
}

func validateProtectedEnvironmentInterpolations(value, filePath, source string, protectedEnvironmentVariables map[string]struct{}, collector *violationCollector) {
	for _, name := range protectedEnvironmentInterpolations(value, protectedEnvironmentVariables) {
		collector.add("environment_isolation", filePath, source+" must not interpolate protected deployment environment variable "+name)
	}
}

func protectedEnvironmentInterpolations(value string, protectedEnvironmentVariables map[string]struct{}) []string {
	found := make(map[string]struct{})
	for index := 0; index < len(value); index++ {
		if value[index] != '$' {
			continue
		}
		if index+1 >= len(value) {
			break
		}
		if value[index+1] == '$' {
			index++
			continue
		}
		start := index + 1
		braced := value[start] == '{'
		if braced {
			start++
			if start < len(value) && value[start] == '!' {
				start++
			}
		}
		end := start
		for end < len(value) && ((value[end] >= 'A' && value[end] <= 'Z') || (value[end] >= 'a' && value[end] <= 'z') || (value[end] >= '0' && value[end] <= '9') || value[end] == '_') {
			end++
		}
		if end == start {
			continue
		}
		name := value[start:end]
		if _, protected := protectedEnvironmentVariables[name]; protected {
			found[name] = struct{}{}
		}
		if braced {
			for end < len(value) && value[end] != '}' {
				end++
			}
			if end < len(value) {
				index = end
			}
		} else {
			index = end - 1
		}
	}
	result := make([]string, 0, len(found))
	for name := range found {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func validateComposeBuildContext(value, filePath string, collector *violationCollector) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed != value || strings.Contains(trimmed, "${") {
		collector.add("environment_isolation", filePath, "compose build.context must be a static isolated relative path")
		return
	}
	normalized := strings.ReplaceAll(trimmed, "\\", "/")
	if strings.HasPrefix(normalized, "/") || isWindowsAbsolutePath(normalized) || strings.Contains(normalized, "://") {
		collector.add("environment_isolation", filePath, "compose build.context must not use an absolute, remote, or task-root path")
		return
	}
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			collector.add("environment_isolation", filePath, "compose build.context must not escape the managed environment directory")
			return
		}
		if segment == "tests" || segment == "solution" {
			collector.add("environment_isolation", filePath, "compose build.context must not include "+segment)
			return
		}
	}
}

func isWindowsAbsolutePath(value string) bool {
	return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && value[2] == '/'
}

func validateComposeVolumes(volumes *yaml.Node, filePath string, collector *violationCollector) {
	for _, value := range yamlScalarValues(volumes) {
		for _, kind := range forbiddenTaskContent(value) {
			collector.add("environment_isolation", filePath, "compose volumes must not mount "+kind)
		}
	}
}

func validateComposeCommandIsolation(command *yaml.Node, filePath string, collector *violationCollector) {
	for _, value := range yamlScalarValues(command) {
		for _, kind := range forbiddenTaskContent(value) {
			collector.add("environment_isolation", filePath, "compose command/entrypoint must not include "+kind)
		}
	}
}

func validateComposeRewardReferences(node *yaml.Node, filePath string, collector *violationCollector) {
	for _, value := range yamlScalarValues(node) {
		if containsRewardReference(value) {
			collector.add("environment_isolation", filePath, "compose environment must not prewrite or include verifier reward files")
		}
	}
}

func yamlScalarValues(node *yaml.Node) []string {
	if node == nil {
		return nil
	}
	values := make([]string, 0)
	var visit func(*yaml.Node)
	visit = func(current *yaml.Node) {
		if current == nil {
			return
		}
		if current.Kind == yaml.ScalarNode {
			values = append(values, current.Value)
			return
		}
		for _, child := range current.Content {
			visit(child)
		}
	}
	visit(node)
	return values
}

func gitEvidenceFromComposeCommand(node *yaml.Node) gitEvidence {
	if node == nil {
		return gitEvidence{}
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return gitEvidenceFromTokens(shellWords(node.Value))
	case yaml.SequenceNode:
		tokens := make([]string, 0, len(node.Content))
		for _, value := range node.Content {
			if value.Kind != yaml.ScalarNode {
				return gitEvidence{}
			}
			tokens = append(tokens, value.Value)
		}
		return gitEvidenceFromTokens(tokens)
	default:
		return gitEvidence{}
	}
}

func forbiddenTaskContent(value string) []string {
	normalized := strings.ToLower(strings.ReplaceAll(value, "\\", "/"))
	found := make(map[string]struct{})
	for _, token := range strings.FieldsFunc(normalized, func(character rune) bool {
		return unicode.IsSpace(character) || strings.ContainsRune("\"'[]{}(),;:=", character)
	}) {
		for _, segment := range strings.Split(strings.Trim(token, "./"), "/") {
			switch segment {
			case "tests", "solution":
				found[segment] = struct{}{}
			}
		}
	}
	if containsRewardReference(normalized) {
		found["verifier reward"] = struct{}{}
	}
	result := make([]string, 0, len(found))
	for kind := range found {
		result = append(result, kind)
	}
	sort.Strings(result)
	return result
}

var rewardFileReference = regexp.MustCompile(`(^|[/'"\s=])reward\.(txt|json)($|[/'"\s,;])`)

func containsRewardReference(value string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(value, "\\", "/"))
	return strings.Contains(normalized, "/logs/verifier/reward") || rewardFileReference.MatchString(normalized)
}

func readRegularTaskFile(root, relativePath string, collector *violationCollector) ([]byte, bool) {
	filePath := filepath.Join(root, filepath.FromSlash(relativePath))
	info, err := os.Lstat(filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			collector.add("task_layout", relativePath, "inspect file: "+err.Error())
		}
		return nil, false
	}
	if !info.Mode().IsRegular() {
		return nil, false
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		collector.add("task_layout", relativePath, "read file: "+err.Error())
		return nil, false
	}
	return raw, true
}
