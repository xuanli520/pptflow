package workflowkit

import (
	"errors"
	"testing"
)

func TestControlledPluginRegistryResolvesExactFrozenBinding(t *testing.T) {
	registry, err := NewControlledPluginRegistry([]PluginRegistration[string]{
		{Binding: PluginBinding{ID: "example.verify", Version: "1.0.0"}, Implementation: "verify-v1"},
		{Binding: PluginBinding{ID: "example.verify", Version: "2.0.0"}, Implementation: "verify-v2"},
	})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}

	implementation, err := registry.ResolveStagePlugin(StageDescriptor{Plugin: PluginBinding{ID: "example.verify", Version: "1.0.0"}})
	if err != nil {
		t.Fatalf("resolve exact frozen plugin: %v", err)
	}
	if implementation != "verify-v1" {
		t.Fatalf("resolved implementation = %q, want verify-v1", implementation)
	}
}

func TestControlledPluginRegistryRejectsUnknownAndVersionDrift(t *testing.T) {
	registry, err := NewControlledPluginRegistry([]PluginRegistration[string]{
		{Binding: PluginBinding{ID: "example.verify", Version: "1.0.0"}, Implementation: "verify-v1"},
	})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}

	if _, err := registry.ResolvePlugin(PluginBinding{ID: "example.unknown", Version: "1.0.0"}); !errors.Is(err, ErrPluginUnavailable) {
		t.Fatalf("unknown plugin error = %v, want ErrPluginUnavailable", err)
	}
	if _, err := registry.ResolvePlugin(PluginBinding{ID: "example.verify", Version: "2.0.0"}); !errors.Is(err, ErrPluginVersionMismatch) {
		t.Fatalf("version drift error = %v, want ErrPluginVersionMismatch", err)
	}
	if _, err := registry.ResolveStagePlugin(StageDescriptor{}); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("missing frozen stage binding error = %v, want ErrInvalidDescriptor", err)
	}
}

func TestControlledPluginRegistryRejectsInvalidDuplicateAndNilRegistrations(t *testing.T) {
	if _, err := NewControlledPluginRegistry([]PluginRegistration[string]{{Implementation: "missing-binding"}}); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("missing binding error = %v, want ErrInvalidDescriptor", err)
	}
	if _, err := NewControlledPluginRegistry([]PluginRegistration[string]{
		{Binding: PluginBinding{ID: "example.verify", Version: "1.0.0"}, Implementation: "first"},
		{Binding: PluginBinding{ID: "example.verify", Version: "1.0.0"}, Implementation: "second"},
	}); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("duplicate binding error = %v, want ErrInvalidDescriptor", err)
	}
	if _, err := NewControlledPluginRegistry([]PluginRegistration[*string]{{Binding: PluginBinding{ID: "example.verify", Version: "1.0.0"}}}); !errors.Is(err, ErrPluginUnavailable) {
		t.Fatalf("nil implementation error = %v, want ErrPluginUnavailable", err)
	}
}
