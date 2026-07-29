package app

import (
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
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	codeEdgePhase1WorkspaceDirectory = "codeedge-phase1-workspaces"

	codeEdgePhase1WorkspaceReceiptFormat  = "harbor.codeedge-phase1.workspace.v1"
	codeEdgePhase1WorkspaceReceiptVersion = "1"
	codeEdgePhase1ReportFormat            = "harbor.codeedge-phase1.stage-report.v1"
	codeEdgePhase1ReportVersion           = "1"
	codeEdgePhase1BuildReceiptFormat      = "harbor.codeedge-phase1.docker-build.v1"
	codeEdgePhase1BuildReceiptVersion     = "1"

	codeEdgePhase1CommandOutputLimit = 128 << 10
	codeEdgePhase1ScriptLimit        = 16 << 20

	codeEdgePhase1VerificationSetupFailureMarker = "harbor-codeedge-phase1: verification setup failed"
)

// CodeEdgePhase1Command is a direct, already-resolved process invocation.
// It deliberately has no shell text or inherited environment surface.
type CodeEdgePhase1Command struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

// CodeEdgePhase1CommandResult preserves only bounded process output for a
// transient in-memory classification. The executor writes fingerprints, not
// raw output, into durable task evidence.
type CodeEdgePhase1CommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// CodeEdgePhase1CommandRunner is the narrow local process boundary. Production
// uses CodeEdgePhase1DirectCommandRunner; tests can provide a deterministic
// runner without requiring Docker.
type CodeEdgePhase1CommandRunner interface {
	Run(context.Context, CodeEdgePhase1Command) (CodeEdgePhase1CommandResult, error)
}

// CodeEdgePhase1DirectCommandRunner invokes a locked executable using direct
// argv. It never invokes a shell or consults PATH to resolve the executable.
type CodeEdgePhase1DirectCommandRunner struct{}

func (CodeEdgePhase1DirectCommandRunner) Run(ctx context.Context, command CodeEdgePhase1Command) (CodeEdgePhase1CommandResult, error) {
	if ctx == nil {
		return CodeEdgePhase1CommandResult{}, errors.New("CodeEdge Phase-1 command context is required")
	}
	process := exec.CommandContext(ctx, command.Path, command.Args...)
	process.Dir = command.Dir
	process.Env = append([]string(nil), command.Env...)
	stdout := &codeEdgePhase1LimitedBuffer{limit: codeEdgePhase1CommandOutputLimit}
	stderr := &codeEdgePhase1LimitedBuffer{limit: codeEdgePhase1CommandOutputLimit}
	process.Stdout = stdout
	process.Stderr = stderr
	err := process.Run()
	result := CodeEdgePhase1CommandResult{
		ExitCode: 0,
		Stdout:   append([]byte(nil), stdout.Bytes()...),
		Stderr:   append([]byte(nil), stderr.Bytes()...),
	}
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

type codeEdgePhase1LimitedBuffer struct {
	limit int
	bytes.Buffer
}

func (buffer *codeEdgePhase1LimitedBuffer) Write(value []byte) (int, error) {
	if buffer == nil || buffer.limit < 0 || buffer.Len()+len(value) > buffer.limit {
		return 0, errors.New("CodeEdge Phase-1 command output exceeds the configured limit")
	}
	return buffer.Buffer.Write(value)
}

// CodeEdgePhase1ParentExecutorConfig contains only deployment-owned facts.
// The command registry is assembled from the parent catalog lock by the root
// composition. No Run can provide a path, argv, model, image, secret, or
// workspace through this API.
type CodeEdgePhase1ParentExecutorConfig struct {
	ManagedRoot      string
	WorkspaceRoot    string
	PreflightProfile codeedge.Profile
	LockedCommands   []stageprovider.LocalExecutableLock
	Runner           CodeEdgePhase1CommandRunner
	CommandTimeout   time.Duration
}

// CodeEdgePhase1ParentExecutor implements the deterministic harbor.builtin
// parent checks and the three closed Docker operations. It is intentionally
// separate from lifecycle composition: the outer catalog/lock attestor remains
// responsible for proving executable bytes immediately before this executor is
// allowed to use a registered absolute path.
type CodeEdgePhase1ParentExecutor struct {
	workspaceRoot string
	profile       codeedge.Profile
	commands      map[string]stageprovider.LocalExecutableLock
	docker        *lockedDockerRuntime
}

// CodeEdgePhase1ParentWorkspaceRoot returns the one fixed run-scoped parent
// workspace root. It does not create task workspaces or contact Docker.
func CodeEdgePhase1ParentWorkspaceRoot(managedRoot string) (string, error) {
	layout, err := newManagedLayout(managedRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(layout.root, codeEdgePhase1WorkspaceDirectory), nil
}

// NewCodeEdgePhase1ParentExecutor constructs the closed parent operation
// implementation. It rejects a partial command registry so an installed
// parent catalog cannot silently fall back to PATH or an arbitrary command.
func NewCodeEdgePhase1ParentExecutor(config CodeEdgePhase1ParentExecutorConfig) (*CodeEdgePhase1ParentExecutor, error) {
	if err := codeedge.ValidateProfile(config.PreflightProfile); err != nil {
		return nil, fmt.Errorf("validate CodeEdge Phase-1 preflight profile: %w", err)
	}
	layout, err := newManagedLayout(config.ManagedRoot)
	if err != nil {
		return nil, err
	}
	if err := layout.ensureRoot(); err != nil {
		return nil, err
	}
	workspaceRoot := filepath.Join(layout.root, codeEdgePhase1WorkspaceDirectory)
	if configured := strings.TrimSpace(config.WorkspaceRoot); configured != "" {
		absolute, absErr := filepath.Abs(configured)
		if absErr != nil || filepath.Clean(absolute) != workspaceRoot {
			return nil, errors.New("CodeEdge Phase-1 workspace root must be the managed run-scoped root")
		}
	}
	if err := ensureCodeEdgePhase1WorkspaceRoot(workspaceRoot); err != nil {
		return nil, err
	}
	commands, err := codeEdgePhase1LockedCommands(config.LockedCommands)
	if err != nil {
		return nil, err
	}
	runner := config.Runner
	if runner == nil {
		runner = CodeEdgePhase1DirectCommandRunner{}
	}
	timeout := config.CommandTimeout
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	docker, err := newLockedDockerRuntime(commands, runner, timeout, nil)
	if err != nil {
		return nil, err
	}
	return &CodeEdgePhase1ParentExecutor{
		workspaceRoot: workspaceRoot,
		profile:       config.PreflightProfile,
		commands:      commands,
		docker:        docker,
	}, nil
}

func codeEdgePhase1LockedCommands(locks []stageprovider.LocalExecutableLock) (map[string]stageprovider.LocalExecutableLock, error) {
	if len(locks) != 3 {
		return nil, errors.New("CodeEdge Phase-1 parent requires exactly three locked Docker commands")
	}
	commands := make(map[string]stageprovider.LocalExecutableLock, len(locks))
	for _, lock := range locks {
		if !stageprovider.IsCodeEdgePhase1LocalCommandID(lock.CommandID) {
			return nil, fmt.Errorf("CodeEdge Phase-1 parent does not authorize local command %q", lock.CommandID)
		}
		if lock.CommandID == "" || strings.TrimSpace(lock.Version) == "" || !filepath.IsAbs(lock.AbsolutePath) ||
			filepath.Clean(lock.AbsolutePath) != lock.AbsolutePath || lock.AbsolutePath == string(os.PathSeparator) {
			return nil, errors.New("CodeEdge Phase-1 local executable lock is incomplete")
		}
		if err := lock.ContentSHA256.Validate(); err != nil {
			return nil, fmt.Errorf("CodeEdge Phase-1 local executable fingerprint: %w", err)
		}
		if _, duplicate := commands[lock.CommandID]; duplicate {
			return nil, fmt.Errorf("CodeEdge Phase-1 local command %q is duplicated", lock.CommandID)
		}
		commands[lock.CommandID] = lock
	}
	for _, commandID := range []string{
		stageprovider.CodeEdgePhase1DockerBuildCommandID,
		stageprovider.CodeEdgePhase1InitialVerifyCommandID,
		stageprovider.CodeEdgePhase1OracleVerifyCommandID,
	} {
		if _, found := commands[commandID]; !found {
			return nil, fmt.Errorf("CodeEdge Phase-1 local command %q is missing", commandID)
		}
	}
	return commands, nil
}

// ExecuteHarborBuiltin implements the fixed deterministic CodeEdge Phase-1
// handlers. Invalid task content becomes completed + needs_repair with a
// structured required report. A broken frozen binding or controlled workspace
// remains an execution error and is never rewritten as a task finding.
func (executor *CodeEdgePhase1ParentExecutor) ExecuteHarborBuiltin(ctx context.Context, invocation stageprovider.StageOperationInvocation, payload workflowadapter.HarborBuiltinOperationPayload) (workflowkit.StageExecutionResult, error) {
	if err := executor.validateBuiltinInvocation(ctx, invocation, payload); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	stage := invocation.Request.Stage.Key
	if stage == workflowkit.StageKey(workflowadapter.Package) {
		return workflowkit.StageExecutionResult{}, errors.New("CodeEdge Phase-1 local package is operator-only and must use the release lifecycle boundary")
	}
	outputName, schema, err := codeEdgePhase1ExpectedOutput(invocation.Request.Stage)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	snapshot, binding, err := codeEdgePhase1ReadTaskSnapshot(ctx, invocation.Request)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	workspace, taskRoot, err := executor.ensureTaskWorkspace(ctx, invocation.Request, binding, snapshot)
	if err != nil {
		if findings, repairable := codeEdgePhase1WorkspaceFindings(err); repairable {
			return executor.reportResult(invocation.Request, outputName, schema, workflowkit.VerdictNeedsRepair, findings, nil, "", workspace)
		}
		return workflowkit.StageExecutionResult{}, err
	}
	report, inspectErr := codeedge.InspectPhase1Task(taskRoot, executor.profile)
	if inspectErr != nil {
		findings, repairable := codeEdgePhase1PreflightFindings(inspectErr)
		if !repairable {
			return workflowkit.StageExecutionResult{}, inspectErr
		}
		return executor.reportResult(invocation.Request, outputName, schema, workflowkit.VerdictNeedsRepair, findings, nil, "", workspace)
	}

	verdict := workflowkit.VerdictPass
	findings := []string{}
	switch payload.HandlerID {
	case stageprovider.CodeEdgePhase1QualityCheckHandlerID:
		verdict, findings = executor.runQualityChecks(taskRoot)
	case stageprovider.CodeEdgePhase1SimilarityCheckHandlerID:
		verdict, findings = executor.runSimilarityChecks(taskRoot)
	case stageprovider.CodeEdgePhase1SubmissionLintHandlerID:
		artifactID, reserveErr := executor.reserveSubmissionArtifactID(workspace, invocation.Request)
		if reserveErr != nil {
			return workflowkit.StageExecutionResult{}, reserveErr
		}
		return executor.reportResult(invocation.Request, outputName, schema, verdict, findings, &report, string(artifactID), workspace)
	}
	return executor.reportResult(invocation.Request, outputName, schema, verdict, findings, &report, "", workspace)
}

// ExecuteLocalCommand implements the three lock-selected Docker operations.
// The payload must have no argv: command construction lives entirely in this
// implementation so a task cannot turn a catalog value into host shell input.
func (executor *CodeEdgePhase1ParentExecutor) ExecuteLocalCommand(ctx context.Context, invocation stageprovider.StageOperationInvocation, payload workflowadapter.LocalCommandOperationPayload) (workflowkit.StageExecutionResult, error) {
	if err := executor.validateLocalInvocation(ctx, invocation, payload); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	outputName, schema, err := codeEdgePhase1ExpectedOutput(invocation.Request.Stage)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	snapshot, binding, err := codeEdgePhase1ReadTaskSnapshot(ctx, invocation.Request)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	workspace, taskRoot, err := executor.ensureTaskWorkspace(ctx, invocation.Request, binding, snapshot)
	if err != nil {
		if findings, repairable := codeEdgePhase1WorkspaceFindings(err); repairable {
			return executor.reportResult(invocation.Request, outputName, schema, workflowkit.VerdictNeedsRepair, findings, nil, "", workspace)
		}
		return workflowkit.StageExecutionResult{}, err
	}
	report, inspectErr := codeedge.InspectPhase1Task(taskRoot, executor.profile)
	if inspectErr != nil {
		findings, repairable := codeEdgePhase1PreflightFindings(inspectErr)
		if !repairable {
			return workflowkit.StageExecutionResult{}, inspectErr
		}
		return executor.reportResult(invocation.Request, outputName, schema, workflowkit.VerdictNeedsRepair, findings, nil, "", workspace)
	}

	switch payload.CommandID {
	case stageprovider.CodeEdgePhase1DockerBuildCommandID:
		return executor.executeDockerBuild(ctx, invocation.Request, outputName, schema, workspace, taskRoot, report)
	case stageprovider.CodeEdgePhase1InitialVerifyCommandID:
		return executor.executeInitialVerify(ctx, invocation.Request, outputName, schema, workspace, taskRoot, report)
	case stageprovider.CodeEdgePhase1OracleVerifyCommandID:
		return executor.executeOracleVerify(ctx, invocation.Request, outputName, schema, workspace, taskRoot, report)
	default:
		return workflowkit.StageExecutionResult{}, fmt.Errorf("CodeEdge Phase-1 local command %q is not implemented", payload.CommandID)
	}
}

func (executor *CodeEdgePhase1ParentExecutor) validateBuiltinInvocation(ctx context.Context, invocation stageprovider.StageOperationInvocation, payload workflowadapter.HarborBuiltinOperationPayload) error {
	if executor == nil || executor.docker == nil {
		return errors.New("CodeEdge Phase-1 parent executor is not configured")
	}
	if ctx == nil {
		return errors.New("CodeEdge Phase-1 parent context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stage, expectedHandler, ok := codeEdgePhase1BuiltinStage(payload.HandlerID)
	if !ok || invocation.Request.Stage.Key != stage || invocation.Resolution.StageKey != stage {
		return errors.New("CodeEdge Phase-1 built-in operation is not bound to its fixed stage")
	}
	if err := codeEdgePhase1ValidateExecutionIdentity(invocation); err != nil {
		return err
	}
	resolved, ok := invocation.Resolution.Operation.Payload.(workflowadapter.HarborBuiltinOperationPayload)
	if !ok || resolved.HandlerID != expectedHandler || resolved.HandlerID != payload.HandlerID {
		return errors.New("CodeEdge Phase-1 built-in payload is not frozen")
	}
	if len(invocation.Resolution.Secrets) != 0 {
		return errors.New("CodeEdge Phase-1 parent built-ins must not receive secret references")
	}
	return nil
}

func (executor *CodeEdgePhase1ParentExecutor) validateLocalInvocation(ctx context.Context, invocation stageprovider.StageOperationInvocation, payload workflowadapter.LocalCommandOperationPayload) error {
	if executor == nil || executor.docker == nil {
		return errors.New("CodeEdge Phase-1 parent executor is not configured")
	}
	if ctx == nil {
		return errors.New("CodeEdge Phase-1 parent context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(payload.Arguments) != 0 {
		return errors.New("CodeEdge Phase-1 local commands do not accept catalog argv")
	}
	stage, ok := codeEdgePhase1LocalStage(payload.CommandID)
	if !ok || invocation.Request.Stage.Key != stage || invocation.Resolution.StageKey != stage {
		return errors.New("CodeEdge Phase-1 local command is not bound to its fixed stage")
	}
	if _, found := executor.commands[payload.CommandID]; !found {
		return fmt.Errorf("CodeEdge Phase-1 local command %q is not installed", payload.CommandID)
	}
	if err := codeEdgePhase1ValidateExecutionIdentity(invocation); err != nil {
		return err
	}
	resolved, ok := invocation.Resolution.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
	if !ok || resolved.CommandID != payload.CommandID || len(resolved.Arguments) != 0 {
		return errors.New("CodeEdge Phase-1 local command payload is not frozen")
	}
	if len(invocation.Resolution.Secrets) != 0 {
		return errors.New("CodeEdge Phase-1 parent local commands must not receive secret references")
	}
	return nil
}

func codeEdgePhase1ValidateExecutionIdentity(invocation stageprovider.StageOperationInvocation) error {
	request := invocation.Request
	if request.Execution.Workflow.ID != workflowadapter.CodeEdgePhase1WorkflowTemplateID || request.Execution.Workflow.Version != workflowadapter.CodeEdgePhase1WorkflowTemplateVersion ||
		!invocation.Resolution.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) || request.Stage.Key != invocation.Resolution.StageKey || request.Stage.Plugin != invocation.Resolution.Plugin {
		return errors.New("CodeEdge Phase-1 parent received another frozen workflow template")
	}
	if request.Execution.ID == "" || request.Claim.Stage == nil || request.Claim.Stage.StageAttempt.ID == "" {
		return errors.New("CodeEdge Phase-1 parent requires a frozen Run and stage attempt")
	}
	if err := store.ValidateUUIDv7(request.Execution.ID); err != nil {
		return fmt.Errorf("CodeEdge Phase-1 Run identity: %w", err)
	}
	if err := store.ValidateUUIDv7(string(request.Claim.Stage.StageAttempt.ID)); err != nil {
		return fmt.Errorf("CodeEdge Phase-1 stage attempt identity: %w", err)
	}
	return nil
}

func codeEdgePhase1BuiltinStage(handlerID string) (workflowkit.StageKey, string, bool) {
	switch handlerID {
	case stageprovider.CodeEdgePhase1TaskLayoutPreflightHandlerID:
		return workflowkit.StageKey(workflowadapter.RepoPrepare), handlerID, true
	case stageprovider.CodeEdgePhase1RepoProvenancePreflightHandlerID:
		return workflowkit.StageKey(workflowadapter.RepoAnalyze), handlerID, true
	case stageprovider.CodeEdgePhase1EnvironmentIsolationPreflightHandlerID:
		return workflowkit.StageKey(workflowadapter.CodeEdgeLint), handlerID, true
	case stageprovider.CodeEdgePhase1TestsAnalysisValidateHandlerID:
		return workflowkit.StageKey(workflowadapter.TestsAnalysis), handlerID, true
	case stageprovider.CodeEdgePhase1QualityCheckHandlerID:
		return workflowkit.StageKey(workflowadapter.QualityCheck), handlerID, true
	case stageprovider.CodeEdgePhase1SimilarityCheckHandlerID:
		return workflowkit.StageKey(workflowadapter.SimilarityCheck), handlerID, true
	case stageprovider.CodeEdgePhase1SubmissionLintHandlerID:
		return workflowkit.StageKey(workflowadapter.SubmissionLint), handlerID, true
	case stageprovider.CodeEdgePhase1LocalPackageHandlerID:
		return workflowkit.StageKey(workflowadapter.Package), handlerID, true
	default:
		return "", "", false
	}
}

func codeEdgePhase1LocalStage(commandID string) (workflowkit.StageKey, bool) {
	switch commandID {
	case stageprovider.CodeEdgePhase1DockerBuildCommandID:
		return workflowkit.StageKey(workflowadapter.DockerBuild), true
	case stageprovider.CodeEdgePhase1InitialVerifyCommandID:
		return workflowkit.StageKey(workflowadapter.InitialVerify), true
	case stageprovider.CodeEdgePhase1OracleVerifyCommandID:
		return workflowkit.StageKey(workflowadapter.OracleVerify), true
	default:
		return "", false
	}
}

func codeEdgePhase1ExpectedOutput(stage workflowkit.StageDescriptor) (string, string, error) {
	expected := ""
	switch stage.Key {
	case workflowkit.StageKey(workflowadapter.RepoPrepare):
		expected = "task_layout_report"
	case workflowkit.StageKey(workflowadapter.RepoAnalyze):
		expected = "repo_provenance_report"
	case workflowkit.StageKey(workflowadapter.CodeEdgeLint):
		expected = "environment_isolation_report"
	case workflowkit.StageKey(workflowadapter.DockerBuild):
		expected = "docker_build_report"
	case workflowkit.StageKey(workflowadapter.InitialVerify):
		expected = "initial_verify_report"
	case workflowkit.StageKey(workflowadapter.OracleVerify):
		expected = "oracle_verify_report"
	case workflowkit.StageKey(workflowadapter.TestsAnalysis):
		expected = "tests_analysis_report"
	case workflowkit.StageKey(workflowadapter.QualityCheck):
		expected = "quality_report"
	case workflowkit.StageKey(workflowadapter.SimilarityCheck):
		expected = "similarity_report"
	case workflowkit.StageKey(workflowadapter.SubmissionLint):
		expected = "submission_lint_report"
	default:
		return "", "", fmt.Errorf("CodeEdge Phase-1 stage %q is not a parent executor stage", stage.Key)
	}
	if len(stage.Outputs) != 1 || stage.Outputs[0].Name != expected || !stage.Outputs[0].Required || strings.TrimSpace(stage.Outputs[0].SchemaVersion) == "" {
		return "", "", fmt.Errorf("CodeEdge Phase-1 stage %q output contract is not frozen", stage.Key)
	}
	return expected, stage.Outputs[0].SchemaVersion, nil
}

func codeEdgePhase1ReadTaskSnapshot(ctx context.Context, request workflowkit.StageExecutionRequest) ([]byte, workflowkit.ArtifactBinding, error) {
	if request.ReadInput == nil {
		return nil, workflowkit.ArtifactBinding{}, errors.New("CodeEdge Phase-1 frozen task snapshot reader is unavailable")
	}
	var selected *workflowkit.ArtifactBinding
	for _, input := range request.Inputs {
		if input.Name != "task_snapshot" {
			continue
		}
		if selected != nil {
			return nil, workflowkit.ArtifactBinding{}, errors.New("CodeEdge Phase-1 stage has duplicate task_snapshot inputs")
		}
		copy := input.Clone()
		selected = &copy
	}
	if selected == nil || selected.SchemaVersion != "harbor.artifact.v1" {
		return nil, workflowkit.ArtifactBinding{}, errors.New("CodeEdge Phase-1 stage requires one managed task_snapshot input")
	}
	bytes, err := request.ReadInput(ctx, *selected)
	if err != nil {
		return nil, workflowkit.ArtifactBinding{}, fmt.Errorf("read frozen CodeEdge task snapshot: %w", err)
	}
	if len(bytes) == 0 || workflowkit.SHA256Fingerprint(bytes) != selected.ContentDigest {
		return nil, workflowkit.ArtifactBinding{}, errors.New("frozen CodeEdge task snapshot bytes do not match their binding")
	}
	return bytes, *selected, nil
}

func ensureCodeEdgePhase1WorkspaceRoot(root string) error {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("create CodeEdge Phase-1 workspace root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("CodeEdge Phase-1 workspace root is not a real directory")
	}
	return nil
}

func (executor *CodeEdgePhase1ParentExecutor) ensureTaskWorkspace(ctx context.Context, request workflowkit.StageExecutionRequest, binding workflowkit.ArtifactBinding, snapshot []byte) (string, string, error) {
	if executor == nil || executor.workspaceRoot == "" {
		return "", "", errors.New("CodeEdge Phase-1 workspace is not configured")
	}
	if err := ensureCodeEdgePhase1WorkspaceRoot(executor.workspaceRoot); err != nil {
		return "", "", err
	}
	runID := request.Execution.ID
	if err := store.ValidateUUIDv7(runID); err != nil {
		return "", "", fmt.Errorf("CodeEdge Phase-1 workspace Run identity: %w", err)
	}
	workspace := filepath.Join(executor.workspaceRoot, runID)
	taskRoot := filepath.Join(workspace, "task")
	if !codeEdgePhase1PathWithin(executor.workspaceRoot, workspace) || !codeEdgePhase1PathWithin(executor.workspaceRoot, taskRoot) {
		return "", "", errors.New("CodeEdge Phase-1 workspace path escapes its managed root")
	}
	if _, err := os.Lstat(workspace); err == nil {
		if verifyErr := codeEdgePhase1VerifyWorkspace(workspace, taskRoot, request, binding); verifyErr != nil {
			return "", "", verifyErr
		}
		return workspace, taskRoot, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("inspect CodeEdge Phase-1 workspace: %w", err)
	}

	staging, err := os.MkdirTemp(executor.workspaceRoot, ".prepare-"+runID+"-")
	if err != nil {
		return "", "", fmt.Errorf("create CodeEdge Phase-1 workspace staging directory: %w", err)
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	stagingTaskRoot := filepath.Join(staging, "task")
	if err := taskpolicy.ExtractManagedSnapshotV2ZIP(ctx, snapshot, stagingTaskRoot, string(request.Execution.Subject.Digest)); err != nil {
		return "", "", fmt.Errorf("materialize frozen CodeEdge task snapshot: %w", err)
	}
	receipt := codeEdgePhase1WorkspaceReceipt{
		Format:             codeEdgePhase1WorkspaceReceiptFormat,
		Version:            codeEdgePhase1WorkspaceReceiptVersion,
		RunID:              runID,
		TaskSnapshotDigest: string(request.Execution.Subject.Digest),
		SnapshotZIPDigest:  string(binding.ContentDigest),
		TaskSnapshotSchema: binding.SchemaVersion,
	}
	if err := codeEdgePhase1WriteJSON(filepath.Join(staging, "workspace-receipt.json"), receipt); err != nil {
		return "", "", err
	}
	if err := os.Rename(staging, workspace); err != nil {
		if _, statErr := os.Lstat(workspace); statErr == nil {
			if verifyErr := codeEdgePhase1VerifyWorkspace(workspace, taskRoot, request, binding); verifyErr != nil {
				return "", "", verifyErr
			}
			return workspace, taskRoot, nil
		}
		return "", "", fmt.Errorf("publish CodeEdge Phase-1 workspace: %w", err)
	}
	removeStaging = false
	if err := codeEdgePhase1VerifyWorkspace(workspace, taskRoot, request, binding); err != nil {
		return "", "", err
	}
	return workspace, taskRoot, nil
}

type codeEdgePhase1WorkspaceReceipt struct {
	Format             string `json:"format"`
	Version            string `json:"version"`
	RunID              string `json:"run_id"`
	TaskSnapshotDigest string `json:"task_snapshot_digest"`
	SnapshotZIPDigest  string `json:"snapshot_zip_digest"`
	TaskSnapshotSchema string `json:"task_snapshot_schema"`
}

func codeEdgePhase1VerifyWorkspace(workspace, taskRoot string, request workflowkit.StageExecutionRequest, binding workflowkit.ArtifactBinding) error {
	info, err := os.Lstat(workspace)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("CodeEdge Phase-1 workspace is not a real directory")
	}
	var receipt codeEdgePhase1WorkspaceReceipt
	if err := codeEdgePhase1ReadJSON(filepath.Join(workspace, "workspace-receipt.json"), &receipt); err != nil {
		return err
	}
	if receipt.Format != codeEdgePhase1WorkspaceReceiptFormat || receipt.Version != codeEdgePhase1WorkspaceReceiptVersion ||
		receipt.RunID != request.Execution.ID || receipt.TaskSnapshotDigest != string(request.Execution.Subject.Digest) ||
		receipt.SnapshotZIPDigest != string(binding.ContentDigest) || receipt.TaskSnapshotSchema != binding.SchemaVersion {
		return errors.New("CodeEdge Phase-1 workspace receipt does not match the frozen task input")
	}
	if err := taskpolicy.ValidateManagedSnapshotV2(taskRoot); err != nil {
		return fmt.Errorf("validate CodeEdge Phase-1 materialized task workspace: %w", err)
	}
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		return fmt.Errorf("digest CodeEdge Phase-1 materialized task workspace: %w", err)
	}
	if digest != string(request.Execution.Subject.Digest) {
		return errors.New("CodeEdge Phase-1 workspace task digest differs from the frozen Run")
	}
	return nil
}

func codeEdgePhase1PathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

func codeEdgePhase1WriteJSON(path string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(encoded)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return writeErr
	}
	return closeErr
}

func codeEdgePhase1ReadJSON(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("CodeEdge Phase-1 controlled receipt is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("CodeEdge Phase-1 controlled receipt is malformed")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("CodeEdge Phase-1 controlled receipt has trailing data")
	}
	return nil
}

func codeEdgePhase1WorkspaceFindings(err error) ([]string, bool) {
	var validation *taskpolicy.SnapshotValidationError
	if errors.As(err, &validation) {
		findings := append([]string(nil), validation.Violations...)
		return findings, true
	}
	message := err.Error()
	if strings.Contains(message, "task snapshot ZIP") || strings.Contains(message, "frozen CodeEdge task snapshot") || strings.Contains(message, "frozen task digest") {
		return []string{"task snapshot archive is malformed or does not match the frozen task digest"}, true
	}
	return nil, false
}

func codeEdgePhase1PreflightFindings(err error) ([]string, bool) {
	var validation *codeedge.ValidationError
	if !errors.As(err, &validation) {
		return nil, false
	}
	findings := make([]string, 0, len(validation.Violations))
	for _, violation := range validation.Violations {
		findings = append(findings, violation.String())
	}
	return findings, true
}

type codeEdgePhase1InspectionReport struct {
	Environment string `json:"environment"`
	Metadata    struct {
		CodeLang    string `json:"code_lang"`
		TaskType    string `json:"task_type"`
		Application string `json:"application"`
		IsZeroToOne bool   `json:"is_zero_to_one"`
		GitHubURL   string `json:"github_url"`
		CommitID    string `json:"commit_id"`
	} `json:"metadata"`
}

type codeEdgePhase1CommandReceipt struct {
	CommandID         string                  `json:"command_id"`
	ExecutableVersion string                  `json:"executable_version"`
	ExitCode          int                     `json:"exit_code"`
	OutputFingerprint workflowkit.Fingerprint `json:"output_fingerprint"`
}

// codeEdgePhase1StageReport is deliberately a report, not a final compliance
// receipt. The latter can only be formed after artifact persistence assigns
// the immutable content digest and lineage binding.
type codeEdgePhase1StageReport struct {
	Format             string                          `json:"format"`
	Version            string                          `json:"version"`
	Stage              string                          `json:"stage"`
	RunID              string                          `json:"run_id"`
	StageAttemptID     string                          `json:"stage_attempt_id"`
	TaskSnapshotDigest string                          `json:"task_snapshot_digest"`
	Verdict            workflowkit.Verdict             `json:"verdict"`
	Findings           []string                        `json:"findings"`
	Inspection         *codeEdgePhase1InspectionReport `json:"inspection,omitempty"`
	Command            *codeEdgePhase1CommandReceipt   `json:"command,omitempty"`
	ReservedArtifactID string                          `json:"reserved_artifact_id,omitempty"`
}

func codeEdgePhase1Inspection(report codeedge.Report) *codeEdgePhase1InspectionReport {
	result := &codeEdgePhase1InspectionReport{Environment: string(report.Environment)}
	result.Metadata.CodeLang = report.Metadata.CodeLang
	result.Metadata.TaskType = report.Metadata.TaskType
	result.Metadata.Application = report.Metadata.Application
	result.Metadata.IsZeroToOne = report.Metadata.IsZeroToOne
	result.Metadata.GitHubURL = report.Metadata.GitHubURL
	result.Metadata.CommitID = report.Metadata.CommitID
	return result
}

func (executor *CodeEdgePhase1ParentExecutor) reportResult(request workflowkit.StageExecutionRequest, outputName, schema string, verdict workflowkit.Verdict, findings []string, inspection *codeedge.Report, reservedArtifactID, _ string) (workflowkit.StageExecutionResult, error) {
	report := codeEdgePhase1StageReport{
		Format:             codeEdgePhase1ReportFormat,
		Version:            codeEdgePhase1ReportVersion,
		Stage:              string(request.Stage.Key),
		RunID:              request.Execution.ID,
		StageAttemptID:     string(request.Claim.Stage.StageAttempt.ID),
		TaskSnapshotDigest: string(request.Execution.Subject.Digest),
		Verdict:            verdict,
		Findings:           append([]string{}, findings...),
		ReservedArtifactID: reservedArtifactID,
	}
	if inspection != nil {
		report.Inspection = codeEdgePhase1Inspection(*inspection)
	}
	return codeEdgePhase1ReportStageResult(report, outputName, schema)
}

func codeEdgePhase1ReportStageResult(report codeEdgePhase1StageReport, outputName, schema string) (workflowkit.StageExecutionResult, error) {
	sort.Strings(report.Findings)
	if report.Findings == nil {
		report.Findings = []string{}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	artifact := workflowkit.StageArtifact{Name: outputName, SchemaVersion: schema, Content: encoded}
	if report.ReservedArtifactID != "" {
		if err := store.ValidateUUIDv7(report.ReservedArtifactID); err != nil {
			return workflowkit.StageExecutionResult{}, err
		}
		artifact.ID = workflowkit.ArtifactID(report.ReservedArtifactID)
	}
	return workflowkit.StageExecutionResult{
		Outcome:   workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: report.Verdict},
		Artifacts: []workflowkit.StageArtifact{artifact},
	}, nil
}

func codeEdgePhase1InfraResult(request workflowkit.StageExecutionRequest, outputName, schema, errorText string, report codeEdgePhase1StageReport, failure workflowkit.FailureClass) (workflowkit.StageExecutionResult, error) {
	sort.Strings(report.Findings)
	if report.Findings == nil {
		report.Findings = []string{}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	return workflowkit.StageExecutionResult{
		Outcome:   workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: failure},
		Artifacts: []workflowkit.StageArtifact{{Name: outputName, SchemaVersion: schema, Content: encoded}},
		ErrorText: errorText,
	}, nil
}

func (executor *CodeEdgePhase1ParentExecutor) reportFor(request workflowkit.StageExecutionRequest, verdict workflowkit.Verdict, findings []string, inspection codeedge.Report) codeEdgePhase1StageReport {
	return codeEdgePhase1StageReport{
		Format:             codeEdgePhase1ReportFormat,
		Version:            codeEdgePhase1ReportVersion,
		Stage:              string(request.Stage.Key),
		RunID:              request.Execution.ID,
		StageAttemptID:     string(request.Claim.Stage.StageAttempt.ID),
		TaskSnapshotDigest: string(request.Execution.Subject.Digest),
		Verdict:            verdict,
		Findings:           append([]string{}, findings...),
		Inspection:         codeEdgePhase1Inspection(inspection),
	}
}

func (executor *CodeEdgePhase1ParentExecutor) reserveSubmissionArtifactID(workspace string, request workflowkit.StageExecutionRequest) (workflowkit.ArtifactID, error) {
	if request.Claim.Stage == nil {
		return "", errors.New("CodeEdge Phase-1 submission stage attempt is required")
	}
	directory := filepath.Join(workspace, "reserved-artifacts")
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("CodeEdge Phase-1 reserved artifact directory is unsafe")
	}
	path := filepath.Join(directory, string(request.Claim.Stage.StageAttempt.ID)+"-submission-lint.id")
	if existing, found, err := codeEdgePhase1ReadReservedID(path); err != nil {
		return "", err
	} else if found {
		return workflowkit.ArtifactID(existing), nil
	}
	id, err := store.NewUUIDv7()
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		existing, found, readErr := codeEdgePhase1ReadReservedID(path)
		if readErr != nil || !found {
			return "", errors.New("CodeEdge Phase-1 reserved artifact ID raced without a valid value")
		}
		return workflowkit.ArtifactID(existing), nil
	}
	if err != nil {
		return "", err
	}
	writeErr := func() error {
		if _, err := io.WriteString(file, id+"\n"); err != nil {
			return err
		}
		return file.Sync()
	}()
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return workflowkit.ArtifactID(id), nil
}

func codeEdgePhase1ReadReservedID(path string) (string, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", false, errors.New("CodeEdge Phase-1 reserved artifact ID is unsafe")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	id := strings.TrimSpace(string(raw))
	if string(raw) != id+"\n" || store.ValidateUUIDv7(id) != nil {
		return "", false, errors.New("CodeEdge Phase-1 reserved artifact ID is malformed")
	}
	return id, true, nil
}

type codeEdgePhase1DockerBuildReceipt struct {
	Format             string                  `json:"format"`
	Version            string                  `json:"version"`
	RunID              string                  `json:"run_id"`
	TaskSnapshotDigest string                  `json:"task_snapshot_digest"`
	ImageTag           string                  `json:"image_tag"`
	ImageID            string                  `json:"image_id"`
	OutputFingerprint  workflowkit.Fingerprint `json:"output_fingerprint"`
}

func (executor *CodeEdgePhase1ParentExecutor) executeDockerBuild(ctx context.Context, request workflowkit.StageExecutionRequest, outputName, schema, workspace, taskRoot string, inspection codeedge.Report) (workflowkit.StageExecutionResult, error) {
	report := executor.reportFor(request, workflowkit.VerdictPass, []string{}, inspection)
	if inspection.Environment != codeedge.EnvironmentDockerfile {
		report.Verdict = workflowkit.VerdictNeedsRepair
		report.Findings = []string{"docker-compose environment requires a separately locked compose operation"}
		return codeEdgePhase1ReportStageResult(report, outputName, schema)
	}
	lock := executor.commands[stageprovider.CodeEdgePhase1DockerBuildCommandID]
	imageTag := codeEdgePhase1ImageTag(request.Execution.ID)
	args := []string{
		"build", "--pull=false", "--network=default",
		"--label", "io.harbor-factory.codeedge.run_id=" + request.Execution.ID,
		"--label", "io.harbor-factory.codeedge.task_digest=" + string(request.Execution.Subject.Digest),
		"--tag", imageTag,
		"--file", filepath.Join(taskRoot, "environment", "Dockerfile"),
		filepath.Join(taskRoot, "environment"),
	}
	result, outputFingerprint, runErr := executor.docker.run(ctx, lock.CommandID, args, workspace)
	report.Command = &codeEdgePhase1CommandReceipt{CommandID: lock.CommandID, ExecutableVersion: lock.Version, ExitCode: result.ExitCode, OutputFingerprint: outputFingerprint}
	if runErr != nil {
		return codeEdgePhase1InfraResult(request, outputName, schema, "controlled Docker build could not start", report, codeEdgePhase1FailureClass(runErr))
	}
	if result.ExitCode != 0 {
		// Training rules treat build/download/network failures as infra rather
		// than a task-quality or model-capability verdict.
		return codeEdgePhase1InfraResult(request, outputName, schema, "controlled Docker build failed", report, workflowkit.FailureProcess)
	}
	// Docker build output is intentionally not a contract: its format changes
	// between legacy and BuildKit implementations. Resolve the tagged image
	// through the same locked Docker executable instead, then freeze that
	// immutable image ID in the run-scoped receipt.
	imageID, inspectErr := executor.inspectDockerImage(ctx, workspace, stageprovider.CodeEdgePhase1DockerBuildCommandID, imageTag)
	if inspectErr != nil {
		return codeEdgePhase1InfraResult(request, outputName, schema, "controlled Docker build image cannot be identified", report, codeEdgePhase1FailureClass(inspectErr))
	}
	if err := executor.writeDockerBuildReceipt(workspace, codeEdgePhase1DockerBuildReceipt{
		Format: codeEdgePhase1BuildReceiptFormat, Version: codeEdgePhase1BuildReceiptVersion,
		RunID: request.Execution.ID, TaskSnapshotDigest: string(request.Execution.Subject.Digest),
		ImageTag: imageTag, ImageID: imageID, OutputFingerprint: outputFingerprint,
	}); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	return codeEdgePhase1ReportStageResult(report, outputName, schema)
}

func (executor *CodeEdgePhase1ParentExecutor) executeInitialVerify(ctx context.Context, request workflowkit.StageExecutionRequest, outputName, schema, workspace, taskRoot string, inspection codeedge.Report) (workflowkit.StageExecutionResult, error) {
	report := executor.reportFor(request, workflowkit.VerdictPass, []string{}, inspection)
	build, err := executor.readDockerBuildReceipt(workspace, request)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if err := executor.verifyDockerBuildImage(ctx, workspace, stageprovider.CodeEdgePhase1InitialVerifyCommandID, build); err != nil {
		return codeEdgePhase1InfraResult(request, outputName, schema, "controlled Docker build image cannot be re-attested", report, codeEdgePhase1FailureClass(err))
	}
	checkout, before, err := codeEdgePhase1PrepareVerificationCheckout(workspace, request, taskRoot, "initial", []string{"tests/test.sh"})
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	lock := executor.commands[stageprovider.CodeEdgePhase1InitialVerifyCommandID]
	result, outputFingerprint, runErr := executor.docker.run(ctx, lock.CommandID,
		codeEdgePhase1DockerRunArgs(build.ImageTag, checkout, codeEdgePhase1ContainerName(request, "initial"), codeEdgePhase1VerificationProgram(false)), workspace)
	report.Command = &codeEdgePhase1CommandReceipt{CommandID: lock.CommandID, ExecutableVersion: lock.Version, ExitCode: result.ExitCode, OutputFingerprint: outputFingerprint}
	if runErr != nil {
		return codeEdgePhase1InfraResult(request, outputName, schema, "controlled initial verification could not start", report, codeEdgePhase1FailureClass(runErr))
	}
	if codeEdgePhase1VerificationSetupFailed(result) {
		return codeEdgePhase1InfraResult(request, outputName, schema, "controlled initial verification setup failed", report, workflowkit.FailureProcess)
	}
	if err := codeEdgePhase1VerifyCheckoutScripts(checkout, before); err != nil {
		report.Verdict = workflowkit.VerdictNeedsRepair
		report.Findings = []string{"initial verifier modified its immutable test script"}
		return codeEdgePhase1ReportStageResult(report, outputName, schema)
	}
	if result.ExitCode == 0 {
		report.Verdict = workflowkit.VerdictNeedsRepair
		report.Findings = []string{"initial verifier passed before the Oracle repair, so the task does not expose the intended problem"}
		return codeEdgePhase1ReportStageResult(report, outputName, schema)
	}
	// A non-zero verifier result is the expected initial state. The command
	// itself completed and returned a trustworthy report, so it is not infra.
	return codeEdgePhase1ReportStageResult(report, outputName, schema)
}

func (executor *CodeEdgePhase1ParentExecutor) executeOracleVerify(ctx context.Context, request workflowkit.StageExecutionRequest, outputName, schema, workspace, taskRoot string, inspection codeedge.Report) (workflowkit.StageExecutionResult, error) {
	report := executor.reportFor(request, workflowkit.VerdictPass, []string{}, inspection)
	build, err := executor.readDockerBuildReceipt(workspace, request)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if err := executor.verifyDockerBuildImage(ctx, workspace, stageprovider.CodeEdgePhase1OracleVerifyCommandID, build); err != nil {
		return codeEdgePhase1InfraResult(request, outputName, schema, "controlled Docker build image cannot be re-attested", report, codeEdgePhase1FailureClass(err))
	}
	checkout, before, err := codeEdgePhase1PrepareVerificationCheckout(workspace, request, taskRoot, "oracle", []string{"solution/solve.sh", "tests/test.sh"})
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	lock := executor.commands[stageprovider.CodeEdgePhase1OracleVerifyCommandID]
	result, outputFingerprint, runErr := executor.docker.run(ctx, lock.CommandID,
		codeEdgePhase1DockerRunArgs(build.ImageTag, checkout, codeEdgePhase1ContainerName(request, "oracle"), codeEdgePhase1VerificationProgram(true)), workspace)
	report.Command = &codeEdgePhase1CommandReceipt{CommandID: lock.CommandID, ExecutableVersion: lock.Version, ExitCode: result.ExitCode, OutputFingerprint: outputFingerprint}
	if runErr != nil {
		return codeEdgePhase1InfraResult(request, outputName, schema, "controlled Oracle verification could not start", report, codeEdgePhase1FailureClass(runErr))
	}
	if codeEdgePhase1VerificationSetupFailed(result) {
		return codeEdgePhase1InfraResult(request, outputName, schema, "controlled Oracle verification setup failed", report, workflowkit.FailureProcess)
	}
	if err := codeEdgePhase1VerifyCheckoutScripts(checkout, before); err != nil {
		report.Verdict = workflowkit.VerdictNeedsRepair
		report.Findings = []string{"Oracle or verifier modified an immutable solution/test script"}
		return codeEdgePhase1ReportStageResult(report, outputName, schema)
	}
	if result.ExitCode != 0 {
		report.Verdict = workflowkit.VerdictNeedsRepair
		report.Findings = []string{"Oracle repair followed by verifier did not pass"}
		return codeEdgePhase1ReportStageResult(report, outputName, schema)
	}
	return codeEdgePhase1ReportStageResult(report, outputName, schema)
}

func codeEdgePhase1FailureClass(err error) workflowkit.FailureClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return workflowkit.FailureTimeout
	}
	return workflowkit.FailureProcess
}

func codeEdgePhase1ImageTag(runID string) string {
	return "harbor-codeedge:" + strings.ToLower(runID)
}

func codeEdgePhase1DockerImageID(raw []byte) (string, error) {
	lines := strings.Fields(string(raw))
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "sha256:") || len(lines[0]) != len("sha256:")+64 {
		return "", errors.New("Docker build image identity is malformed")
	}
	for _, value := range lines[0][len("sha256:"):] {
		if !(value >= '0' && value <= '9') && !(value >= 'a' && value <= 'f') {
			return "", errors.New("Docker build image identity is malformed")
		}
	}
	return lines[0], nil
}

func (executor *CodeEdgePhase1ParentExecutor) writeDockerBuildReceipt(workspace string, receipt codeEdgePhase1DockerBuildReceipt) error {
	if receipt.Format != codeEdgePhase1BuildReceiptFormat || receipt.Version != codeEdgePhase1BuildReceiptVersion ||
		receipt.RunID == "" || receipt.TaskSnapshotDigest == "" || receipt.ImageTag == "" || receipt.ImageID == "" || receipt.OutputFingerprint.Validate() != nil {
		return errors.New("CodeEdge Phase-1 Docker build receipt is invalid")
	}
	directory := filepath.Join(workspace, "runtime")
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("CodeEdge Phase-1 runtime receipt directory is unsafe")
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".docker-build-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(directory, "docker-build.json"))
}

func (executor *CodeEdgePhase1ParentExecutor) readDockerBuildReceipt(workspace string, request workflowkit.StageExecutionRequest) (codeEdgePhase1DockerBuildReceipt, error) {
	var receipt codeEdgePhase1DockerBuildReceipt
	if err := codeEdgePhase1ReadJSON(filepath.Join(workspace, "runtime", "docker-build.json"), &receipt); err != nil {
		return codeEdgePhase1DockerBuildReceipt{}, fmt.Errorf("read controlled Docker build receipt: %w", err)
	}
	if receipt.Format != codeEdgePhase1BuildReceiptFormat || receipt.Version != codeEdgePhase1BuildReceiptVersion || receipt.RunID != request.Execution.ID ||
		receipt.TaskSnapshotDigest != string(request.Execution.Subject.Digest) || receipt.ImageTag != codeEdgePhase1ImageTag(request.Execution.ID) ||
		receipt.OutputFingerprint.Validate() != nil {
		return codeEdgePhase1DockerBuildReceipt{}, errors.New("controlled Docker build receipt does not match the frozen Run")
	}
	if _, err := codeEdgePhase1DockerImageID([]byte(receipt.ImageID)); err != nil {
		return codeEdgePhase1DockerBuildReceipt{}, err
	}
	return receipt, nil
}

func (executor *CodeEdgePhase1ParentExecutor) verifyDockerBuildImage(ctx context.Context, workspace, commandID string, build codeEdgePhase1DockerBuildReceipt) error {
	imageID, err := executor.inspectDockerImage(ctx, workspace, commandID, build.ImageTag)
	if err != nil {
		return err
	}
	if imageID != build.ImageID {
		return errors.New("controlled Docker image identity differs from the frozen build receipt")
	}
	return nil
}

func (executor *CodeEdgePhase1ParentExecutor) inspectDockerImage(ctx context.Context, workspace, commandID, imageTag string) (string, error) {
	imageID, _, _, err := executor.docker.inspectImage(ctx, workspace, commandID, imageTag)
	return imageID, err
}

func codeEdgePhase1DockerRunArgs(imageTag, checkout, name, shellProgram string) []string {
	return []string{
		"run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		// CodeEdge verifiers publish their binary reward under /logs. Keep that
		// protocol path ephemeral and constrained while the image root stays read-only.
		"--tmpfs", "/logs:rw,noexec,nosuid,size=8m",
		// Docker's --mount grammar treats a bare "rw" field as invalid. Bind
		// mounts are writable unless readonly is set, so omitting it preserves the
		// controlled Oracle worktree's required write access.
		"--mount", "type=bind,src=" + checkout + ",dst=/oracle",
		"--workdir", "/oracle", "--name", "harbor-codeedge-" + name,
		"--entrypoint", "/bin/sh", imageTag, "-ec", shellProgram,
	}
}

func codeEdgePhase1VerificationProgram(applySolution bool) string {
	prepare := "rm -rf /oracle/workspace && mkdir -p /oracle/workspace && cp -R /workspace/source/. /oracle/workspace/ && cd /oracle/workspace"
	command := "sh /oracle/tests/test.sh"
	if applySolution {
		command = "sh /oracle/solution/solve.sh && sh /oracle/tests/test.sh"
	}
	return prepare + " || { echo '" + codeEdgePhase1VerificationSetupFailureMarker + "' >&2; exit 125; }; " + command
}

func codeEdgePhase1VerificationSetupFailed(result CodeEdgePhase1CommandResult) bool {
	return result.ExitCode == 125 && bytes.Contains(result.Stderr, []byte(codeEdgePhase1VerificationSetupFailureMarker))
}

func codeEdgePhase1ContainerName(request workflowkit.StageExecutionRequest, kind string) string {
	return "harbor-codeedge-" + kind + "-" + strings.ToLower(request.Execution.ID) + "-" + strings.ToLower(string(request.Claim.Stage.StageAttempt.ID))
}

func codeEdgePhase1PrepareVerificationCheckout(workspace string, request workflowkit.StageExecutionRequest, taskRoot, kind string, files []string) (string, map[string]workflowkit.Fingerprint, error) {
	if kind != "initial" && kind != "oracle" {
		return "", nil, errors.New("CodeEdge Phase-1 verification checkout kind is unsupported")
	}
	if request.Claim.Stage == nil || request.Claim.Stage.StageAttempt.ID == "" {
		return "", nil, errors.New("CodeEdge Phase-1 verification stage attempt is required")
	}
	checkout := filepath.Join(workspace, "verification", kind, string(request.Claim.Stage.StageAttempt.ID))
	if !codeEdgePhase1PathWithin(workspace, checkout) {
		return "", nil, errors.New("CodeEdge Phase-1 verification checkout escapes the managed workspace")
	}
	if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
		return "", nil, err
	}
	if err := codeEdgePhase1PrepareVerificationCheckoutRoot(checkout); err != nil {
		return "", nil, err
	}
	before := make(map[string]workflowkit.Fingerprint, len(files))
	for _, relative := range files {
		if !codeEdgePhase1VerificationFile(relative) {
			return "", nil, errors.New("CodeEdge Phase-1 verification checkout requested an unsafe file")
		}
		source := filepath.Join(taskRoot, filepath.FromSlash(relative))
		content, digest, err := codeEdgePhase1ReadTaskScript(source)
		if err != nil {
			return "", nil, err
		}
		destination := filepath.Join(checkout, filepath.FromSlash(relative))
		directory := filepath.Dir(destination)
		if err := codeEdgePhase1PrepareVerificationScriptDirectory(directory); err != nil {
			return "", nil, err
		}
		if info, statErr := os.Lstat(destination); errors.Is(statErr, os.ErrNotExist) {
			if err := writeNewBytesWithMode(destination, content, 0o444); err != nil {
				return "", nil, err
			}
		} else if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", nil, errors.New("CodeEdge Phase-1 verification checkout script is unsafe")
		} else {
			existing, existingDigest, readErr := codeEdgePhase1ReadTaskScript(destination)
			if readErr != nil || existingDigest != digest || !bytes.Equal(existing, content) {
				return "", nil, errors.New("CodeEdge Phase-1 verification checkout script differs from the frozen task")
			}
		}
		before[relative] = digest
	}
	return checkout, before, nil
}

// The image may select a non-root USER. The bind-mounted checkout must let
// that user create /oracle/worktree while leaving the host-owned script bytes
// readable; post-run digest verification remains the mutation authority.
func codeEdgePhase1PrepareVerificationCheckoutRoot(path string) error {
	if err := os.Mkdir(path, 0o777); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("CodeEdge Phase-1 verification checkout is unsafe")
	}
	return os.Chmod(path, 0o777|os.ModeSticky)
}

func codeEdgePhase1PrepareVerificationScriptDirectory(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("CodeEdge Phase-1 verification script directory is unsafe")
	}
	return os.Chmod(path, 0o755)
}

func codeEdgePhase1VerificationFile(relative string) bool {
	return relative == "solution/solve.sh" || relative == "tests/test.sh"
}

func codeEdgePhase1ReadTaskScript(path string) ([]byte, workflowkit.Fingerprint, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > codeEdgePhase1ScriptLimit {
		return nil, "", errors.New("CodeEdge Phase-1 verification script is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, "", errors.New("CodeEdge Phase-1 verification script changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, codeEdgePhase1ScriptLimit+1))
	if err != nil || int64(len(content)) != info.Size() {
		return nil, "", errors.New("CodeEdge Phase-1 verification script changed while reading")
	}
	if after, err := file.Stat(); err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != int64(len(content)) {
		return nil, "", errors.New("CodeEdge Phase-1 verification script changed while reading")
	}
	return content, workflowkit.SHA256Fingerprint(content), nil
}

func codeEdgePhase1VerifyCheckoutScripts(checkout string, expected map[string]workflowkit.Fingerprint) error {
	for relative, digest := range expected {
		_, actual, err := codeEdgePhase1ReadTaskScript(filepath.Join(checkout, filepath.FromSlash(relative)))
		if err != nil || actual != digest {
			return errors.New("CodeEdge Phase-1 verification checkout script changed")
		}
	}
	return nil
}

func (executor *CodeEdgePhase1ParentExecutor) runQualityChecks(taskRoot string) (workflowkit.Verdict, []string) {
	var findings []string

	// Check 1: test.sh should not rely solely on file-existence checks.
	testPath := filepath.Join(taskRoot, "tests", "test.sh")
	if testContent, err := os.ReadFile(testPath); err == nil {
		text := string(testContent)
		if hasOnlyFileExistenceChecks(text) {
			findings = append(findings, "tests/test.sh appears to rely primarily on file-existence checks (test -f/test -e); add functional assertions that verify correct behavior")
		}
	}

	// Check 2: instruction.md should not leak the solution.
	instrPath := filepath.Join(taskRoot, "instruction.md")
	if instrContent, err := os.ReadFile(instrPath); err == nil {
		if leaksAnswer(string(instrContent)) {
			findings = append(findings, "instruction.md appears to contain substantial code blocks that may leak the solution; keep instruction focused on requirements, not implementation")
		}
	}

	if len(findings) > 0 {
		return workflowkit.VerdictNeedsRepair, findings
	}
	return workflowkit.VerdictPass, nil
}

func hasOnlyFileExistenceChecks(script string) bool {
	lines := strings.Split(script, "\n")
	functionalCount := 0
	existenceCount := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "test -f") || strings.Contains(trimmed, "test -e") || strings.Contains(trimmed, "test -d") {
			existenceCount++
			continue
		}
		if strings.Contains(trimmed, "grep") || strings.Contains(trimmed, "diff") || strings.Contains(trimmed, "cmp") ||
			strings.Contains(trimmed, "awk") || strings.Contains(trimmed, "sed") || strings.Contains(trimmed, "curl") ||
			strings.Contains(trimmed, "assert") || strings.Contains(trimmed, "--exit-code") {
			functionalCount++
		}
	}
	return existenceCount > 0 && functionalCount == 0
}

func leaksAnswer(instruction string) bool {
	lines := strings.Split(instruction, "\n")
	codeBlockLines := 0
	inCodeBlock := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			codeBlockLines++
		}
	}
	totalLines := len(lines)
	if totalLines == 0 {
		return false
	}
	return float64(codeBlockLines)/float64(totalLines) > 0.30
}

func (executor *CodeEdgePhase1ParentExecutor) runSimilarityChecks(taskRoot string) (workflowkit.Verdict, []string) {
	instrPath := filepath.Join(taskRoot, "instruction.md")
	instrContent, err := os.ReadFile(instrPath)
	if err != nil {
		return workflowkit.VerdictAdvisory, []string{"cannot read instruction.md for similarity comparison"}
	}
	instruction := string(instrContent)
	if len(strings.TrimSpace(instruction)) < 100 {
		return workflowkit.VerdictAdvisory, []string{"instruction too short for meaningful similarity check"}
	}
	// Full corpus comparison (TB3, GitHub issues, intra-author) requires the
	// durable review gate. This basic check ensures the instruction is substantive.
	return workflowkit.VerdictPass, nil
}

var _ stageprovider.HarborBuiltinOperationExecutor = (*CodeEdgePhase1ParentExecutor)(nil)
var _ stageprovider.LocalCommandOperationExecutor = (*CodeEdgePhase1ParentExecutor)(nil)
