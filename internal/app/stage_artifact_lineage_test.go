package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestPersistStageArtifactsBindsImmutableOutputToFrozenLineage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newLifecycleServicesForTest(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "artifact-lineage", Actor: "tester", Reason: "fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "artifact lineage\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "verify", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = dataStore.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err := dataStore.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "source", StageGroup: "source_prepare", Ordinal: 1, InputFingerprint: string(emptyInputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err = dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeAttempt, err := dataStore.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: stageAttempt.ID, NodeID: "source", Generation: 0, Attempt: 1, IdempotencyKey: "artifact-lineage-node", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeAttempt, err = dataStore.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: nodeAttempt.ID, ExpectedVersion: nodeAttempt.Version, Status: store.NodeAttemptRunning, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage := workflowkit.StageDescriptor{
		Key: "source", Version: "v1", Outputs: []workflowkit.ArtifactSpec{{Name: "report", SchemaVersion: "v1", Required: true}},
	}
	manifest, references, err := persistStageArtifacts(ctx, services.core, run, revision, stageAttempt, nodeAttempt, stage, nil, []StageArtifact{{
		Key: "report", SchemaVersion: "v1", Content: []byte("actual report\n"), TurnOrdinal: 1,
	}}, "tester", "persist actual output")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].ManifestID != manifest.ID || references[0].SubjectRevisionID != revision.ID || references[0].WorkflowFingerprint != run.DefinitionHash {
		t.Fatalf("artifact refs = %+v", references)
	}
	stageAttempt, err = dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionCompleted,
		Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: nodeAttempt.ID, ExpectedVersion: nodeAttempt.Version, Status: store.NodeAttemptCompleted, Actor: "tester", Reason: "fixture",
	}); err != nil {
		t.Fatal(err)
	}
	child := workflowkit.StageDescriptor{
		Key: "child", Version: "v1", Inputs: []workflowkit.ArtifactSpec{{Name: "report", SchemaVersion: "v1", Required: true}},
	}
	bindings, err := resolveStageInputs(ctx, dataStore, services.core.objects, run, revision, child)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].ArtifactID != workflowkit.ArtifactID(references[0].ID) || bindings[0].ContentDigest != workflowkit.Fingerprint(references[0].ContentDigest) {
		t.Fatalf("resolved immutable input bindings = %+v", bindings)
	}
	reader := newStageInputReader(dataStore, services.core.objects, run, revision, bindings)
	content, err := reader(ctx, bindings[0])
	if err != nil || string(content) != "actual report\n" {
		t.Fatalf("read frozen stage input = %q, %v", content, err)
	}
	forged := bindings[0]
	forged.Name = "another-report"
	if _, err := reader(ctx, forged); !errors.Is(err, ErrInvalidStageExecution) {
		t.Fatalf("read forged stage binding = %v, want ErrInvalidStageExecution", err)
	}
	// Replaying the same physical output uses the stage-attempt idempotency
	// keys and cannot create a second manifest/ref lineage record.
	replayed, replayedRefs, err := persistStageArtifacts(ctx, services.core, run, revision, stageAttempt, nodeAttempt, stage, nil, []StageArtifact{{
		Key: "report", SchemaVersion: "v1", Content: []byte("actual report\n"), TurnOrdinal: 1,
	}}, "tester", "persist actual output")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != manifest.ID || len(replayedRefs) != 1 || replayedRefs[0].ID != references[0].ID {
		t.Fatalf("artifact replay changed immutable lineage: manifest=%+v refs=%+v", replayed, replayedRefs)
	}
	if _, _, err := persistStageArtifacts(ctx, services.core, run, revision, stageAttempt, nodeAttempt, stage, nil, []StageArtifact{{
		Key: "report", SchemaVersion: "v1", Content: []byte("different report\n"), TurnOrdinal: 1,
	}}, "tester", "reject changed replay"); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("changed output replay error = %v, want ErrIdempotencyConflict", err)
	}

	manifestIndex, err := loadStageArtifactManifestIndex(ctx, dataStore, manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := manifestIndex.objectFor(references[0])
	if err != nil {
		t.Fatal(err)
	}
	path, err := services.core.objects.ObjectPath(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove immutable object for corruption fixture: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered report\n"), 0o600); err != nil {
		t.Fatalf("write corrupt immutable object: %v", err)
	}
	if _, err := resolveStageInputs(ctx, dataStore, services.core.objects, run, revision, child); !errors.Is(err, workflowruntime.ErrObjectCorrupt) {
		t.Fatalf("corrupt object input resolution error = %v, want ErrObjectCorrupt", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove immutable object for missing fixture: %v", err)
	}
	if _, err := resolveStageInputs(ctx, dataStore, services.core.objects, run, revision, child); !errors.Is(err, workflowruntime.ErrObjectNotFound) {
		t.Fatalf("missing object input resolution error = %v, want ErrObjectNotFound", err)
	}
	optionalChild := child
	optionalChild.Inputs[0].Required = false
	if bindings, err := resolveStageInputs(ctx, dataStore, services.core.objects, run, revision, optionalChild); err != nil || len(bindings) != 0 {
		t.Fatalf("missing optional object input = %+v, %v; want unavailable binding omitted", bindings, err)
	}
}

// TestLaterArtifactCandidateOrdersAcrossStageKeysByCompletion pins the rule that
// selects a producer when two different stages write the same artifact name.
//
// Ordinal counts attempts of one stage key, so it is only an ordering across
// retries of that same stage. Comparing it across stage keys ranked stages by
// how often they had run: host_candidate_verify runs once per repair wave and
// reached ordinal 18 while authoring_repair reached 9, so the verifier's older
// validation_receipt permanently outranked the repair's fresh one. Package
// admission then received a receipt describing the pre-repair candidate
// alongside the post-repair candidate_snapshot and failed closed on the digest
// mismatch every time repair actually changed the candidate.
func TestLaterArtifactCandidateOrdersAcrossStageKeysByCompletion(t *testing.T) {
	at := func(minute int) *time.Time {
		moment := time.Date(2026, 8, 8, 0, minute, 0, 0, time.UTC)
		return &moment
	}
	verifier := stageArtifactCandidate{
		attempt: store.StageAttempt{ID: "hcv-18", StageKey: "host_candidate_verify", Ordinal: 18, FinishedAt: at(26)},
		ref:     store.ArtifactRef{ID: "ref-hcv", ArtifactKey: "validation_receipt"},
	}
	repair := stageArtifactCandidate{
		attempt: store.StageAttempt{ID: "repair-9", StageKey: "authoring_repair", Ordinal: 9, FinishedAt: at(32)},
		ref:     store.ArtifactRef{ID: "ref-repair", ArtifactKey: "validation_receipt"},
	}

	if !laterArtifactCandidate(repair.attempt, repair.ref, verifier) {
		t.Fatal("authoring_repair finished after host_candidate_verify but did not supersede it; a lower ordinal on a different stage key must not lose")
	}
	if laterArtifactCandidate(verifier.attempt, verifier.ref, repair) {
		t.Fatal("host_candidate_verify finished earlier and must not supersede the repair output on ordinal alone")
	}

	// Retries of one stage key must still order by ordinal, which is the
	// meaning ordinal actually has.
	first := stageArtifactCandidate{
		attempt: store.StageAttempt{ID: "repair-9", StageKey: "authoring_repair", Ordinal: 9, FinishedAt: at(32)},
		ref:     store.ArtifactRef{ID: "ref-a"},
	}
	retry := store.StageAttempt{ID: "repair-10", StageKey: "authoring_repair", Ordinal: 10, FinishedAt: at(30)}
	if !laterArtifactCandidate(retry, store.ArtifactRef{ID: "ref-b"}, first) {
		t.Fatal("a higher ordinal of the same stage key must win regardless of recorded completion time")
	}
}

// TestStageArtifactDurableAtFallsBackWhenUnfinished keeps an attempt without a
// completion timestamp comparable instead of collapsing to the zero time, which
// would make every unfinished attempt tie and fall through to an ID comparison.
func TestStageArtifactDurableAtFallsBackWhenUnfinished(t *testing.T) {
	created := time.Date(2026, 8, 8, 0, 10, 0, 0, time.UTC)
	finished := time.Date(2026, 8, 8, 0, 20, 0, 0, time.UTC)

	if got := stageArtifactDurableAt(store.StageAttempt{CreatedAt: created}); !got.Equal(created) {
		t.Fatalf("unfinished attempt durable-at = %s, want creation time %s", got, created)
	}
	if got := stageArtifactDurableAt(store.StageAttempt{CreatedAt: created, FinishedAt: &finished}); !got.Equal(finished) {
		t.Fatalf("finished attempt durable-at = %s, want completion time %s", got, finished)
	}
}
