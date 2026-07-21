package app

import (
	"context"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCurrentStandardAuthoringConsumersSelectRecoveredArtifacts(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryLaunchFixture(t)
	fixture.run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)

	seedCurrentAuthoringArtifactAttempt(t, ctx, fixture, workflowadapter.GenerateTaskFiles, 1, "", "generated_task_files", []byte(`{"files":["instruction.md"]}`))
	seedCurrentAuthoringArtifactAttempt(t, ctx, fixture, workflowadapter.TaskDesign, 1, "", "task_proposal", []byte(`{"feature":"request header limit"}`))

	dockerFirst, _ := seedCurrentAuthoringArtifactAttemptWithCompanions(t, ctx, fixture, workflowadapter.DockerfileBuildValidate, 1, "", workflowadapter.StandardAuthoringValidatedDockerfileArtifact, []byte("FROM first\n"), map[string][]byte{
		workflowadapter.StandardAuthoringDockerfileBuildReportArtifact: []byte(`{"passed":true}`),
	})
	dockerSecond, dockerReference := seedCurrentAuthoringArtifactAttemptWithCompanions(t, ctx, fixture, workflowadapter.DockerfileBuildValidate, 2, dockerFirst.ID, workflowadapter.StandardAuthoringValidatedDockerfileArtifact, []byte("FROM recovered\n"), map[string][]byte{
		workflowadapter.StandardAuthoringDockerfileBuildReportArtifact: []byte(`{"passed":true}`),
	})
	testFirst, _ := seedCurrentAuthoringArtifactAttemptWithCompanions(t, ctx, fixture, workflowadapter.AuthoringHarness, 1, "", workflowadapter.StandardAuthoringValidatedTestScriptArtifact, []byte("#!/bin/sh\necho first\n"), map[string][]byte{
		workflowadapter.StandardAuthoringValidatedSolveScriptArtifact: []byte("#!/bin/sh\necho solve first\n"),
		workflowadapter.StandardAuthoringHarnessReportArtifact:        []byte(`{"passed":true}`),
	})
	testSecond, testReference := seedCurrentAuthoringArtifactAttemptWithCompanions(t, ctx, fixture, workflowadapter.AuthoringHarness, 2, testFirst.ID, workflowadapter.StandardAuthoringValidatedTestScriptArtifact, []byte("#!/bin/sh\necho recovered\n"), map[string][]byte{
		workflowadapter.StandardAuthoringValidatedSolveScriptArtifact: []byte("#!/bin/sh\necho solve recovered\n"),
		workflowadapter.StandardAuthoringHarnessReportArtifact:        []byte(`{"passed":true}`),
	})

	if dockerSecond.Ordinal != 2 || dockerSecond.RetryOfStageAttemptID != dockerFirst.ID || testSecond.Ordinal != 2 || testSecond.RetryOfStageAttemptID != testFirst.ID {
		t.Fatalf("recovered producer attempts lost retry lineage: docker=%+v test=%+v", dockerSecond, testSecond)
	}

	subject, err := fixture.services.core.resolveWorkflowRunSubject(ctx, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		stage       workflowkit.NodeID
		input       string
		want        store.ArtifactRef
		wantContent string
	}{
		{stage: workflowkit.NodeID(workflowadapter.SolveGen), input: workflowadapter.StandardAuthoringValidatedDockerfileArtifact, want: dockerReference, wantContent: "FROM recovered\n"},
		{stage: workflowkit.NodeID(workflowadapter.TestGen), input: workflowadapter.StandardAuthoringValidatedDockerfileArtifact, want: dockerReference, wantContent: "FROM recovered\n"},
		{stage: workflowkit.NodeID(workflowadapter.TestsAnalysis), input: workflowadapter.StandardAuthoringValidatedTestScriptArtifact, want: testReference, wantContent: "#!/bin/sh\necho recovered\n"},
	} {
		t.Run(string(testCase.stage), func(t *testing.T) {
			stage, present := fixture.workflow.Stage(testCase.stage)
			if !present {
				t.Fatalf("current authoring workflow omits %q", testCase.stage)
			}
			bindings, err := resolveStageInputsForSubject(ctx, fixture.store, fixture.services.core.objects, fixture.run, subject, stage)
			if err != nil {
				t.Fatal(err)
			}
			var selected workflowkit.ArtifactBinding
			for _, binding := range bindings {
				if binding.Name == testCase.input {
					selected = binding
					break
				}
			}
			if selected.ArtifactID != workflowkit.ArtifactID(testCase.want.ID) || selected.ContentDigest != workflowkit.Fingerprint(testCase.want.ContentDigest) {
				t.Fatalf("stage %q selected %q = %+v, want recovered artifact %s", testCase.stage, testCase.input, selected, testCase.want.ID)
			}
			reader := newStageInputReaderForSubject(fixture.store, fixture.services.core.objects, fixture.run, subject, bindings)
			content, err := reader(ctx, selected)
			if err != nil || string(content) != testCase.wantContent {
				t.Fatalf("stage %q recovered input content = %q, %v", testCase.stage, content, err)
			}
		})
	}
}

func seedCurrentAuthoringArtifactAttempt(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture, stageKey string, ordinal int, retryOf, artifactKey string, content []byte) (store.StageAttempt, store.ArtifactRef) {
	return seedCurrentAuthoringArtifactAttemptWithCompanions(t, ctx, fixture, stageKey, ordinal, retryOf, artifactKey, content, nil)
}

func seedCurrentAuthoringArtifactAttemptWithCompanions(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture, stageKey string, ordinal int, retryOf, artifactKey string, content []byte, companions map[string][]byte) (store.StageAttempt, store.ArtifactRef) {
	t.Helper()
	stage, present := fixture.workflow.Stage(workflowkit.NodeID(stageKey))
	if !present {
		t.Fatalf("current authoring workflow omits producer %q", stageKey)
	}
	var output workflowkit.ArtifactSpec
	for _, candidate := range stage.Outputs {
		if candidate.Name == artifactKey {
			output = candidate
			break
		}
	}
	if output.Name == "" {
		t.Fatalf("producer %q omits output %q", stageKey, artifactKey)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: fixture.run.ID, RetryOfStageAttemptID: retryOf, StageKey: string(stage.Key), StageGroup: stage.Group,
		Ordinal: ordinal, InputFingerprint: string(emptyInputs), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "worker", Reason: "seed current authoring recovered artifact",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: "worker", Reason: "run current authoring recovered producer",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := fixture.store.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: attempt.ID, NodeID: string(stage.Key), Generation: ordinal - 1, Attempt: 1,
		IdempotencyKey: authoringRecoveryUUID(t), Actor: "worker", Reason: "seed current authoring recovered node",
	})
	if err != nil {
		t.Fatal(err)
	}
	subject, err := fixture.services.core.resolveWorkflowRunSubject(ctx, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	contents := make(map[string][]byte, len(companions)+1)
	contents[artifactKey] = content
	for name, companion := range companions {
		contents[name] = companion
	}
	artifacts := make([]StageArtifact, 0, len(contents))
	for _, specification := range stage.Outputs {
		artifactContent, found := contents[specification.Name]
		if !found {
			continue
		}
		artifacts = append(artifacts, StageArtifact{
			ID: authoringRecoveryUUID(t), Key: specification.Name, SchemaVersion: specification.SchemaVersion,
			Content: append([]byte(nil), artifactContent...), TurnOrdinal: 1,
		})
	}
	manifest, references, err := persistStageArtifactsForSubject(ctx, fixture.services.core, fixture.run, subject, attempt, node, stage, nil, artifacts, "worker", "persist current authoring recovered artifact")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != len(contents) {
		t.Fatalf("producer %q persisted references = %+v", stageKey, references)
	}
	attempt, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCompleted,
		Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID, Actor: "worker", Reason: "complete current authoring recovered producer",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range references {
		if reference.ArtifactKey == artifactKey {
			return attempt, reference
		}
	}
	t.Fatalf("producer %q omitted selected artifact reference %q", stageKey, artifactKey)
	return store.StageAttempt{}, store.ArtifactRef{}
}
