package infra

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/repoprep"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestRepoPreparePluginValidate(t *testing.T) {
	plugin := RepoPreparePlugin{}
	if err := plugin.Validate(workflow.NodeSpec{ID: "repo", Kind: RepoPrepareKind, Config: map[string]any{"repo_url": "https://github.com/org/repo"}}); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("expected missing commit validation error, got %v", err)
	}
	if err := plugin.Validate(repoPrepareSpec()); err != nil {
		t.Fatal(err)
	}
}

func TestRepoPreparePluginExecuteStoresArtifact(t *testing.T) {
	store := newStore(t)
	plugin := RepoPreparePlugin{Prepare: func(_ context.Context, opts repoprep.Options) (domain.RepoPrepared, error) {
		if opts.RepoURL != "https://github.com/org/repo" || opts.Commit != "abc123" || opts.Workspace == "" {
			t.Fatalf("unexpected repo prepare options: %+v", opts)
		}
		return domain.RepoPrepared{SchemaVersion: "harbor.repo_prepared.v1", RepoURL: opts.RepoURL, RequestedCommit: opts.Commit, ResolvedCommit: opts.Commit, TreeHash: "tree"}, nil
	}}
	result, err := plugin.Execute(context.Background(), workflow.NodeRequest{Spec: repoPrepareSpec(), WorkspaceRoot: t.TempDir(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Type != "repo_prepared" || result.Artifacts[0].SHA256 == "" {
		t.Fatalf("unexpected repo artifact: %+v", result)
	}
	var prepared domain.RepoPrepared
	if _, err := store.ReadJSON(context.Background(), result.Artifacts[0].Name, &prepared); err != nil || prepared.ResolvedCommit != "abc123" {
		t.Fatalf("stored repo artifact mismatch: %+v, %v", prepared, err)
	}
}

func TestRepoPreparePluginExecutePropagatesFailure(t *testing.T) {
	want := errors.New("clone failed")
	plugin := RepoPreparePlugin{Prepare: func(context.Context, repoprep.Options) (domain.RepoPrepared, error) {
		return domain.RepoPrepared{}, want
	}}
	_, err := plugin.Execute(context.Background(), workflow.NodeRequest{Spec: repoPrepareSpec(), WorkspaceRoot: t.TempDir(), Store: newStore(t)})
	if !errors.Is(err, want) {
		t.Fatalf("expected prepare failure, got %v", err)
	}
}

func TestHumanGatePluginValidate(t *testing.T) {
	plugin := HumanGatePlugin{}
	if err := plugin.Validate(workflow.NodeSpec{ID: "gate", Kind: HumanGateKind, Config: map[string]any{"phase": "phase1"}}); err == nil || !strings.Contains(err.Error(), "gate_id") {
		t.Fatalf("expected missing gate_id error, got %v", err)
	}
	if err := plugin.Validate(humanGateSpec()); err != nil {
		t.Fatal(err)
	}
}

func TestHumanGatePluginStoresAndReusesStableDecision(t *testing.T) {
	store := newStore(t)
	broker := &fakeGateBroker{decision: domain.GateDecision{Approved: true, Action: "approve", Notes: "reviewed"}}
	now := time.Date(2026, 7, 12, 1, 2, 3, 0, time.UTC)
	req := workflow.NodeRequest{RunID: "run-1", Spec: humanGateSpec(), Attempt: 1, Store: store, Events: workflow.NewEventRecorder()}
	result, err := (HumanGatePlugin{Broker: broker, Now: func() time.Time { return now }}).Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if broker.calls != 1 || broker.request.RequestID != "run-1:r0:task_review" || len(result.Artifacts) != 1 {
		t.Fatalf("unexpected gate result/broker request: %+v %+v", result, broker)
	}

	// A recovered execution consumes the canonical decision without contacting a broker.
	reused, err := (HumanGatePlugin{}).Execute(context.Background(), req)
	if err != nil || len(reused.Artifacts) != 1 {
		t.Fatalf("reusable gate decision failed: %+v, %v", reused, err)
	}
}

func TestHumanGatePluginDerivesChecklistAndPreviewsFromCanonicalArtifacts(t *testing.T) {
	store := newStore(t)
	report := domain.LintReport{
		SchemaVersion: "harbor.lint_report.v1", Passed: false,
		Checks: []domain.CheckResult{
			{ID: "docker_isolation", Status: domain.CheckPass, Message: "Docker context excludes solution and tests"},
			{ID: "oracle_alignment", Status: domain.CheckFail, Message: "oracle does not satisfy verifier"},
		},
	}
	ref, err := store.PutJSON(context.Background(), "phase2/artifacts/codeedge_lint/lint_report.json", "lint_report", "codeedge_lint", report)
	if err != nil {
		t.Fatal(err)
	}
	broker := &fakeGateBroker{decision: domain.GateDecision{Approved: true, Action: "approve"}}
	req := workflow.NodeRequest{RunID: "run-evidence", Spec: humanGateSpec(), Attempt: 1, Store: store, Inputs: []workflow.ArtifactRef{ref}}
	if _, err := (HumanGatePlugin{Broker: broker}).Execute(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	items := map[string]domain.ChecklistItem{}
	for _, item := range broker.request.Checklist {
		items[item.ID] = item
	}
	if item, ok := items["lint_overall"]; !ok || item.Passed || !item.Critical {
		t.Fatalf("lint overall checklist was not derived from report: %+v", broker.request.Checklist)
	}
	if item, ok := items["lint_oracle_alignment"]; !ok || item.Passed || !item.Critical {
		t.Fatalf("lint failure checklist was not derived from report: %+v", broker.request.Checklist)
	}
	if len(broker.request.Artifacts) != 1 || broker.request.Artifacts[0].Path != ref.Path || !strings.Contains(broker.request.Artifacts[0].Content, "oracle_alignment") {
		t.Fatalf("canonical artifact preview missing: %+v", broker.request.Artifacts)
	}
}

func TestHumanGatePluginRejectsDecisionAndPersistsIt(t *testing.T) {
	store := newStore(t)
	plugin := HumanGatePlugin{Broker: &fakeGateBroker{decision: domain.GateDecision{Action: "reject", Approved: false}}}
	result, err := plugin.Execute(context.Background(), workflow.NodeRequest{Spec: humanGateSpec(), Store: store})
	var rejected GateRejectedError
	if !errors.As(err, &rejected) || len(result.Artifacts) != 1 {
		t.Fatalf("expected persisted gate rejection, got result=%+v err=%v", result, err)
	}
	if rejected.Retryable() || rejected.FailureKind() != workflow.FailurePermanent {
		t.Fatalf("gate rejection must be permanent: %+v", rejected)
	}
}

type fakeGateBroker struct {
	decision domain.GateDecision
	err      error
	calls    int
	request  domain.GateRequest
}

func (b *fakeGateBroker) Decide(_ context.Context, request domain.GateRequest) (domain.GateDecision, error) {
	b.calls++
	b.request = request
	return b.decision, b.err
}

func repoPrepareSpec() workflow.NodeSpec {
	return workflow.NodeSpec{ID: "repo_prepare", Kind: RepoPrepareKind, Config: map[string]any{"repo_url": "https://github.com/org/repo", "commit": "abc123"}}
}

func humanGateSpec() workflow.NodeSpec {
	return workflow.NodeSpec{ID: "task_review", Kind: HumanGateKind, Config: map[string]any{"phase": "phase1", "gate_id": "task_review", "gate_name": "Task Review"}}
}

func newStore(t *testing.T) *workflow.FileArtifactStore {
	t.Helper()
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}
