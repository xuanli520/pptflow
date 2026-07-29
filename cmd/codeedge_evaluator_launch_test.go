package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
	"github.com/spf13/cobra"
)

type commandEvaluatorDefinitionProvider struct {
	profile workflowadapter.ExecutionProfile
	spec    workflowadapter.RunExecutionSpec
	err     error
	calls   int
}

func (provider *commandEvaluatorDefinitionProvider) DefinitionForEvaluatorRun(_ context.Context, _ app.EvaluatorRunDefinitionRequest) (app.EvaluatorRunDefinition, error) {
	if provider == nil {
		return app.EvaluatorRunDefinition{}, app.ErrCodeEdgeEvaluatorDefinitionUnavailable
	}
	provider.calls++
	if provider.err != nil {
		return app.EvaluatorRunDefinition{}, provider.err
	}
	return app.EvaluatorRunDefinition{Profile: provider.profile.Clone(), ExecutionSpec: provider.spec.Clone()}, nil
}

type recordingCommandEvaluatorWorkerLauncher struct {
	requests []app.RunWorkerHandoffLaunchRequest
}

func (launcher *recordingCommandEvaluatorWorkerLauncher) LaunchRunWorker(_ context.Context, request app.RunWorkerHandoffLaunchRequest) (app.RunWorkerHandoffLaunchReceipt, error) {
	launcher.requests = append(launcher.requests, request)
	return app.RunWorkerHandoffLaunchReceipt{
		RunID: request.RunID, Owner: request.Owner, ProcessID: 8600 + len(launcher.requests),
		LogPath: filepath.Join("/tmp", "command-codeedge-evaluator-"+request.HandoffOperationID+".log"),
	}, nil
}

func TestRunEvaluateCLIRequiresPreparedFrozenInputsAndReplaysOneWorkerHandoff(t *testing.T) {
	ctx := context.Background()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root := t.TempDir()
	provider := &commandEvaluatorDefinitionProvider{profile: commandCodeEdgeEvaluatorProfile(t)}
	factory := func(factoryRoot string, database *store.Store) (*app.LifecycleServices, error) {
		return app.NewLifecycleServicesWithOptions(factoryRoot, database, app.LifecycleServicesOptions{
			OperationResolver:              testsupport.AcceptAllStageOperationResolver(),
			EvaluatorRunDefinitionProvider: provider,
		})
	}
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	services, err := factory(root, database)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	task, revision := createCommandStandardMaterializedTask(t, ctx, root, services, "cli-codeedge-evaluator", "CLI CodeEdge Evaluator", "CodeEdge evaluator CLI fixture\n", actor)
	provider.spec = testsupport.CompleteCodeEdgeEvaluatorChildRunExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	parent, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		Profile:       commandCodeEdgePhase1Profile(t),
		ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger:       "codeedge-evaluator-cli-fixture", Actor: actor, Reason: "freeze CodeEdge evaluator parent fixture",
	})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	parent = commandApproveCodeEdgeFinalReviewGate(t, ctx, services, parent, revision, actor)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root, newLifecycleService: factory}
	provider.err = errors.New("provider credential=command-test-secret must remain private")
	safeErrorOutput, safeError := executeRunEvaluateCommand(t, ctx, newRunEvaluatePrepareCommand(config), []string{
		"--parent-run", parent.ID, "--idempotency-key", commandLifecycleUUID(t), "--reason", "reject unsafe provider error text",
	})
	if !errors.Is(safeError, app.ErrCodeEdgeEvaluatorDefinitionInvalid) || strings.Contains(safeErrorOutput, "command-test-secret") || strings.Contains(safeError.Error(), "command-test-secret") {
		t.Fatalf("CLI exposed a provider error instead of a safe definition failure: output=%q err=%v", safeErrorOutput, safeError)
	}
	provider.err = nil

	key := commandLifecycleUUID(t)
	prepareOutput, err := executeRunEvaluateCommand(t, ctx, newRunEvaluatePrepareCommand(config), []string{
		"--parent-run", parent.ID, "--idempotency-key", key, "--reason", "freeze the approved CodeEdge evaluator",
	})
	if err != nil {
		t.Fatalf("run evaluate prepare: %v\n%s", err, prepareOutput)
	}
	var prepared app.PreparedCodeEdgeEvaluatorLaunch
	if err := json.Unmarshal([]byte(prepareOutput), &prepared); err != nil {
		t.Fatalf("decode prepare output: %v\n%s", err, prepareOutput)
	}
	if prepared.InputBundleID != key || prepared.ParentRunID != parent.ID || prepared.ProfileFingerprint == "" || prepared.ExecutionSpecFingerprint == "" {
		t.Fatalf("prepared evaluator input bundle = %+v", prepared)
	}
	check := openCommandEvaluatorLifecycle(t, root, factory)
	runs, err := check.Store().ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil || len(runs) != 1 || runs[0].ID != parent.ID {
		_ = check.Store().Close()
		t.Fatalf("prepare created evaluator child Run: %+v, %v", runs, err)
	}
	if err := check.Store().Close(); err != nil {
		t.Fatal(err)
	}

	launcher := &recordingCommandEvaluatorWorkerLauncher{}
	confirmArgs := []string{"--parent-run", parent.ID, "--idempotency-key", key, "--reason", "freeze the approved CodeEdge evaluator"}
	confirmOutput, err := executeRunEvaluateCommand(t, ctx, newRunEvaluateConfirmCommand(config, launcher), confirmArgs)
	if err != nil {
		t.Fatalf("run evaluate confirm: %v\n%s", err, confirmOutput)
	}
	var first app.CodeEdgeEvaluatorLaunchResult
	if err := json.Unmarshal([]byte(confirmOutput), &first); err != nil {
		t.Fatalf("decode confirm output: %v\n%s", err, confirmOutput)
	}
	if first.Receipt.RunID == "" || first.Receipt.ParentRunID != parent.ID || first.Handoff.RunID != first.Receipt.RunID || first.Handoff.State != store.RunWorkerHandoffLaunching {
		t.Fatalf("confirm result = %+v", first)
	}
	if len(launcher.requests) != 1 || launcher.requests[0].RunID != first.Receipt.RunID {
		t.Fatalf("confirm worker launches = %+v, want one child worker", launcher.requests)
	}

	replayOutput, err := executeRunEvaluateCommand(t, ctx, newRunEvaluateConfirmCommand(config, launcher), confirmArgs)
	if err != nil {
		t.Fatalf("run evaluate confirm replay: %v\n%s", err, replayOutput)
	}
	var replay app.CodeEdgeEvaluatorLaunchResult
	if err := json.Unmarshal([]byte(replayOutput), &replay); err != nil {
		t.Fatalf("decode replay output: %v\n%s", err, replayOutput)
	}
	if replay.Receipt.RunID != first.Receipt.RunID || replay.Handoff.ID != first.Handoff.ID || len(launcher.requests) != 1 {
		t.Fatalf("confirm replay changed child authority: first=%+v replay=%+v launches=%+v", first, replay, launcher.requests)
	}

	missingPrepareKey := commandLifecycleUUID(t)
	missingOutput, missingErr := executeRunEvaluateCommand(t, ctx, newRunEvaluateConfirmCommand(config, launcher), []string{
		"--parent-run", parent.ID, "--idempotency-key", missingPrepareKey, "--reason", "must reject an unfrozen evaluator launch",
	})
	if missingErr == nil {
		t.Fatalf("confirm without prepare unexpectedly succeeded: %s", missingOutput)
	}
	if len(launcher.requests) != 1 {
		t.Fatalf("confirm without prepare launched a worker: %+v", launcher.requests)
	}
	callsBeforeExistingChildPrepare := provider.calls
	provider.err = errors.New("provider credential=command-test-secret must remain private")
	existingChildOutput, existingChildErr := executeRunEvaluateCommand(t, ctx, newRunEvaluatePrepareCommand(config), []string{
		"--parent-run", parent.ID, "--idempotency-key", commandLifecycleUUID(t), "--reason", "reject competing evaluator launch",
	})
	if !errors.Is(existingChildErr, app.ErrCodeEdgeEvaluatorChildAlreadyExists) || strings.Contains(existingChildOutput, "command-test-secret") || strings.Contains(existingChildErr.Error(), "command-test-secret") {
		t.Fatalf("CLI existing-child rejection = output=%q err=%v", existingChildOutput, existingChildErr)
	}
	if provider.calls != callsBeforeExistingChildPrepare {
		t.Fatalf("existing child prepare invoked provider: calls=%d want %d", provider.calls, callsBeforeExistingChildPrepare)
	}
	check = openCommandEvaluatorLifecycle(t, root, factory)
	defer check.Store().Close()
	runs, err = check.Store().ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("confirm without prepare changed evaluator Run count: %+v, %v", runs, err)
	}
	jobs, err := check.Store().ListDurableJobsForRun(ctx, first.Receipt.RunID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("evaluator child durable jobs = %+v, %v; want one", jobs, err)
	}
	handoffs, err := check.Store().ListRunWorkerHandoffsForRun(ctx, first.Receipt.RunID)
	if err != nil || len(handoffs) != 1 || handoffs[0].ID != first.Handoff.ID {
		t.Fatalf("evaluator child handoffs = %+v, %v; want one", handoffs, err)
	}
}

func executeRunEvaluateCommand(t *testing.T, ctx context.Context, command *cobra.Command, args []string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.ExecuteContext(ctx)
	return output.String(), err
}

func openCommandEvaluatorLifecycle(t *testing.T, root string, factory func(string, *store.Store) (*app.LifecycleServices, error)) *app.LifecycleServices {
	t.Helper()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	services, err := factory(root, database)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	return services
}

func createCommandStandardMaterializedTask(t *testing.T, ctx context.Context, root string, services *app.LifecycleServices, slug, title, instruction, actor string) (store.TaskV2, store.TaskRevision) {
	t.Helper()
	task, session, run, objects := createCommandStandardAuthoringRun(t, ctx, root, services, slug, title, actor)
	run, err := services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: actor, Reason: "run Standard materialization fixture",
	})
	if err != nil {
		t.Fatalf("transition Standard authoring Run fixture: %v", err)
	}

	revisionID := commandLifecycleUUID(t)
	_, digest, manifestID := materializeCommandRevisionSnapshot(t, ctx, root, objects, task.ID, revisionID, instruction)
	result, err := services.Store().MaterializeAuthoringTask(ctx, store.MaterializeAuthoringTaskRequest{
		IdempotencyKey:      "command-standard-materialization:" + slug,
		AuthoringSessionID:  session.ID,
		AuthoringRunID:      run.ID,
		ExpectedTaskVersion: task.Version,
		ExpectedRunVersion:  run.Version,
		RevisionID:          revisionID,
		TaskDigest:          digest,
		ProposalDigest:      string(workflowkit.SHA256Fingerprint([]byte("command-standard-proposal:" + slug))),
		ManifestID:          manifestID,
		ChangeSummary:       "materialize Standard fixture",
		MetadataJSON:        `{}`,
		Actor:               actor,
		Reason:              "materialize Standard fixture task",
	})
	if err != nil {
		t.Fatalf("materialize Standard fixture task: %v", err)
	}
	return result.Task, result.Revision
}

func createCommandStandardAuthoringRun(t *testing.T, ctx context.Context, root string, services *app.LifecycleServices, slug, title, actor string) (store.TaskV2, store.AuthoringSession, store.WorkflowRun, *workflowruntime.ArtifactObjectStore) {
	t.Helper()
	objects, err := workflowruntime.NewArtifactObjectStore(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	sourceObject, err := objects.PutBytes(ctx, commandStandardSourceSnapshot(t))
	if err != nil {
		t.Fatalf("store source snapshot fixture: %v", err)
	}
	reference := workflowadapter.StandardAuthoringCurrentTemplateReference()
	repositoryURL := "https://github.com/purplevoid/" + slug + ".git"
	commitSHA := strings.Repeat("1", 40)
	source, err := services.Store().CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: repositoryURL, CommitSHA: commitSHA,
		SnapshotArtifactRef:   string(sourceObject.Digest),
		SnapshotContentDigest: string(sourceObject.Digest),
		SnapshotSchemaVersion: workflowadapter.CodeEdgeSourceSnapshotSchemaVersion,
		IdempotencyKey:        "command-standard-source:" + slug,
		Actor:                 actor,
		Reason:                "freeze Standard source fixture",
	})
	if err != nil {
		t.Fatalf("create Standard source fixture: %v", err)
	}
	task, err := services.Store().CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: slug, Title: title, SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA,
		Actor: actor, Reason: "reserve Standard draft task fixture",
	})
	if err != nil {
		t.Fatalf("create Standard draft task fixture: %v", err)
	}
	session, err := services.Store().CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID,
		WorkflowTemplateID: reference.ID, WorkflowTemplateVersion: reference.Version,
		SessionManifestJSON: `{"mode":"standard","fixture":true}`,
		IdempotencyKey:      "command-standard-session:" + slug,
		Actor:               actor,
		Reason:              "freeze Standard session fixture",
	})
	if err != nil {
		t.Fatalf("create Standard session fixture: %v", err)
	}
	run, err := services.Store().CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID,
		WorkflowTemplateID: reference.ID, WorkflowTemplateVersion: reference.Version,
		ResolvedProfileHash: string(workflowkit.SHA256Fingerprint([]byte("command-standard-profile:" + slug))),
		DefinitionHash:      string(workflowkit.SHA256Fingerprint([]byte("command-standard-definition:" + slug))),
		RunManifestJSON:     `{}`,
		Trigger:             "command-standard-fixture",
		Actor:               actor,
		Reason:              "start Standard fixture Run",
	})
	if err != nil {
		t.Fatalf("create Standard authoring Run fixture: %v", err)
	}
	return task, session, run, objects
}

func materializeCommandRevisionSnapshot(t *testing.T, ctx context.Context, root string, objects *workflowruntime.ArtifactObjectStore, taskID, revisionID, instruction string) (string, string, string) {
	t.Helper()
	source := writeCommandTaskSnapshot(t, instruction)
	revisionDirectory := filepath.Join(root, "tasks", taskID, "revisions", revisionID)
	snapshot := filepath.Join(revisionDirectory, "snapshot")
	if err := os.MkdirAll(filepath.Dir(revisionDirectory), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snapshot, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, file := range taskpolicy.CanonicalFiles() {
		content, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(file.Path)))
		if os.IsNotExist(err) && file.Environment {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(snapshot, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, content, file.Mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := taskpolicy.ValidateManagedSnapshotV2(snapshot); err != nil {
		t.Fatalf("validate materialized command snapshot: %v", err)
	}
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	manifest := struct {
		Format       string    `json:"format"`
		TaskID       string    `json:"task_id"`
		RevisionID   string    `json:"revision_id"`
		TaskDigest   string    `json:"task_digest"`
		SnapshotPath string    `json:"snapshot_path"`
		CreatedAt    time.Time `json:"created_at"`
	}{
		Format: "harbor.task-revision-manifest.v2", TaskID: taskID, RevisionID: revisionID,
		TaskDigest: digest, SnapshotPath: "snapshot", CreatedAt: time.Now().UTC(),
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	object, err := objects.PutBytes(ctx, manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	manifestFile, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	manifestFile = append(manifestFile, '\n')
	if err := os.WriteFile(filepath.Join(revisionDirectory, "manifest.json"), manifestFile, 0o640); err != nil {
		t.Fatal(err)
	}
	return snapshot, digest, string(object.Digest)
}

func commandStandardSourceSnapshot(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	entries := []struct {
		name    string
		mode    int64
		content []byte
		dir     bool
	}{
		{name: "source/", mode: 0o755, dir: true},
		{name: "source/README.md", mode: 0o644, content: []byte("fixture source tree\n")},
	}
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode}
		if entry.dir {
			header.Typeflag = tar.TypeDir
		} else {
			header.Typeflag = tar.TypeReg
			header.Size = int64(len(entry.content))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if !entry.dir {
			if _, err := writer.Write(entry.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func commandCodeEdgePhase1Profile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	return commandProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate())
}

func commandCodeEdgeEvaluatorProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	return commandProfileForTemplate(t, workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplate())
}

// commandProfileForTemplate mirrors the existing command integration fixture
// shape. It is not a deployment profile: production receives its frozen
// profile only through the catalog-derived evaluator definition provider.
func commandProfileForTemplate(t *testing.T, template workflowadapter.WorkflowTemplate) workflowadapter.ExecutionProfile {
	t.Helper()
	profile := workflowadapter.ExecutionProfile{
		Template:            template.Reference(),
		ID:                  "command-codeedge-evaluator-test",
		Version:             "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod:  30 * time.Second,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{
			AttemptTimeout: time.Second,
		},
	}
	for _, stage := range template.Catalog.Stages {
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

func commandApproveCodeEdgeFinalReviewGate(t *testing.T, ctx context.Context, services *app.LifecycleServices, parent store.WorkflowRun, revision store.TaskRevision, actor string) store.WorkflowRun {
	t.Helper()
	running, err := services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: parent.ID, ExpectedVersion: parent.Version, Status: store.WorkflowRunRunning,
		Actor: actor, Reason: "open approved CodeEdge FinalReview CLI fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, found := workflowadapter.CodeEdgePhase1StageCatalog().Stage(workflowkit.StageKey(workflowadapter.FinalReview))
	if !found {
		t.Fatal("CodeEdge Phase-1 catalog lacks FinalReview")
	}
	inputFingerprint := "sha256:" + strings.Repeat("c", 64)
	stage, err := services.Store().CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: running.ID, StageKey: workflowadapter.FinalReview, StageGroup: string(definition.Group), Ordinal: 1,
		InputFingerprint: inputFingerprint, BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: actor, Reason: "create approved CodeEdge FinalReview CLI fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := services.Store().OpenReviewGate(ctx, store.OpenReviewGateRequest{
		RunID: running.ID, ExpectedRunVersion: running.Version, RevisionID: revision.ID, RevisionDigest: revision.TaskDigest, DefinitionHash: running.DefinitionHash,
		StageAttemptID: stage.ID, ExpectedStageAttemptVersion: stage.Version, StageKey: workflowadapter.FinalReview, ReviewKind: string(workflowadapter.ReviewFinalQuality),
		NodeGeneration: 0, NodeAttempt: 1, InputBindingsJSON: `[]`, InputFingerprint: inputFingerprint, EvidenceManifestDigest: "sha256:command-codeedge-final-review-fixture",
		Actor: actor, Reason: "open approved CodeEdge FinalReview CLI fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Store().RecordReviewGateDecision(ctx, store.RecordReviewGateDecisionRequest{
		ReviewRequestID: opened.Review.ID, RunID: opened.Run.ID, RevisionID: revision.ID, StageAttemptID: opened.StageAttempt.ID,
		ExpectedRevisionDigest: revision.TaskDigest, ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version,
		Action: store.ReviewDecisionApprove, ResolutionPayloadJSON: `{}`, Actor: actor, Reason: "approve CodeEdge FinalReview CLI fixture",
	}); err != nil {
		t.Fatal(err)
	}
	currentStage, err := services.Store().GetStageAttempt(ctx, opened.StageAttempt.ID)
	if err != nil || currentStage == nil {
		t.Fatalf("read approved CodeEdge FinalReview CLI stage = %+v, %v", currentStage, err)
	}
	if _, err := services.Store().TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: currentStage.ID, ExpectedVersion: currentStage.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass,
		Actor: actor, Reason: "complete approved CodeEdge FinalReview CLI fixture",
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := services.Runs.Get(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}
