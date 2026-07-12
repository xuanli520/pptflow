package gen

import (
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func TestGenerationPromptsRenderEmbeddedInputs(t *testing.T) {
	repoPrompt, err := repoAnalyzePrompt(domain.RepoPrepared{
		RepoURL:        "https://github.com/org/repo",
		ResolvedCommit: "abc123",
		TreeHash:       "tree456",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"https://github.com/org/repo", "abc123", "tree456", "harbor.repo_analysis.v1"} {
		if !strings.Contains(repoPrompt, want) {
			t.Fatalf("repo prompt missing %q", want)
		}
	}

	designPrompt, err := taskDesignPrompt(`{"schema_version":"harbor.repo_analysis.v1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(designPrompt, "harbor.repo_analysis.v1") || !strings.Contains(designPrompt, "harbor.task_proposal.v1") {
		t.Fatalf("unexpected task design prompt:\n%s", designPrompt)
	}

	filesPrompt, err := taskFilesPrompt(`{"repo":"canonical"}`, `{"proposal":"canonical"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"repo":"canonical"`, `"proposal":"canonical"`, "harbor.generated_task_files.v1"} {
		if !strings.Contains(filesPrompt, want) {
			t.Fatalf("task files prompt missing %q", want)
		}
	}

	selfCheck, err := runtimeSelfCheckPrompt()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(selfCheck, "Run solution/solve.sh followed by tests/test.sh") {
		t.Fatalf("unexpected runtime self-check prompt:\n%s", selfCheck)
	}
}
