// Package evaluator contains the controlled local implementation of the two
// CodeEdge Harbor evaluator operations. It deliberately depends on the Harbor
// domain adapter rather than workflowkit alone: generic workflowkit must not
// learn about Harbor CLI layouts, model credentials, or pass@4 evidence.
package evaluator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	maxTranscriptBytes = 24 << 10
	// maxStagedClaudeCodeBytes bounds the deployment-owned executable copied
	// into an attempt workspace. Claude Code's locked standalone binary is
	// currently about 248 MiB, so this leaves deliberate headroom without
	// allowing an unbounded host file to consume evaluator storage.
	maxStagedClaudeCodeBytes int64 = 512 << 20

	stagedClaudeCodeDirectory = "claude-runtime"
	stagedClaudeCodeFilename  = "claude"
	claudeCodeContainerPath   = "/usr/local/bin/claude"

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
	prelaunchAttestor             stageprovider.HarborEvaluatorInvocationPrelaunchAttestor
	runner                        CommandRunner
}

// HarborEvaluatorLocalCommandExecutorConfig supplies only deployment-owned
// facts. Invocations must be created from the exact approved lock during
// composition; environment values are deliberately looked up only at launch.
type HarborEvaluatorLocalCommandExecutorConfig struct {
	WorkspaceRoot     string
	Invocations       []stageprovider.HarborEvaluatorInvocation
	LookupEnv         func(string) (string, bool)
	PrelaunchAttestor stageprovider.HarborEvaluatorInvocationPrelaunchAttestor
	Runner            CommandRunner
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
	if config.PrelaunchAttestor == nil {
		return nil, errors.New("Harbor evaluator prelaunch runtime attestor is required")
	}
	executor := &HarborEvaluatorLocalCommandExecutor{root: root, invocations: make(map[string]stageprovider.HarborEvaluatorInvocation, len(config.Invocations)), lookupEnv: lookup, prelaunchAttestor: config.PrelaunchAttestor, runner: runner}
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
	harborHome := filepath.Join(workspace, "harbor-home")
	if err := os.Mkdir(harborHome, 0o700); err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("create isolated Harbor evaluator home: %w", err)
	}
	if err := os.Mkdir(filepath.Join(harborHome, ".docker"), 0o700); err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("create isolated Harbor evaluator Docker configuration: %w", err)
	}
	jobName := evaluatorJobName(invocation.Request, payload.CommandID)
	if err := validateNoExistingJob(jobsRoot, jobName); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	processEnvironment, err := executor.prelaunchAttestor.AttestHarborEvaluatorInvocationBeforeLaunch(ctx, config, harborHome)
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("attest Harbor evaluator immediately before launch: %w", err)
	}
	stagedClaudeCode, err := stageLockedClaudeCodeExecutable(ctx, workspace, config)
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("stage locked Claude Code executable: %w", err)
	}

	envFile, sensitiveValues, err := executor.writeApprovedEnvFile(workspace, config)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	defer os.Remove(envFile)
	launchEnvironment, err := executor.prelaunchAttestor.AttestHarborEvaluatorInvocationBeforeLaunch(ctx, config, harborHome)
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("re-attest Harbor evaluator immediately before process start: %w", err)
	}
	if !slices.Equal(processEnvironment, launchEnvironment) {
		return workflowkit.StageExecutionResult{}, errors.New("Harbor evaluator launch environment changed between runtime attestations")
	}
	command, err := evaluatorCommand(config, taskRoot, jobsRoot, jobName, envFile, stagedClaudeCode, launchEnvironment)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
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
	dockerPATH, err := stageprovider.HarborEvaluatorDockerPATH(invocation.DockerCLIPath)
	if err != nil || invocation.DockerPATH != dockerPATH {
		return errors.New("incomplete Harbor evaluator Docker PATH attestation")
	}
	if !cleanNonRootAbsolutePath(invocation.LauncherPath) || invocation.LauncherVersion == "" || !cleanNonRootAbsolutePath(invocation.ClaudeCodeExecutablePath) || invocation.ClaudeCodeVersion == "" || !cleanNonRootAbsolutePath(invocation.PythonInterpreterPath) || invocation.PythonInterpreterVersion == "" || !cleanNonRootAbsolutePath(invocation.PythonSourceTreePath) ||
		invocation.HarborVersion != stageprovider.HarborEvaluatorHarborVersion || invocation.AgentID != "claude-code" || invocation.AgentVersion == "" || invocation.ModelID == "" ||
		invocation.DockerVersion != stageprovider.HarborEvaluatorDockerVersion || invocation.DockerServerVersion != stageprovider.HarborEvaluatorDockerServerVersion ||
		!filepath.IsAbs(invocation.DockerComposePluginPath) || filepath.Clean(invocation.DockerComposePluginPath) != invocation.DockerComposePluginPath || filepath.Base(invocation.DockerComposePluginPath) != "docker-compose" || invocation.DockerComposeVersion != stageprovider.HarborEvaluatorDockerComposeVersion || invocation.DockerComposeVersionOutput != stageprovider.HarborEvaluatorDockerComposeVersionOutput ||
		!filepath.IsAbs(invocation.DockerBuildxPluginPath) || filepath.Clean(invocation.DockerBuildxPluginPath) != invocation.DockerBuildxPluginPath || filepath.Base(invocation.DockerBuildxPluginPath) != "docker-buildx" || invocation.DockerBuildxVersion != stageprovider.HarborEvaluatorDockerBuildxVersion || invocation.DockerBuildxVersionOutput != stageprovider.HarborEvaluatorDockerBuildxVersionOutput ||
		invocation.TaskArtifactPort != stageprovider.HarborEvaluatorTaskArtifactPort || invocation.TaskArtifactSchema != stageprovider.HarborEvaluatorTaskArtifactSchema ||
		invocation.Attempts != stageprovider.HarborEvaluatorTrialCount || invocation.ConcurrentTrials != stageprovider.HarborEvaluatorConcurrentTrials || invocation.MaxRetries != stageprovider.HarborEvaluatorMaxRetries || !invocation.RequireTrajectory ||
		invocation.EndpointEnvName == "" || invocation.EndpointChildEnvKey == "" || len(invocation.SecretEnvTemplates) == 0 {
		return errors.New("incomplete Harbor evaluator invocation")
	}
	if err := invocation.LauncherContentSHA256.Validate(); err != nil {
		return fmt.Errorf("Harbor evaluator launcher fingerprint: %w", err)
	}
	if invocation.ClaudeCodeVersion != invocation.AgentVersion {
		return errors.New("Harbor evaluator Claude Code version does not match the frozen agent version")
	}
	if err := invocation.ClaudeCodeContentSHA256.Validate(); err != nil {
		return fmt.Errorf("Harbor evaluator Claude Code fingerprint: %w", err)
	}
	if err := invocation.PythonInterpreterContentSHA256.Validate(); err != nil {
		return fmt.Errorf("Harbor evaluator Python interpreter fingerprint: %w", err)
	}
	if err := invocation.PythonSourceFilesSHA256.Validate(); err != nil {
		return fmt.Errorf("Harbor evaluator Python source tree fingerprint: %w", err)
	}
	for label, fingerprint := range map[string]workflowkit.Fingerprint{
		"Docker CLI":            invocation.DockerCLIContentSHA256,
		"Docker Compose plugin": invocation.DockerComposeContentSHA256,
		"Docker Buildx plugin":  invocation.DockerBuildxContentSHA256,
	} {
		if err := fingerprint.Validate(); err != nil {
			return fmt.Errorf("Harbor evaluator %s fingerprint: %w", label, err)
		}
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

func cleanNonRootAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != string(filepath.Separator)
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
		return "", nil, errors.New("approved Harbor evaluator endpoint does not match the installed lock")
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

func evaluatorCommand(config stageprovider.HarborEvaluatorInvocation, taskRoot, jobsRoot, jobName, envFile, stagedClaudeCode string, environment []string) (Command, error) {
	mounts, err := canonicalClaudeCodeMountJSON(stagedClaudeCode)
	if err != nil {
		return Command{}, fmt.Errorf("construct fixed Claude Code mount: %w", err)
	}
	return Command{
		Path: config.LauncherPath,
		Args: []string{
			"run", "--path", taskRoot,
			"--agent", config.AgentID, "--model", config.ModelID,
			"--agent-kwarg", "version=" + config.AgentVersion,
			"--n-attempts", fmt.Sprintf("%d", config.Attempts), "--n-concurrent", fmt.Sprintf("%d", config.ConcurrentTrials), "--max-retries", fmt.Sprintf("%d", config.MaxRetries),
			"--job-name", jobName, "--jobs-dir", jobsRoot,
			"--env-file", envFile,
			"--mounts", mounts,
			// This process owns only the managed local job directory. Upload and
			// every sharing flag are omitted by construction.
			"--quiet", "--yes",
		},
		Dir: filepath.Dir(taskRoot),
		Env: append([]string(nil), environment...),
	}, nil
}

// canonicalClaudeCodeMountJSON is the sole path by which this evaluator can
// expose a host file to a Harbor task container. The source is a copy in the
// controlled attempt workspace, never a deployment runtime pathname.
func canonicalClaudeCodeMountJSON(stagedClaudeCode string) (string, error) {
	if !cleanNonRootAbsolutePath(stagedClaudeCode) {
		return "", errors.New("staged Claude Code executable path is unsafe")
	}
	mount := []map[string]any{{
		"bind": map[string]any{
			"create_host_path": false,
		},
		"read_only": true,
		"source":    stagedClaudeCode,
		"target":    claudeCodeContainerPath,
		"type":      "bind",
	}}
	encoded, err := json.Marshal(mount)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// stageLockedClaudeCodeExecutable copies the independently prelaunch-attested
// binary into a fresh attempt-local directory. It re-proves the source and
// destination bytes during the copy so the Docker bind mount never points at
// a mutable deployment installation.
func stageLockedClaudeCodeExecutable(ctx context.Context, workspace string, invocation stageprovider.HarborEvaluatorInvocation) (string, error) {
	if ctx == nil {
		return "", errors.New("command context is required")
	}
	if !cleanNonRootAbsolutePath(workspace) || !cleanNonRootAbsolutePath(invocation.ClaudeCodeExecutablePath) {
		return "", errors.New("Claude Code staging path is unsafe")
	}
	if err := invocation.ClaudeCodeContentSHA256.Validate(); err != nil {
		return "", fmt.Errorf("locked Claude Code fingerprint: %w", err)
	}
	workspaceInfo, err := os.Lstat(workspace)
	if err != nil || !workspaceInfo.IsDir() || workspaceInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("controlled Harbor evaluator workspace is invalid")
	}
	runtimeDirectory := filepath.Join(workspace, stagedClaudeCodeDirectory)
	if err := os.Mkdir(runtimeDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create controlled Claude Code staging directory: %w", err)
	}
	runtimeInfo, err := os.Lstat(runtimeDirectory)
	if err != nil || !runtimeInfo.IsDir() || runtimeInfo.Mode()&os.ModeSymlink != 0 || runtimeInfo.Mode().Perm() != 0o700 {
		return "", errors.New("controlled Claude Code staging directory is invalid")
	}
	destination := filepath.Join(runtimeDirectory, stagedClaudeCodeFilename)
	if err := copyLockedClaudeCodeExecutable(ctx, invocation.ClaudeCodeExecutablePath, destination, invocation.ClaudeCodeContentSHA256); err != nil {
		return "", err
	}
	return destination, nil
}

func copyLockedClaudeCodeExecutable(ctx context.Context, sourcePath, destinationPath string, expected workflowkit.Fingerprint) (err error) {
	sourceInfo, err := inspectRegularNonSymlinkFile(sourcePath)
	if err != nil {
		return fmt.Errorf("locked Claude Code source is invalid: %w", err)
	}
	if sourceInfo.Size() <= 0 || sourceInfo.Size() > maxStagedClaudeCodeBytes {
		return errors.New("locked Claude Code source exceeds the staging size limit")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open locked Claude Code source: %w", err)
	}
	defer source.Close()
	openedSourceInfo, err := source.Stat()
	if err != nil || !openedSourceInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, openedSourceInfo) {
		return errors.New("locked Claude Code source changed while opening")
	}
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create staged Claude Code executable: %w", err)
	}
	completed := false
	closed := false
	defer func() {
		if !closed {
			if closeErr := destination.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
		}
		if !completed {
			_ = os.Remove(destinationPath)
		}
	}()

	hasher := sha256.New()
	buffer := make([]byte, 64*1024)
	var copied int64
	for {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			copied += int64(count)
			if copied > maxStagedClaudeCodeBytes {
				return errors.New("locked Claude Code source exceeds the staging size limit")
			}
			if _, hashErr := hasher.Write(buffer[:count]); hashErr != nil {
				return hashErr
			}
			written, writeErr := destination.Write(buffer[:count])
			if writeErr != nil {
				return fmt.Errorf("write staged Claude Code executable: %w", writeErr)
			}
			if written != count {
				return io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read locked Claude Code source: %w", readErr)
		}
		if count == 0 {
			return errors.New("locked Claude Code source read made no progress")
		}
	}
	if copied != sourceInfo.Size() || copied != openedSourceInfo.Size() {
		return errors.New("locked Claude Code source changed while reading")
	}
	if actual := workflowkit.Fingerprint("sha256:" + hex.EncodeToString(hasher.Sum(nil))); actual != expected {
		return errors.New("locked Claude Code source fingerprint does not match")
	}
	finalSourceInfo, err := source.Stat()
	if err != nil || !finalSourceInfo.Mode().IsRegular() || !os.SameFile(openedSourceInfo, finalSourceInfo) {
		return errors.New("locked Claude Code source changed while reading")
	}
	finalSourcePathInfo, err := inspectRegularNonSymlinkFile(sourcePath)
	if err != nil || !os.SameFile(openedSourceInfo, finalSourcePathInfo) {
		return errors.New("locked Claude Code source path changed while reading")
	}
	if err := destination.Sync(); err != nil {
		return fmt.Errorf("sync staged Claude Code executable: %w", err)
	}
	closeErr := destination.Close()
	closed = true
	if closeErr != nil {
		return fmt.Errorf("close staged Claude Code executable: %w", closeErr)
	}
	if err := os.Chmod(destinationPath, 0o555); err != nil {
		return fmt.Errorf("lock staged Claude Code executable mode: %w", err)
	}
	destinationInfo, err := inspectRegularNonSymlinkFile(destinationPath)
	if err != nil || destinationInfo.Mode().Perm() != 0o555 || destinationInfo.Size() != copied {
		return errors.New("staged Claude Code executable is invalid")
	}
	destinationFingerprint, err := fingerprintBoundedRegularFile(ctx, destinationPath, maxStagedClaudeCodeBytes)
	if err != nil || destinationFingerprint != expected {
		return errors.New("staged Claude Code executable fingerprint does not match")
	}
	completed = true
	return nil
}

func inspectRegularNonSymlinkFile(path string) (os.FileInfo, error) {
	if !cleanNonRootAbsolutePath(path) {
		return nil, errors.New("path is not a clean non-root absolute path")
	}
	components := make([]string, 0, 8)
	for current := path; ; {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	var result os.FileInfo
	for index := len(components) - 1; index >= 0; index-- {
		info, err := os.Lstat(components[index])
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symbolic link in path")
		}
		if index > 0 && !info.IsDir() {
			return nil, errors.New("non-directory parent path component")
		}
		if index == 0 {
			result = info
		}
	}
	if result == nil || !result.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	return result, nil
}

func fingerprintBoundedRegularFile(ctx context.Context, path string, maximum int64) (workflowkit.Fingerprint, error) {
	info, err := inspectRegularNonSymlinkFile(path)
	if err != nil || info.Size() < 0 || info.Size() > maximum {
		return "", errors.New("regular file is invalid or exceeds its size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return "", errors.New("regular file changed while opening")
	}
	hasher := sha256.New()
	buffer := make([]byte, 64*1024)
	var read int64
	for {
		if ctx == nil {
			return "", errors.New("command context is required")
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			read += int64(count)
			if read > maximum {
				return "", errors.New("regular file exceeds its size limit")
			}
			if _, err := hasher.Write(buffer[:count]); err != nil {
				return "", err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
		if count == 0 {
			return "", errors.New("regular file read made no progress")
		}
	}
	if read != info.Size() {
		return "", errors.New("regular file changed while reading")
	}
	final, err := file.Stat()
	if err != nil || !final.Mode().IsRegular() || !os.SameFile(opened, final) {
		return "", errors.New("regular file changed while reading")
	}
	finalPath, err := inspectRegularNonSymlinkFile(path)
	if err != nil || !os.SameFile(opened, finalPath) {
		return "", errors.New("regular file path changed while reading")
	}
	return workflowkit.Fingerprint("sha256:" + hex.EncodeToString(hasher.Sum(nil))), nil
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
	return taskpolicy.ExtractManagedSnapshotV2ZIP(ctx, raw, destination, string(expected))
}

var _ stageprovider.LocalCommandOperationExecutor = (*HarborEvaluatorLocalCommandExecutor)(nil)
