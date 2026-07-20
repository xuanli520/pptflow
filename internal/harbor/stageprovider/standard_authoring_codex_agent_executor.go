package stageprovider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/runtime/codexruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringCodexTurnRequestFormat is the sole structured document
	// sent to a Codex authoring turn. It carries frozen artifact bytes only as
	// standard base64. Dockerfile turns additionally receive the already validated
	// non-secret base-image coordinate as host-derived metadata so the model does
	// not have to guess a value that the submission authority will reject.
	StandardAuthoringCodexTurnRequestFormat  = "harbor.standard-authoring-codex-turn-request.v1"
	StandardAuthoringCodexTurnRequestVersion = "1"

	standardAuthoringCodexCheckpointFormat = "harbor.standard-authoring-codex-turn-checkpoint.v1"

	standardAuthoringCodexFailureConfiguration = "standard_authoring_codex_agent_turn.configuration"
	standardAuthoringCodexFailureInput         = "standard_authoring_codex_agent_turn.input"
	standardAuthoringCodexFailureCheckpoint    = "standard_authoring_codex_agent_turn.checkpoint"
	standardAuthoringCodexFailureQuota         = "standard_authoring_codex_agent_turn.quota"
	standardAuthoringCodexFailureRuntime       = "standard_authoring_codex_agent_turn.runtime"
	standardAuthoringCodexFailureSource        = "standard_authoring_codex_agent_turn.source_integrity"
	standardAuthoringCodexFailureOutput        = "standard_authoring_codex_agent_turn.output"
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

const standardAuthoringCodexOutputSchemaCanonicalJSON = `{"$id":"harbor.standard-authoring-codex-stage-output.v1","$schema":"http://json-schema.org/draft-07/schema#","additionalProperties":false,"properties":{"artifacts":{"items":{"additionalProperties":false,"properties":{"content_base64":{"type":"string"}},"required":["content_base64"],"type":"object"},"type":"array"},"verdict":{"type":"string"}},"required":["verdict","artifacts"],"type":"object"}`

// StandardAuthoringCodexOutputSchemaFingerprint exposes the identity of the
// real JSON Schema template pinned by deployment materials. The executor
// derives a stricter per-stage schema from this closed template and the frozen
// StageDescriptor immediately before opening its App Server conversation.
func StandardAuthoringCodexOutputSchemaFingerprint() workflowkit.Fingerprint {
	fingerprint, err := workflowkit.FingerprintBytes("harbor.standard-authoring-codex-stage-output-schema.v1", []byte(standardAuthoringCodexOutputSchemaCanonicalJSON))
	if err != nil {
		panic("fixed Standard Authoring Codex output schema fingerprint: " + err.Error())
	}
	return fingerprint
}

func standardAuthoringCodexOutputSchemaTemplate() []byte {
	return []byte(standardAuthoringCodexOutputSchemaCanonicalJSON)
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

// ValidateStandardAuthoringCodexOutputSchemaAsset accepts only the exact
// versioned JSON Schema template used to generate turn/start.outputSchema. It
// accepts at most one POSIX terminal LF under the same raw-byte-locking rule as
// the prompt asset. A field-list document is intentionally not accepted: the
// App Server requires an actual JSON Schema value.
func ValidateStandardAuthoringCodexOutputSchemaAsset(raw []byte) error {
	if len(raw) == 0 || len(raw) > standardAuthoringCodexContractAssetLimit {
		return fmt.Errorf("%w: output schema asset has invalid size", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return fmt.Errorf("%w: output schema asset has duplicate fields", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if !json.Valid(raw) || !bytes.Equal(standardAuthoringCodexCanonicalAssetBody(raw), standardAuthoringCodexOutputSchemaTemplate()) {
		return fmt.Errorf("%w: output schema asset is not the locked JSON Schema template", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return nil
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
)

// StandardAuthoringCodexAgentTurnExecutorConfig contains only composition-owned
// facts. InvocationFactory must obtain an invocation immediately before the
// App Server conversation opens. ProgramByStage closes the prompt-choice
// surface: an operation can only use the prompt program registered for its
// frozen stage key.
type StandardAuthoringCodexAgentTurnExecutorConfig struct {
	InvocationFactory StandardAuthoringCodexInvocationFactory
	WorkspaceRoot     string
	WorkspaceMode     StandardAuthoringCodexWorkspaceMode
	SourceVerifier    StandardAuthoringCodexFrozenSourceVerifier
	ProgramByStage    map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram
	RuntimeFactory    StandardAuthoringCodexRuntimeFactory
	Now               func() time.Time
}

// StandardAuthoringCodexAgentTurnExecutor invokes only a locked Codex App
// Server. It never reads os.Environ, accepts an ad-hoc prompt, logs a provider
// response, or exposes a raw provider error to a durable StageExecutionResult.
type StandardAuthoringCodexAgentTurnExecutor struct {
	invocationFactory StandardAuthoringCodexInvocationFactory
	workspaceRoot     string
	workspaceMode     StandardAuthoringCodexWorkspaceMode
	sourceVerifier    StandardAuthoringCodexFrozenSourceVerifier
	programByStage    map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram
	runtimeFactory    StandardAuthoringCodexRuntimeFactory
	now               func() time.Time
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
		runtimeFactory: config.RuntimeFactory, now: now,
	}, nil
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
	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexInterrupted(), nil
	}
	inputs, inputFingerprint, err := standardAuthoringCodexReadInputs(ctx, request)
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePermanent, standardAuthoringCodexFailureInput), nil
	}
	environmentPolicy, err := standardAuthoringCodexDockerfileEnvironmentPolicy(request.Stage, inputs)
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePermanent, standardAuthoringCodexFailureInput), nil
	}
	requestDocument, err := standardAuthoringCodexRequestDocument(request, program, inputs, inputFingerprint, environmentPolicy)
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	workspace, err := executor.workspaceForExecution(request.Execution.ID)
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	maxSubmissions, hasSubmissionQuota := standardAuthoringCodexOutputSubmissionQuota(request.Stage)
	if !hasSubmissionQuota {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	submission, err := newStandardAuthoringCodexOutputSubmission(request, program.MaxOutputBytes, int(maxSubmissions), executor.now, environmentPolicy)
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	attestedInvocation, runtime, failure := executor.runtimeForEffect(ctx, invocation, payload)
	if failure != "" {
		if contextError(ctx) != nil {
			return standardAuthoringCodexInterrupted(), nil
		}
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, failure), nil
	}
	sourceIdentity, err := executor.verifyFrozenSource(ctx, request.Execution, workspace)
	if err != nil {
		if contextError(ctx) != nil {
			return standardAuthoringCodexInterrupted(), nil
		}
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureSource), nil
	}
	conversation, err := runtime.OpenConversation(ctx, agent.ConversationRequest{
		ProjectPath:       workspace,
		Model:             attestedInvocation.ModelID,
		ReasoningEffort:   string(attestedInvocation.ReasoningEffort),
		SandboxMode:       attestedInvocation.SandboxMode,
		SandboxPolicy:     attestedInvocation.SandboxPolicy,
		NetworkAccess:     false,
		WorkspaceRoots:    []string{workspace},
		TimeoutSeconds:    standardAuthoringCodexTimeoutSeconds(request.Stage.Budget.TurnTimeout),
		MaxOutputBytes:    program.MaxOutputBytes,
		CapabilitySummary: attestedInvocation.CLIVersionOutput,
		// App Server protocol logs contain only hashes in the current runtime, but
		// an operation must not leave even those records in a managed workspace.
		LogPath:      os.DevNull,
		DynamicTools: []agent.DynamicTool{submission.dynamicTool()},
	})
	if err != nil {
		if contextError(ctx) != nil {
			return standardAuthoringCodexInterrupted(), nil
		}
		return standardAuthoringCodexFailure(workflowkit.FailureProcess, standardAuthoringCodexFailureRuntime), nil
	}
	for ordinal, prompt := range program.TurnPrompts {
		turn := ordinal + 1
		if err := submission.beginTurn(turn); err != nil {
			_ = conversation.Close()
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
		}
		if err := executor.checkpoint(ctx, request, program, inputFingerprint, turn, "turn_ready", ""); err != nil {
			_ = conversation.Close()
			if contextError(ctx) != nil {
				return standardAuthoringCodexInterrupted(), nil
			}
			return standardAuthoringCodexFailure(workflowkit.FailureUnknown, standardAuthoringCodexFailureCheckpoint), nil
		}
		if err := request.Charge(ctx, workflowkit.StageUsage{
			OperationKey: standardAuthoringCodexUsageKey(request, turn), Dimension: "agent_turn", Units: 1, OccurredAt: executor.now().UTC(),
		}); err != nil {
			_ = conversation.Close()
			if contextError(ctx) != nil {
				return standardAuthoringCodexInterrupted(), nil
			}
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureQuota), nil
		}
		beforeTurnIdentity, verifyErr := executor.verifyFrozenSource(ctx, request.Execution, workspace)
		if verifyErr != nil || beforeTurnIdentity != sourceIdentity {
			_ = conversation.Close()
			if contextError(ctx) != nil {
				return standardAuthoringCodexInterrupted(), nil
			}
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureSource), nil
		}
		turnRequest := agent.TurnRequest{
			ProjectPath:       workspace,
			Prompt:            prompt,
			Model:             attestedInvocation.ModelID,
			ReasoningEffort:   string(attestedInvocation.ReasoningEffort),
			SandboxMode:       attestedInvocation.SandboxMode,
			SandboxPolicy:     attestedInvocation.SandboxPolicy,
			NetworkAccess:     false,
			WorkspaceRoots:    []string{workspace},
			TimeoutSeconds:    standardAuthoringCodexTimeoutSeconds(request.Stage.Budget.TurnTimeout),
			MaxOutputBytes:    program.MaxOutputBytes,
			CapabilitySummary: attestedInvocation.CLIVersionOutput,
			LogPath:           os.DevNull,
			OutputSchema:      submission.outputSchema(),
		}
		if turn == 1 {
			turnRequest.Input = []agent.InputPart{{Type: "text", Text: string(requestDocument)}}
		}
		result, turnErr := conversation.Turn(ctx, turnRequest)
		afterTurnIdentity, verifyErr := executor.verifyFrozenSource(ctx, request.Execution, workspace)
		if verifyErr != nil || afterTurnIdentity != sourceIdentity {
			_ = conversation.Close()
			if contextError(ctx) != nil {
				return standardAuthoringCodexInterrupted(), nil
			}
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureSource), nil
		}
		if turnErr != nil {
			_ = conversation.Close()
			if contextError(ctx) != nil {
				return standardAuthoringCodexInterrupted(), nil
			}
			return standardAuthoringCodexFailure(workflowkit.FailureProcess, standardAuthoringCodexFailureRuntime), nil
		}
		if result.Model != attestedInvocation.ModelID {
			_ = conversation.Close()
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureRuntime), nil
		}
		if submission.failure() != "" {
			_ = conversation.Close()
			return standardAuthoringCodexFailure(standardAuthoringCodexSubmissionFailureClass(submission.failure()), submission.failure()), nil
		}
		responseDigest := workflowkit.SHA256Fingerprint([]byte(result.Text))
		if err := executor.checkpoint(ctx, request, program, inputFingerprint, turn, "turn_completed", string(responseDigest)); err != nil {
			_ = conversation.Close()
			if contextError(ctx) != nil {
				return standardAuthoringCodexInterrupted(), nil
			}
			return standardAuthoringCodexFailure(workflowkit.FailureUnknown, standardAuthoringCodexFailureCheckpoint), nil
		}
		if accepted, ok := submission.acceptedResult(); ok {
			closeErr := conversation.Close()
			closedIdentity, verifyErr := executor.verifyFrozenSource(ctx, request.Execution, workspace)
			if verifyErr != nil || closedIdentity != sourceIdentity {
				if contextError(ctx) != nil {
					return standardAuthoringCodexInterrupted(), nil
				}
				return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureSource), nil
			}
			if closeErr != nil {
				return standardAuthoringCodexFailure(workflowkit.FailureProcess, standardAuthoringCodexFailureRuntime), nil
			}
			return accepted, nil
		}
	}
	closeErr := conversation.Close()
	closedIdentity, verifyErr := executor.verifyFrozenSource(ctx, request.Execution, workspace)
	if verifyErr != nil || closedIdentity != sourceIdentity {
		if contextError(ctx) != nil {
			return standardAuthoringCodexInterrupted(), nil
		}
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureSource), nil
	}
	if closeErr != nil {
		return standardAuthoringCodexFailure(workflowkit.FailureProcess, standardAuthoringCodexFailureRuntime), nil
	}
	if submission.failure() != "" {
		return standardAuthoringCodexFailure(standardAuthoringCodexSubmissionFailureClass(submission.failure()), submission.failure()), nil
	}
	// The frozen inputs and output contract remain valid; this particular agent
	// process simply ended without delivering the required structured result.
	return standardAuthoringCodexFailure(workflowkit.FailureProcess, standardAuthoringCodexSubmissionFailureAbsent), nil
}

func standardAuthoringCodexSubmissionFailureClass(code string) workflowkit.FailureClass {
	if code == standardAuthoringCodexSubmissionFailureQuota {
		return workflowkit.FailurePolicy
	}
	return workflowkit.FailureUnknown
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
	expectedSubmissions, hasSubmissionQuota := standardAuthoringCodexOutputSubmissionQuota(request.Stage)
	if !IsCodexAppServerProductionPayload(payload) || payload.MaxTurns != len(program.TurnPrompts) || payload.MaxTurns > request.Stage.Budget.MaxTurns || !hasAgentTurnQuota || int64(payload.MaxTurns) != expectedAgentTurns || !hasSubmissionQuota || expectedSubmissions != workflowadapter.StandardAuthoringOutputSubmissionClaimUnits {
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

func standardAuthoringCodexOutputSubmissionQuota(stage workflowkit.StageDescriptor) (int64, bool) {
	return standardAuthoringCodexQuotaClaim(stage, standardAuthoringCodexOutputSubmissionQuotaDimension)
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

func (executor *StandardAuthoringCodexAgentTurnExecutor) checkpoint(ctx context.Context, request workflowkit.StageExecutionRequest, program StandardAuthoringCodexTurnProgram, inputFingerprint workflowkit.Fingerprint, turn int, substep, responseDigest string) error {
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
	_, err = request.Checkpoint(ctx, workflowkit.StageCheckpoint{
		CheckpointID:   standardAuthoringCodexCheckpointKey(request, turn, substep),
		IdempotencyKey: standardAuthoringCodexCheckpointKey(request, turn, substep),
		TurnOrdinal:    turn,
		Substep:        substep,
		Payload:        payload,
		// A Codex App Server conversation is intentionally ephemeral. Persisting
		// its raw transcript to claim resumability would violate the no-secret-log
		// rule, so recovery must create a new fenced stage attempt for now.
		Resumable:  false,
		OccurredAt: executor.now().UTC(),
	})
	return err
}

type standardAuthoringCodexInput struct {
	Name          string                  `json:"name"`
	SchemaVersion string                  `json:"schema_version"`
	ContentDigest workflowkit.Fingerprint `json:"content_digest"`
	ContentBase64 string                  `json:"content_base64"`
}

func standardAuthoringCodexReadInputs(ctx context.Context, request workflowkit.StageExecutionRequest) ([]standardAuthoringCodexInput, workflowkit.Fingerprint, error) {
	bindings := append([]workflowkit.ArtifactBinding(nil), request.Inputs...)
	sort.Slice(bindings, func(left, right int) bool { return bindings[left].Name < bindings[right].Name })
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(bindings)
	if err != nil {
		return nil, "", err
	}
	inputs := make([]standardAuthoringCodexInput, 0, len(bindings))
	for _, binding := range bindings {
		if err := binding.Validate(); err != nil {
			return nil, "", err
		}
		content, err := request.ReadInput(ctx, binding)
		if err != nil || workflowkit.SHA256Fingerprint(content) != binding.ContentDigest {
			return nil, "", errors.New("frozen stage input is unavailable or changed")
		}
		inputs = append(inputs, standardAuthoringCodexInput{
			Name: binding.Name, SchemaVersion: binding.SchemaVersion, ContentDigest: binding.ContentDigest,
			ContentBase64: base64.StdEncoding.EncodeToString(content),
		})
	}
	return inputs, inputFingerprint, nil
}

// standardAuthoringCodexDockerfileEnvironmentPolicy extracts the intrinsic
// session policy only for the Dockerfile producer. Its bytes have already been
// read through the frozen binding and digest-checked by
// standardAuthoringCodexReadInputs; requiring canonical bytes here prevents an
// equivalent-but-unfrozen JSON spelling from becoming an output authority.
func standardAuthoringCodexDockerfileEnvironmentPolicy(stage workflowkit.StageDescriptor, inputs []standardAuthoringCodexInput) (*workflowadapter.StandardAuthoringEnvironmentPolicy, error) {
	if stage.Key != workflowkit.StageKey(workflowadapter.DockerfileGen) {
		return nil, nil
	}

	var policy *workflowadapter.StandardAuthoringEnvironmentPolicy
	for _, input := range inputs {
		if input.Name != workflowadapter.StandardAuthoringEnvironmentPolicyArtifact {
			continue
		}
		if policy != nil || input.SchemaVersion != workflowadapter.StandardAuthoringEnvironmentPolicySchemaVersion {
			return nil, errors.New("frozen Dockerfile environment policy binding is invalid")
		}
		content, err := base64.StdEncoding.DecodeString(input.ContentBase64)
		if err != nil || workflowkit.SHA256Fingerprint(content) != input.ContentDigest {
			return nil, errors.New("frozen Dockerfile environment policy content is invalid")
		}
		parsed, err := workflowadapter.ParseStandardAuthoringEnvironmentPolicyJSON(content)
		if err != nil {
			return nil, errors.New("frozen Dockerfile environment policy is invalid")
		}
		canonical, err := parsed.CanonicalJSON()
		if err != nil || !bytes.Equal(canonical, content) {
			return nil, errors.New("frozen Dockerfile environment policy is not canonical")
		}
		policy = &parsed
	}
	if policy == nil {
		return nil, errors.New("frozen Dockerfile environment policy is missing")
	}
	return policy, nil
}

func standardAuthoringCodexRequestDocument(request workflowkit.StageExecutionRequest, program StandardAuthoringCodexTurnProgram, inputs []standardAuthoringCodexInput, inputFingerprint workflowkit.Fingerprint, environmentPolicy *workflowadapter.StandardAuthoringEnvironmentPolicy) ([]byte, error) {
	outputs := make([]struct {
		Name          string `json:"name"`
		SchemaVersion string `json:"schema_version"`
	}, 0, len(request.Stage.Outputs))
	for _, output := range request.Stage.Outputs {
		outputs = append(outputs, struct {
			Name          string `json:"name"`
			SchemaVersion string `json:"schema_version"`
		}{Name: output.Name, SchemaVersion: output.SchemaVersion})
	}
	var frozenEnvironmentPolicy *workflowadapter.StandardAuthoringEnvironmentPolicy
	if request.Stage.Key == workflowkit.StageKey(workflowadapter.DockerfileGen) {
		if environmentPolicy == nil || environmentPolicy.Validate() != nil {
			return nil, errors.New("frozen Dockerfile environment policy is unavailable")
		}
		policyCopy := *environmentPolicy
		frozenEnvironmentPolicy = &policyCopy
	}
	return json.Marshal(struct {
		Format                  string                                              `json:"format"`
		Version                 string                                              `json:"version"`
		ProgramID               string                                              `json:"program_id"`
		ProgramVersion          string                                              `json:"program_version"`
		ProgramFingerprint      workflowkit.Fingerprint                             `json:"program_fingerprint"`
		OutputSchemaFingerprint workflowkit.Fingerprint                             `json:"output_schema_fingerprint"`
		StageKey                workflowkit.StageKey                                `json:"stage_key"`
		InputFingerprint        workflowkit.Fingerprint                             `json:"input_fingerprint"`
		Inputs                  []standardAuthoringCodexInput                       `json:"inputs"`
		FrozenEnvironmentPolicy *workflowadapter.StandardAuthoringEnvironmentPolicy `json:"frozen_environment_policy,omitempty"`
		Outputs                 []struct {
			Name          string `json:"name"`
			SchemaVersion string `json:"schema_version"`
		} `json:"outputs"`
	}{
		Format: StandardAuthoringCodexTurnRequestFormat, Version: StandardAuthoringCodexTurnRequestVersion,
		ProgramID: program.ID, ProgramVersion: program.Version, ProgramFingerprint: program.Fingerprint,
		OutputSchemaFingerprint: StandardAuthoringCodexOutputSchemaFingerprint(), StageKey: request.Stage.Key,
		InputFingerprint: inputFingerprint, Inputs: inputs, FrozenEnvironmentPolicy: frozenEnvironmentPolicy, Outputs: outputs,
	})
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
	if invocation.AgentID != CodexAppServerProductionAgentID || invocation.ModelID != CodexAppServerProductionModelID || invocation.ReasoningEffort != CodexAppServerProductionReasoningEffort || invocation.SandboxMode != CodexAppServerSandboxModeReadOnly || invocation.SandboxPolicy != CodexAppServerSandboxPolicyReadOnly || invocation.NetworkAccess || !filepath.IsAbs(invocation.JavaScriptLauncherPath) || !filepath.IsAbs(invocation.NodeExecutablePath) || !filepath.IsAbs(invocation.CodexHomeDirectory) {
		return fmt.Errorf("%w: invocation does not satisfy the locked Codex production policy", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return nil
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
