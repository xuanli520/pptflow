package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
)

type commandAuthoringHandoffDefinitionProvider struct{}

func (commandAuthoringHandoffDefinitionProvider) DefinitionForCodeEdgePhase1Run(context.Context, app.CodeEdgePhase1RunDefinitionRequest) (app.CodeEdgePhase1RunDefinition, error) {
	// Redrive only checks that a controlled provider is installed. The worker
	// invokes the provider later against the persisted handoff artifact.
	return app.CodeEdgePhase1RunDefinition{}, nil
}

func TestAuthoringHandoffRedriveCommandRepublishesOnlyInDoubtDeliveryIdempotently(t *testing.T) {
	ctx := context.Background()
	if defaultLifecycleActor() == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	run, original := commandInDoubtAuthoringHandoffFixture(t, ctx, database)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	config := &lifecycleCLIConfig{
		root: root,
		newLifecycleService: func(factoryRoot string, dataStore *store.Store) (*app.LifecycleServices, error) {
			return app.NewLifecycleServicesWithOptions(factoryRoot, dataStore, app.LifecycleServicesOptions{
				OperationResolver:                   testsupport.AcceptAllStageOperationResolver(),
				CodeEdgePhase1RunDefinitionProvider: commandAuthoringHandoffDefinitionProvider{},
			})
		},
	}
	key := commandLifecycleUUID(t)
	args := []string{"handoff", "redrive", "--authoring-run", run.ID, "--idempotency-key", key, "--reason", "install approved Phase-1 definition"}
	output, err := executeAuthoringCommand(t, ctx, config, args)
	if err != nil {
		t.Fatalf("authoring handoff redrive: %v\n%s", err, output)
	}
	var result authoringHandoffRedriveOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode handoff redrive output: %v\n%s", err, output)
	}
	if result.JobID == "" || result.CommandType != "standard_authoring.handoff.redrive" || result.State != string(store.JobQueued) || result.AuthoringRunID != run.ID {
		t.Fatalf("handoff redrive output = %+v", result)
	}
	if strings.Contains(output, "payload") || strings.Contains(output, original.PayloadJSON) {
		t.Fatalf("handoff redrive CLI exposed durable payload: %s", output)
	}
	replayOutput, err := executeAuthoringCommand(t, ctx, config, args)
	if err != nil {
		t.Fatalf("authoring handoff redrive replay: %v\n%s", err, replayOutput)
	}
	var replay authoringHandoffRedriveOutput
	if err := json.Unmarshal([]byte(replayOutput), &replay); err != nil || replay.JobID != result.JobID {
		t.Fatalf("handoff redrive replay = %+v err=%v; first=%+v", replay, err, result)
	}

	check, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	persistedOriginal, err := check.GetDurableJob(ctx, original.ID)
	if err != nil || persistedOriginal == nil || persistedOriginal.State != store.JobInDoubt {
		t.Fatalf("original handoff after CLI redrive = %+v, %v; want in_doubt", persistedOriginal, err)
	}
	jobs, err := check.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	redriveCount := 0
	for _, job := range jobs {
		if job.CommandType == "standard_authoring.handoff.redrive" {
			redriveCount++
			if job.ID != result.JobID || job.PayloadJSON != original.PayloadJSON {
				t.Fatalf("redrive job did not preserve original immutable payload: %+v", job)
			}
		}
	}
	if redriveCount != 1 {
		t.Fatalf("redrive jobs = %d; want exactly one", redriveCount)
	}
}

func TestAuthoringHandoffRedriveCommandExposesOnlyOperatorInputs(t *testing.T) {
	command, _, err := newAuthoringCommand(&lifecycleCLIConfig{root: t.TempDir()}).Find([]string{"handoff", "redrive"})
	if err != nil || command == nil || command.Name() != "redrive" {
		t.Fatalf("find authoring handoff redrive command: command=%v err=%v", command, err)
	}
	for _, required := range []string{"authoring-run", "idempotency-key", "reason"} {
		if command.Flags().Lookup(required) == nil {
			t.Fatalf("authoring handoff redrive is missing --%s", required)
		}
	}
	for _, forbidden := range []string{"child-run", "profile", "execution-spec", "provider", "model", "agent", "secret", "payload", "catalog", "lock"} {
		if command.Flags().Lookup(forbidden) != nil {
			t.Fatalf("authoring handoff redrive exposes deployment-owned override --%s", forbidden)
		}
	}
}

func commandInDoubtAuthoringHandoffFixture(t *testing.T, ctx context.Context, database *store.Store) (store.WorkflowRun, store.DurableJob) {
	t.Helper()
	sourceDigest := "sha256:" + strings.Repeat("a", 64)
	source, err := database.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: "https://github.com/tower-rs/tower-http.git", CommitSHA: "f066e10ebc07ea9050a2ce4576315abfa568edf4",
		SnapshotArtifactRef: sourceDigest, SnapshotContentDigest: sourceDigest, SnapshotSchemaVersion: "harbor.source-snapshot.v1",
		IdempotencyKey: "command-handoff-source-" + t.Name(), Actor: "author", Reason: "freeze command handoff source",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: "command-handoff-" + strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-")), Title: "Command handoff",
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author", Reason: "reserve command handoff task",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringWorkflowTemplateVersion, SessionManifestJSON: `{"mode":"standard"}`,
		IdempotencyKey: "command-handoff-session-" + t.Name(), Actor: "author", Reason: "freeze command handoff session",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:command-handoff-profile", DefinitionHash: "sha256:command-handoff-definition", RunManifestJSON: `{}`,
		Trigger: "task.generate", Actor: "author", Reason: "create command handoff Run",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.MaterializeTask, StageGroup: string(workflowadapter.StageTaskGeneration), Ordinal: 1,
		InputFingerprint: "sha256:command-handoff-stage", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "worker", Reason: "create command handoff materialization stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	handoffArtifactID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	childRunID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{
		"format": "harbor.standard-authoring-handoff-job.v1", "authoring_run_id": run.ID, "stage_attempt_id": stage.ID,
		"handoff_artifact_id": handoffArtifactID, "child_run_id": childRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := database.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "standard_authoring.handoff", EntityType: "artifact_ref", EntityID: handoffArtifactID, RunID: run.ID, StageAttemptID: stage.ID,
		PayloadJSON: string(payload), IdempotencyKey: "standard-authoring-handoff:" + stage.ID, Actor: "worker", Reason: "create in_doubt command handoff",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err = database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{JobID: job.ID, ExpectedVersion: job.Version, State: store.JobRunning, Actor: "worker", Reason: "claim command handoff"})
	if err != nil {
		t.Fatal(err)
	}
	job, err = database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{JobID: job.ID, ExpectedVersion: job.Version, State: store.JobInDoubt, Actor: "worker", Reason: "hold command handoff for approved definition"})
	if err != nil {
		t.Fatal(err)
	}
	return run, job
}
