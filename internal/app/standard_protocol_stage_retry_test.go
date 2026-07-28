package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardProtocolStageRetryAdmitsEveryStandardAuthoringAgentStage(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	profile := standardAuthoringLaunchTestProfile()
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	var agentStages int
	for _, stage := range resolved.Descriptor.Stages {
		if stage.AgentRole == nil {
			if standardProtocolRetryAgentStage(stage) {
				t.Fatalf("non-agent Standard authoring stage %q was admitted", stage.Key)
			}
			continue
		}
		agentStages++
		if !standardProtocolRetryAgentStage(stage) {
			t.Fatalf("Standard authoring Agent stage %q (%+v) was rejected", stage.Key, *stage.AgentRole)
		}
	}
	if agentStages == 0 {
		t.Fatal("Standard authoring workflow has no Agent stages")
	}
}

func TestRunServicePreviewStandardProtocolStageRetryRejectsNonProtocolFailureAndInputDrift(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []struct {
		name             string
		failureCode      string
		inputFingerprint workflowkit.Fingerprint
	}{
		{name: "non protocol failure", failureCode: "agent.provider.timeout"},
		{name: "frozen input drift", failureCode: stageprovider.StandardAuthoringProtocolFailureMissingSubmission, inputFingerprint: "sha256:drifted-inputs"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newStandardProtocolRetryFixture(t, ctx, scenario.failureCode, scenario.inputFingerprint)
			_, err := fixture.services.Runs.PreviewStandardProtocolStageRetry(ctx, fixture.run.ID, fixture.source.ID)
			if !errors.Is(err, ErrStandardProtocolStageRetryIneligible) {
				t.Fatalf("preview error = %v, want ineligible retry", err)
			}
		})
	}
}

func TestRunServiceStandardProtocolStageRetryFencesCheckpointAndCommitsOneRetry(t *testing.T) {
	ctx := context.Background()
	fixture := newStandardProtocolRetryFixture(t, ctx, stageprovider.StandardAuthoringProtocolFailureMissingSubmission, "")
	preview, err := fixture.services.Runs.PreviewStandardProtocolStageRetry(ctx, fixture.run.ID, fixture.source.ID)
	if err != nil {
		t.Fatalf("preview retry: %v", err)
	}
	if preview.SourceStageAttempt.ID != fixture.source.ID || preview.SourceNodeAttempt.ID != fixture.node.ID || preview.Transcript.FailureCode != fixture.source.ErrorText || preview.RetryGeneration != preview.SourceGeneration+1 {
		t.Fatalf("preview = %+v", preview)
	}

	staleKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RecordAgentTurnTranscriptWithCheckpoint(ctx, store.RecordAgentTurnTranscriptWithCheckpointRequest{
		Transcript: store.CreateAgentTurnTranscriptRequest{
			NodeAttemptID: fixture.node.ID, Turn: 2, ResponseText: "the retry preview is now stale", ModelID: "test-model",
			SubmissionStatus: store.AgentTurnSubmissionRejected, ProtocolRejectionCode: fixture.source.ErrorText, FailureCode: fixture.source.ErrorText,
			Actor: "tester", Reason: "record newer protocol rejection",
		},
		Checkpoint: store.AgentTurnTranscriptCheckpoint{NodeAttemptID: fixture.node.ID, Turn: 2, Substep: "turn_completed", InputDigest: fixture.source.InputFingerprint, PayloadJSON: `{}`},
		Actor:      "tester", Reason: "record newer protocol rejection",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.services.Runs.RetryStandardProtocolStage(ctx, StandardProtocolStageRetryCommand{
		IdempotencyKey: staleKey, Actor: "operator", Reason: "confirm stale protocol retry", RunID: fixture.run.ID,
		SourceStageAttemptID: fixture.source.ID, Expected: preview.Checkpoint,
	})
	if !errors.Is(err, ErrStandardProtocolStageRetryStale) {
		t.Fatalf("stale confirmation error = %v, want stale preview", err)
	}

	preview, err = fixture.services.Runs.PreviewStandardProtocolStageRetry(ctx, fixture.run.ID, fixture.source.ID)
	if err != nil {
		t.Fatalf("refresh retry preview: %v", err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardProtocolStageRetryCommand{
		IdempotencyKey: key, Actor: "operator", Reason: "confirm protocol-only same-stage retry", RunID: fixture.run.ID,
		SourceStageAttemptID: fixture.source.ID, Expected: preview.Checkpoint,
	}
	receipt, err := fixture.services.Runs.RetryStandardProtocolStage(ctx, command)
	if err != nil {
		t.Fatalf("commit retry: %v", err)
	}
	if receipt.Replayed || receipt.Source.ID != fixture.source.ID || receipt.Retry.RetryOfStageAttemptID != fixture.source.ID ||
		receipt.Retry.Ordinal != fixture.source.Ordinal+1 || receipt.Retry.ExecutionStatus != store.StageExecutionQueued ||
		receipt.Run.Status != store.WorkflowRunRunning || receipt.Run.ExecutionEpoch != fixture.run.ExecutionEpoch+1 ||
		receipt.RunAttempt.ResumeFrom != "stage_attempt:"+fixture.source.ID || receipt.Job.StageAttemptID != receipt.Retry.ID {
		t.Fatalf("retry receipt = %+v", receipt)
	}
	var retrySnapshot runtimeStageAttemptSnapshot
	if err := json.Unmarshal([]byte(receipt.Retry.RetrySnapshotJSON), &retrySnapshot); err != nil || retrySnapshot.Generation != preview.RetryGeneration || retrySnapshot.ExecutionKey != "initial" {
		t.Fatalf("retry snapshot = %+v, %v", retrySnapshot, err)
	}
	var payload frozenStageExecutionPayload
	if err := json.Unmarshal([]byte(receipt.Job.PayloadJSON), &payload); err != nil || payload.StageAttemptID != receipt.Retry.ID || payload.Generation != preview.RetryGeneration || payload.StageKey != preview.StageKey {
		t.Fatalf("retry payload = %+v, %v", payload, err)
	}
	pending, err := fixture.store.ListPendingOutboxEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundOutbox := false
	for _, event := range pending {
		if event.Topic == store.DurableJobQueuedOutboxTopic && event.EntityID == receipt.Job.ID && event.IdempotencyKey == standardProtocolStageRetryJobKey(key)+":queued" {
			foundOutbox = true
		}
	}
	if !foundOutbox {
		t.Fatalf("retry job outbox is missing from %+v", pending)
	}

	replayed, err := fixture.services.Runs.RetryStandardProtocolStage(ctx, command)
	if err != nil || !replayed.Replayed || replayed.Retry.ID != receipt.Retry.ID || replayed.Job.ID != receipt.Job.ID || replayed.RunAttempt.ID != receipt.RunAttempt.ID {
		t.Fatalf("retry replay = %+v, %v", replayed, err)
	}
}

type standardProtocolRetryFixture struct {
	services *LifecycleServices
	store    *store.Store
	run      store.WorkflowRun
	source   store.StageAttempt
	node     store.NodeAttempt
}

func newStandardProtocolRetryFixture(t *testing.T, ctx context.Context, failureCode string, inputFingerprintOverride workflowkit.Fingerprint) standardProtocolRetryFixture {
	t.Helper()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	launchKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	launched, err := services.AuthoringLaunches.Start(ctx, StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: launchKey, Actor: "tester", Reason: "create protocol retry fixture"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		BaseImage: standardAuthoringLaunchTestBaseImage, TaskType: standardAuthoringLaunchTestTaskType, Application: standardAuthoringLaunchTestApplication,
		CodeLang: "rust", Objective: standardAuthoringLaunchTestObjective, Slug: "protocol-retry-fixture", Title: "Protocol retry fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.GetWorkflowRun(ctx, launched.RunID)
	if err != nil || run == nil {
		t.Fatalf("load launched run = %+v, %v", run, err)
	}
	runValue, err := database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "tester", Reason: "prepare protocol retry fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run = &runValue
	frozen, err := decodeFrozenRunDefinition(*run)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := services.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		t.Fatal(err)
	}
	repoStage, found := frozen.Workflow.Stage(workflowkit.StageKey(workflowadapter.RepoPrepare))
	if !found {
		t.Fatal("frozen Standard authoring workflow omitted repo_prepare")
	}
	repoInputs, err := resolveStageInputsForSubjectWithExplicitInputs(ctx, database, services.core.objects, *run, subject, repoStage, nil)
	if err != nil {
		t.Fatal(err)
	}
	repoInputFingerprint, err := workflowkit.FingerprintArtifactBindings(repoInputs)
	if err != nil {
		t.Fatal(err)
	}
	repoSnapshot, err := json.Marshal(runtimeStageAttemptSnapshot{Format: runtimeStageAttemptSnapshotFormat, ExecutionKey: "initial", Generation: 0})
	if err != nil {
		t.Fatal(err)
	}
	repoAttempt, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: string(repoStage.Key), StageGroup: repoStage.Group, Ordinal: 1, InputFingerprint: string(repoInputFingerprint),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: string(repoSnapshot), Actor: "tester", Reason: "seed repo preparation lineage",
	})
	if err != nil {
		t.Fatal(err)
	}
	repoNode, err := database.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: repoAttempt.ID, NodeID: string(repoStage.Key), Generation: 0, Attempt: 1,
		IdempotencyKey: "protocol-retry-repo-node:" + repoAttempt.ID, Actor: "tester", Reason: "seed repo preparation lineage",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := persistStageArtifactsForSubject(ctx, services.core, *run, subject, repoAttempt, repoNode, repoStage, repoInputs, []StageArtifact{{
		Key: "repo_prepared", SchemaVersion: "harbor.artifact.v1", Content: []byte("prepared immutable source"), TurnOrdinal: 1,
	}}, "tester", "seed repo preparation lineage")
	if err != nil {
		t.Fatal(err)
	}
	repoAttempt, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: repoAttempt.ID, ExpectedVersion: repoAttempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "tester", Reason: "start repo preparation lineage",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{NodeAttemptID: repoNode.ID, ExpectedVersion: repoNode.Version, Status: store.NodeAttemptRunning, Actor: "tester", Reason: "start repo preparation node"}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: repoAttempt.ID, ExpectedVersion: repoAttempt.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass,
		ArtifactManifestID: manifest.ID, Actor: "tester", Reason: "complete repo preparation lineage",
	}); err != nil {
		t.Fatal(err)
	}

	stage, found := frozen.Workflow.Stage(workflowkit.StageKey(workflowadapter.RepoStructureResearch))
	if !found || !standardProtocolRetryAgentStage(stage) {
		t.Fatal("frozen Standard authoring workflow omitted retryable research Agent stage")
	}
	inputs, err := resolveStageInputsForSubjectWithExplicitInputs(ctx, database, services.core.objects, *run, subject, stage, nil)
	if err != nil {
		t.Fatal(err)
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if inputFingerprintOverride != "" {
		inputFingerprint = inputFingerprintOverride
	}
	source, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1, InputFingerprint: string(inputFingerprint),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: string(repoSnapshot), Actor: "tester", Reason: "create protocol-failed Agent stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: source.ID, ExpectedVersion: source.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "tester", Reason: "start protocol-failed Agent stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: source.ID, NodeID: string(stage.Key), Generation: 0, Attempt: 1,
		IdempotencyKey: "protocol-retry-node:" + source.ID, Actor: "tester", Reason: "record failed Agent node",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err = database.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptRunning, Actor: "tester", Reason: "start failed Agent node",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err = database.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptInfraFailed, ErrorText: failureCode, Actor: "tester", Reason: "record protocol rejection",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordAgentTurnTranscriptWithCheckpoint(ctx, store.RecordAgentTurnTranscriptWithCheckpointRequest{
		Transcript: store.CreateAgentTurnTranscriptRequest{
			NodeAttemptID: node.ID, Turn: 1, ResponseText: "agent response rejected by the terminal protocol", ModelID: "test-model",
			SubmissionStatus: store.AgentTurnSubmissionRejected, ProtocolRejectionCode: failureCode, FailureCode: failureCode,
			Actor: "tester", Reason: "record protocol rejection transcript",
		},
		Checkpoint: store.AgentTurnTranscriptCheckpoint{NodeAttemptID: node.ID, Turn: 1, Substep: "turn_completed", InputDigest: string(inputFingerprint), PayloadJSON: `{}`},
		Actor:      "tester", Reason: "record protocol rejection transcript",
	}); err != nil {
		t.Fatal(err)
	}
	source, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: source.ID, ExpectedVersion: source.Version, ExecutionStatus: store.StageExecutionInfraFailed, ErrorText: failureCode,
		FailureClass: "protocol", Actor: "tester", Reason: "record protocol-only stage failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	runValue, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunFailedRecoverable, Actor: "tester", Reason: "protocol-only stage failure is recoverable",
	})
	if err != nil {
		t.Fatal(err)
	}
	return standardProtocolRetryFixture{services: services, store: database, run: runValue, source: source, node: node}
}
