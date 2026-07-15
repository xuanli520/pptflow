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
	runner        CodeEdgePhase1CommandRunner
	timeout       time.Duration
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
	return &CodeEdgePhase1ParentExecutor{
		workspaceRoot: workspaceRoot,
		profile:       config.PreflightProfile,
		commands:      commands,
		runner:        runner,
		timeout:       timeout,
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
		verdict = workflowkit.VerdictAdvisory
		findings = []string{"semantic quality assessment is reserved for the durable review gate"}
	case stageprovider.CodeEdgePhase1SimilarityCheckHandlerID:
		verdict = workflowkit.VerdictAdvisory
		findings = []string{"similarity corpus comparison is reserved for the controlled review service"}
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
	if executor == nil || executor.runner == nil {
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
	if executor == nil || executor.runner == nil {
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
		Format:              codeEdgePhase1WorkspaceReceiptFormat,
		Version:             codeEdgePhase1WorkspaceReceiptVersion,
		RunID:               runID,
		TaskSnapshotDigest:  string(request.Execution.Subject.Digest),
		SnapshotZIPDigest:   string(binding.ContentDigest),
		TaskSnapshotSchema:  binding.SchemaVersion,
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
