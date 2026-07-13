package deterministic

import (
	"context"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/lint"
	"github.com/purplevoid/harbor-factory/internal/harbor/similarity"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestCodeEdgeLintPluginValidate(t *testing.T) {
	plugin := CodeEdgeLintPlugin{}
	if err := plugin.Validate(workflow.NodeSpec{ID: "lint", Kind: CodeEdgeLintKind}); err == nil || !strings.Contains(err.Error(), "task_dir") {
		t.Fatalf("expected task_dir validation error, got %v", err)
	}
	if err := plugin.Validate(lintSpec()); err != nil {
		t.Fatal(err)
	}
}

func TestCodeEdgeLintPluginSuccessAndFailure(t *testing.T) {
	store := newStore(t)
	passing := CodeEdgeLintPlugin{Run: func(_ context.Context, opts lint.Options) (domain.LintReport, error) {
		if opts.TaskDir != "/task" || !opts.StrictSubmission {
			t.Fatalf("unexpected lint options: %+v", opts)
		}
		return domain.LintReport{SchemaVersion: "harbor.lint_report.v1", Passed: true}, nil
	}}
	result, err := passing.Execute(context.Background(), workflow.NodeRequest{Spec: lintSpec(), Store: store})
	if err != nil || len(result.Artifacts) != 1 || result.Artifacts[0].Type != "lint_report" {
		t.Fatalf("unexpected lint success: %+v, %v", result, err)
	}

	failing := CodeEdgeLintPlugin{Run: func(context.Context, lint.Options) (domain.LintReport, error) {
		return domain.LintReport{Passed: false}, nil
	}}
	failedResult, err := failing.Execute(context.Background(), workflow.NodeRequest{Spec: lintSpec(), Store: store})
	if err == nil || len(failedResult.Artifacts) != 1 {
		t.Fatalf("expected failed lint with stored report: %+v, %v", failedResult, err)
	}
}

func TestSimilarityPluginValidate(t *testing.T) {
	plugin := SimilarityPlugin{}
	if err := plugin.Validate(workflow.NodeSpec{ID: "similarity", Kind: SimilarityKind, Config: map[string]any{"task_dir": "/task"}}); err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("expected source validation error, got %v", err)
	}
	if err := plugin.Validate(similaritySpec()); err != nil {
		t.Fatal(err)
	}
}

func TestSimilarityPluginSuccessAndFailure(t *testing.T) {
	store := newStore(t)
	passing := SimilarityPlugin{Run: func(_ context.Context, opts similarity.Options) (domain.SimilarityReport, error) {
		if !opts.StrictSources || len(opts.HistoryDirs) != 1 {
			t.Fatalf("similarity plugin weakened source policy: %+v", opts)
		}
		return domain.SimilarityReport{SchemaVersion: "harbor.similarity_report.v1", OverallPass: true, SuccessfulSources: []string{"history:/history"}}, nil
	}}
	result, err := passing.Execute(context.Background(), workflow.NodeRequest{Spec: similaritySpec(), Store: store})
	if err != nil || len(result.Artifacts) != 1 {
		t.Fatalf("unexpected similarity success: %+v, %v", result, err)
	}

	failing := SimilarityPlugin{Run: func(context.Context, similarity.Options) (domain.SimilarityReport, error) {
		return domain.SimilarityReport{OverallPass: true}, nil
	}}
	failedResult, err := failing.Execute(context.Background(), workflow.NodeRequest{Spec: similaritySpec(), Store: store})
	if err == nil || len(failedResult.Artifacts) != 1 {
		t.Fatalf("expected no-source similarity failure with artifact: %+v, %v", failedResult, err)
	}
}

func lintSpec() workflow.NodeSpec {
	return workflow.NodeSpec{ID: "codeedge_lint", Kind: CodeEdgeLintKind, Config: map[string]any{"task_dir": "/task", "strict_submission": true}}
}

func similaritySpec() workflow.NodeSpec {
	return workflow.NodeSpec{ID: "similarity_check", Kind: SimilarityKind, Config: map[string]any{"task_dir": "/task", "history_dirs": []string{"/history"}}}
}

func newStore(t *testing.T) *workflow.FileArtifactStore {
	t.Helper()
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}
