package stageprovider

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// HarborEvaluatorOperationContractFormat identifies the closed CodeEdge
	// Harbor 0.18 evaluator policy. It is intentionally not an upstream Harbor
	// config format: the provider owns construction of the one allowed command.
	HarborEvaluatorOperationContractFormat  = "harbor.evaluator-operation.v0.18"
	HarborEvaluatorOperationContractVersion = "1"

	// HarborEvaluatorHarborVersion is the only Harbor release accepted by this
	// adapter. A later release requires a new observed-contract and typed lock
	// revision rather than an allow-list edit that silently changes semantics.
	HarborEvaluatorHarborVersion    = "0.18.0"
	HarborEvaluatorResultABIFormat  = "harbor.run-bundle.v0.18"
	HarborEvaluatorResultABIVersion = "1"

	HarborEvaluatorTaskArtifactPort   = "task_snapshot"
	HarborEvaluatorTaskArtifactSchema = "harbor.artifact.v1"
	HarborEvaluatorTrialCount         = 4
	HarborEvaluatorConcurrentTrials   = 1
	// HarborEvaluatorMaxRetries is the approved per-logical-trial Harbor
	// internal technical retry limit. It is not a generic workflow-stage retry:
	// Harbor preserves the logical trial name and overwrites its final result.
	HarborEvaluatorMaxRetries = 3

	// HarborEvaluatorSecretValueTemplate is the only permitted secret template.
	// It is a marker consumed by a controlled provider, never an interpolation
	// language and never a place for a secret value or shell fragment.
	HarborEvaluatorSecretValueTemplate = "{{secret}}"

	// HarborEvaluatorQwenCommandID and HarborEvaluatorOpusCommandID are the
	// two local.command identities that may carry the evaluator extension.
	// They accept no caller-supplied argv; a provider derives all paths and the
	// fixed Harbor invocation from the frozen typed contract.
	HarborEvaluatorQwenCommandID = "codeedge-qwen-pass4"
	HarborEvaluatorOpusCommandID = "codeedge-opus-pass4"

	// HarborEvaluatorPythonCommandID labels the independently pinned Python
	// interpreter used by the immutable Harbor launcher shebang.
	HarborEvaluatorPythonCommandID = "harbor.python.interpreter"
	// HarborEvaluatorDockerCommandID identifies the Docker CLI that Harbor uses
	// to build and run the isolated task environment.
	HarborEvaluatorDockerCommandID = "harbor.docker.cli"
	HarborEvaluatorDockerVersion   = "29.5.2"
)

const harborEvaluatorEndpointFingerprintDomain = "harbor.stageprovider.harbor-evaluator-endpoint.v1"

// HarborEvaluatorSecretEnvTemplate binds one catalog-approved secret reference
// to one child-process environment key. Template is deliberately fixed to a
// placeholder, so neither the catalog nor the lock can serialize a credential
// or an arbitrary environment-expression language.
type HarborEvaluatorSecretEnvTemplate struct {
	Secret      workflowadapter.SecretReference `json:"secret"`
	HostEnvKey  string                          `json:"host_env_key"`
	ChildEnvKey string                          `json:"child_env_key"`
	Template    string                          `json:"template"`
}

// HarborEvaluatorScreenshotRenderer pins the implementation that deterministically
// renders redacted Harbor terminal evidence into the required screenshot artifact.
type HarborEvaluatorScreenshotRenderer struct {
	ID            string `json:"id"`
	Version       string `json:"version"`
	SchemaVersion string `json:"schema_version"`
}

// HarborEvaluatorOperationContract is the semantic, source-controlled half of
// one CodeEdge Qwen or Opus evaluator operation. Runtime-specific filesystem
// identities belong to HarborEvaluatorOperationLock below. EndpointFingerprint
// is a fingerprint of a canonical endpoint, never the endpoint value itself.
type HarborEvaluatorOperationContract struct {
	Format              string                             `json:"format"`
	Version             string                             `json:"version"`
	HarborVersion       string                             `json:"harbor_version"`
	ResultABIFormat     string                             `json:"result_abi_format"`
	ResultABIVersion    string                             `json:"result_abi_version"`
	TaskArtifactPort    string                             `json:"task_artifact_port"`
	TaskArtifactSchema  string                             `json:"task_artifact_schema"`
	AgentID             string                             `json:"agent_id"`
	AgentVersion        string                             `json:"agent_version"`
	ModelID             string                             `json:"model_id"`
	ModelVersion        string                             `json:"model_version"`
	EndpointEnvName     string                             `json:"endpoint_env_name"`
	EndpointChildEnvKey string                             `json:"endpoint_child_env_key"`
	EndpointFingerprint workflowkit.Fingerprint            `json:"endpoint_fingerprint"`
	SecretEnvTemplates  []HarborEvaluatorSecretEnvTemplate `json:"secret_env_templates"`
	Attempts            int                                `json:"attempts"`
	ConcurrentTrials    int                                `json:"concurrent_trials"`
	MaxRetries          int                                `json:"max_retries"`
	RequireTrajectory   bool                               `json:"require_trajectory"`
	ScreenshotRenderer  HarborEvaluatorScreenshotRenderer  `json:"screenshot_renderer"`
}

// Clone returns a defensively owned evaluator contract.
func (contract HarborEvaluatorOperationContract) Clone() HarborEvaluatorOperationContract {
	contract.SecretEnvTemplates = append([]HarborEvaluatorSecretEnvTemplate(nil), contract.SecretEnvTemplates...)
	return contract
}

// HarborPythonSourceTreeLock pins the Python sources imported by the Harbor
// launcher. PythonFilesSHA256 follows the observed-contract algorithm exactly:
// SHA-256 over sorted sha256sum-style records for every regular *.py file.
type HarborPythonSourceTreeLock struct {
	AbsolutePath      string                  `json:"absolute_path"`
	PythonFilesSHA256 workflowkit.Fingerprint `json:"python_files_sha256"`
}

// HarborEvaluatorOperationLock is the host-specific half of a Harbor evaluator
// operation. Contract must be byte-for-byte equivalent to the catalog contract
// after canonical ordering. Launcher duplicates the enclosing local executable
// deliberately: this makes the evaluator's direct invocation identity explicit
// and makes any mismatch fail before a process can start.
type HarborEvaluatorOperationLock struct {
	Contract            HarborEvaluatorOperationContract `json:"contract"`
	Launcher            LocalExecutableLock              `json:"launcher"`
	PythonInterpreter   LocalExecutableLock              `json:"python_interpreter"`
	PythonSourceTree    HarborPythonSourceTreeLock       `json:"python_source_tree"`
	DockerCLI           LocalExecutableLock              `json:"docker_cli"`
	HarborVersionOutput string                           `json:"harbor_version_output"`
}

// Clone returns a defensively owned evaluator lock.
func (lock HarborEvaluatorOperationLock) Clone() HarborEvaluatorOperationLock {
	lock.Contract = lock.Contract.Clone()
	return lock
}

func (contract HarborEvaluatorOperationContract) canonicalized() HarborEvaluatorOperationContract {
	canonical := contract.Clone()
	sort.Slice(canonical.SecretEnvTemplates, func(left, right int) bool {
		return harborEvaluatorSecretTemplateLess(canonical.SecretEnvTemplates[left], canonical.SecretEnvTemplates[right])
	})
	return canonical
}

func harborEvaluatorSecretTemplateLess(left, right HarborEvaluatorSecretEnvTemplate) bool {
	if left.HostEnvKey != right.HostEnvKey {
		return left.HostEnvKey < right.HostEnvKey
	}
	if left.ChildEnvKey != right.ChildEnvKey {
		return left.ChildEnvKey < right.ChildEnvKey
	}
	if left.Secret.ID != right.Secret.ID {
		return left.Secret.ID < right.Secret.ID
	}
	if left.Secret.Provider != right.Secret.Provider {
		return left.Secret.Provider < right.Secret.Provider
	}
	if left.Secret.Version != right.Secret.Version {
		return left.Secret.Version < right.Secret.Version
	}
	return left.Template < right.Template
}

func sameHarborEvaluatorContract(left, right HarborEvaluatorOperationContract) bool {
	return reflect.DeepEqual(left.canonicalized(), right.canonicalized())
}

func (contract HarborEvaluatorOperationContract) Validate() error {
	if contract.Format != HarborEvaluatorOperationContractFormat {
		return fmt.Errorf("%w: unsupported Harbor evaluator contract format %q", ErrInvalidDeploymentOperationCatalogLock, contract.Format)
	}
	if contract.Version != HarborEvaluatorOperationContractVersion {
		return fmt.Errorf("%w: unsupported Harbor evaluator contract version %q", ErrInvalidDeploymentOperationCatalogLock, contract.Version)
	}
	if contract.HarborVersion != HarborEvaluatorHarborVersion {
		return fmt.Errorf("%w: Harbor evaluator Harbor version must be %q", ErrInvalidDeploymentOperationCatalogLock, HarborEvaluatorHarborVersion)
	}
	if contract.ResultABIFormat != HarborEvaluatorResultABIFormat || contract.ResultABIVersion != HarborEvaluatorResultABIVersion {
		return fmt.Errorf("%w: Harbor evaluator result ABI must be %s@%s", ErrInvalidDeploymentOperationCatalogLock, HarborEvaluatorResultABIFormat, HarborEvaluatorResultABIVersion)
	}
	if contract.TaskArtifactPort != HarborEvaluatorTaskArtifactPort || contract.TaskArtifactSchema != HarborEvaluatorTaskArtifactSchema {
		return fmt.Errorf("%w: Harbor evaluator task artifact must be %s@%s", ErrInvalidDeploymentOperationCatalogLock, HarborEvaluatorTaskArtifactPort, HarborEvaluatorTaskArtifactSchema)
	}
	for _, field := range []struct {
		label string
		value string
	}{
		{"agent id", contract.AgentID},
		{"model id", contract.ModelID},
		{"screenshot renderer id", contract.ScreenshotRenderer.ID},
		{"screenshot renderer schema", contract.ScreenshotRenderer.SchemaVersion},
	} {
		if err := validateOperationCatalogLockString("Harbor evaluator "+field.label, field.value); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		label string
		value string
	}{
		{"agent", contract.AgentVersion},
		{"model", contract.ModelVersion},
		{"screenshot renderer", contract.ScreenshotRenderer.Version},
	} {
		if err := validateOperationCatalogLockVersion("Harbor evaluator "+field.label, field.value); err != nil {
			return err
		}
	}
	if err := validateHarborEvaluatorEnvironmentKey("endpoint environment name", contract.EndpointEnvName); err != nil {
		return err
	}
	if err := validateHarborEvaluatorEnvironmentKey("endpoint child environment key", contract.EndpointChildEnvKey); err != nil {
		return err
	}
	if err := contract.EndpointFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: Harbor evaluator endpoint fingerprint: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if contract.SecretEnvTemplates == nil || len(contract.SecretEnvTemplates) == 0 {
		return fmt.Errorf("%w: Harbor evaluator secret environment templates must be a non-empty explicit array", ErrInvalidDeploymentOperationCatalogLock)
	}
	seenSecrets := make(map[string]workflowadapter.SecretReference, len(contract.SecretEnvTemplates))
	seenHostKeys := make(map[string]struct{}, len(contract.SecretEnvTemplates))
	seenKeys := make(map[string]struct{}, len(contract.SecretEnvTemplates))
	for _, mapping := range contract.SecretEnvTemplates {
		if err := validateOperationCatalogLockSecret(mapping.Secret); err != nil {
			return fmt.Errorf("%w: Harbor evaluator secret environment template: %v", ErrInvalidDeploymentOperationCatalogLock, err)
		}
		if err := validateHarborEvaluatorEnvironmentKey("secret child environment key", mapping.ChildEnvKey); err != nil {
			return err
		}
		if err := validateHarborEvaluatorEnvironmentKey("secret host environment key", mapping.HostEnvKey); err != nil {
			return err
		}
		if mapping.Template != HarborEvaluatorSecretValueTemplate {
			return fmt.Errorf("%w: Harbor evaluator secret template must be the fixed placeholder", ErrInvalidDeploymentOperationCatalogLock)
		}
		if existing, duplicate := seenSecrets[mapping.Secret.ID]; duplicate {
			if existing != mapping.Secret {
				return fmt.Errorf("%w: Harbor evaluator secret %q has conflicting provider/version", ErrInvalidDeploymentOperationCatalogLock, mapping.Secret.ID)
			}
			return fmt.Errorf("%w: duplicate Harbor evaluator secret %q", ErrInvalidDeploymentOperationCatalogLock, mapping.Secret.ID)
		}
		if _, duplicate := seenKeys[mapping.ChildEnvKey]; duplicate {
			return fmt.Errorf("%w: duplicate Harbor evaluator child environment key", ErrInvalidDeploymentOperationCatalogLock)
		}
		if _, duplicate := seenHostKeys[mapping.HostEnvKey]; duplicate {
			return fmt.Errorf("%w: duplicate Harbor evaluator host environment key", ErrInvalidDeploymentOperationCatalogLock)
		}
		seenSecrets[mapping.Secret.ID] = mapping.Secret
		seenKeys[mapping.ChildEnvKey] = struct{}{}
		seenHostKeys[mapping.HostEnvKey] = struct{}{}
	}
	if contract.Attempts != HarborEvaluatorTrialCount || contract.ConcurrentTrials != HarborEvaluatorConcurrentTrials {
		return fmt.Errorf("%w: Harbor evaluator must use exactly %d attempts with concurrency %d", ErrInvalidDeploymentOperationCatalogLock, HarborEvaluatorTrialCount, HarborEvaluatorConcurrentTrials)
	}
	if contract.MaxRetries != HarborEvaluatorMaxRetries {
		return fmt.Errorf("%w: Harbor evaluator must use exactly %d internal retries per logical trial", ErrInvalidDeploymentOperationCatalogLock, HarborEvaluatorMaxRetries)
	}
	if !contract.RequireTrajectory {
		return fmt.Errorf("%w: Harbor evaluator must require trajectory evidence", ErrInvalidDeploymentOperationCatalogLock)
	}
	return nil
}

// Validate proves the host-specific lock contains only pinned identities and
// agrees internally with the semantic Harbor evaluator contract.
func (lock HarborEvaluatorOperationLock) Validate() error {
	if err := lock.Contract.Validate(); err != nil {
		return err
	}
	if err := validateLocalExecutableLock(lock.Launcher); err != nil {
		return err
	}
	if err := validateLocalExecutableLock(lock.PythonInterpreter); err != nil {
		return err
	}
	if lock.PythonInterpreter.CommandID != HarborEvaluatorPythonCommandID {
		return fmt.Errorf("%w: Harbor evaluator Python interpreter command id must be %q", ErrInvalidDeploymentOperationCatalogLock, HarborEvaluatorPythonCommandID)
	}
	if err := validateLocalExecutableLock(lock.DockerCLI); err != nil {
		return err
	}
	if lock.DockerCLI.CommandID != HarborEvaluatorDockerCommandID || lock.DockerCLI.Version != HarborEvaluatorDockerVersion {
		return fmt.Errorf("%w: Harbor evaluator Docker CLI must be %s@%s", ErrInvalidDeploymentOperationCatalogLock, HarborEvaluatorDockerCommandID, HarborEvaluatorDockerVersion)
	}
	if err := lock.PythonSourceTree.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(lock.HarborVersionOutput) != lock.HarborVersionOutput || lock.HarborVersionOutput != lock.Contract.HarborVersion {
		return fmt.Errorf("%w: Harbor evaluator --version output must exactly equal locked Harbor version", ErrInvalidDeploymentOperationCatalogLock)
	}
	return nil
}

// Validate proves the source-tree location and observed Python-file digest are
// safe to use as an immutable deployment lock input.
func (lock HarborPythonSourceTreeLock) Validate() error {
	if err := validateHarborEvaluatorAbsolutePath("Harbor evaluator Python source tree", lock.AbsolutePath); err != nil {
		return err
	}
	if err := lock.PythonFilesSHA256.Validate(); err != nil {
		return fmt.Errorf("%w: Harbor evaluator Python source tree SHA-256: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

// CanonicalHarborEvaluatorEndpointFingerprint returns the only endpoint
// identity stored in catalog or lock material. It never returns or embeds the
// source endpoint, so callers can compare environment configuration without
// persisting endpoint text or accidental credentials.
func CanonicalHarborEvaluatorEndpointFingerprint(value string) (workflowkit.Fingerprint, error) {
	canonical, err := canonicalHarborEvaluatorEndpoint(value)
	if err != nil {
		return "", fmt.Errorf("%w: Harbor evaluator endpoint is invalid", ErrInvalidDeploymentOperationCatalogLock)
	}
	fingerprint, err := workflowkit.FingerprintBytes(harborEvaluatorEndpointFingerprintDomain, []byte(canonical))
	if err != nil {
		return "", fmt.Errorf("%w: fingerprint Harbor evaluator endpoint", ErrInvalidDeploymentOperationCatalogLock)
	}
	return fingerprint, nil
}

func canonicalHarborEvaluatorEndpoint(value string) (string, error) {
	if value == "" || value != strings.TrimSpace(value) {
		return "", fmt.Errorf("endpoint is empty or padded")
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return "", fmt.Errorf("endpoint has whitespace or control characters")
		}
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil {
		return "", fmt.Errorf("endpoint parse failed")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("endpoint scheme is not HTTP")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", fmt.Errorf("endpoint contains noncanonical credentials, query, fragment, or path escaping")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("endpoint host is missing")
	}
	port := parsed.Port()
	if port != "" {
		for _, character := range port {
			if character < '0' || character > '9' {
				return "", fmt.Errorf("endpoint port is invalid")
			}
		}
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	cleanPath := path.Clean(parsed.Path)
	if cleanPath == "." || cleanPath == "/" {
		cleanPath = ""
	}
	if cleanPath != "" && !strings.HasPrefix(cleanPath, "/") {
		return "", fmt.Errorf("endpoint path is not absolute")
	}
	return parsed.Scheme + "://" + host + cleanPath, nil
}

func validateHarborEvaluatorEnvironmentKey(label, value string) error {
	if err := validateOperationCatalogLockString("Harbor evaluator "+label, value); err != nil {
		return err
	}
	for index, character := range value {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || character == '_' || (index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return fmt.Errorf("%w: Harbor evaluator %s is not a portable environment variable name", ErrInvalidDeploymentOperationCatalogLock, label)
	}
	return nil
}

func validateHarborEvaluatorAbsolutePath(label, value string) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
		return fmt.Errorf("%w: %s must be a clean non-root absolute path", ErrInvalidDeploymentOperationCatalogLock, label)
	}
	return validateOperationCatalogLockString(label, value)
}

func isHarborEvaluatorCommandID(commandID string) bool {
	return commandID == HarborEvaluatorQwenCommandID || commandID == HarborEvaluatorOpusCommandID
}

func validateHarborEvaluatorCatalogRegistration(contract HarborEvaluatorOperationContract, registration DeploymentOperationRegistration, catalog workflowadapter.StageCatalog) error {
	if err := contract.Validate(); err != nil {
		return err
	}
	payload, ok := registration.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
	if !ok || !isHarborEvaluatorCommandID(payload.CommandID) {
		return fmt.Errorf("%w: Harbor evaluator contract requires one approved local.command evaluator command id", ErrInvalidDeploymentOperationCatalog)
	}
	if len(payload.Arguments) != 0 {
		return fmt.Errorf("%w: Harbor evaluator local.command must not accept argv", ErrInvalidDeploymentOperationCatalog)
	}
	if !catalog.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return fmt.Errorf("%w: Harbor evaluator contract is only valid for the CodeEdge evaluator child template", ErrInvalidDeploymentOperationCatalog)
	}
	if registration.Provider.Kind != "evaluation" {
		return fmt.Errorf("%w: Harbor evaluator contract requires an evaluation provider", ErrInvalidDeploymentOperationCatalog)
	}
	var expectedBundle, expectedScreenshot string
	switch payload.CommandID {
	case HarborEvaluatorQwenCommandID:
		if registration.Stage.Key != workflowadapter.HarborRunQwen || registration.Stage.Type != workflowadapter.StageBindingHarborRunQwen {
			return fmt.Errorf("%w: Qwen Harbor evaluator command is bound to another stage", ErrInvalidDeploymentOperationCatalog)
		}
		expectedBundle = workflowadapter.CodeEdgeEvaluatorQwenBundleArtifact
		expectedScreenshot = workflowadapter.CodeEdgeEvaluatorQwenScreenshotArtifact
	case HarborEvaluatorOpusCommandID:
		if registration.Stage.Key != workflowadapter.HarborRunOpus || registration.Stage.Type != workflowadapter.StageBindingHarborRunOpus {
			return fmt.Errorf("%w: Opus Harbor evaluator command is bound to another stage", ErrInvalidDeploymentOperationCatalog)
		}
		expectedBundle = workflowadapter.CodeEdgeEvaluatorOpusBundleArtifact
		expectedScreenshot = workflowadapter.CodeEdgeEvaluatorOpusScreenshotArtifact
	}
	definition, found := catalog.Stage(registration.Stage.Key)
	if !found || !harborEvaluatorStageHasTaskInput(definition, contract.TaskArtifactPort, contract.TaskArtifactSchema) {
		return fmt.Errorf("%w: Harbor evaluator task artifact contract does not match the stage descriptor", ErrInvalidDeploymentOperationCatalog)
	}
	if !harborEvaluatorStageHasOutput(definition, expectedBundle, contract.ResultABIFormat) ||
		!harborEvaluatorStageHasOutput(definition, expectedScreenshot, contract.ScreenshotRenderer.SchemaVersion) {
		return fmt.Errorf("%w: Harbor evaluator bundle or screenshot artifact contract does not match the stage descriptor", ErrInvalidDeploymentOperationCatalog)
	}
	contractSecrets := make([]workflowadapter.SecretReference, 0, len(contract.SecretEnvTemplates))
	for _, mapping := range contract.SecretEnvTemplates {
		contractSecrets = append(contractSecrets, mapping.Secret)
	}
	if !sameDeploymentSecrets(contractSecrets, registration.Secrets) {
		return fmt.Errorf("%w: Harbor evaluator secret environment mappings do not exactly match catalog secret references", ErrInvalidDeploymentOperationCatalog)
	}
	return nil
}

func harborEvaluatorStageHasTaskInput(definition workflowadapter.StageDefinition, port, schema string) bool {
	for _, input := range definition.Inputs {
		if input.Name == port && input.SchemaVersion == schema && input.Required {
			return true
		}
	}
	return false
}

func harborEvaluatorStageHasOutput(definition workflowadapter.StageDefinition, name, schema string) bool {
	for _, output := range definition.Outputs {
		if output.Name == name && output.SchemaVersion == schema && output.Required {
			return true
		}
	}
	return false
}
