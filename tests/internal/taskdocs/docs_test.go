package taskdocs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

func TestAttachPreservesArbitraryNameAndBuildsContext(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "自测 report 01.md")
	if err := os.WriteFile(source, []byte("# self test\nok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := taskdocs.Attach(root, "TASK-1", source, "note", "tester", config.Default().Docs)
	if err != nil {
		t.Fatal(err)
	}
	if doc.OriginalName != "自测 report 01.md" || !doc.TextIncluded {
		t.Fatalf("unexpected doc: %#v", doc)
	}
	if _, err := os.Stat(filepath.Join(taskdocs.StoreDir(root, "TASK-1"), "files", doc.StoredName)); err != nil {
		t.Fatal(err)
	}
	context, err := taskdocs.BuildContext(root, "TASK-1", config.Default().Docs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(context.Text, "BEGIN UNTRUSTED ATTACHED DOC") || !strings.Contains(context.Text, "# self test") {
		t.Fatalf("context missing attached doc: %s", context.Text)
	}
}

func TestAttachListsBinaryWithoutEmbedding(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(source, []byte{0, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := taskdocs.Attach(root, "TASK-1", source, "", "tester", config.Default().Docs)
	if err != nil {
		t.Fatal(err)
	}
	if doc.TextIncluded || doc.SkipReason == "" {
		t.Fatalf("binary doc should be listed with skip reason: %#v", doc)
	}
}
