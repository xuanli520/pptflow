package stageprovider

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// WorkflowkitStageOperationProvider resolves one exact, frozen Harbor
// provider/operation selection to a public workflowkit executor. Resolution
// must be side-effect free so it can be used during StartRun preflight.
type WorkflowkitStageOperationProvider interface {
	ResolveWorkflowkitStageOperation(workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error)
}

// WorkflowkitProviderRegistration installs one exact provider ID, kind, and
// version. Neither provider nor operation resolution performs fallback.
type WorkflowkitProviderRegistration struct {
	Provider workflowadapter.ProviderReference
	Adapter  WorkflowkitStageOperationProvider
}

// WorkflowkitProviderOperationResolver combines prepare-time capability
// validation with execution-time exact operation lookup.
type WorkflowkitProviderOperationResolver interface {
	workflowadapter.StageOperationResolver
	ResolveWorkflowkitStageOperation(workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error)
}

// ControlledWorkflowkitProviderRegistry owns the exact provider binding
// boundary for generic workflowkit execution. It does not import or expose
// application runtime types.
type ControlledWorkflowkitProviderRegistry struct {
	byID map[string]map[string]workflowkitProviderRegistration
}

type workflowkitProviderRegistration struct {
	provider workflowadapter.ProviderReference
	adapter  WorkflowkitStageOperationProvider
}

// NewControlledWorkflowkitProviderRegistry rejects invalid or duplicate
// provider registrations before an execution can be prepared.
func NewControlledWorkflowkitProviderRegistry(registrations []WorkflowkitProviderRegistration) (*ControlledWorkflowkitProviderRegistry, error) {
	registry := &ControlledWorkflowkitProviderRegistry{byID: make(map[string]map[string]workflowkitProviderRegistration, len(registrations))}
	for _, registration := range registrations {
		if err := validateProviderReference(registration.Provider); err != nil {
			return nil, err
		}
		if isNilWorkflowkitStageOperationProvider(registration.Adapter) {
			return nil, fmt.Errorf("%w: provider %q version %q has a nil adapter", ErrProviderUnavailable, registration.Provider.ID, registration.Provider.Version)
		}
		versions := registry.byID[registration.Provider.ID]
		if versions == nil {
			versions = make(map[string]workflowkitProviderRegistration)
			registry.byID[registration.Provider.ID] = versions
		}
		if _, exists := versions[registration.Provider.Version]; exists {
			return nil, fmt.Errorf("%w: duplicate provider %q version %q", ErrInvalidStageOperation, registration.Provider.ID, registration.Provider.Version)
		}
		versions[registration.Provider.Version] = workflowkitProviderRegistration{provider: registration.Provider, adapter: registration.Adapter}
	}
	return registry, nil
}

// ResolveWorkflowkitStageOperation resolves exactly the provider ID/version,
// provider kind, stage key, operation ID/version, and typed frozen payload.
func (registry *ControlledWorkflowkitProviderRegistry) ResolveWorkflowkitStageOperation(resolution workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	if err := validateStageOperationResolution(resolution); err != nil {
		return nil, err
	}
	if registry == nil {
		return nil, fmt.Errorf("%w: provider registry is not configured", ErrProviderUnavailable)
	}
	versions, found := registry.byID[resolution.Provider.ID]
	if !found {
		return nil, fmt.Errorf("%w: provider %q", ErrProviderUnavailable, resolution.Provider.ID)
	}
	registration, found := versions[resolution.Provider.Version]
	if !found || registration.provider.Kind != resolution.Provider.Kind {
		return nil, fmt.Errorf("%w: provider %q version %q", ErrProviderVersionMismatch, resolution.Provider.ID, resolution.Provider.Version)
	}
	executor, err := registration.adapter.ResolveWorkflowkitStageOperation(resolution.Clone())
	if err != nil {
		return nil, fmt.Errorf("resolve provider %q operation %q@%q for stage %q: %w", resolution.Provider.ID, resolution.Operation.OperationID, resolution.Operation.Version, resolution.StageKey, err)
	}
	if isNilWorkflowkitStageExecutor(executor) {
		return nil, fmt.Errorf("%w: provider %q operation %q@%q for stage %q", ErrStageOperationUnavailable, resolution.Provider.ID, resolution.Operation.OperationID, resolution.Operation.Version, resolution.StageKey)
	}
	return executor, nil
}

// ValidateStageOperation proves the exact frozen provider operation is
// installed without executing it.
func (registry *ControlledWorkflowkitProviderRegistry) ValidateStageOperation(resolution workflowadapter.StageOperationResolution) error {
	_, err := registry.ResolveWorkflowkitStageOperation(resolution)
	return err
}

// WorkflowkitStaticStageOperationRegistration installs a deterministic
// exact-match adapter. It is suitable for durable-review projections and
// integration fixtures; production command/container/agent providers should
// use TypedWorkflowkitStageOperationProvider below.
type WorkflowkitStaticStageOperationRegistration struct {
	StageKey  workflowkit.StageKey
	Operation workflowadapter.StageOperationBinding
	Executor  workflowkit.StageExecutor
}

// StaticWorkflowkitStageOperationProvider resolves only registrations whose
// identity and canonical typed payload equal the frozen operation selection.
type StaticWorkflowkitStageOperationProvider struct {
	operations map[staticOperationKey]staticWorkflowkitOperation
}

type staticOperationKey struct {
	stageKey    workflowkit.StageKey
	providerID  string
	operationID string
	version     string
}

type staticWorkflowkitOperation struct {
	executor workflowkit.StageExecutor
	payload  []byte
}

// NewStaticWorkflowkitStageOperationProvider validates static exact operation
// registrations eagerly. A static executor cannot become a payload fallback:
// the provider verifies the canonical payload again on every resolution.
func NewStaticWorkflowkitStageOperationProvider(registrations []WorkflowkitStaticStageOperationRegistration) (*StaticWorkflowkitStageOperationProvider, error) {
	provider := &StaticWorkflowkitStageOperationProvider{operations: make(map[staticOperationKey]staticWorkflowkitOperation, len(registrations))}
	for _, registration := range registrations {
		if strings.TrimSpace(string(registration.StageKey)) == "" {
			return nil, fmt.Errorf("%w: static operation stage key is required", ErrInvalidStageOperation)
		}
		payload, err := canonicalOperationBindingPayload(registration.Operation)
		if err != nil {
			return nil, err
		}
		if isNilWorkflowkitStageExecutor(registration.Executor) {
			return nil, fmt.Errorf("%w: static operation %q@%q for stage %q is nil", ErrStageOperationUnavailable, registration.Operation.OperationID, registration.Operation.Version, registration.StageKey)
		}
		key := operationKey(registration.StageKey, registration.Operation)
		if _, exists := provider.operations[key]; exists {
			return nil, fmt.Errorf("%w: duplicate static operation %q@%q for stage %q", ErrInvalidStageOperation, registration.Operation.OperationID, registration.Operation.Version, registration.StageKey)
		}
		provider.operations[key] = staticWorkflowkitOperation{executor: registration.Executor, payload: payload}
	}
	return provider, nil
}

// ResolveWorkflowkitStageOperation implements WorkflowkitStageOperationProvider.
func (provider *StaticWorkflowkitStageOperationProvider) ResolveWorkflowkitStageOperation(resolution workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	if provider == nil {
		return nil, fmt.Errorf("%w: static provider is not configured", ErrProviderUnavailable)
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
	return operation.executor, nil
}

func operationKey(stageKey workflowkit.StageKey, binding workflowadapter.StageOperationBinding) staticOperationKey {
	return staticOperationKey{stageKey: stageKey, providerID: binding.ProviderID, operationID: binding.OperationID, version: binding.Version}
}

func canonicalOperationBindingPayload(binding workflowadapter.StageOperationBinding) ([]byte, error) {
	if err := validateOperationBinding(binding); err != nil {
		return nil, err
	}
	payload, err := workflowadapter.CanonicalStageOperationPayloadJSON(binding.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: operation payload: %v", ErrInvalidStageOperation, err)
	}
	return payload, nil
}

func validateProviderReference(reference workflowadapter.ProviderReference) error {
	if strings.TrimSpace(reference.ID) == "" || strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.Version) == "" {
		return fmt.Errorf("%w: provider id, kind, and version are required", ErrInvalidStageOperation)
	}
	return nil
}

func validateOperationBinding(binding workflowadapter.StageOperationBinding) error {
	if strings.TrimSpace(binding.ProviderID) == "" || strings.TrimSpace(binding.OperationID) == "" || strings.TrimSpace(binding.Version) == "" {
		return fmt.Errorf("%w: operation provider id, id, and version are required", ErrInvalidStageOperation)
	}
	if _, err := workflowadapter.CanonicalStageOperationPayloadJSON(binding.Payload); err != nil {
		return fmt.Errorf("%w: operation payload: %v", ErrInvalidStageOperation, err)
	}
	return nil
}

func validateStageOperationResolution(resolution workflowadapter.StageOperationResolution) error {
	if strings.TrimSpace(string(resolution.StageKey)) == "" {
		return fmt.Errorf("%w: resolution stage key is required", ErrInvalidStageOperation)
	}
	if err := resolution.Plugin.Validate(); err != nil {
		return fmt.Errorf("%w: resolution plugin: %v", ErrInvalidStageOperation, err)
	}
	if err := validateProviderReference(resolution.Provider); err != nil {
		return err
	}
	if err := validateOperationBinding(resolution.Operation); err != nil {
		return err
	}
	if resolution.Operation.ProviderID != resolution.Provider.ID {
		return fmt.Errorf("%w: operation provider %q does not match provider %q", ErrInvalidStageOperation, resolution.Operation.ProviderID, resolution.Provider.ID)
	}
	return nil
}

func isNilWorkflowkitStageOperationProvider(provider WorkflowkitStageOperationProvider) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isNilWorkflowkitStageExecutor(executor workflowkit.StageExecutor) bool {
	if executor == nil {
		return true
	}
	value := reflect.ValueOf(executor)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

var _ WorkflowkitProviderOperationResolver = (*ControlledWorkflowkitProviderRegistry)(nil)
var _ WorkflowkitStageOperationProvider = (*StaticWorkflowkitStageOperationProvider)(nil)
