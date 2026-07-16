package workflowadapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringEnvironmentPolicyFormat and Version identify the one
	// task-specific environment contract accepted by Standard authoring. The
	// policy deliberately contains only an immutable base-image reference; it
	// is not a general container runtime configuration surface.
	StandardAuthoringEnvironmentPolicyFormat  = "harbor.standard-authoring-environment-policy.v1"
	StandardAuthoringEnvironmentPolicyVersion = "1"

	// StandardAuthoringEnvironmentPolicyArtifact is the intrinsic immutable
	// AuthoringSession input consumed by stages that need to design, review, or
	// materialize a task environment.
	StandardAuthoringEnvironmentPolicyArtifact      = "environment_policy"
	StandardAuthoringEnvironmentPolicySchemaVersion = StandardAuthoringEnvironmentPolicyFormat

	standardAuthoringDockerfileMaxBytes = 4 * 1024 * 1024
)

// StandardAuthoringEnvironmentPolicy freezes the exact base image selected
// for one AuthoringSession. BaseImage must use a fully-qualified repository
// and a sha256 digest. A tag may be included for readability, but the digest
// is the immutable identity.
type StandardAuthoringEnvironmentPolicy struct {
	Format    string `json:"format"`
	Version   string `json:"version"`
	BaseImage string `json:"base_image"`
}

// NewStandardAuthoringEnvironmentPolicy validates a caller-selected immutable
// base image and returns the complete canonical policy value to freeze into an
// AuthoringSession manifest.
func NewStandardAuthoringEnvironmentPolicy(baseImage string) (StandardAuthoringEnvironmentPolicy, error) {
	policy := StandardAuthoringEnvironmentPolicy{
		Format: StandardAuthoringEnvironmentPolicyFormat, Version: StandardAuthoringEnvironmentPolicyVersion, BaseImage: baseImage,
	}
	if err := policy.Validate(); err != nil {
		return StandardAuthoringEnvironmentPolicy{}, err
	}
	return policy, nil
}

// Validate proves that the policy has the one supported schema and a
// canonical immutable image reference. It deliberately rejects image aliases,
// unqualified Docker Hub names, variables, and mutable tags without a digest.
func (policy StandardAuthoringEnvironmentPolicy) Validate() error {
	if policy.Format != StandardAuthoringEnvironmentPolicyFormat {
		return fmt.Errorf("%w: unsupported Standard authoring environment policy format %q", errInvalidCatalog, policy.Format)
	}
	if policy.Version != StandardAuthoringEnvironmentPolicyVersion {
		return fmt.Errorf("%w: unsupported Standard authoring environment policy version %q", errInvalidCatalog, policy.Version)
	}
	if err := validateStandardAuthoringImmutableImageReference(policy.BaseImage); err != nil {
		return fmt.Errorf("%w: Standard authoring environment policy base image: %v", errInvalidCatalog, err)
	}
	return nil
}

// CanonicalJSON returns the stable immutable policy bytes stored in a session
// manifest and exposed through the environment_policy artifact input.
func (policy StandardAuthoringEnvironmentPolicy) CanonicalJSON() ([]byte, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("%w: encode Standard authoring environment policy: %v", errInvalidCatalog, err)
	}
	return encoded, nil
}

// ContentDigest returns the object-content identity used by frozen artifact
// bindings. It is intentionally a plain SHA-256 of CanonicalJSON so it can be
// compared directly with workflowkit ArtifactBinding.ContentDigest.
func (policy StandardAuthoringEnvironmentPolicy) ContentDigest() (workflowkit.Fingerprint, error) {
	canonical, err := policy.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.SHA256Fingerprint(canonical), nil
}

// Fingerprint is retained as the compact policy identity used by launch and
// session code. It is equivalent to ContentDigest rather than domain-separated
// because policy bytes become a frozen artifact input.
func (policy StandardAuthoringEnvironmentPolicy) Fingerprint() (workflowkit.Fingerprint, error) {
	return policy.ContentDigest()
}

// ParseStandardAuthoringEnvironmentPolicyJSON strictly parses one policy
// document. It rejects duplicate/unknown keys and trailing JSON, while callers
// use CanonicalJSON to obtain the stable stored representation.
func ParseStandardAuthoringEnvironmentPolicyJSON(raw []byte) (StandardAuthoringEnvironmentPolicy, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return StandardAuthoringEnvironmentPolicy{}, fmt.Errorf("decode Standard authoring environment policy: %w", err)
	}
	var document standardAuthoringEnvironmentPolicyDocument
	if err := decodeExecutionSpecJSON(raw, &document); err != nil {
		return StandardAuthoringEnvironmentPolicy{}, fmt.Errorf("decode Standard authoring environment policy: %w", err)
	}
	policy := StandardAuthoringEnvironmentPolicy(document)
	if err := policy.Validate(); err != nil {
		return StandardAuthoringEnvironmentPolicy{}, err
	}
	return policy, nil
}

// The alias prevents the strict parser from recursively invoking
// StandardAuthoringEnvironmentPolicy.UnmarshalJSON.
type standardAuthoringEnvironmentPolicyDocument StandardAuthoringEnvironmentPolicy

// UnmarshalJSON keeps direct decoding at the same strict boundary as the
// named parser, so no caller can bypass policy validation through json.Unmarshal.
func (policy *StandardAuthoringEnvironmentPolicy) UnmarshalJSON(raw []byte) error {
	if policy == nil {
		return fmt.Errorf("%w: nil Standard authoring environment policy", errInvalidCatalog)
	}
	parsed, err := ParseStandardAuthoringEnvironmentPolicyJSON(raw)
	if err != nil {
		return err
	}
	*policy = parsed
	return nil
}

// ValidateDockerfileBaseImage verifies that every Dockerfile image source is
// pinned to the policy's exact immutable base. Multi-stage Dockerfiles may use
// only that image in FROM, and COPY/ADD/RUN may refer only to an already
// declared local build stage. This closes BuildKit's image-bearing --from and
// --mount=from forms as well as ordinary FROM substitutions.
func ValidateDockerfileBaseImage(dockerfile []byte, policy StandardAuthoringEnvironmentPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if len(dockerfile) == 0 || len(dockerfile) > standardAuthoringDockerfileMaxBytes {
		return fmt.Errorf("Standard authoring Dockerfile has an invalid size")
	}
	// Docker removes a leading UTF-8 BOM before processing parser directives.
	// Normalize it here too so it cannot hide a directive from this validator.
	dockerfile = bytes.TrimPrefix(dockerfile, []byte{0xef, 0xbb, 0xbf})

	scanner := bufio.NewScanner(bytes.NewReader(dockerfile))
	scanner.Buffer(make([]byte, 0, 4096), standardAuthoringDockerfileMaxBytes+1)
	stages := dockerfileBuildStages{aliases: make(map[string]int), current: -1}
	foundFrom := false
	var logicalLine strings.Builder
	continued := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if standardAuthoringDockerfileParserDirective(line) {
			return fmt.Errorf("Standard authoring Dockerfile may not select a parser directive")
		}
		if logicalLine.Len() == 0 && (line == "" || strings.HasPrefix(line, "#")) {
			continue
		}
		if logicalLine.Len() != 0 && strings.HasPrefix(line, "#") {
			return fmt.Errorf("Standard authoring Dockerfile has an unsupported continued comment")
		}
		continues := strings.HasSuffix(line, "\\")
		if continues {
			line = strings.TrimSuffix(line, "\\")
			if !(strings.HasSuffix(line, " ") || strings.HasSuffix(line, "\t")) {
				return fmt.Errorf("Standard authoring Dockerfile continuation must follow whitespace")
			}
			line = strings.TrimSpace(line)
		}
		if line == "" {
			return fmt.Errorf("Standard authoring Dockerfile has an invalid continued instruction")
		}
		if logicalLine.Len() != 0 {
			logicalLine.WriteByte(' ')
		}
		logicalLine.WriteString(line)
		if continues {
			continued = true
			continue
		}
		instruction := logicalLine.String()
		if err := validateStandardAuthoringDockerfileInstruction(instruction, continued, policy, &stages); err != nil {
			return err
		}
		instructionFields := strings.Fields(instruction)
		if len(instructionFields) != 0 && strings.EqualFold(instructionFields[0], "FROM") {
			foundFrom = true
		}
		logicalLine.Reset()
		continued = false
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Standard authoring Dockerfile")
	}
	if logicalLine.Len() != 0 {
		return fmt.Errorf("Standard authoring Dockerfile has an unterminated continued instruction")
	}
	if !foundFrom {
		return fmt.Errorf("Standard authoring Dockerfile has no frozen base image")
	}
	return nil
}

type dockerfileBuildStages struct {
	aliases map[string]int
	count   int
	current int
}

func validateStandardAuthoringDockerfileInstruction(instruction string, continued bool, policy StandardAuthoringEnvironmentPolicy, stages *dockerfileBuildStages) error {
	fields := strings.Fields(instruction)
	if len(fields) == 0 {
		return nil
	}
	switch strings.ToUpper(fields[0]) {
	case "FROM":
		if continued || len(fields) < 2 || strings.HasPrefix(fields[1], "--") || fields[1] != policy.BaseImage {
			return fmt.Errorf("Standard authoring Dockerfile FROM does not match the frozen base image")
		}
		switch len(fields) {
		case 2:
		case 4:
			alias, valid := canonicalDockerBuildStageAlias(fields[3])
			if !strings.EqualFold(fields[2], "AS") || !valid {
				return fmt.Errorf("Standard authoring Dockerfile FROM does not match the frozen base image")
			}
			if _, exists := stages.aliases[alias]; exists {
				return fmt.Errorf("Standard authoring Dockerfile repeats build stage alias")
			}
			stages.aliases[alias] = stages.count
		default:
			return fmt.Errorf("Standard authoring Dockerfile FROM does not match the frozen base image")
		}
		stages.current = stages.count
		stages.count++
		return nil
	case "COPY", "ADD":
		if err := rejectEscapedStandardAuthoringDockerfileBuilderFlags(fields); err != nil {
			return err
		}
		return validateStandardAuthoringCopyOrAddSources(fields, stages)
	case "RUN":
		if err := rejectEscapedStandardAuthoringDockerfileBuilderFlags(fields); err != nil {
			return err
		}
		return validateStandardAuthoringRunMountSources(fields, stages)
	case "ONBUILD":
		return fmt.Errorf("Standard authoring Dockerfile may not declare ONBUILD instructions")
	default:
		return nil
	}
}

// standardAuthoringDockerfileParserDirective recognizes every parser directive
// accepted by Docker's built-in parser, plus BuildKit's alternate // syntax
// form. The policy has one fixed parser contract, so any directive is rejected.
func standardAuthoringDockerfileParserDirective(line string) bool {
	var comment string
	switch {
	case strings.HasPrefix(line, "#"):
		comment = strings.TrimSpace(strings.TrimPrefix(line, "#"))
	case strings.HasPrefix(line, "//"):
		comment = strings.TrimSpace(strings.TrimPrefix(line, "//"))
	default:
		return false
	}
	name, _, found := strings.Cut(comment, "=")
	if !found {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "syntax", "escape", "check":
		return true
	default:
		return false
	}
}

// Docker's builder-flag lexer permits quotes and backslash escapes inside a
// flag name. This validator intentionally accepts only unescaped flags, which
// prevents a static scan from diverging from Docker on disguised --from or
// --mount forms (for example, --fr\\om or --mo"unt).
func rejectEscapedStandardAuthoringDockerfileBuilderFlags(fields []string) error {
	for index := 1; index < len(fields) && strings.HasPrefix(fields[index], "--"); index++ {
		if strings.ContainsAny(fields[index], "\\\\\"'") {
			return fmt.Errorf("Standard authoring Dockerfile may not escape builder flags")
		}
	}
	return nil
}

func validateStandardAuthoringCopyOrAddSources(fields []string, stages *dockerfileBuildStages) error {
	for index := 1; index < len(fields) && strings.HasPrefix(fields[index], "--"); index++ {
		flag := fields[index]
		if strings.HasPrefix(strings.ToLower(flag), "--from=") {
			if err := validateStandardAuthoringLocalStageReference(flag[len("--from="):], stages, true); err != nil {
				return fmt.Errorf("Standard authoring Dockerfile COPY/ADD --from must name an earlier local stage: %w", err)
			}
			continue
		}
		if strings.EqualFold(flag, "--from") {
			if index+1 >= len(fields) {
				return fmt.Errorf("Standard authoring Dockerfile COPY/ADD --from is incomplete")
			}
			if err := validateStandardAuthoringLocalStageReference(fields[index+1], stages, true); err != nil {
				return fmt.Errorf("Standard authoring Dockerfile COPY/ADD --from must name an earlier local stage: %w", err)
			}
			index++
		}
	}
	return nil
}

func validateStandardAuthoringRunMountSources(fields []string, stages *dockerfileBuildStages) error {
	for index := 1; index < len(fields) && strings.HasPrefix(fields[index], "--"); index++ {
		flag := fields[index]
		value := ""
		switch {
		case strings.HasPrefix(strings.ToLower(flag), "--mount="):
			value = flag[len("--mount="):]
		case strings.EqualFold(flag, "--mount"):
			if index+1 >= len(fields) {
				return fmt.Errorf("Standard authoring Dockerfile RUN --mount is incomplete")
			}
			value = fields[index+1]
			index++
		default:
			continue
		}
		for _, option := range strings.Split(value, ",") {
			key, source, found := strings.Cut(strings.TrimSpace(option), "=")
			if !found || !strings.EqualFold(strings.TrimSpace(key), "from") {
				continue
			}
			if err := validateStandardAuthoringLocalStageReference(strings.TrimSpace(source), stages, false); err != nil {
				return fmt.Errorf("Standard authoring Dockerfile RUN --mount=from must name an earlier local stage: %w", err)
			}
		}
	}
	return nil
}

func validateStandardAuthoringLocalStageReference(reference string, stages *dockerfileBuildStages, allowNumeric bool) error {
	if stages == nil || stages.current < 0 || reference == "" || strings.ContainsAny(reference, "${}\\\"'`") {
		return fmt.Errorf("invalid local build stage reference")
	}
	if index, found := stages.aliases[strings.ToLower(reference)]; found && index < stages.current {
		return nil
	}
	if !allowNumeric || !decimal(reference) {
		return fmt.Errorf("not an earlier local build stage")
	}
	index, err := strconv.Atoi(reference)
	if err != nil || index < 0 || index >= stages.current {
		return fmt.Errorf("not an earlier local build stage")
	}
	return nil
}

// ValidateDockerfile is the policy-oriented form used by code that has
// already parsed the immutable session input.
func (policy StandardAuthoringEnvironmentPolicy) ValidateDockerfile(dockerfile []byte) error {
	return ValidateDockerfileBaseImage(dockerfile, policy)
}

func validateStandardAuthoringImmutableImageReference(reference string) error {
	if reference == "" || reference != strings.TrimSpace(reference) {
		return fmt.Errorf("must be a canonical immutable image reference")
	}
	if strings.ContainsFunc(reference, unicode.IsSpace) || strings.ContainsAny(reference, "${}\\\"'`") {
		return fmt.Errorf("must be a canonical immutable image reference")
	}
	if strings.Count(reference, "@") != 1 {
		return fmt.Errorf("must contain exactly one digest separator")
	}
	name, digest, found := strings.Cut(reference, "@")
	if !found || !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 || !lowerHex(digest[len("sha256:"):]) {
		return fmt.Errorf("must use a lowercase sha256 digest")
	}
	if err := validateStandardAuthoringImageName(name); err != nil {
		return err
	}
	return nil
}

func validateStandardAuthoringImageName(name string) error {
	parts := strings.Split(name, "/")
	if len(parts) < 2 || strings.Contains(name, "//") {
		return fmt.Errorf("must use a fully-qualified repository")
	}
	if !isStandardAuthoringRegistry(parts[0]) {
		return fmt.Errorf("must use an explicit registry host")
	}
	last := parts[len(parts)-1]
	if separator := strings.LastIndexByte(last, ':'); separator >= 0 {
		tag := last[separator+1:]
		if !isStandardAuthoringImageTag(tag) {
			return fmt.Errorf("has an invalid image tag")
		}
		last = last[:separator]
		parts[len(parts)-1] = last
	}
	for _, part := range parts[1:] {
		if !isStandardAuthoringRepositoryComponent(part) {
			return fmt.Errorf("has an invalid repository path")
		}
	}
	return nil
}

func isStandardAuthoringRegistry(value string) bool {
	if value == "localhost" {
		return true
	}
	host := value
	if separator := strings.LastIndexByte(value, ':'); separator >= 0 {
		host = value[:separator]
		port := value[separator+1:]
		if host == "" || port == "" || !decimal(port) {
			return false
		}
	}
	if !strings.Contains(host, ".") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || !isStandardAuthoringRegistryLabel(label) {
			return false
		}
	}
	return true
}

func isStandardAuthoringRegistryLabel(value string) bool {
	if value == "" || !asciiLowerDigit(value[0]) || !asciiLowerDigit(value[len(value)-1]) {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-') {
			return false
		}
	}
	return true
}

func isStandardAuthoringRepositoryComponent(value string) bool {
	if value == "" || !asciiLowerDigit(value[0]) || !asciiLowerDigit(value[len(value)-1]) {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func isStandardAuthoringImageTag(value string) bool {
	if len(value) == 0 || len(value) > 128 || !asciiLowerDigitOrUnderscore(value[0]) {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' || character == '.' || character == '-') {
			return false
		}
	}
	return true
}

func canonicalDockerBuildStageAlias(value string) (string, bool) {
	canonical := strings.ToLower(value)
	if len(canonical) == 0 || canonical[0] < 'a' || canonical[0] > 'z' {
		return "", false
	}
	for _, character := range canonical[1:] {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-') {
			return "", false
		}
	}
	return canonical, true
}

func asciiLowerDigit(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= '0' && value <= '9')
}

func asciiLowerDigitOrUnderscore(value byte) bool {
	return asciiLowerDigit(value) || value == '_'
}

func lowerHex(value string) bool {
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func decimal(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
