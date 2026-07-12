package templates

import (
	"strings"
	"testing"
)

func TestEmbeddedTemplatesExposeStableMetadata(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"phase1/repo_analyze",
		"phase1/task_design",
		"phase1/task_files",
		"phase2/quality_check",
		"phase2/runtime_self_check",
		"phase2/task_repair",
	}
	metadata := engine.MetadataList()
	if len(metadata) != len(want) {
		t.Fatalf("metadata count=%d, want %d: %+v", len(metadata), len(want), metadata)
	}
	for i, name := range want {
		if metadata[i].Name != name {
			t.Fatalf("metadata[%d].Name=%q, want %q", i, metadata[i].Name, name)
		}
		if metadata[i].Version == "" || !strings.HasPrefix(metadata[i].Digest, "sha256:") || len(metadata[i].Digest) != len("sha256:")+64 {
			t.Fatalf("template %s has invalid metadata: %+v", name, metadata[i])
		}
		byName, ok := engine.Metadata(name + ".md")
		if !ok || byName != metadata[i] {
			t.Fatalf("metadata lookup mismatch for %s: %+v, %v", name, byName, ok)
		}
	}
}

func TestRenderRejectsMissingTemplateField(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Render("phase1/repo_analyze", map[string]string{
		"RepoURL":   "https://github.com/org/repo",
		"CommitSHA": "abc123",
	})
	if err == nil || !strings.Contains(err.Error(), "TreeHash") {
		t.Fatalf("expected missingkey error for TreeHash, got %v", err)
	}
}

func TestRenderUsesCanonicalNameAndDoesNotEmitVersionMarker(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := engine.Render("./phase1/repo_analyze.md", map[string]string{
		"RepoURL":   "https://github.com/org/repo",
		"CommitSHA": "abc123",
		"TreeHash":  "tree456",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"https://github.com/org/repo", "abc123", "tree456"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("rendered prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "template-version") {
		t.Fatalf("rendered prompt leaked version marker:\n%s", prompt)
	}
}

func TestRenderRejectsUnknownTemplate(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Render("phase9/missing", struct{}{}); err == nil {
		t.Fatal("expected unknown template error")
	}
}
