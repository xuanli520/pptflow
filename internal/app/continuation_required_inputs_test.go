package app

import (
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestSameRunContinuationFreezesRequiredScheduledInputSubset(t *testing.T) {
	workflow := parallelCoordinatorWorkflow()
	repair := workflowkit.ArtifactBinding{
		Name:          "review_feedback",
		ArtifactID:    "review-feedback-artifact",
		ContentDigest: workflowkit.SHA256Fingerprint([]byte("request changes")),
		SchemaVersion: "harbor.review-decision.v1",
	}
	workflow.Stages[0].Inputs = []workflowkit.ArtifactSpec{{Name: repair.Name, SchemaVersion: repair.SchemaVersion, Required: false}}
	definition, err := workflow.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	empty, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := continuationRunState{
		ReuseStates: make([]workflowkit.StageReuseState, 0, len(workflow.Stages)),
		Generations: map[workflowkit.NodeID]int{},
		Latest:      map[workflowkit.NodeID]store.StageAttempt{},
	}
	for _, stage := range workflow.Stages {
		state.ReuseStates = append(state.ReuseStates, workflowkit.StageReuseState{
			NodeID: stage.Key, Present: true, ArtifactsIntact: true, ExpectedInputFingerprint: empty,
		})
	}
	invalidation, err := workflowkit.PlanInvalidation(workflow, workflowkit.InvalidationRequest{
		RecomputeNodes: []workflowkit.NodeID{workflow.Stages[0].Key}, ReuseStates: state.ReuseStates,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := workflowkit.SubjectDigest(workflowkit.SHA256Fingerprint([]byte("subject")))
	checkpoint := workflowkit.CheckpointRef{
		Sequence: 1, SubjectID: "subject", SubjectRevisionID: "revision", SubjectDigest: digest,
		WorkflowFingerprint: definition,
	}
	snapshot, err := buildSameRunContinuationPlan(
		"plan", "command", continuationPlanInput{Expected: checkpoint, RequiredScheduledInputs: map[workflowkit.NodeID][]workflowkit.ArtifactBinding{
			workflow.Stages[0].Key: {repair},
		}},
		store.WorkflowRun{ID: "run"}, "revision", digest, workflow, state, invalidation,
		[]workflowkit.NodeID{workflow.Stages[0].Key}, workflowkit.StrategyRetryAttempt, time.Now().Add(time.Hour), true,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range snapshot.Nodes {
		if transition.NodeID != workflow.Stages[0].Key {
			continue
		}
		wantFingerprint, err := workflowkit.FingerprintArtifactBindings([]workflowkit.ArtifactBinding{repair})
		if err != nil {
			t.Fatal(err)
		}
		if transition.Disposition != workflowkit.DispositionSchedule || len(transition.InputBindings) != 1 || transition.InputBindings[0] != repair || transition.ExpectedInputFingerprint != wantFingerprint {
			t.Fatalf("scheduled repair transition = %+v", transition)
		}
		return
	}
	t.Fatal("repair transition is missing")
}

func TestSameRunContinuationRejectsRequiredInputForPreservedStage(t *testing.T) {
	workflow := parallelCoordinatorWorkflow()
	repair := workflowkit.ArtifactBinding{
		Name: "review_feedback", ArtifactID: "review-feedback-artifact",
		ContentDigest: workflowkit.SHA256Fingerprint([]byte("request changes")), SchemaVersion: "harbor.review-decision.v1",
	}
	workflow.Stages[1].Inputs = []workflowkit.ArtifactSpec{{Name: repair.Name, SchemaVersion: repair.SchemaVersion, Required: false}}
	definition, err := workflow.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	empty, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := continuationRunState{ReuseStates: make([]workflowkit.StageReuseState, 0, len(workflow.Stages)), Generations: map[workflowkit.NodeID]int{}, Latest: map[workflowkit.NodeID]store.StageAttempt{}}
	for _, stage := range workflow.Stages {
		state.ReuseStates = append(state.ReuseStates, workflowkit.StageReuseState{NodeID: stage.Key, Present: true, ArtifactsIntact: true, ExpectedInputFingerprint: empty})
	}
	invalidation, err := workflowkit.PlanInvalidation(workflow, workflowkit.InvalidationRequest{RecomputeNodes: []workflowkit.NodeID{workflow.Stages[0].Key}, ReuseStates: state.ReuseStates})
	if err != nil {
		t.Fatal(err)
	}
	digest := workflowkit.SubjectDigest(workflowkit.SHA256Fingerprint([]byte("subject")))
	_, err = buildSameRunContinuationPlan(
		"plan", "command", continuationPlanInput{Expected: workflowkit.CheckpointRef{
			Sequence: 1, SubjectID: "subject", SubjectRevisionID: "revision", SubjectDigest: digest, WorkflowFingerprint: definition,
		}, RequiredScheduledInputs: map[workflowkit.NodeID][]workflowkit.ArtifactBinding{workflow.Stages[1].Key: {repair}}},
		store.WorkflowRun{ID: "run"}, "revision", digest, workflow, state, invalidation,
		[]workflowkit.NodeID{workflow.Stages[0].Key}, workflowkit.StrategyRetryAttempt, time.Now().Add(time.Hour), true,
	)
	if err == nil || !strings.Contains(err.Error(), "is not scheduled") {
		t.Fatalf("preserved required input error = %v", err)
	}
}

func TestRequirePlannedStageInputsMatchesExactSubset(t *testing.T) {
	required := workflowkit.ArtifactBinding{
		Name: "review_feedback", ArtifactID: "review-feedback-artifact",
		ContentDigest: workflowkit.SHA256Fingerprint([]byte("request changes")), SchemaVersion: "harbor.review-decision.v1",
	}
	transition := workflowkit.NodeTransition{Disposition: workflowkit.DispositionSchedule, InputBindings: []workflowkit.ArtifactBinding{required}}
	extra := workflowkit.ArtifactBinding{
		Name: "source", ArtifactID: "source-artifact", ContentDigest: workflowkit.SHA256Fingerprint([]byte("source")), SchemaVersion: "source.v1",
	}
	if err := requirePlannedStageInputs(transition, []workflowkit.ArtifactBinding{extra, required}); err != nil {
		t.Fatalf("exact planned subset rejected: %v", err)
	}
	changed := required
	changed.ContentDigest = workflowkit.SHA256Fingerprint([]byte("different"))
	if err := requirePlannedStageInputs(transition, []workflowkit.ArtifactBinding{extra, changed}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed planned input error = %v", err)
	}
	if err := requirePlannedStageInputs(transition, []workflowkit.ArtifactBinding{extra}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing planned input error = %v", err)
	}
}
