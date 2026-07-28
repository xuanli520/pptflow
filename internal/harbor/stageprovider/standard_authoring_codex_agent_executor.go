package stageprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/runtime/codexruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringCodexCheckpointFormat = "harbor.standard-authoring-codex-turn-checkpoint.v1"

	standardAuthoringCodexFailureConfiguration = "standard_authoring_codex_agent_turn.configuration"
	standardAuthoringCodexFailureInput         = "standard_authoring_codex_agent_turn.input"
	standardAuthoringCodexFailureCheckpoint    = "standard_authoring_codex_agent_turn.checkpoint"
	standardAuthoringCodexFailureQuota         = "standard_authoring_codex_agent_turn.quota"
	standardAuthoringCodexFailureRuntime       = "standard_authoring_codex_agent_turn.runtime"
	standardAuthoringCodexFailureSource        = "standard_authoring_codex_agent_turn.source_integrity"
	standardAuthoringCodexFailureWorkspace     = "standard_authoring_codex_agent_turn.workspace"
	standardAuthoringCodexFailureInterrupted   = "standard_authoring_codex_agent_turn.interrupted"

	standardAuthoringCodexContractAssetLimit = 1 << 20
)

var (
	// ErrStandardAuthoringCodexAgentTurnConfiguration is returned only while a
	// deployment composition constructs this executor. Its text never includes
	// prompt bytes, model output, environment values, or provider errors.
	ErrStandardAuthoringCodexAgentTurnConfiguration = errors.New("standard authoring Codex agent turn configuration is invalid")
)

// StandardAuthoringCodexTurnProgram is a versioned, immutable prompt program
// for exactly one Standard Authoring agent stage. A program has one static
// instruction per allowed App Server turn, so MaxTurns never becomes an
// implicit "continue" policy owned by the executor.
//
// Programs belong in deployment-controlled assets and their Fingerprint must
// be represented by the corresponding catalog/lock integration. This executor
// intentionally accepts neither a prompt from a Run nor a caller-provided map.
type StandardAuthoringCodexTurnProgram struct {
	ID             string                  `json:"id"`
	Version        string                  `json:"version"`
	TurnPrompts    []string                `json:"turn_prompts"`
	MaxOutputBytes int                     `json:"max_output_bytes"`
	Fingerprint    workflowkit.Fingerprint `json:"fingerprint"`
}

// NewStandardAuthoringCodexTurnProgram creates one immutable prompt program
// and calculates the fingerprint that a deployment catalog/lock should pin.
func NewStandardAuthoringCodexTurnProgram(id, version string, turnPrompts []string, maxOutputBytes int) (StandardAuthoringCodexTurnProgram, error) {
	program := StandardAuthoringCodexTurnProgram{
		ID:             strings.TrimSpace(id),
		Version:        strings.TrimSpace(version),
		TurnPrompts:    append([]string(nil), turnPrompts...),
		MaxOutputBytes: maxOutputBytes,
	}
	if err := program.validate(); err != nil {
		return StandardAuthoringCodexTurnProgram{}, err
	}
	fingerprint, err := standardAuthoringCodexTurnProgramFingerprint(program)
	if err != nil {
		return StandardAuthoringCodexTurnProgram{}, err
	}
	program.Fingerprint = fingerprint
	return program, nil
}

func (program StandardAuthoringCodexTurnProgram) clone() StandardAuthoringCodexTurnProgram {
	program.TurnPrompts = append([]string(nil), program.TurnPrompts...)
	return program
}

func (program StandardAuthoringCodexTurnProgram) validate() error {
	if err := standardAuthoringCodexToken("program id", program.ID); err != nil {
		return err
	}
	if err := standardAuthoringCodexToken("program version", program.Version); err != nil {
		return err
	}
	if len(program.TurnPrompts) == 0 {
		return fmt.Errorf("%w: prompt program must contain at least one turn", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if program.MaxOutputBytes <= 0 {
		return fmt.Errorf("%w: prompt program max output bytes must be positive", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	for index, prompt := range program.TurnPrompts {
		if strings.TrimSpace(prompt) == "" || strings.ContainsRune(prompt, '\x00') {
			return fmt.Errorf("%w: prompt program turn %d is empty or invalid", ErrStandardAuthoringCodexAgentTurnConfiguration, index+1)
		}
	}
	if program.Fingerprint != "" {
		if err := program.Fingerprint.Validate(); err != nil {
			return fmt.Errorf("%w: prompt program fingerprint", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		expected, err := standardAuthoringCodexTurnProgramFingerprint(program)
		if err != nil || expected != program.Fingerprint {
			return fmt.Errorf("%w: prompt program fingerprint does not match program content", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
	}
	return nil
}

func standardAuthoringCodexTurnProgramFingerprint(program StandardAuthoringCodexTurnProgram) (workflowkit.Fingerprint, error) {
	encoded, err := json.Marshal(struct {
		ID             string   `json:"id"`
		Version        string   `json:"version"`
		TurnPrompts    []string `json:"turn_prompts"`
		MaxOutputBytes int      `json:"max_output_bytes"`
	}{
		ID: program.ID, Version: program.Version, TurnPrompts: append([]string(nil), program.TurnPrompts...), MaxOutputBytes: program.MaxOutputBytes,
	})
	if err != nil {
		return "", fmt.Errorf("%w: encode prompt program", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return workflowkit.FingerprintBytes("harbor.standard-authoring-codex-turn-program.v1", encoded)
}

// ParseStandardAuthoringCodexTurnProgramAsset decodes the one supported
// deployment prompt asset. The asset must be canonical JSON for
// StandardAuthoringCodexTurnProgram, with at most one POSIX terminal LF, and
// must carry its own computed fingerprint. The raw bytes remain separately
// SHA-256 locked by the deployment contract; accepting the terminal LF only
// avoids treating ordinary POSIX text-file serialization as semantic drift.
func ParseStandardAuthoringCodexTurnProgramAsset(raw []byte) (StandardAuthoringCodexTurnProgram, error) {
	if len(raw) == 0 || len(raw) > standardAuthoringCodexContractAssetLimit {
		return StandardAuthoringCodexTurnProgram{}, fmt.Errorf("%w: prompt program asset has invalid size", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return StandardAuthoringCodexTurnProgram{}, fmt.Errorf("%w: prompt program asset has duplicate fields", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	var program StandardAuthoringCodexTurnProgram
	if err := decodeDeploymentCatalogJSON(raw, &program); err != nil {
		return StandardAuthoringCodexTurnProgram{}, fmt.Errorf("%w: prompt program asset is invalid", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if program.Fingerprint == "" {
		return StandardAuthoringCodexTurnProgram{}, fmt.Errorf("%w: prompt program asset fingerprint is required", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := program.validate(); err != nil {
		return StandardAuthoringCodexTurnProgram{}, err
	}
	canonical, err := json.Marshal(program)
	if err != nil || !bytes.Equal(standardAuthoringCodexCanonicalAssetBody(raw), canonical) {
		return StandardAuthoringCodexTurnProgram{}, fmt.Errorf("%w: prompt program asset is not canonical", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return program.clone(), nil
}

// standardAuthoringCodexCanonicalAssetBody permits exactly the final LF that
// POSIX text tools conventionally write. It never trims spaces, tabs, CRLF, or
// multiple newlines, so the semantic parser stays strict while the deployment
// lock continues to attest the original file bytes.
func standardAuthoringCodexCanonicalAssetBody(raw []byte) []byte {
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		return raw[:len(raw)-1]
	}
	return raw
}

// StandardAuthoringCodexInvocationFactory obtains the secret-free, already
// attested Codex invocation for exactly one external effect. Implementations
// must not cache an invocation: its launcher bytes, CODEX_HOME, CLI capability,
// and contract assets are all runtime facts that must be re-proven for every
// stage execution attempt.
//
// The invocation and payload are defensive copies from the typed provider. A
// factory cannot select a different stage, agent, model, prompt, or operation.
type StandardAuthoringCodexInvocationFactory func(context.Context, StageOperationInvocation, workflowadapter.AgentTurnOperationPayload) (CodexAppServerInvocation, error)

// StandardAuthoringCodexRuntimeFactory is a narrow test seam for constructing
// a runtime from one freshly attested invocation. Production leaves it nil and
// receives a new controlled codexruntime.Runtime per external effect.
type StandardAuthoringCodexRuntimeFactory func(CodexAppServerInvocation) agent.Runtime

// StandardAuthoringCodexFrozenSourceVerifier re-proves the exact source tree
// visible to a RunScoped turn against the Store-bound immutable source object.
// The returned identity must name that frozen object, not a fresh digest of an
// otherwise untrusted workspace. Production wires the repo_prepare executor,
// which owns both the object store and the materialized checkout contract.
type StandardAuthoringCodexFrozenSourceVerifier interface {
	VerifyStandardAuthoringCodexFrozenSource(context.Context, workflowkit.FrozenExecution, string) (workflowkit.Fingerprint, error)
}

// StandardAuthoringCodexWorkspaceMode determines how a composition-owned
// workspace root is projected into an individual Codex turn. Static remains a
// focused test/embed seam. Production Standard authoring uses RunScoped, so a
// turn may inspect only its own prepared immutable source checkout and never a
// shared ambient directory.
type StandardAuthoringCodexWorkspaceMode string

const (
	StandardAuthoringCodexWorkspaceStatic    StandardAuthoringCodexWorkspaceMode = "static"
	StandardAuthoringCodexWorkspaceRunScoped StandardAuthoringCodexWorkspaceMode = "run_scoped"
	// StandardAuthoringCodexRunSourceDirectory is the fixed internal archive
	// root materialized by the lock-bound repo_prepare executor. It deliberately
	// does not derive from a caller-selected repository URL, keeping workspace
	// layout stable and path-safe for every supported source.
	StandardAuthoringCodexRunSourceDirectory = "source"
	// A workspace-write turn receives only its stage-attempt-owned work root.
	// The frozen source remains outside that writable root and is copied by
	// value into work/source. Host-owned authoring tools use work/task for fixed
	// candidate paths, so repository files cannot collide with task artifacts.
	StandardAuthoringCodexRunAttemptsDirectory   = "attempts"
	StandardAuthoringCodexAttemptWorkDirectory   = "work"
	StandardAuthoringCodexAttemptSourceDirectory = "source"
	StandardAuthoringCodexAttemptTaskDirectory   = "task"
)

// StandardAuthoringCodexAgentTurnExecutorConfig contains only composition-owned
// facts. InvocationFactory must obtain an invocation immediately before the
// App Server conversation opens. ProgramByStage closes the prompt-choice
// surface: an operation can only use the prompt program registered for its
// frozen stage key.
type StandardAuthoringCodexAgentTurnExecutorConfig struct {
	InvocationFactory  StandardAuthoringCodexInvocationFactory
	WorkspaceRoot      string
	WorkspaceMode      StandardAuthoringCodexWorkspaceMode
	SourceVerifier     StandardAuthoringCodexFrozenSourceVerifier
	ProgramByStage     map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram
	RuntimeFactory     StandardAuthoringCodexRuntimeFactory
	CandidateValidator StandardAuthoringCandidateValidationTool
	Now                func() time.Time
}

// StandardAuthoringCandidateValidationRequest carries only host-captured
// candidate bytes and frozen identities into the verifier.
type StandardAuthoringCandidateValidationRequest struct {
	RunID                string
	StageAttemptID       string
	Snapshot             workflowkit.CandidateSnapshot
	Files                map[string][]byte
	VerificationContract []byte
}

// StandardAuthoringCandidateValidationTool validates a candidate without
// exposing a workspace path, command, environment, or raw tool output.
type StandardAuthoringCandidateValidationTool interface {
	ValidateStandardAuthoringCandidate(context.Context, StandardAuthoringCandidateValidationRequest) (workflowkit.ValidationReceipt, error)
}

// StandardAuthoringCodexAgentTurnExecutor invokes only a locked Codex App
// Server. It never reads os.Environ, accepts an ad-hoc prompt, logs a provider
// response, or exposes a raw provider error to a durable StageExecutionResult.
type StandardAuthoringCodexAgentTurnExecutor struct {
	invocationFactory  StandardAuthoringCodexInvocationFactory
	workspaceRoot      string
	workspaceMode      StandardAuthoringCodexWorkspaceMode
	sourceVerifier     StandardAuthoringCodexFrozenSourceVerifier
	programByStage     map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram
	runtimeFactory     StandardAuthoringCodexRuntimeFactory
	candidateValidator StandardAuthoringCandidateValidationTool
	now                func() time.Time
}

// NewStandardAuthoringCodexAgentTurnExecutor constructs a controlled Codex
// agent.turn handler. Dynamic byte/version verification remains the catalog
// attestor's responsibility, but InvocationFactory is called by ExecuteAgentTurn
// immediately before it opens an App Server conversation. This prevents a
// composition from caching an invocation or its controlled environment across
// effects.
func NewStandardAuthoringCodexAgentTurnExecutor(config StandardAuthoringCodexAgentTurnExecutorConfig) (*StandardAuthoringCodexAgentTurnExecutor, error) {
	if isNilInterface(config.InvocationFactory) {
		return nil, fmt.Errorf("%w: invocation factory is required", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	workspaceRoot, err := standardAuthoringCodexWorkspaceRoot(config.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	workspaceMode, err := standardAuthoringCodexWorkspaceMode(config.WorkspaceMode)
	if err != nil {
		return nil, err
	}
	if workspaceMode == StandardAuthoringCodexWorkspaceRunScoped && isNilInterface(config.SourceVerifier) {
		return nil, fmt.Errorf("%w: RunScoped frozen source verifier is required", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if len(config.ProgramByStage) == 0 {
		return nil, fmt.Errorf("%w: at least one stage prompt program is required", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	programs := make(map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram, len(config.ProgramByStage))
	for stageKey, program := range config.ProgramByStage {
		if err := standardAuthoringCodexToken("program stage key", string(stageKey)); err != nil {
			return nil, err
		}
		if err := program.validate(); err != nil {
			return nil, err
		}
		if program.Fingerprint == "" {
			return nil, fmt.Errorf("%w: stage %q prompt program fingerprint is required", ErrStandardAuthoringCodexAgentTurnConfiguration, stageKey)
		}
		programs[stageKey] = program.clone()
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &StandardAuthoringCodexAgentTurnExecutor{
		invocationFactory: config.InvocationFactory, workspaceRoot: workspaceRoot, workspaceMode: workspaceMode, sourceVerifier: config.SourceVerifier, programByStage: programs,
		runtimeFactory: config.RuntimeFactory, candidateValidator: config.CandidateValidator, now: now,
	}, nil
}

type standardAuthoringCodexTurnCompletion struct {
	result agent.TurnResult
	err    error
}

// standardAuthoringCodexRunTurnUntilAccepted gives a successful private tool
// submission precedence over an App Server that continues sampling after the
// tool receipt. The derived context is canceled only after the accepted result
// is immutable. Close is then an active-turn termination barrier, and Turn is
// awaited before handing control back for source re-attestation and
// materialization.
func standardAuthoringCodexRunTurnUntilAccepted(ctx context.Context, conversation agent.Conversation, request agent.TurnRequest, accepted <-chan workflowkit.StageExecutionResult) (agent.TurnResult, error, workflowkit.StageExecutionResult, bool, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make(chan standardAuthoringCodexTurnCompletion, 1)
	go func() {
		result, err := conversation.Turn(turnCtx, request)
		completed <- standardAuthoringCodexTurnCompletion{result: result, err: err}
	}()

	select {
	case completion := <-completed:
		return completion.result, completion.err, workflowkit.StageExecutionResult{}, false, nil
	case result := <-accepted:
		cancel()
		closeErr := conversation.Close()
		completion := <-completed
		return completion.result, completion.err, result, true, closeErr
	}
}

// ExecuteAgentTurn implements AgentTurnOperationExecutor. It consumes every
// declared frozen input through ReadInput, records secret-free checkpoints and
// quota events for every real App Server turn, and admits only an artifact
// accepted by its private validate-and-submit dynamic tool. Free assistant
// text is never an output authority.
func (executor *StandardAuthoringCodexAgentTurnExecutor) ExecuteAgentTurn(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload) (workflowkit.StageExecutionResult, error) {
	if executor == nil || isNilInterface(executor.invocationFactory) {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	request := invocation.Request
	program, failure := executor.validateExecutionRequest(invocation, payload)
	if failure != "" {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, failure), nil
	}
	if request.Stage.AgentRole != nil {
		return executor.executeV3AgentTurn(ctx, invocation, payload, program)
	}
	// No retired Standard Authoring stage is executable after the 3.0 cutover.
	// The remaining branch is retained only until its source is removed with the
	// historical test fixtures; it is deliberately unreachable from a frozen
	// installed template.
	return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
}

func (executor *StandardAuthoringCodexAgentTurnExecutor) validateExecutionRequest(invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload) (StandardAuthoringCodexTurnProgram, string) {
	request := invocation.Request
	resolvedPayload, isAgentTurn := invocation.Resolution.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
	if invocation.Resolution.StageKey != request.Stage.Key || !isAgentTurn || resolvedPayload != payload {
		return StandardAuthoringCodexTurnProgram{}, standardAuthoringCodexFailureConfiguration
	}
	program, found := executor.programByStage[request.Stage.Key]
	if !found || program.Fingerprint == "" {
		return StandardAuthoringCodexTurnProgram{}, standardAuthoringCodexFailureConfiguration
	}
	expectedAgentTurns, hasAgentTurnQuota := standardAuthoringCodexAgentTurnQuota(request.Stage)
	if request.Stage.AgentRole == nil || request.Stage.AgentRole.MaxTurns != payload.MaxTurns || !IsCodexAppServerProductionPayload(payload) || payload.MaxTurns != len(program.TurnPrompts) || payload.MaxTurns > request.Stage.Budget.MaxTurns || !hasAgentTurnQuota || int64(payload.MaxTurns) != expectedAgentTurns {
		return StandardAuthoringCodexTurnProgram{}, standardAuthoringCodexFailureConfiguration
	}
	if request.Claim.Stage == nil || strings.TrimSpace(string(request.Claim.Stage.StageAttempt.ID)) == "" || request.Checkpoint == nil || request.Charge == nil {
		return StandardAuthoringCodexTurnProgram{}, standardAuthoringCodexFailureConfiguration
	}
	if request.Stage.Budget.TurnTimeout <= 0 {
		return StandardAuthoringCodexTurnProgram{}, standardAuthoringCodexFailureConfiguration
	}
	if len(request.Inputs) > 0 && request.ReadInput == nil {
		return StandardAuthoringCodexTurnProgram{}, standardAuthoringCodexFailureConfiguration
	}
	return program.clone(), ""
}

// runtimeForEffect obtains the invocation only after all frozen inputs have
// been checked and the canonical request has been built. That keeps the
// attestation-to-OpenConversation interval small and ensures a fresh runtime
// environment is derived from each successful attestation rather than cached
// at provider composition time.
func (executor *StandardAuthoringCodexAgentTurnExecutor) runtimeForEffect(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload) (CodexAppServerInvocation, agent.Runtime, string) {
	if executor == nil || isNilInterface(executor.invocationFactory) {
		return CodexAppServerInvocation{}, nil, standardAuthoringCodexFailureConfiguration
	}
	attestedInvocation, err := executor.invocationFactory(ctx, invocation.clone(), payload)
	if err != nil {
		return CodexAppServerInvocation{}, nil, standardAuthoringCodexFailureConfiguration
	}
	if err := validateStandardAuthoringCodexInvocation(attestedInvocation); err != nil || attestedInvocation.AgentID != payload.AgentID || attestedInvocation.ModelID != payload.ModelID || attestedInvocation.ReasoningEffort != payload.ReasoningEffort {
		return CodexAppServerInvocation{}, nil, standardAuthoringCodexFailureConfiguration
	}
	runtimeFactory := executor.runtimeFactory
	if runtimeFactory != nil {
		runtime := runtimeFactory(attestedInvocation)
		if isNilInterface(runtime) {
			return CodexAppServerInvocation{}, nil, standardAuthoringCodexFailureConfiguration
		}
		return attestedInvocation, runtime, ""
	}
	runtime := codexruntime.New(nil, attestedInvocation.JavaScriptLauncherPath, attestedInvocation.Environment())
	return attestedInvocation, runtime, ""
}

func standardAuthoringCodexAgentTurnQuota(stage workflowkit.StageDescriptor) (int64, bool) {
	return standardAuthoringCodexQuotaClaim(stage, "agent_turn")
}

func standardAuthoringCodexQuotaClaim(stage workflowkit.StageDescriptor, dimension string) (int64, bool) {
	var units int64
	found := false
	for _, claim := range stage.QuotaClaims {
		if claim.Dimension != dimension {
			continue
		}
		if found || claim.Units <= 0 {
			return 0, false
		}
		units = claim.Units
		found = true
	}
	return units, found
}

func (executor *StandardAuthoringCodexAgentTurnExecutor) checkpoint(ctx context.Context, request workflowkit.StageExecutionRequest, program StandardAuthoringCodexTurnProgram, inputFingerprint workflowkit.Fingerprint, turn int, substep, responseDigest string, transcript *workflowkit.AgentTurnTranscript) error {
	payload, err := json.Marshal(struct {
		Format             string                  `json:"format"`
		ProgramFingerprint workflowkit.Fingerprint `json:"program_fingerprint"`
		InputFingerprint   workflowkit.Fingerprint `json:"input_fingerprint"`
		ResponseDigest     workflowkit.Fingerprint `json:"response_digest,omitempty"`
		Turn               int                     `json:"turn"`
		Substep            string                  `json:"substep"`
	}{
		Format: standardAuthoringCodexCheckpointFormat, ProgramFingerprint: program.Fingerprint, InputFingerprint: inputFingerprint,
		ResponseDigest: workflowkit.Fingerprint(responseDigest), Turn: turn, Substep: substep,
	})
	if err != nil {
		return err
	}
	checkpoint := workflowkit.StageCheckpoint{
		CheckpointID:   standardAuthoringCodexCheckpointKey(request, turn, substep),
		IdempotencyKey: standardAuthoringCodexCheckpointKey(request, turn, substep),
		TurnOrdinal:    turn,
		Substep:        substep,
		Payload:        payload,
		// A Codex App Server conversation is intentionally ephemeral. Its retained
		// diagnostic transcript is not resumable conversation state, so recovery
		// must create a new fenced stage attempt.
		Resumable:  false,
		OccurredAt: executor.now().UTC(),
	}
	if transcript != nil {
		value := transcript.Clone()
		checkpoint.AgentTurnTranscript = &value
	}
	_, err = request.Checkpoint(ctx, checkpoint)
	return err
}

func validateStandardAuthoringCodexInvocation(invocation CodexAppServerInvocation) error {
	for label, value := range map[string]string{
		"agent id": invocation.AgentID, "agent version": invocation.AgentVersion, "model version": invocation.ModelVersion,
		"JavaScript launcher path": invocation.JavaScriptLauncherPath, "Node executable path": invocation.NodeExecutablePath,
		"Codex home directory": invocation.CodexHomeDirectory, "CLI version output": invocation.CLIVersionOutput,
	} {
		if err := standardAuthoringCodexToken(label, value); err != nil {
			return err
		}
	}
	if invocation.AgentID != CodexAppServerProductionAgentID || invocation.ModelID != CodexAppServerProductionModelID || invocation.ReasoningEffort != CodexAppServerProductionReasoningEffort || invocation.ApprovalPolicy != CodexAppServerApprovalPolicyNever || !validCodexAppServerSandbox(invocation.SandboxMode, invocation.SandboxPolicy) || invocation.NetworkAccess || !filepath.IsAbs(invocation.JavaScriptLauncherPath) || !filepath.IsAbs(invocation.NodeExecutablePath) || !filepath.IsAbs(invocation.CodexHomeDirectory) {
		return fmt.Errorf("%w: invocation does not satisfy the locked Codex production policy", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return nil
}

func standardAuthoringCodexInvocationWorkspaceWrite(invocation CodexAppServerInvocation) bool {
	return invocation.SandboxMode == CodexAppServerSandboxModeWorkspaceWrite && invocation.SandboxPolicy == CodexAppServerSandboxPolicyWorkspaceWrite
}

// StandardAuthoringCodexSandboxForWorkspace maps the frozen workspace
// capability to the only Codex App Server sandbox that may realize it. The
// lock builder and executor share this mapping so a stage cannot be given a
// broader sandbox than the descriptor permits.
func StandardAuthoringCodexSandboxForWorkspace(mode workflowkit.WorkspaceMode) (string, string, error) {
	switch mode {
	case workflowkit.WorkspaceExclusiveWriter:
		return CodexAppServerSandboxModeWorkspaceWrite, CodexAppServerSandboxPolicyWorkspaceWrite, nil
	case workflowkit.WorkspaceNone, workflowkit.WorkspaceReadOnlySnapshot:
		return CodexAppServerSandboxModeReadOnly, CodexAppServerSandboxPolicyReadOnly, nil
	default:
		return "", "", fmt.Errorf("%w: unsupported Standard authoring workspace mode %q", ErrStandardAuthoringCodexAgentTurnConfiguration, mode)
	}
}

func standardAuthoringCodexWorkspaceRoot(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%w: workspace root is required", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	abs, err := filepath.Abs(value)
	if err != nil || filepath.Clean(abs) != abs {
		return "", fmt.Errorf("%w: workspace root must be a clean absolute directory", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: workspace root must be an existing non-symlink directory", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return abs, nil
}

func standardAuthoringCodexWorkspaceMode(value StandardAuthoringCodexWorkspaceMode) (StandardAuthoringCodexWorkspaceMode, error) {
	if value == "" {
		return StandardAuthoringCodexWorkspaceStatic, nil
	}
	switch value {
	case StandardAuthoringCodexWorkspaceStatic, StandardAuthoringCodexWorkspaceRunScoped:
		return value, nil
	default:
		return "", fmt.Errorf("%w: workspace mode is invalid", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
}

// workspaceForExecution derives the sole directory a RunScoped Codex turn may
// inspect. The frozen execution ID is Store-issued UUIDv7 data; revalidating
// it here prevents malformed opaque bindings from becoming filesystem paths.
// repo_prepare creates and verifies this directory from the immutable source
// snapshot before an agent turn becomes dispatchable.
func (executor *StandardAuthoringCodexAgentTurnExecutor) workspaceForExecution(executionID string) (string, error) {
	if executor == nil {
		return "", fmt.Errorf("%w: executor is required", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if executor.workspaceMode == StandardAuthoringCodexWorkspaceStatic {
		return executor.workspaceRoot, nil
	}
	parsed, err := uuid.Parse(strings.TrimSpace(executionID))
	if err != nil || parsed.Version() != 7 {
		return "", fmt.Errorf("%w: RunScoped workspace requires a UUIDv7 execution ID", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	runRoot := filepath.Join(executor.workspaceRoot, parsed.String())
	workspace := filepath.Join(runRoot, StandardAuthoringCodexRunSourceDirectory)
	if !standardAuthoringPathWithin(executor.workspaceRoot, workspace) {
		return "", fmt.Errorf("%w: RunScoped workspace escapes its managed root", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	for _, candidate := range []string{runRoot, workspace} {
		info, statErr := os.Lstat(candidate)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%w: RunScoped workspace is unavailable", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
	}
	return workspace, nil
}

// StandardAuthoringAttemptWorkspacePath derives the only writable root for a
// Standard Authoring stage attempt. authoringWorkspaceRoot is the value
// returned by app.StandardAuthoringCodexWorkspaceRoot, not the broader managed
// root. Missing attempt descendants are allowed so the executor can create the
// path atomically; every ancestor that already exists is required to be a real
// directory rather than a symbolic link.
func StandardAuthoringAttemptWorkspacePath(authoringWorkspaceRoot, runID string, stageKey workflowkit.StageKey, stageAttemptID string) (string, error) {
	root, err := standardAuthoringCodexWorkspaceRoot(authoringWorkspaceRoot)
	if err != nil {
		return "", err
	}
	rootInfo, err := inspectLockedLocalExecutablePath(root)
	if err != nil || !rootInfo.IsDir() {
		return "", fmt.Errorf("%w: authoring workspace root has an unsafe path component", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	parsed, err := uuid.Parse(strings.TrimSpace(runID))
	if err != nil || parsed.Version() != 7 {
		return "", fmt.Errorf("%w: attempt workspace requires a UUIDv7 execution ID", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	stageComponent := strings.TrimSpace(string(stageKey))
	attemptComponent := strings.TrimSpace(stageAttemptID)
	if !standardAuthoringCodexSafePathComponent(stageComponent) || !standardAuthoringCodexSafePathComponent(attemptComponent) {
		return "", fmt.Errorf("%w: attempt workspace identity is not a safe path component", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	workspace := filepath.Join(root, parsed.String(), StandardAuthoringCodexRunAttemptsDirectory, stageComponent, attemptComponent, StandardAuthoringCodexAttemptWorkDirectory)
	if !standardAuthoringPathWithin(root, workspace) {
		return "", fmt.Errorf("%w: attempt workspace escapes its managed root", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := standardAuthoringCodexVerifyExistingDirectoryChain(root, workspace, false); err != nil {
		return "", err
	}
	return workspace, nil
}

// ValidateStandardAuthoringAttemptWorkspacePath proves that candidate is the
// exact, already materialized work root for the supplied durable identities.
// It also checks the fixed source and task roots used by the agent and host
// harness. Callers never accept a model-selected path.
func ValidateStandardAuthoringAttemptWorkspacePath(authoringWorkspaceRoot, runID string, stageKey workflowkit.StageKey, stageAttemptID, candidate string) error {
	expected, err := StandardAuthoringAttemptWorkspacePath(authoringWorkspaceRoot, runID, stageKey, stageAttemptID)
	if err != nil {
		return err
	}
	root, err := standardAuthoringCodexWorkspaceRoot(authoringWorkspaceRoot)
	if err != nil {
		return err
	}
	candidateAbs, err := filepath.Abs(strings.TrimSpace(candidate))
	if err != nil || filepath.Clean(candidateAbs) != candidateAbs || candidateAbs != expected {
		return fmt.Errorf("%w: attempt workspace path does not match its durable identity", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := standardAuthoringCodexVerifyExistingDirectoryChain(root, expected, true); err != nil {
		return err
	}
	for _, directory := range []string{StandardAuthoringCodexAttemptSourceDirectory, StandardAuthoringCodexAttemptTaskDirectory} {
		path := filepath.Join(expected, directory)
		info, inspectErr := inspectLockedLocalExecutablePath(path)
		if inspectErr != nil || !info.IsDir() {
			return fmt.Errorf("%w: attempt workspace fixed directory is unavailable or unsafe", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
	}
	return standardAuthoringCodexValidateWorkTree(expected)
}

func (executor *StandardAuthoringCodexAgentTurnExecutor) prepareAttemptWorkspace(ctx context.Context, request workflowkit.StageExecutionRequest, sourceRoot string) (string, error) {
	if executor == nil || request.Claim.Stage == nil {
		return "", fmt.Errorf("%w: attempt workspace request is incomplete", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	workRoot, err := StandardAuthoringAttemptWorkspacePath(executor.workspaceRoot, request.Execution.ID, request.Stage.Key, string(request.Claim.Stage.StageAttempt.ID))
	if err != nil {
		return "", err
	}
	attemptRoot := filepath.Dir(workRoot)
	stageRoot := filepath.Dir(attemptRoot)
	if err := standardAuthoringCodexEnsureDirectory(filepath.Dir(stageRoot), 0o750); err != nil {
		return "", err
	}
	if err := standardAuthoringCodexEnsureDirectory(stageRoot, 0o750); err != nil {
		return "", err
	}
	if _, err := os.Lstat(attemptRoot); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: attempt workspace already exists", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	staging, err := os.MkdirTemp(stageRoot, ".attempt-")
	if err != nil {
		return "", fmt.Errorf("%w: create attempt workspace staging directory", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	published := false
	defer func() {
		if !published {
			standardAuthoringCodexRemoveTree(staging)
		}
	}()
	stagedWorkRoot := filepath.Join(staging, StandardAuthoringCodexAttemptWorkDirectory)
	if err := os.Mkdir(stagedWorkRoot, 0o750); err != nil {
		return "", fmt.Errorf("%w: create staged work root", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := standardAuthoringCodexCopySourceTree(ctx, sourceRoot, filepath.Join(stagedWorkRoot, StandardAuthoringCodexAttemptSourceDirectory)); err != nil {
		return "", err
	}
	if err := os.Mkdir(filepath.Join(stagedWorkRoot, StandardAuthoringCodexAttemptTaskDirectory), 0o750); err != nil {
		return "", fmt.Errorf("%w: create staged task root", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	// Codex's Linux sandbox keeps its private bookkeeping under work/.codex.
	// Keep the copied source sealed, but leave the attempt root owner-writable
	// so that bookkeeping cannot fail before the model reaches task/.
	if err := os.Chmod(stagedWorkRoot, 0o700); err != nil {
		return "", fmt.Errorf("%w: prepare staged work root", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := os.Rename(staging, attemptRoot); err != nil {
		return "", fmt.Errorf("%w: publish attempt workspace", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := ValidateStandardAuthoringAttemptWorkspacePath(executor.workspaceRoot, request.Execution.ID, request.Stage.Key, string(request.Claim.Stage.StageAttempt.ID), workRoot); err != nil {
		standardAuthoringCodexRemoveTree(attemptRoot)
		return "", err
	}
	published = true
	return workRoot, nil
}

func standardAuthoringCodexCopySourceTree(ctx context.Context, sourceRoot, destinationRoot string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := filepath.WalkDir(sourceRoot, func(sourcePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return fmt.Errorf("%w: source copy encountered an unsafe entry", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		relative, err := filepath.Rel(sourceRoot, sourcePath)
		if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: source copy entry escapes its root", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		destinationPath := filepath.Join(destinationRoot, relative)
		if !standardAuthoringPathWithin(filepath.Dir(destinationRoot), destinationPath) {
			return fmt.Errorf("%w: destination copy entry escapes its work root", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return standardAuthoringCodexCopySourceLink(sourceRoot, sourcePath, destinationPath)
		}
		if info.IsDir() {
			if err := os.Mkdir(destinationPath, 0o750); err != nil {
				return fmt.Errorf("%w: create source-copy directory", ErrStandardAuthoringCodexAgentTurnConfiguration)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: source copy encountered a non-regular entry", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		return standardAuthoringCodexCopyRegularFile(ctx, sourcePath, destinationPath, info)
	}); err != nil {
		return err
	}
	return standardAuthoringCodexSealSourceCopy(destinationRoot)
}

func standardAuthoringCodexCopySourceLink(sourceRoot, sourcePath, destinationPath string) error {
	target, err := os.Readlink(sourcePath)
	if err != nil || !standardAuthoringCodexSafeSourceLink(sourceRoot, sourcePath, target) {
		return fmt.Errorf("%w: source copy encountered an unsafe symbolic link", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := os.Symlink(target, destinationPath); err != nil {
		return fmt.Errorf("%w: create source-copy symbolic link", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return nil
}

func standardAuthoringCodexSealSourceCopy(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: inspect source-copy entry", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%w: source-copy entry is unsafe", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil || !standardAuthoringCodexSafeSourceLink(root, path, target) {
				return fmt.Errorf("%w: source-copy symbolic link is unsafe", ErrStandardAuthoringCodexAgentTurnConfiguration)
			}
			return nil
		}
		if info.IsDir() {
			// The sealed source is bind-mounted into a rootless verification
			// container. Keep it immutable while allowing that container to
			// traverse every source directory.
			if err := os.Chmod(path, 0o555); err != nil {
				return fmt.Errorf("%w: seal source-copy directory", ErrStandardAuthoringCodexAgentTurnConfiguration)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: source-copy entry is non-regular", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		if err := os.Chmod(path, 0o444); err != nil {
			return fmt.Errorf("%w: seal source-copy file", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		return nil
	})
}

func standardAuthoringCodexRemoveTree(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(root)
}

func standardAuthoringCodexCopyRegularFile(ctx context.Context, sourcePath, destinationPath string, expected os.FileInfo) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("%w: open source-copy file", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	defer source.Close()
	openedInfo, err := source.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(expected, openedInfo) {
		return fmt.Errorf("%w: source-copy file changed while opening", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w: create source-copy file", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	_, copyErr := io.Copy(destination, &standardAuthoringCodexContextReader{ctx: ctx, reader: source})
	closeErr := destination.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("%w: copy source file", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	finalSourceInfo, err := source.Stat()
	if err != nil || !os.SameFile(openedInfo, finalSourceInfo) {
		return fmt.Errorf("%w: source-copy file changed while reading", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return nil
}

type standardAuthoringCodexContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *standardAuthoringCodexContextReader) Read(buffer []byte) (int, error) {
	if err := contextError(reader.ctx); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func standardAuthoringCodexEnsureDirectory(path string, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: create attempt workspace directory", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: attempt workspace directory is unavailable or unsafe", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return nil
}

func standardAuthoringCodexVerifyExistingDirectoryChain(root, candidate string, requireCandidate bool) error {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("%w: attempt workspace path escapes its root", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	current := filepath.Clean(root)
	components := strings.Split(relative, string(filepath.Separator))
	for index, component := range components {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if requireCandidate {
				return fmt.Errorf("%w: attempt workspace path is unavailable", ErrStandardAuthoringCodexAgentTurnConfiguration)
			}
			return nil
		}
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: attempt workspace path contains an unsafe component", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		if index == len(components)-1 {
			return nil
		}
	}
	return nil
}

func standardAuthoringCodexSafePathComponent(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > 255 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func standardAuthoringCodexValidateWorkTree(root string) error {
	sourceRoot := filepath.Join(root, StandardAuthoringCodexAttemptSourceDirectory)
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: inspect attempt workspace entry", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%w: inspect attempt workspace entry", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if !standardAuthoringCodexPathWithinOrEqual(sourceRoot, path) || readErr != nil || !standardAuthoringCodexSafeSourceLink(sourceRoot, path, target) {
				return fmt.Errorf("%w: attempt workspace contains an unsafe symbolic link", ErrStandardAuthoringCodexAgentTurnConfiguration)
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: attempt workspace contains a non-regular entry", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 1 {
			return fmt.Errorf("%w: attempt workspace contains a hard-linked file", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		return nil
	})
}

func standardAuthoringCodexSafeSourceLink(sourceRoot, linkPath, target string) bool {
	if target == "" || filepath.IsAbs(target) || !standardAuthoringCodexPathWithinOrEqual(sourceRoot, linkPath) {
		return false
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
	return standardAuthoringCodexPathWithinOrEqual(sourceRoot, resolved)
}

func standardAuthoringCodexPathWithinOrEqual(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func (executor *StandardAuthoringCodexAgentTurnExecutor) verifyFrozenSource(ctx context.Context, execution workflowkit.FrozenExecution, root string) (workflowkit.Fingerprint, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if executor != nil && executor.workspaceMode == StandardAuthoringCodexWorkspaceRunScoped {
		if isNilInterface(executor.sourceVerifier) {
			return "", fmt.Errorf("%w: RunScoped frozen source verifier is unavailable", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		identity, err := executor.sourceVerifier.VerifyStandardAuthoringCodexFrozenSource(ctx, execution, root)
		if err != nil {
			return "", err
		}
		if err := identity.Validate(); err != nil {
			return "", fmt.Errorf("%w: frozen source verifier returned an invalid identity", ErrStandardAuthoringCodexAgentTurnConfiguration)
		}
		return identity, nil
	}
	return standardAuthoringCodexSourceTreeIdentity(root)
}

func standardAuthoringCodexSourceTreeIdentity(root string) (workflowkit.Fingerprint, error) {
	type sourceEntry struct {
		Path          string                  `json:"path"`
		Type          string                  `json:"type"`
		Mode          uint32                  `json:"mode"`
		ContentSHA256 workflowkit.Fingerprint `json:"content_sha256,omitempty"`
	}
	entries := []sourceEntry{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe source workspace entry")
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular source workspace entry")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("source workspace entry escapes root")
		}
		identity := sourceEntry{Path: filepath.ToSlash(relative), Mode: uint32(info.Mode().Perm())}
		if info.IsDir() {
			identity.Type = "directory"
			if info.Mode().Perm() != 0o555 {
				return fmt.Errorf("source workspace directory mode changed")
			}
		} else {
			identity.Type = "regular"
			if info.Mode().Perm() != 0o444 {
				return fmt.Errorf("source workspace file mode changed")
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			identity.ContentSHA256 = workflowkit.SHA256Fingerprint(contents)
		}
		entries = append(entries, identity)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("%w: source workspace is unavailable or unsafe", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Path < entries[right].Path })
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("%w: encode source workspace identity", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return workflowkit.FingerprintBytes("harbor.standard-authoring-codex-source-tree.v1", encoded)
}

func standardAuthoringPathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func standardAuthoringCodexToken(label, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%w: %s is required and must not contain control characters", ErrStandardAuthoringCodexAgentTurnConfiguration, label)
	}
	return nil
}

func standardAuthoringCodexTimeoutSeconds(timeout time.Duration) int {
	seconds := int(timeout / time.Second)
	if timeout%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}

func standardAuthoringCodexExecutionKey(request workflowkit.StageExecutionRequest, turn int, substep string) string {
	stageAttemptID := ""
	if request.Claim.Stage != nil {
		stageAttemptID = string(request.Claim.Stage.StageAttempt.ID)
	}
	fingerprint, err := workflowkit.FingerprintParts("harbor.standard-authoring-codex-turn-key.v1", []workflowkit.FingerprintPart{
		{Name: "execution_id", Value: []byte(request.Execution.ID)}, {Name: "stage_attempt_id", Value: []byte(stageAttemptID)},
		{Name: "stage_key", Value: []byte(request.Stage.Key)}, {Name: "turn", Value: []byte(strconv.Itoa(turn))}, {Name: "substep", Value: []byte(substep)},
	})
	if err != nil {
		return "invalid"
	}
	return string(fingerprint)
}

func standardAuthoringCodexCheckpointKey(request workflowkit.StageExecutionRequest, turn int, substep string) string {
	return "standard-authoring-codex-checkpoint:" + standardAuthoringCodexExecutionKey(request, turn, substep)
}

func standardAuthoringCodexUsageKey(request workflowkit.StageExecutionRequest, turn int) string {
	return "standard-authoring-codex-usage:" + standardAuthoringCodexExecutionKey(request, turn, "usage")
}

func standardAuthoringCodexFailure(class workflowkit.FailureClass, code string) workflowkit.StageExecutionResult {
	return workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: class}, ErrorText: code,
	}
}

func standardAuthoringCodexInterrupted() workflowkit.StageExecutionResult {
	return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInterrupted}, ErrorText: standardAuthoringCodexFailureInterrupted}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

var _ AgentTurnOperationExecutor = (*StandardAuthoringCodexAgentTurnExecutor)(nil)
