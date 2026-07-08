package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testPlugin struct {
	order *[]string
	fail  map[string]bool
}

func (p testPlugin) Manifest() PluginManifest {
	return PluginManifest{ID: "test.plugin", Version: "0", Kinds: []string{"test"}}
}

func (p testPlugin) Validate(NodeSpec) error {
	return nil
}

func (p testPlugin) Execute(ctx context.Context, req NodeRequest) (NodeResult, error) {
	*p.order = append(*p.order, req.Spec.ID)
	if p.fail[req.Spec.ID] {
		return NodeResult{}, context.Canceled
	}
	return NodeResult{}, nil
}

func TestEngineRunsDependsOnInTopologicalOrder(t *testing.T) {
	order := []string{}
	registry := NewRegistry()
	if err := registry.Register(testPlugin{order: &order}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(registry, Runtimes{})
	result, err := engine.Run(context.Background(), RunRequest{
		Workflow: WorkflowDefinition{
			ID: "topo",
			Nodes: []NodeSpec{
				{ID: "b", Kind: "test", DependsOn: []string{"a"}},
				{ID: "a", Kind: "test"},
			},
		},
		ArtifactRoot:  filepath.Join(t.TempDir(), "artifacts"),
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	if got := strings.Join(order, ","); got != "a,b" {
		t.Fatalf("order = %s", got)
	}
}

func TestEngineUsesEdgesAsDependencies(t *testing.T) {
	order := []string{}
	registry := NewRegistry()
	if err := registry.Register(testPlugin{order: &order}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(registry, Runtimes{})
	_, err := engine.Run(context.Background(), RunRequest{
		Workflow: WorkflowDefinition{
			ID: "edges",
			Nodes: []NodeSpec{
				{ID: "b", Kind: "test"},
				{ID: "a", Kind: "test"},
			},
			Edges: []EdgeSpec{{From: "a", To: "b"}},
		},
		ArtifactRoot:  filepath.Join(t.TempDir(), "artifacts"),
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "a,b" {
		t.Fatalf("order = %s", got)
	}
}

func TestEngineRejectsDependencyCycle(t *testing.T) {
	registry := NewRegistry()
	order := []string{}
	if err := registry.Register(testPlugin{order: &order}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(registry, Runtimes{})
	_, err := engine.Run(context.Background(), RunRequest{
		Workflow: WorkflowDefinition{
			ID:    "cycle",
			Nodes: []NodeSpec{{ID: "a", Kind: "test"}, {ID: "b", Kind: "test"}},
			Edges: []EdgeSpec{{From: "a", To: "b"}, {From: "b", To: "a"}},
		},
		ArtifactRoot:  filepath.Join(t.TempDir(), "artifacts"),
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v", err)
	}
}

func TestEngineSkipsDownstreamAfterDependencyFailure(t *testing.T) {
	order := []string{}
	registry := NewRegistry()
	if err := registry.Register(testPlugin{order: &order, fail: map[string]bool{"a": true}}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(registry, Runtimes{})
	result, err := engine.Run(context.Background(), RunRequest{
		Workflow: WorkflowDefinition{
			ID: "failed-dep",
			Nodes: []NodeSpec{
				{ID: "a", Kind: "test"},
				{ID: "b", Kind: "test", DependsOn: []string{"a"}},
				{ID: "c", Kind: "test"},
			},
		},
		ArtifactRoot:  filepath.Join(t.TempDir(), "artifacts"),
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
	})
	if err == nil {
		t.Fatal("expected workflow error")
	}
	if got := strings.Join(order, ","); got != "a,c" {
		t.Fatalf("order = %s", got)
	}
	if len(result.Nodes) != 3 {
		t.Fatalf("nodes = %d", len(result.Nodes))
	}
	if result.Nodes[1].NodeID != "b" || result.Nodes[1].Status != NodeSkipped {
		t.Fatalf("node b = %+v", result.Nodes[1])
	}
	if result.Nodes[2].NodeID != "c" || result.Nodes[2].Status != NodeSucceeded {
		t.Fatalf("node c = %+v", result.Nodes[2])
	}
}

func TestFileArtifactStoreRegisterRecordsExistingFile(t *testing.T) {
	store, err := NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path("slide_images/slide_01.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register(context.Background(), RegisterArtifactRequest{Name: "slide_images/slide_01.png", Type: "image", Producer: "generate_slides", Path: path}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register(context.Background(), RegisterArtifactRequest{Name: "slide_images/slide_01.png", Type: "image", Producer: "generate_slides", Path: path}); err != nil {
		t.Fatal(err)
	}
	refs, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %d", len(refs))
	}
	if refs[0].Name != "slide_images/slide_01.png" || refs[0].Type != "image" {
		t.Fatalf("ref = %+v", refs[0])
	}
}
