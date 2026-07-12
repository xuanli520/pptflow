package workflow

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Registry struct {
	mu      sync.RWMutex
	plugins map[string]Plugin
	byKind  map[string]Plugin
}

func NewRegistry() *Registry {
	return &Registry{plugins: map[string]Plugin{}, byKind: map[string]Plugin{}}
}

func (r *Registry) Register(plugin Plugin) error {
	if plugin == nil {
		return fmt.Errorf("plugin is nil")
	}
	manifest := plugin.Manifest()
	id := strings.TrimSpace(manifest.ID)
	if id == "" {
		return fmt.Errorf("plugin id is required")
	}
	if len(manifest.Kinds) == 0 {
		return fmt.Errorf("plugin %s must declare at least one kind", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.plugins == nil {
		r.plugins = map[string]Plugin{}
	}
	if r.byKind == nil {
		r.byKind = map[string]Plugin{}
	}
	if _, exists := r.plugins[id]; exists {
		return fmt.Errorf("plugin %s already registered", id)
	}
	for _, kind := range manifest.Kinds {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			return fmt.Errorf("plugin %s declares empty kind", id)
		}
		if _, exists := r.byKind[kind]; exists {
			return fmt.Errorf("node kind %s already registered", kind)
		}
	}
	r.plugins[id] = plugin
	for _, kind := range manifest.Kinds {
		r.byKind[strings.TrimSpace(kind)] = plugin
	}
	return nil
}

func (r *Registry) Lookup(spec NodeSpec) (Plugin, error) {
	if r == nil {
		return nil, fmt.Errorf("registry is nil")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if id := strings.TrimSpace(spec.PluginID); id != "" {
		plugin := r.plugins[id]
		if plugin == nil {
			return nil, fmt.Errorf("plugin %s is not registered", id)
		}
		return plugin, nil
	}
	kind := strings.TrimSpace(spec.Kind)
	if kind == "" {
		return nil, fmt.Errorf("node kind is required")
	}
	plugin := r.byKind[kind]
	if plugin == nil {
		return nil, fmt.Errorf("node kind %s is not registered", kind)
	}
	return plugin, nil
}

func (r *Registry) Manifests() []PluginManifest {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]PluginManifest, 0, len(r.plugins))
	for _, plugin := range r.plugins {
		result = append(result, plugin.Manifest())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
