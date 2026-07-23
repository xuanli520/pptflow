package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestRunControlAndBudgetCommandsUseLifecycleServices(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	services := openCommandLifecycle(t, root)
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "control-cli", Actor: actor, Reason: "create CLI control fixture"},
		SourceDirectory:        writeCommandTaskSnapshot(t, "control command fixture\n"),
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: commandCompleteProfile(t), ExecutionSpec: commandExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "verify",
		Actor: actor, Reason: "start CLI control fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: actor, Reason: "start worker fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	account, err := services.Store().CreateQuotaAccount(ctx, store.CreateQuotaAccountRequest{
		ScopeKind: store.QuotaScopeTask, ScopeID: task.ID, Dimension: "agent_tokens", LimitUnits: 10,
		Actor: actor, Reason: "configure CLI budget fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root}
	dryRun := newRunCommandV2(config)
	var dryOutput bytes.Buffer
	dryRun.SetOut(&dryOutput)
	dryRun.SetErr(&dryOutput)
	dryRun.SetArgs([]string{"pause", "--run", run.ID, "--operation-key", "pause-cli-dry", "--reason", "inspect pause", "--dry-run"})
	if err := dryRun.ExecuteContext(ctx); err != nil {
		t.Fatalf("pause dry-run: %v\n%s", err, dryOutput.String())
	}
	var preview executionControlPreview
	if err := json.Unmarshal(dryOutput.Bytes(), &preview); err != nil {
		t.Fatalf("decode pause dry-run: %v\n%s", err, dryOutput.String())
	}
	if preview.WillMutate || preview.Action != store.ControlActionPause || preview.Expected.Sequence != uint64(run.Version) || preview.GracePeriod != 30*time.Second {
		t.Fatalf("pause dry-run preview = %+v", preview)
	}
	check := openCommandLifecycle(t, root)
	unchanged, err := check.Runs.Get(ctx, run.ID)
	if err != nil {
		check.Store().Close()
		t.Fatal(err)
	}
	if unchanged.Status != store.WorkflowRunRunning || unchanged.Version != run.Version {
		check.Store().Close()
		t.Fatalf("pause dry-run changed run: %+v", unchanged)
	}
	if err := check.Store().Close(); err != nil {
		t.Fatal(err)
	}

	pause := newRunCommandV2(config)
	var pauseOutput bytes.Buffer
	pause.SetOut(&pauseOutput)
	pause.SetErr(&pauseOutput)
	pause.SetArgs([]string{"pause", "--run", run.ID, "--operation-key", "pause-cli-real", "--reason", "pause run"})
	if err := pause.ExecuteContext(ctx); err != nil {
		t.Fatalf("pause command: %v\n%s", err, pauseOutput.String())
	}
	var operation store.DurableControlOperation
	if err := json.Unmarshal(pauseOutput.Bytes(), &operation); err != nil {
		t.Fatalf("decode pause command output: %v\n%s", err, pauseOutput.String())
	}
	if operation.Status != store.ControlOperationRequested || operation.Action != store.ControlActionPause || operation.GracePeriod != 30*time.Second {
		t.Fatalf("pause operation = %+v", operation)
	}

	grant := newBudgetCommand(config)
	var grantOutput bytes.Buffer
	grant.SetOut(&grantOutput)
	grant.SetErr(&grantOutput)
	grant.SetArgs([]string{
		"grant", "--run", run.ID, "--dimension", "agent_tokens", "--delta", "4",
		"--expected-version", "1", "--idempotency-key", "grant-cli-real", "--reason", "grant fixture budget",
	})
	if err := grant.ExecuteContext(ctx); err != nil {
		t.Fatalf("budget grant command: %v\n%s", err, grantOutput.String())
	}
	var budgetGrant store.DurableBudgetGrant
	if err := json.Unmarshal(grantOutput.Bytes(), &budgetGrant); err != nil {
		t.Fatalf("decode budget grant output: %v\n%s", err, grantOutput.String())
	}
	if budgetGrant.AccountID != account.ID || budgetGrant.LimitUnits != 14 || budgetGrant.Actor != actor {
		t.Fatalf("budget grant = %+v", budgetGrant)
	}

	show := newBudgetCommand(config)
	var showOutput bytes.Buffer
	show.SetOut(&showOutput)
	show.SetErr(&showOutput)
	show.SetArgs([]string{"show", "--run", run.ID})
	if err := show.ExecuteContext(ctx); err != nil {
		t.Fatalf("budget show command: %v\n%s", err, showOutput.String())
	}
	var accounts []store.QuotaAccount
	if err := json.Unmarshal(showOutput.Bytes(), &accounts); err != nil {
		t.Fatalf("decode budget show output: %v\n%s", err, showOutput.String())
	}
	if len(accounts) != 1 || accounts[0].ID != account.ID || accounts[0].LimitUnits != 14 {
		t.Fatalf("budget show accounts = %+v", accounts)
	}
}

func TestBudgetCommandsResolveAuthoringSessionRunQuotaTask(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	services := openCommandLifecycle(t, root)
	digest := "sha256:" + strings.Repeat("b", 64)
	template := workflowadapter.StandardAuthoringCurrentTemplateReference()
	source, err := services.Store().CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: "https://github.com/tower-rs/tower-http.git", CommitSHA: "f066e10ebc07ea9050a2ce4576315abfa568edf4",
		SnapshotArtifactRef: digest, SnapshotContentDigest: digest, SnapshotSchemaVersion: "harbor.source-snapshot.v1",
		IdempotencyKey: "command-authoring-budget-source", Actor: actor, Reason: "freeze authoring budget source",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	task, err := services.Store().CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: "command-authoring-budget", Title: "Command authoring budget", SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA,
		Actor: actor, Reason: "reserve authoring budget task",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	session, err := services.Store().CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: template.ID, WorkflowTemplateVersion: template.Version,
		SessionManifestJSON: `{"mode":"standard"}`, IdempotencyKey: "command-authoring-budget-session", Actor: actor, Reason: "freeze authoring budget session",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err := services.Store().CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:command-authoring-budget-profile", DefinitionHash: "sha256:command-authoring-budget-definition",
		RunManifestJSON: `{}`, Trigger: "task.generate", Actor: actor, Reason: "start authoring budget run",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if run.TaskID != "" {
		services.Store().Close()
		t.Fatalf("authoring Run unexpectedly has task ID %q", run.TaskID)
	}
	account, err := services.Store().CreateQuotaAccount(ctx, store.CreateQuotaAccountRequest{
		ScopeKind: store.QuotaScopeTask, ScopeID: task.ID, Dimension: "agent_tokens", LimitUnits: 10,
		Actor: actor, Reason: "configure authoring CLI budget",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root}
	dryRun := newBudgetCommand(config)
	var dryRunOutput bytes.Buffer
	dryRun.SetOut(&dryRunOutput)
	dryRun.SetErr(&dryRunOutput)
	dryRun.SetArgs([]string{
		"grant", "--run", run.ID, "--dimension", "agent_tokens", "--delta", "4",
		"--expected-version", "1", "--idempotency-key", "authoring-budget-cli-dry", "--reason", "inspect authoring budget", "--dry-run",
	})
	if err := dryRun.ExecuteContext(ctx); err != nil {
		t.Fatalf("authoring budget grant dry-run: %v\n%s", err, dryRunOutput.String())
	}
	var preview budgetGrantPreview
	if err := json.Unmarshal(dryRunOutput.Bytes(), &preview); err != nil {
		t.Fatalf("decode authoring budget dry-run: %v\n%s", err, dryRunOutput.String())
	}
	if preview.TaskID != task.ID || preview.RunID != run.ID || preview.WillMutate {
		t.Fatalf("authoring budget grant preview = %+v", preview)
	}

	show := newBudgetCommand(config)
	var showOutput bytes.Buffer
	show.SetOut(&showOutput)
	show.SetErr(&showOutput)
	show.SetArgs([]string{"show", "--run", run.ID})
	if err := show.ExecuteContext(ctx); err != nil {
		t.Fatalf("authoring budget show: %v\n%s", err, showOutput.String())
	}
	var accounts []store.QuotaAccount
	if err := json.Unmarshal(showOutput.Bytes(), &accounts); err != nil {
		t.Fatalf("decode authoring budget show: %v\n%s", err, showOutput.String())
	}
	if len(accounts) != 1 || accounts[0].ID != account.ID || accounts[0].ScopeID != task.ID || accounts[0].LimitUnits != 10 {
		t.Fatalf("authoring budget show accounts = %+v", accounts)
	}

	grant := newBudgetCommand(config)
	var grantOutput bytes.Buffer
	grant.SetOut(&grantOutput)
	grant.SetErr(&grantOutput)
	grant.SetArgs([]string{
		"grant", "--run", run.ID, "--dimension", "agent_tokens", "--delta", "4",
		"--expected-version", "1", "--idempotency-key", "authoring-budget-cli-real", "--reason", "grant authoring budget",
	})
	if err := grant.ExecuteContext(ctx); err != nil {
		t.Fatalf("authoring budget grant: %v\n%s", err, grantOutput.String())
	}
	var budgetGrant store.DurableBudgetGrant
	if err := json.Unmarshal(grantOutput.Bytes(), &budgetGrant); err != nil {
		t.Fatalf("decode authoring budget grant: %v\n%s", err, grantOutput.String())
	}
	if budgetGrant.AccountID != account.ID || budgetGrant.ScopeID != task.ID || budgetGrant.LimitUnits != 14 || budgetGrant.Actor != actor {
		t.Fatalf("authoring budget grant = %+v", budgetGrant)
	}
}

func TestRunControlCommandRequiresOperationKeyAndRejectsGraceOverride(t *testing.T) {
	config := &lifecycleCLIConfig{root: t.TempDir()}
	command := newRunCommandV2(config)
	command.SetArgs([]string{"pause", "--run", "00000000-0000-7000-8000-000000000001", "--reason", "missing operation key"})
	if err := command.ExecuteContext(context.Background()); err == nil {
		t.Fatal("pause command accepted a missing operation key")
	}
	command = newRunCommandV2(config)
	command.SetArgs([]string{"pause", "--run", "00000000-0000-7000-8000-000000000001", "--operation-key", "key", "--grace", "5s", "--reason", "attempt override"})
	if err := command.ExecuteContext(context.Background()); err == nil {
		t.Fatal("pause command accepted a caller-controlled grace override")
	}
}

func commandCompleteProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	catalog := workflowadapter.StandardStageCatalog()
	profile := workflowadapter.ExecutionProfile{
		Template:            workflowadapter.StandardTemplateReference(),
		ID:                  "command-integration",
		Version:             "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod:  30 * time.Second,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{
			AttemptTimeout: time.Second,
		},
	}
	for _, stage := range catalog.Stages {
		turns := stage.RequiredTurns
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{
			StageKey: stage.Key,
			Budget: workflowkit.ExecutionBudget{
				TurnTimeout:    time.Second,
				MaxTurns:       turns,
				AttemptTimeout: time.Duration(turns) * time.Second,
				MaxAttempts:    1,
				MaxElapsed:     time.Duration(turns) * time.Second,
			},
		})
	}
	return profile
}

func TestCommandCompleteProfileFreezesStandardTemplateInProfileJSON(t *testing.T) {
	profile := commandCompleteProfile(t)
	if !profile.Template.Equal(workflowadapter.StandardTemplateReference()) {
		t.Fatalf("command profile template = %#v, want %#v", profile.Template, workflowadapter.StandardTemplateReference())
	}
	raw, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical command profile: %v", err)
	}
	var document struct {
		Template workflowadapter.TemplateReference `json:"template"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode canonical command profile: %v", err)
	}
	if !document.Template.Equal(workflowadapter.StandardTemplateReference()) {
		t.Fatalf("canonical command profile template = %#v, want %#v", document.Template, workflowadapter.StandardTemplateReference())
	}
}
