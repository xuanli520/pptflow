// Package evaluator contains the controlled local implementation of the two
// CodeEdge Harbor evaluator operations. It deliberately depends on the Harbor
// domain adapter rather than workflowkit alone: generic workflowkit must not
// learn about Harbor CLI layouts, model credentials, or pass@4 evidence.
package evaluator

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	maxTaskSnapshotZIPBytes   = 32 << 20
	maxTaskSnapshotFileBytes  = 16 << 20
	maxTaskSnapshotTotalBytes = 48 << 20
	maxTranscriptBytes        = 24 << 10

	// forbiddenLocalEvaluatorCredentialEnvironment is deliberately forbidden
	// from this local-only evaluator. Model credentials have their separately
	// frozen mappings; this unrelated credential must never become a
	// model-process input or an env-file entry.
	forbiddenLocalEvaluatorCredentialEnvironment = "HARBOR_API_KEY"
)

// Command describes a direct process invocation. The runner does not expose a
// shell string, which prevents evaluator input from becoming shell syntax.
type Command struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

// CommandResult contains bounded captured output. Callers must redact it
// before it reaches any durable local evidence.
type CommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// CommandRunner makes the only process boundary testable. Production uses
// DirectCommandRunner; tests supply a deterministic fake without models,
// Docker, or a Harbor installation.
type CommandRunner interface {
	Run(context.Context, Command) (CommandResult, error)
}

// DirectCommandRunner invokes the supplied executable and direct argv.
type DirectCommandRunner struct{}

func (DirectCommandRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	if ctx == nil {
		return CommandResult{}, errors.New("command context is required")
	}
	process := exec.CommandContext(ctx, command.Path, command.Args...)
	process.Dir = command.Dir
	process.Env = append([]string(nil), command.Env...)
	stdout := &limitedBuffer{limit: maxTranscriptBytes}
	stderr := &limitedBuffer{limit: maxTranscriptBytes}
	process.Stdout = stdout
	process.Stderr = stderr
	err := process.Run()
	result := CommandResult{ExitCode: 0, Stdout: append([]byte(nil), stdout.Bytes()...), Stderr: append([]byte(nil), stderr.Bytes()...)}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

type limitedBuffer struct {
	limit int
	bytes.Buffer
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	if buffer == nil || buffer.limit < 0 || buffer.Len()+len(value) > buffer.limit {
		return 0, errors.New("Harbor evaluator command output exceeds limit")
	}
	return buffer.Buffer.Write(value)
}

// HarborEvaluatorLocalCommandExecutor is an exact implementation of the two
// catalog-bound evaluator command IDs. It does not accept arbitrary argv,
// host task paths, models, endpoints, or secret references from callers.
type HarborEvaluatorLocalCommandExecutor struct {
	root                          string
	invocations                   map[string]stageprovider.HarborEvaluatorInvocation
	protectedEnvironmentVariables []string
	lookupEnv                     func(string) (string, bool)
	runner                        CommandRunner
}

// HarborEvaluatorLocalCommandExecutorConfig supplies only deployment-owned
// facts. Invocations must be created from the exact approved lock during
// composition; environment values are deliberately looked up only at launch.
type HarborEvaluatorLocalCommandExecutorConfig struct {
	WorkspaceRoot string
	Invocations   []stageprovider.HarborEvaluatorInvocation
	LookupEnv     func(string) (string, bool)
	Runner        CommandRunner
}

// NewHarborEvaluatorLocalCommandExecutor builds a closed command registry.
func NewHarborEvaluatorLocalCommandExecutor(config HarborEvaluatorLocalCommandExecutorConfig) (*HarborEvaluatorLocalCommandExecutor, error) {
	root, err := controlledWorkspaceRoot(config.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	lookup := config.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	runner := config.Runner
	if runner == nil {
		runner = DirectCommandRunner{}
	}
	executor := &HarborEvaluatorLocalCommandExecutor{root: root, invocations: make(map[string]stageprovider.HarborEvaluatorInvocation, len(config.Invocations)), lookupEnv: lookup, runner: runner}
	for _, invocation := range config.Invocations {
		if err := validateInvocation(invocation); err != nil {
			return nil, err
		}
		if _, exists := executor.invocations[invocation.CommandID]; exists {
			return nil, fmt.Errorf("duplicate Harbor evaluator invocation %q", invocation.CommandID)
		}
		executor.invocations[invocation.CommandID] = invocation.Clone()
	}
	if len(executor.invocations) != 2 {
		return nil, fmt.Errorf("Harbor evaluator executor requires exactly Qwen and Opus invocations")
	}
	for _, commandID := range []string{stageprovider.HarborEvaluatorQwenCommandID, stageprovider.HarborEvaluatorOpusCommandID} {
		if _, found := executor.invocations[commandID]; !found {
			return nil, fmt.Errorf("Harbor evaluator executor is missing invocation %q", commandID)
		}
	}
	executor.protectedEnvironmentVariables = evaluatorProtectedEnvironmentVariables(executor.invocations)
	if len(executor.protectedEnvironmentVariables) == 0 {
		return nil, errors.New("Harbor evaluator executor has no protected deployment environment variables")
	}
	return executor, nil
}

// ExecuteLocalCommand fulfills stageprovider.LocalCommandOperationExecutor.
func (executor *HarborEvaluatorLocalCommandExecutor) ExecuteLocalCommand(ctx context.Context, invocation stageprovider.StageOperationInvocation, payload workflowadapter.LocalCommandOperationPayload) (workflowkit.StageExecutionResult, error) {
	if executor == nil || executor.runner == nil {
		return workflowkit.StageExecutionResult{}, errors.New("Harbor evaluator executor is not configured")
	}
	if len(payload.Arguments) != 0 {
		return workflowkit.StageExecutionResult{}, errors.New("Harbor evaluator does not accept caller-provided argv")
	}
	config, found := executor.invocations[payload.CommandID]
	if !found || config.CommandID != payload.CommandID {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("unapproved Harbor evaluator command %q", payload.CommandID)
	}
	if err := evaluatorStageMatchesCommand(invocation.Request.Stage.Key, payload.CommandID); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	taskInput, err := evaluatorTaskInput(invocation.Request, config)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	snapshot, err := invocation.Request.ReadInput(ctx, taskInput)
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("read frozen Harbor task snapshot: %w", err)
	}
	if len(snapshot) == 0 || len(snapshot) > maxTaskSnapshotZIPBytes {
		return workflowkit.StageExecutionResult{}, errors.New("frozen Harbor task snapshot ZIP has invalid size")
	}

	workspace, err := executor.workspace(invocation.Request)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	taskRoot := filepath.Join(workspace, "task")
	if err := extractManagedTaskSnapshot(ctx, snapshot, taskRoot, invocation.Request.Execution.Subject.Digest); err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("materialize frozen Harbor task snapshot: %w", err)
	}
	// Harbor's --env-file becomes part of Harbor's process environment, which
	// Compose can otherwise interpolate into a task-owned container. Validate
	// names only before resolving any credential or writing the env file.
	if err := codeedge.ValidateProtectedEnvironmentReferences(taskRoot, executor.protectedEnvironmentVariables); err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("preflight frozen Harbor task environment: %w", err)
	}
	jobsRoot := filepath.Join(workspace, "jobs")
	if err := os.Mkdir(jobsRoot, 0o700); err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("create Harbor evaluator jobs directory: %w", err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "harbor-home"), 0o700); err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("create isolated Harbor evaluator home: %w", err)
	}
	jobName := evaluatorJobName(invocation.Request, payload.CommandID)
	if err := validateNoExistingJob(jobsRoot, jobName); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}

	envFile, sensitiveValues, err := executor.writeApprovedEnvFile(workspace, config)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	defer os.Remove(envFile)
	command := evaluatorCommand(config, taskRoot, jobsRoot, jobName, envFile)
	result, runErr := executor.runner.Run(ctx, command)
	rawOutputDigest, digestErr := evaluatorRawOutputFingerprint(result, runErr)
	if digestErr != nil {
		return workflowkit.StageExecutionResult{}, digestErr
	}
	transcript := evaluatorTranscript(command, result, runErr, "", rawOutputDigest, sensitiveValues)
	if err := writeRedactedTranscript(workspace, transcript); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if runErr != nil {
		return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailureProcess}, ErrorText: "Harbor evaluator process could not start"}, nil
	}
	if result.ExitCode != 0 {
		return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailureProcess}, ErrorText: "Harbor evaluator command failed"}, nil
	}

	jobRoot := filepath.Join(jobsRoot, jobName)
	if err := writeHarborOutputProvenance(jobRoot, config.CommandID, rawOutputDigest); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	bundle, err := codeedge.CaptureHarborRunBundleV018(codeedge.HarborRunBundleCaptureRequest{
		JobDirectory:         jobRoot,
		MaterializedTaskRoot: taskRoot, FrozenTaskSnapshotDigest: invocation.Request.Execution.Subject.Digest,
		HarborCLI: codeedge.HarborCLIIdentity{CommandID: config.CommandID, Version: config.HarborVersion, ContentFingerprint: config.LauncherContentSHA256},
	})
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("capture trusted Harbor evaluator bundle: %w", err)
	}
	if err := verifyFrozenEvaluatorIdentity(bundle, config); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	bundleBytes, err := bundle.CanonicalJSON()
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	bundleDigest := workflowkit.SHA256Fingerprint(bundleBytes)
	transcript = evaluatorTranscript(command, result, nil, string(bundleDigest), rawOutputDigest, sensitiveValues)
	if err := writeRedactedTranscript(workspace, transcript); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	transcriptDigest := workflowkit.SHA256Fingerprint([]byte(transcript))
	screenshot, err := stageprovider.RenderHarborEvaluatorTerminalPNG(transcript, transcriptDigest)
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("render Harbor evaluator terminal evidence: %w", err)
	}
	bundleName, screenshotName := evaluatorOutputNames(payload.CommandID)
	return workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass},
		Artifacts: []workflowkit.StageArtifact{
			{Name: bundleName, SchemaVersion: codeedge.HarborRunBundleV018Format, Content: bundleBytes},
			{Name: screenshotName, SchemaVersion: config.ScreenshotRenderer.SchemaVersion, Content: screenshot},
		},
	}, nil
}

// ObserveCompletedHarborEvaluator is the provider-specific, read-only side
// effect reconciliation path. It never invokes Harbor, Docker, a model, a
// remote service, or a resume command. When a previous worker died after the
// fenced invocation, it can prove completion only from the deterministic local
// job directory and the safe output provenance written before bundle capture.
//
// observed=false means the directory does not yet contain a complete result;
// callers must leave the Run/Trial in_doubt. A non-nil error means the local
// evidence is malformed or unsafe and must likewise remain fail-closed.
func (executor *HarborEvaluatorLocalCommandExecutor) ObserveCompletedHarborEvaluator(ctx context.Context, invocation stageprovider.StageOperationInvocation, payload workflowadapter.LocalCommandOperationPayload) (result workflowkit.StageExecutionResult, observed bool, err error) {
	if executor == nil {
		return workflowkit.StageExecutionResult{}, false, errors.New("Harbor evaluator executor is not configured")
	}
	if len(payload.Arguments) != 0 {
		return workflowkit.StageExecutionResult{}, false, errors.New("Harbor evaluator does not accept caller-provided argv")
	}
	config, found := executor.invocations[payload.CommandID]
	if !found {
		return workflowkit.StageExecutionResult{}, false, fmt.Errorf("unapproved Harbor evaluator command %q", payload.CommandID)
	}
	if err := evaluatorStageMatchesCommand(invocation.Request.Stage.Key, payload.CommandID); err != nil {
		return workflowkit.StageExecutionResult{}, false, err
	}
	workspace, found, err := executor.existingWorkspace(invocation.Request)
	if err != nil || !found {
		return workflowkit.StageExecutionResult{}, false, err
	}
	jobName := evaluatorJobName(invocation.Request, payload.CommandID)
	jobRoot := filepath.Join(workspace, "jobs", jobName)
	info, statErr := os.Lstat(jobRoot)
	if errors.Is(statErr, os.ErrNotExist) {
		return workflowkit.StageExecutionResult{}, false, nil
	}
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return workflowkit.StageExecutionResult{}, false, errors.New("controlled Harbor evaluator job directory is invalid")
	}
	provenance, err := readHarborOutputProvenance(jobRoot, payload.CommandID)
	if errors.Is(err, os.ErrNotExist) {
		return workflowkit.StageExecutionResult{}, false, nil
	}
	if err != nil {
		return workflowkit.StageExecutionResult{}, false, err
	}
	taskRoot := filepath.Join(workspace, "task")
	bundle, err := codeedge.CaptureHarborRunBundleV018(codeedge.HarborRunBundleCaptureRequest{
		JobDirectory: jobRoot, MaterializedTaskRoot: taskRoot, FrozenTaskSnapshotDigest: invocation.Request.Execution.Subject.Digest,
		HarborCLI: codeedge.HarborCLIIdentity{CommandID: config.CommandID, Version: config.HarborVersion, ContentFingerprint: config.LauncherContentSHA256},
	})
	if err != nil {
		return workflowkit.StageExecutionResult{}, false, fmt.Errorf("capture reconciled Harbor evaluator bundle: %w", err)
	}
	if err := verifyFrozenEvaluatorIdentity(bundle, config); err != nil {
		return workflowkit.StageExecutionResult{}, false, err
	}
	bundleBytes, err := bundle.CanonicalJSON()
	if err != nil {
		return workflowkit.StageExecutionResult{}, false, err
	}
	bundleDigest := workflowkit.SHA256Fingerprint(bundleBytes)
	transcript := canonicalTerminalTranscript(strings.Join([]string{
		"Harbor evaluator local result observation",
		"command_id=" + payload.CommandID,
		"raw_output_sha256=" + string(provenance.RawOutputSHA256),
		"bundle_sha256=" + string(bundleDigest),
		"status=completed from local immutable job evidence",
	}, "\n"))
	screenshot, err := stageprovider.RenderHarborEvaluatorTerminalPNG(transcript, workflowkit.SHA256Fingerprint([]byte(transcript)))
	if err != nil {
		return workflowkit.StageExecutionResult{}, false, err
	}
	bundleName, screenshotName := evaluatorOutputNames(payload.CommandID)
	return workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass},
		Artifacts: []workflowkit.StageArtifact{
			{Name: bundleName, SchemaVersion: codeedge.HarborRunBundleV018Format, Content: bundleBytes},
			{Name: screenshotName, SchemaVersion: config.ScreenshotRenderer.SchemaVersion, Content: screenshot},
		},
	}, true, nil
}

func controlledWorkspaceRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("Harbor evaluator workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return "", fmt.Errorf("create Harbor evaluator workspace root: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Harbor evaluator workspace root is not a real directory")
	}
	return filepath.Clean(abs), nil
}

func validateInvocation(invocation stageprovider.HarborEvaluatorInvocation) error {
	if invocation.CommandID != stageprovider.HarborEvaluatorQwenCommandID && invocation.CommandID != stageprovider.HarborEvaluatorOpusCommandID {
		return fmt.Errorf("unsupported Harbor evaluator command %q", invocation.CommandID)
	}
	if invocation.LauncherPath == "" || invocation.HarborVersion != stageprovider.HarborEvaluatorHarborVersion || invocation.AgentID != "claude-code" || invocation.AgentVersion == "" || invocation.ModelID == "" ||
		invocation.TaskArtifactPort != stageprovider.HarborEvaluatorTaskArtifactPort || invocation.TaskArtifactSchema != stageprovider.HarborEvaluatorTaskArtifactSchema ||
		invocation.Attempts != stageprovider.HarborEvaluatorTrialCount || invocation.ConcurrentTrials != stageprovider.HarborEvaluatorConcurrentTrials || invocation.MaxRetries != stageprovider.HarborEvaluatorMaxRetries || !invocation.RequireTrajectory ||
		invocation.EndpointEnvName == "" || invocation.EndpointChildEnvKey == "" || len(invocation.SecretEnvTemplates) == 0 {
		return errors.New("incomplete Harbor evaluator invocation")
	}
	if err := invocation.LauncherContentSHA256.Validate(); err != nil {
		return fmt.Errorf("Harbor evaluator launcher fingerprint: %w", err)
	}
	if err := invocation.EndpointFingerprint.Validate(); err != nil {
		return fmt.Errorf("Harbor evaluator endpoint fingerprint: %w", err)
	}
	for _, name := range append([]string{invocation.EndpointEnvName, invocation.EndpointChildEnvKey}, evaluatorInvocationEnvironmentNames(invocation.SecretEnvTemplates)...) {
		if strings.EqualFold(strings.TrimSpace(name), forbiddenLocalEvaluatorCredentialEnvironment) {
			return errors.New("local-only Harbor evaluator must not reference HARBOR_API_KEY")
		}
	}
	return nil
}

func evaluatorInvocationEnvironmentNames(mappings []stageprovider.HarborEvaluatorSecretEnvTemplate) []string {
	names := make([]string, 0, len(mappings)*2)
	for _, mapping := range mappings {
		names = append(names, mapping.HostEnvKey, mapping.ChildEnvKey)
	}
	return names
}

func evaluatorProtectedEnvironmentVariables(invocations map[string]stageprovider.HarborEvaluatorInvocation) []string {
	protected := make(map[string]struct{})
	for _, invocation := range invocations {
		for _, name := range []string{invocation.EndpointEnvName, invocation.EndpointChildEnvKey} {
			if name != "" {
				protected[name] = struct{}{}
			}
		}
		for _, mapping := range invocation.SecretEnvTemplates {
			for _, name := range []string{mapping.HostEnvKey, mapping.ChildEnvKey} {
				if name != "" {
					protected[name] = struct{}{}
				}
			}
		}
	}
	names := make([]string, 0, len(protected))
	for name := range protected {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func evaluatorStageMatchesCommand(stage workflowkit.StageKey, commandID string) error {
	switch commandID {
	case stageprovider.HarborEvaluatorQwenCommandID:
		if stage != workflowkit.StageKey(workflowadapter.HarborRunQwen) {
			return errors.New("Qwen Harbor evaluator command received another stage")
		}
	case stageprovider.HarborEvaluatorOpusCommandID:
		if stage != workflowkit.StageKey(workflowadapter.HarborRunOpus) {
			return errors.New("Opus Harbor evaluator command received another stage")
		}
	default:
		return errors.New("unrecognized Harbor evaluator command")
	}
	return nil
}

func evaluatorTaskInput(request workflowkit.StageExecutionRequest, config stageprovider.HarborEvaluatorInvocation) (workflowkit.ArtifactBinding, error) {
	var found *workflowkit.ArtifactBinding
	for _, input := range request.Inputs {
		if input.Name != config.TaskArtifactPort {
			continue
		}
		if input.SchemaVersion != config.TaskArtifactSchema {
			return workflowkit.ArtifactBinding{}, errors.New("Harbor evaluator task snapshot schema drift")
		}
		if found != nil {
			return workflowkit.ArtifactBinding{}, errors.New("Harbor evaluator received duplicate task snapshot inputs")
		}
		copy := input.Clone()
		found = &copy
	}
	if found == nil || len(request.Inputs) != 1 {
		return workflowkit.ArtifactBinding{}, errors.New("Harbor evaluator requires exactly one frozen task snapshot input")
	}
	return *found, nil
}

func (executor *HarborEvaluatorLocalCommandExecutor) workspace(request workflowkit.StageExecutionRequest) (string, error) {
	if request.Claim.Stage == nil || request.Claim.Stage.StageAttempt.ID == "" {
		return "", errors.New("Harbor evaluator stage attempt identity is required")
	}
	stageAttemptID := string(request.Claim.Stage.StageAttempt.ID)
	for _, component := range []string{request.Execution.ID, stageAttemptID} {
		if !safePathComponent(component) {
			return "", errors.New("Harbor evaluator durable identity is not a safe path component")
		}
	}
	path := filepath.Join(executor.root, request.Execution.ID, "external-evaluators", stageAttemptID, "initial")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", errors.New("Harbor evaluator workspace already exists; reconciliation is required")
		}
		return "", err
	}
	return path, nil
}

func (executor *HarborEvaluatorLocalCommandExecutor) existingWorkspace(request workflowkit.StageExecutionRequest) (string, bool, error) {
	if request.Claim.Stage == nil || request.Claim.Stage.StageAttempt.ID == "" {
		return "", false, errors.New("Harbor evaluator stage attempt identity is required")
	}
	stageAttemptID := string(request.Claim.Stage.StageAttempt.ID)
	for _, component := range []string{request.Execution.ID, stageAttemptID} {
		if !safePathComponent(component) {
			return "", false, errors.New("Harbor evaluator durable identity is not a safe path component")
		}
	}
	path := filepath.Join(executor.root, request.Execution.ID, "external-evaluators", stageAttemptID, "initial")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("controlled Harbor evaluator workspace is invalid")
	}
	return path, true, nil
}

func safePathComponent(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\\\\x00") {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.') {
			return false
		}
	}
	return true
}

func (executor *HarborEvaluatorLocalCommandExecutor) writeApprovedEnvFile(workspace string, config stageprovider.HarborEvaluatorInvocation) (string, []string, error) {
	endpoint, present := executor.lookupEnv(config.EndpointEnvName)
	if !present || endpoint == "" {
		return "", nil, errors.New("approved Harbor evaluator endpoint environment is unavailable")
	}
	fingerprint, err := stageprovider.CanonicalHarborEvaluatorEndpointFingerprint(endpoint)
	if err != nil || fingerprint != config.EndpointFingerprint {
		return "", nil, errors.New("approved Harbor evaluator endpoint does not match the frozen lock")
	}
	entries := []string{config.EndpointChildEnvKey + "=" + endpoint}
	sensitiveValues := []string{endpoint}
	seenChildKeys := map[string]struct{}{config.EndpointChildEnvKey: {}}
	for _, mapping := range config.SecretEnvTemplates {
		value, exists := executor.lookupEnv(mapping.HostEnvKey)
		if !exists || value == "" {
			return "", nil, fmt.Errorf("approved Harbor evaluator secret environment %q is unavailable", mapping.HostEnvKey)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", nil, fmt.Errorf("approved Harbor evaluator secret environment %q is not a single-line value", mapping.HostEnvKey)
		}
		if _, duplicate := seenChildKeys[mapping.ChildEnvKey]; duplicate {
			return "", nil, errors.New("approved Harbor evaluator environment mappings have duplicate child keys")
		}
		seenChildKeys[mapping.ChildEnvKey] = struct{}{}
		entries = append(entries, mapping.ChildEnvKey+"="+value)
		sensitiveValues = append(sensitiveValues, value)
	}
	sort.Strings(entries)
	path := filepath.Join(workspace, "harbor.env")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nil, err
	}
	_, writeErr := io.WriteString(file, strings.Join(entries, "\n")+"\n")
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return "", nil, writeErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", nil, closeErr
	}
	return path, sensitiveValues, nil
}

func evaluatorCommand(config stageprovider.HarborEvaluatorInvocation, taskRoot, jobsRoot, jobName, envFile string) Command {
	return Command{
		Path: config.LauncherPath,
		Args: []string{
			"run", "--path", taskRoot,
			"--agent", config.AgentID, "--model", config.ModelID,
			"--agent-kwarg", "version=" + config.AgentVersion,
			"--n-attempts", fmt.Sprintf("%d", config.Attempts), "--n-concurrent", fmt.Sprintf("%d", config.ConcurrentTrials), "--max-retries", fmt.Sprintf("%d", config.MaxRetries),
			"--job-name", jobName, "--jobs-dir", jobsRoot,
			"--env-file", envFile,
			// This process owns only the managed local job directory. Upload and
			// every sharing flag are omitted by construction.
			"--quiet", "--yes",
		},
		Dir: filepath.Dir(taskRoot),
		Env: []string{
			"HOME=" + filepath.Join(filepath.Dir(taskRoot), "harbor-home"),
			"LANG=C.UTF-8",
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
	}
}

func evaluatorJobName(request workflowkit.StageExecutionRequest, commandID string) string {
	return "codeedge-" + commandID + "-" + string(request.Claim.Stage.StageAttempt.ID)
}

func validateNoExistingJob(jobsRoot, jobName string) error {
	_, err := os.Lstat(filepath.Join(jobsRoot, jobName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("Harbor evaluator job directory already exists; reconciliation is required")
}

func evaluatorOutputNames(commandID string) (string, string) {
	if commandID == stageprovider.HarborEvaluatorQwenCommandID {
		return workflowadapter.CodeEdgeEvaluatorQwenBundleArtifact, workflowadapter.CodeEdgeEvaluatorQwenScreenshotArtifact
	}
	return workflowadapter.CodeEdgeEvaluatorOpusBundleArtifact, workflowadapter.CodeEdgeEvaluatorOpusScreenshotArtifact
}

func evaluatorTranscript(command Command, result CommandResult, runErr error, bundleDigest string, rawOutputDigest workflowkit.Fingerprint, sensitiveValues []string) string {
	parts := []string{
		"Harbor evaluator command",
		strings.Join(commandlog.RedactArgv(append([]string{command.Path}, command.Args...)), " "),
		"exit_code=" + fmt.Sprintf("%d", result.ExitCode),
		"raw_output_sha256=" + string(rawOutputDigest),
	}
	if runErr != nil {
		parts = append(parts, "process_error="+redactEvaluatorText(runErr.Error(), sensitiveValues))
	}
	if bundleDigest != "" {
		parts = append(parts, "bundle_sha256="+bundleDigest)
	}
	if len(result.Stdout) > 0 {
		parts = append(parts, "stdout:\n"+redactEvaluatorText(string(result.Stdout), sensitiveValues))
	}
	if len(result.Stderr) > 0 {
		parts = append(parts, "stderr:\n"+redactEvaluatorText(string(result.Stderr), sensitiveValues))
	}
	return canonicalTerminalTranscript(redactEvaluatorText(strings.Join(parts, "\n"), sensitiveValues))
}

func redactEvaluatorText(text string, sensitiveValues []string) string {
	text = commandlog.RedactText(text)
	values := append([]string(nil), sensitiveValues...)
	sort.Slice(values, func(left, right int) bool { return len(values[left]) > len(values[right]) })
	for _, value := range values {
		if value != "" {
			text = strings.ReplaceAll(text, value, "<redacted>")
		}
	}
	return text
}

// canonicalTerminalTranscript makes a bounded, printable, deterministic view
// of already-redacted process output. The renderer deliberately accepts only
// ASCII, so hostile terminal control bytes and multi-byte text cannot change
// its layout or conceal evidence outside the visible surface.
func canonicalTerminalTranscript(raw string) string {
	raw = commandlog.RedactText(raw)
	var lines []string
	var line strings.Builder
	flush := func() {
		lines = append(lines, line.String())
		line.Reset()
	}
	for _, character := range raw {
		if character == '\r' {
			continue
		}
		if character == '\n' {
			flush()
			continue
		}
		value := byte('?')
		if character >= 0x20 && character <= 0x7e {
			value = byte(character)
		}
		line.WriteByte(value)
		if line.Len() == 112 {
			flush()
		}
	}
	if line.Len() > 0 || len(lines) == 0 {
		flush()
	}
	if len(lines) > 88 {
		lines = append(lines[:87], "[terminal output truncated]")
	}
	return strings.Join(lines, "\n")
}

func writeRedactedTranscript(workspace, transcript string) error {
	return os.WriteFile(filepath.Join(workspace, "terminal-transcript.txt"), []byte(commandlog.RedactText(transcript)), 0o600)
}

const harborOutputProvenanceFormat = "harbor.evaluator-output-provenance.v1"

// harborOutputProvenance carries only irreversible output facts. It is placed
// inside the controlled Harbor job directory before capture, so the canonical
// bundle ties terminal evidence to the exact raw process output without ever
// retaining that raw output or an environment value.
type harborOutputProvenance struct {
	Format          string                  `json:"format"`
	Version         string                  `json:"version"`
	CommandID       string                  `json:"command_id"`
	RawOutputSHA256 workflowkit.Fingerprint `json:"raw_output_sha256"`
}

func evaluatorRawOutputFingerprint(result CommandResult, runErr error) (workflowkit.Fingerprint, error) {
	parts := []workflowkit.FingerprintPart{
		{Name: "exit_code", Value: []byte(fmt.Sprintf("%d", result.ExitCode))},
		{Name: "stderr", Value: append([]byte(nil), result.Stderr...)},
		{Name: "stdout", Value: append([]byte(nil), result.Stdout...)},
	}
	if runErr != nil {
		parts = append(parts, workflowkit.FingerprintPart{Name: "process_error", Value: []byte(runErr.Error())})
	}
	return workflowkit.FingerprintParts("harbor.evaluator.raw-output.v1", parts)
}

func writeHarborOutputProvenance(jobRoot, commandID string, rawOutputDigest workflowkit.Fingerprint) error {
	if err := rawOutputDigest.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(harborOutputProvenance{
		Format: harborOutputProvenanceFormat, Version: "1", CommandID: commandID, RawOutputSHA256: rawOutputDigest,
	})
	if err != nil {
		return err
	}
	path := filepath.Join(jobRoot, "harbor-flow-provenance.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write Harbor output provenance: %w", err)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readHarborOutputProvenance(jobRoot, commandID string) (harborOutputProvenance, error) {
	raw, err := os.ReadFile(filepath.Join(jobRoot, "harbor-flow-provenance.json"))
	if err != nil {
		return harborOutputProvenance{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var provenance harborOutputProvenance
	if err := decoder.Decode(&provenance); err != nil {
		return harborOutputProvenance{}, errors.New("Harbor output provenance is malformed")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return harborOutputProvenance{}, errors.New("Harbor output provenance has trailing data")
	}
	if provenance.Format != harborOutputProvenanceFormat || provenance.Version != "1" || provenance.CommandID != commandID {
		return harborOutputProvenance{}, errors.New("Harbor output provenance does not match the frozen command")
	}
	if err := provenance.RawOutputSHA256.Validate(); err != nil {
		return harborOutputProvenance{}, errors.New("Harbor output provenance has an invalid output digest")
	}
	return provenance, nil
}

func verifyFrozenEvaluatorIdentity(bundle codeedge.HarborRunBundleV018, invocation stageprovider.HarborEvaluatorInvocation) error {
	inspection, err := codeedge.InspectHarborRunBundleV018(bundle)
	if err != nil {
		return err
	}
	if err := verifyFrozenEvaluatorRetryConfiguration(inspection, invocation); err != nil {
		return err
	}
	for _, trial := range inspection.Trials() {
		if trial.Evaluator.AgentName != invocation.AgentID || trial.Evaluator.AgentVersion != invocation.AgentVersion || trial.Evaluator.ModelName == nil || *trial.Evaluator.ModelName != invocation.ModelID {
			return errors.New("Harbor result evaluator identity does not match frozen agent version and model")
		}
	}
	return nil
}

// verifyFrozenEvaluatorRetryConfiguration proves the captured Harbor job lock
// retained the immutable serial and retry policy used to launch the command.
// Harbor 0.18 replaces a retried trial's final result under the same logical
// name and exposes only aggregate stats.n_retries. Therefore this check keeps
// that aggregate bounded but deliberately does not fabricate per-trial
// TrialAttempt lineage without an authenticated mapping artifact.
func verifyFrozenEvaluatorRetryConfiguration(inspection *codeedge.HarborRunBundleInspectionV018, invocation stageprovider.HarborEvaluatorInvocation) error {
	if inspection == nil {
		return errors.New("Harbor evaluator inspection is required")
	}
	lockRaw, err := inspection.JobLockJSON()
	if err != nil {
		return fmt.Errorf("read captured Harbor evaluator job lock: %w", err)
	}
	var lock struct {
		NConcurrentTrials int `json:"n_concurrent_trials"`
		Retry             struct {
			MaxRetries int `json:"max_retries"`
		} `json:"retry"`
	}
	decoder := json.NewDecoder(bytes.NewReader(lockRaw))
	if err := decoder.Decode(&lock); err != nil {
		return errors.New("captured Harbor evaluator job lock is malformed")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("captured Harbor evaluator job lock has trailing data")
	}
	if lock.NConcurrentTrials != invocation.ConcurrentTrials || lock.Retry.MaxRetries != invocation.MaxRetries {
		return errors.New("captured Harbor evaluator job lock does not match frozen serial retry policy")
	}
	job := inspection.Job()
	if job.InternalRetryCount > invocation.Attempts*invocation.MaxRetries {
		return errors.New("captured Harbor evaluator retry count exceeds the frozen per-trial retry bound")
	}
	return nil
}

func extractManagedTaskSnapshot(ctx context.Context, raw []byte, destination string, expected workflowkit.SubjectDigest) error {
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("expected task digest: %w", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fmt.Errorf("read task snapshot ZIP: %w", err)
	}
	allowed := make(map[string]taskpolicy.CanonicalFile)
	for _, file := range taskpolicy.CanonicalFiles() {
		allowed["task/"+file.Path] = file
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(allowed))
	var total uint64
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Name == "" || strings.Contains(entry.Name, "\\\\") || strings.HasPrefix(entry.Name, "/") || strings.Contains(entry.Name, "../") || strings.HasPrefix(entry.Name, "../") || filepath.Clean(entry.Name) != entry.Name {
			return errors.New("task snapshot ZIP contains an unsafe path")
		}
		canonical, allowedEntry := allowed[entry.Name]
		if !allowedEntry || entry.FileInfo().IsDir() || entry.FileInfo().Mode()&os.ModeSymlink != 0 || !entry.FileInfo().Mode().IsRegular() {
			return errors.New("task snapshot ZIP contains an unexpected or non-regular entry")
		}
		if _, duplicate := seen[entry.Name]; duplicate {
			return errors.New("task snapshot ZIP contains a duplicate entry")
		}
		if entry.UncompressedSize64 > maxTaskSnapshotFileBytes || entry.UncompressedSize64 > maxTaskSnapshotTotalBytes-total {
			return errors.New("task snapshot ZIP exceeds extraction limits")
		}
		seen[entry.Name] = struct{}{}
		total += entry.UncompressedSize64
		path := filepath.Join(destination, filepath.FromSlash(canonical.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		input, openErr := entry.Open()
		if openErr != nil {
			return openErr
		}
		output, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, canonical.Mode)
		if createErr != nil {
			_ = input.Close()
			return createErr
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, int64(entry.UncompressedSize64)+1))
		closeOutputErr := output.Close()
		closeInputErr := input.Close()
		if copyErr != nil || closeOutputErr != nil || closeInputErr != nil || uint64(written) != entry.UncompressedSize64 {
			return errors.New("extract task snapshot ZIP entry")
		}
		if err := os.Chmod(path, canonical.Mode); err != nil {
			return err
		}
	}
	if len(seen) == 0 {
		return errors.New("task snapshot ZIP is empty")
	}
	if err := taskpolicy.ValidateManagedSnapshotV2(destination); err != nil {
		return err
	}
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(destination)
	if err != nil {
		return err
	}
	if workflowkit.SubjectDigest(digest) != expected {
		return errors.New("materialized task snapshot digest does not equal frozen subject")
	}
	return nil
}

var _ stageprovider.LocalCommandOperationExecutor = (*HarborEvaluatorLocalCommandExecutor)(nil)
