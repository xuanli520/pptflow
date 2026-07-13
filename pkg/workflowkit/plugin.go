package workflowkit

import (
	"fmt"
	"reflect"
)

// PluginBinding is the immutable implementation identity frozen with a stage
// descriptor. It is intentionally domain-neutral: a binding can identify a
// local binary plugin, a provider adapter, or another controlled implementation
// without exposing a mutable configuration map.
type PluginBinding struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// Validate ensures that a frozen binding has both parts of its identity.
func (binding PluginBinding) Validate() error {
	if err := validateRequired("plugin id", binding.ID, ErrInvalidDescriptor); err != nil {
		return err
	}
	return validateRequired("plugin version", binding.Version, ErrInvalidDescriptor)
}

// PluginRegistration installs one concrete implementation under exactly one
// immutable plugin binding. The registry indexes registrations at construction
// time, so later mutations to the caller's registration collection cannot alter
// binding resolution.
type PluginRegistration[T any] struct {
	Binding        PluginBinding
	Implementation T
}

// PluginResolver is the typed execution-boundary contract. Domain runtimes
// choose T (for example a stage-executor interface), while workflowkit only
// enforces resolution against the frozen ID and version.
type PluginResolver[T any] interface {
	ResolvePlugin(PluginBinding) (T, error)
}

// ControlledPluginRegistry resolves only explicitly installed plugin bindings.
// It never falls back from a requested version to a newer or older version.
type ControlledPluginRegistry[T any] struct {
	byID map[string]map[string]T
}

// NewControlledPluginRegistry validates registrations and rejects duplicate
// immutable bindings before the registry becomes available to a worker.
func NewControlledPluginRegistry[T any](registrations []PluginRegistration[T]) (*ControlledPluginRegistry[T], error) {
	registry := &ControlledPluginRegistry[T]{byID: make(map[string]map[string]T, len(registrations))}
	for _, registration := range registrations {
		if err := registration.Binding.Validate(); err != nil {
			return nil, err
		}
		if isNilPluginImplementation(registration.Implementation) {
			return nil, fmt.Errorf("%w: plugin %q version %q has a nil implementation", ErrPluginUnavailable, registration.Binding.ID, registration.Binding.Version)
		}
		versions := registry.byID[registration.Binding.ID]
		if versions == nil {
			versions = make(map[string]T)
			registry.byID[registration.Binding.ID] = versions
		}
		if _, exists := versions[registration.Binding.Version]; exists {
			return nil, fmt.Errorf("%w: duplicate plugin %q version %q", ErrInvalidDescriptor, registration.Binding.ID, registration.Binding.Version)
		}
		versions[registration.Binding.Version] = registration.Implementation
	}
	return registry, nil
}

// ResolvePlugin returns an implementation only when the exact frozen binding
// is installed. A known ID with a missing requested version is intentionally
// distinct from an entirely unknown ID so callers can report actionable drift.
func (registry *ControlledPluginRegistry[T]) ResolvePlugin(binding PluginBinding) (T, error) {
	var zero T
	if err := binding.Validate(); err != nil {
		return zero, err
	}
	if registry == nil {
		return zero, fmt.Errorf("%w: registry is not configured", ErrPluginUnavailable)
	}
	versions, found := registry.byID[binding.ID]
	if !found {
		return zero, fmt.Errorf("%w: plugin %q", ErrPluginUnavailable, binding.ID)
	}
	implementation, found := versions[binding.Version]
	if !found {
		return zero, fmt.Errorf("%w: plugin %q version %q", ErrPluginVersionMismatch, binding.ID, binding.Version)
	}
	return implementation, nil
}

// ResolveStagePlugin resolves the binding carried by a frozen stage. It is the
// intended bridge for a runtime that consumes a StageDescriptor directly.
func (registry *ControlledPluginRegistry[T]) ResolveStagePlugin(stage StageDescriptor) (T, error) {
	return registry.ResolvePlugin(stage.Plugin)
}

func isNilPluginImplementation[T any](implementation T) bool {
	value := reflect.ValueOf(implementation)
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
