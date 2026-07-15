package stageprovider

import (
	"bytes"
	"context"
	"fmt"
	"reflect"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// StageOperationInvocation is the complete immutable input made available to
// a typed provider. Resolution includes only frozen Harbor references; the
// generic request includes only the claimed workflowkit execution capability.
type StageOperationInvocation struct {
	Request    workflowkit.StageExecutionRequest
	Resolution workflowadapter.StageOperationResolution
}

func (invocation StageOperationInvocation) clone() StageOperationInvocation {
	request := invocation.Request
	request.Execution = request.Execution.Clone()
	request.Claim = request.Claim.Clone()
	request.Stage = request.Stage.Clone()
	request.Inputs = append([]workflowkit.ArtifactBinding(nil), request.Inputs...)
	invocation.Request = request
	invocation.Resolution = invocation.Resolution.Clone()
	return invocation
}

// LocalCommandOperationExecutor executes a typed local.command payload. The
// implementation resolves CommandID from its own controlled command registry
// and must invoke argv directly, never through a shell.
type LocalCommandOperationExecutor interface {
	ExecuteLocalCommand(context.Context, StageOperationInvocation, workflowadapter.LocalCommandOperationPayload) (workflowkit.StageExecutionResult, error)
}

// LocalCommandOperationExecutorFunc adapts a local command callback.
type LocalCommandOperationExecutorFunc func(context.Context, StageOperationInvocation, workflowadapter.LocalCommandOperationPayload) (workflowkit.StageExecutionResult, error)

// ExecuteLocalCommand invokes the adapted callback.
func (function LocalCommandOperationExecutorFunc) ExecuteLocalCommand(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.LocalCommandOperationPayload) (workflowkit.StageExecutionResult, error) {
	return function(ctx, invocation.clone(), cloneLocalCommandPayload(payload))
}

// ContainerCommandOperationExecutor executes a typed container.command
// payload. The implementation receives a digest-pinned image and direct argv.
type ContainerCommandOperationExecutor interface {
	ExecuteContainerCommand(context.Context, StageOperationInvocation, workflowadapter.ContainerCommandOperationPayload) (workflowkit.StageExecutionResult, error)
}

// ContainerCommandOperationExecutorFunc adapts a container command callback.
type ContainerCommandOperationExecutorFunc func(context.Context, StageOperationInvocation, workflowadapter.ContainerCommandOperationPayload) (workflowkit.StageExecutionResult, error)

// ExecuteContainerCommand invokes the adapted callback.
func (function ContainerCommandOperationExecutorFunc) ExecuteContainerCommand(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.ContainerCommandOperationPayload) (workflowkit.StageExecutionResult, error) {
	return function(ctx, invocation.clone(), cloneContainerCommandPayload(payload))
}

// AgentTurnOperationExecutor executes a typed agent.turn payload. Prompt and
// evidence data are read through frozen stage inputs, not an arbitrary map.
type AgentTurnOperationExecutor interface {
	ExecuteAgentTurn(context.Context, StageOperationInvocation, workflowadapter.AgentTurnOperationPayload) (workflowkit.StageExecutionResult, error)
}

// AgentTurnOperationExecutorFunc adapts an agent turn callback.
type AgentTurnOperationExecutorFunc func(context.Context, StageOperationInvocation, workflowadapter.AgentTurnOperationPayload) (workflowkit.StageExecutionResult, error)

// ExecuteAgentTurn invokes the adapted callback.
func (function AgentTurnOperationExecutorFunc) ExecuteAgentTurn(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload) (workflowkit.StageExecutionResult, error) {
	return function(ctx, invocation.clone(), payload)
}

// HarborBuiltinOperationExecutor executes one explicitly registered
// Go-controlled Harbor Flow operation. Unlike LocalCommandOperationExecutor,
// it receives no executable path, argv, environment, or shell surface. Its
// exact handler identity is frozen by HarborBuiltinOperationPayload and its
// linker-bound implementation is proven by the deployment lock attestor.
type HarborBuiltinOperationExecutor interface {
	ExecuteHarborBuiltin(context.Context, StageOperationInvocation, workflowadapter.HarborBuiltinOperationPayload) (workflowkit.StageExecutionResult, error)
}

// HarborBuiltinOperationExecutorFunc is the function adapter for a typed
// Harbor-built-in executor. Keeping it separate from LocalCommandOperationExecutor
// makes accidental use of an external command path impossible at this boundary.
type HarborBuiltinOperationExecutorFunc func(context.Context, StageOperationInvocation, workflowadapter.HarborBuiltinOperationPayload) (workflowkit.StageExecutionResult, error)

// ExecuteHarborBuiltin invokes the adapted built-in operation callback.
func (function HarborBuiltinOperationExecutorFunc) ExecuteHarborBuiltin(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.HarborBuiltinOperationPayload) (workflowkit.StageExecutionResult, error) {
	return function(ctx, invocation.clone(), payload)
}

// TypedWorkflowkitOperationHandlers installs the capability-specific
// executors supported by one controlled provider. A provider cannot resolve a
// registered payload kind whose executor is absent.
type TypedWorkflowkitOperationHandlers struct {
	LocalCommand     LocalCommandOperationExecutor
	ContainerCommand ContainerCommandOperationExecutor
	AgentTurn        AgentTurnOperationExecutor
	HarborBuiltin    HarborBuiltinOperationExecutor
}

// TypedWorkflowkitStageOperationRegistration describes an exact stage
// operation and the sealed payload that must be present in its frozen spec.
type TypedWorkflowkitStageOperationRegistration struct {
	StageKey  workflowkit.StageKey
	Operation workflowadapter.StageOperationBinding
}

// TypedWorkflowkitStageOperationProviderConfig supplies the static exact
// operation catalog and its controlled capability executors.
type TypedWorkflowkitStageOperationProviderConfig struct {
	Handlers   TypedWorkflowkitOperationHandlers
	Operations []TypedWorkflowkitStageOperationRegistration
}

// TypedWorkflowkitStageOperationProvider implements a controlled operation
// catalog for local.command, container.command, agent.turn, and
// harbor.builtin. The catalog stores canonical payload bytes and rejects any
// identity or payload drift.
type TypedWorkflowkitStageOperationProvider struct {
	handlers   TypedWorkflowkitOperationHandlers
	operations map[staticOperationKey]typedWorkflowkitOperation
}

type typedWorkflowkitOperation struct {
	stageKey    workflowkit.StageKey
	operation   workflowadapter.StageOperationBinding
	payload     []byte
	payloadKind workflowadapter.StageOperationPayloadKind
}

// NewTypedWorkflowkitStageOperationProvider creates an immutable provider
// catalog. Durable review operations are intentionally excluded here because
// their nonterminal projection is owned by the public Harbor adapter.
func NewTypedWorkflowkitStageOperationProvider(config TypedWorkflowkitStageOperationProviderConfig) (*TypedWorkflowkitStageOperationProvider, error) {
	provider := &TypedWorkflowkitStageOperationProvider{
		handlers:   config.Handlers,
		operations: make(map[staticOperationKey]typedWorkflowkitOperation, len(config.Operations)),
	}
	for _, registration := range config.Operations {
		if registration.StageKey == "" {
			return nil, fmt.Errorf("%w: typed operation stage key is required", ErrInvalidStageOperation)
		}
		payload, err := canonicalOperationBindingPayload(registration.Operation)
		if err != nil {
			return nil, err
		}
		if err := validateTypedPayloadHandler(registration.Operation.Payload, config.Handlers); err != nil {
			return nil, err
		}
		key := operationKey(registration.StageKey, registration.Operation)
		if _, exists := provider.operations[key]; exists {
			return nil, fmt.Errorf("%w: duplicate typed operation %q@%q for stage %q", ErrInvalidStageOperation, registration.Operation.OperationID, registration.Operation.Version, registration.StageKey)
		}
		provider.operations[key] = typedWorkflowkitOperation{
			stageKey: registration.StageKey, operation: registration.Operation.Clone(), payload: payload, payloadKind: registration.Operation.Payload.Kind(),
		}
	}
	return provider, nil
}

// ResolveWorkflowkitStageOperation implements WorkflowkitStageOperationProvider.
func (provider *TypedWorkflowkitStageOperationProvider) ResolveWorkflowkitStageOperation(resolution workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	if provider == nil {
		return nil, fmt.Errorf("%w: typed provider is not configured", ErrProviderUnavailable)
	}
	operation, found := provider.operations[operationKey(resolution.StageKey, resolution.Operation)]
	if !found {
		return nil, fmt.Errorf("%w: operation %q@%q for stage %q", ErrStageOperationUnavailable, resolution.Operation.OperationID, resolution.Operation.Version, resolution.StageKey)
	}
	payload, err := canonicalOperationBindingPayload(resolution.Operation)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(operation.payload, payload) {
		return nil, fmt.Errorf("%w: operation %q@%q for stage %q", ErrFrozenOperationPayloadMismatch, resolution.Operation.OperationID, resolution.Operation.Version, resolution.StageKey)
	}
	return typedWorkflowkitStageExecutor{handlers: provider.handlers, operation: operation, resolution: resolution.Clone()}, nil
}

func validateTypedPayloadHandler(payload workflowadapter.StageOperationPayload, handlers TypedWorkflowkitOperationHandlers) error {
	switch payload.(type) {
	case workflowadapter.LocalCommandOperationPayload:
		if isNilLocalCommandOperationExecutor(handlers.LocalCommand) {
			return fmt.Errorf("%w: local.command executor is not configured", ErrStageOperationUnavailable)
		}
	case workflowadapter.ContainerCommandOperationPayload:
		if isNilContainerCommandOperationExecutor(handlers.ContainerCommand) {
			return fmt.Errorf("%w: container.command executor is not configured", ErrStageOperationUnavailable)
		}
	case workflowadapter.AgentTurnOperationPayload:
		if isNilAgentTurnOperationExecutor(handlers.AgentTurn) {
			return fmt.Errorf("%w: agent.turn executor is not configured", ErrStageOperationUnavailable)
		}
	case workflowadapter.HarborBuiltinOperationPayload:
		if isNilHarborBuiltinOperationExecutor(handlers.HarborBuiltin) {
			return fmt.Errorf("%w: harbor.builtin executor is not configured", ErrStageOperationUnavailable)
		}
	default:
		return fmt.Errorf("%w: typed provider does not support payload %T", ErrStageOperationUnavailable, payload)
	}
	return nil
}

type typedWorkflowkitStageExecutor struct {
	handlers   TypedWorkflowkitOperationHandlers
	operation  typedWorkflowkitOperation
	resolution workflowadapter.StageOperationResolution
}

func (executor typedWorkflowkitStageExecutor) ExecuteStage(ctx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
	if request.Stage.Key != executor.operation.stageKey || request.Stage.Plugin != executor.resolution.Plugin {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: typed provider operation %q@%q received stage %q with plugin %s@%s", ErrInvalidStageOperation, executor.operation.operation.OperationID, executor.operation.operation.Version, request.Stage.Key, request.Stage.Plugin.ID, request.Stage.Plugin.Version)
	}
	invocation := StageOperationInvocation{Request: request, Resolution: executor.resolution}.clone()
	switch payload := executor.operation.operation.Payload.(type) {
	case workflowadapter.LocalCommandOperationPayload:
		return executor.handlers.LocalCommand.ExecuteLocalCommand(ctx, invocation, cloneLocalCommandPayload(payload))
	case workflowadapter.ContainerCommandOperationPayload:
		return executor.handlers.ContainerCommand.ExecuteContainerCommand(ctx, invocation, cloneContainerCommandPayload(payload))
	case workflowadapter.AgentTurnOperationPayload:
		return executor.handlers.AgentTurn.ExecuteAgentTurn(ctx, invocation, payload)
	case workflowadapter.HarborBuiltinOperationPayload:
		return executor.handlers.HarborBuiltin.ExecuteHarborBuiltin(ctx, invocation, payload)
	default:
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: typed provider cannot execute payload %T", ErrInvalidStageOperation, payload)
	}
}

func cloneLocalCommandPayload(payload workflowadapter.LocalCommandOperationPayload) workflowadapter.LocalCommandOperationPayload {
	payload.Arguments = append([]string(nil), payload.Arguments...)
	return payload
}

func cloneContainerCommandPayload(payload workflowadapter.ContainerCommandOperationPayload) workflowadapter.ContainerCommandOperationPayload {
	payload.Command = append([]string(nil), payload.Command...)
	return payload
}

func isNilLocalCommandOperationExecutor(executor LocalCommandOperationExecutor) bool {
	return isNilInterface(executor)
}

func isNilContainerCommandOperationExecutor(executor ContainerCommandOperationExecutor) bool {
	return isNilInterface(executor)
}

func isNilAgentTurnOperationExecutor(executor AgentTurnOperationExecutor) bool {
	return isNilInterface(executor)
}

func isNilHarborBuiltinOperationExecutor(executor HarborBuiltinOperationExecutor) bool {
	return isNilInterface(executor)
}

func isNilInterface(value interface{}) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var _ WorkflowkitStageOperationProvider = (*TypedWorkflowkitStageOperationProvider)(nil)
var _ workflowkit.StageExecutor = typedWorkflowkitStageExecutor{}
