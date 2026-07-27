package workflowkit

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"
)

func TestCompileDependencyExecutionPlanFirstFitsResourceAndWorkspaceConflicts(t *testing.T) {
	resourceWriter := testStage("resource_writer", nil, EffectEvidenceOnly, nil, []ResourceKey{"candidate/shared"})
	resourceReader := testStage("resource_reader", nil, EffectEvidenceOnly, []ResourceKey{"candidate/shared"}, []ResourceKey{"evidence/resource-reader"})
	independent := testStage("independent", nil, EffectEvidenceOnly, nil, []ResourceKey{"evidence/independent"})
	workspaceWriter := snapshotWorkspaceStage("workspace_writer", WorkspaceExclusiveWriter, []ResourceKey{"candidate/workspace-writer"})
	workspaceReader := snapshotWorkspaceStage("workspace_reader", WorkspaceReadOnlySnapshot, []ResourceKey{"evidence/workspace-reader"})
	workflow := WorkflowDescriptor{
		ID:      "first-fit-conflicts",
		Version: "1",
		Stages: []StageDescriptor{
			resourceWriter,
			resourceReader,
			independent,
			workspaceWriter,
			workspaceReader,
		},
	}

	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile first-fit plan: %v", err)
	}
	if err := plan.Validate(workflow); err != nil {
		t.Fatalf("validate first-fit plan: %v", err)
	}
	if len(plan.Batches) != 2 {
		t.Fatalf("first-fit batches = %#v, want two batches", plan.Batches)
	}
	if got, want := plan.Batches[0].NodeIDs, []NodeID{"resource_writer", "independent", "workspace_writer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first batch = %#v, want %#v", got, want)
	}
	if got, want := plan.Batches[1].NodeIDs, []NodeID{"resource_reader", "workspace_reader"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second batch = %#v, want %#v", got, want)
	}
}

func TestEveryScheduleAdmissionBoundaryRejectsConcurrentConflicts(t *testing.T) {
	writer := testStage("writer", nil, EffectEvidenceOnly, nil, []ResourceKey{"candidate/shared"})
	reader := testStage("reader", nil, EffectEvidenceOnly, []ResourceKey{"candidate/shared"}, []ResourceKey{"evidence/reader"})
	workflow := WorkflowDescriptor{ID: "forged-concurrent-batch", Version: "1", Stages: []StageDescriptor{writer, reader}}
	batch := ScheduleBatch{ID: "forged", NodeIDs: []NodeID{"writer", "reader"}}

	fingerprint, err := fingerprintExecutionPlan([]ScheduleBatch{batch})
	if err != nil {
		t.Fatalf("fingerprint forged plan: %v", err)
	}
	forgedPlan := ExecutionPlan{Batches: []ScheduleBatch{batch}, Fingerprint: fingerprint}
	if err := forgedPlan.Validate(workflow); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("forged execution plan error = %v, want ErrInvalidExecution", err)
	}

	transitions := map[NodeID]NodeTransition{
		"writer": {NodeID: "writer", Disposition: DispositionSchedule},
		"reader": {NodeID: "reader", Disposition: DispositionSchedule},
	}
	if err := validateSchedule([]ScheduleBatch{batch}, workflow, transitions, nil); !errors.Is(err, ErrInvalidContinuationPlan) {
		t.Fatalf("forged continuation schedule error = %v, want ErrInvalidContinuationPlan", err)
	}

	directives := map[NodeID]coordinatorDirective{
		"writer": {disposition: DispositionSchedule},
		"reader": {disposition: DispositionSchedule},
	}
	if err := validateCoordinatorBatchDependencies(workflow, directives, []ScheduleBatch{batch}, false); !errors.Is(err, ErrInvalidCoordinatorInput) {
		t.Fatalf("forged coordinator schedule error = %v, want ErrInvalidCoordinatorInput", err)
	}
}

func TestWorkspaceExclusiveWriterPreventsConcurrentReadOnlyProjection(t *testing.T) {
	writer := snapshotWorkspaceStage("writer", WorkspaceExclusiveWriter, []ResourceKey{"evidence/writer"})
	reader := snapshotWorkspaceStage("reader", WorkspaceReadOnlySnapshot, []ResourceKey{"evidence/reader"})
	if err := ValidateConcurrentStages([]StageDescriptor{writer, reader}); err == nil {
		t.Fatal("shared workspace writer and reader were accepted in one batch")
	}

	reader.Concurrency.Workspace.Key = "candidate/other"
	if err := ValidateConcurrentStages([]StageDescriptor{writer, reader}); err != nil {
		t.Fatalf("separate workspace keys conflict: %v", err)
	}
}

func TestCompileDependencyExecutionPlanPropertyNoDependencyOrConcurrencyConflicts(t *testing.T) {
	random := rand.New(rand.NewSource(20260726))
	for trial := 0; trial < 200; trial++ {
		workflow := randomSchedulingWorkflow(random, trial)
		first, err := CompileDependencyExecutionPlan(workflow)
		if err != nil {
			t.Fatalf("compile random workflow %d: %v", trial, err)
		}
		second, err := CompileDependencyExecutionPlan(workflow)
		if err != nil {
			t.Fatalf("recompile random workflow %d: %v", trial, err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("non-deterministic first-fit plan for random workflow %d: %#v != %#v", trial, first, second)
		}
		if err := first.Validate(workflow); err != nil {
			t.Fatalf("validate random workflow %d plan: %v", trial, err)
		}
		batchByNode := make(map[NodeID]int)
		for batchIndex, batch := range first.Batches {
			stages := make([]StageDescriptor, 0, len(batch.NodeIDs))
			for _, nodeID := range batch.NodeIDs {
				stage, found := workflow.Stage(StageKey(nodeID))
				if !found {
					t.Fatalf("random plan %d refers to missing stage %q", trial, nodeID)
				}
				stages = append(stages, stage)
				batchByNode[nodeID] = batchIndex
			}
			if err := ValidateConcurrentStages(stages); err != nil {
				t.Fatalf("random plan %d has concurrent conflict: %v", trial, err)
			}
		}
		for _, stage := range workflow.Stages {
			for _, dependency := range stage.Dependencies {
				if batchByNode[dependency] >= batchByNode[stage.Key] {
					t.Fatalf("random plan %d scheduled dependency %q after %q", trial, dependency, stage.Key)
				}
			}
		}
	}
}

func snapshotWorkspaceStage(key string, mode WorkspaceMode, writes []ResourceKey) StageDescriptor {
	stage := testStage(StageKey(key), nil, EffectEvidenceOnly, nil, writes)
	stage.Inputs = []ArtifactSpec{{Name: "candidate_snapshot", SchemaVersion: "candidate/v1", Required: true}}
	stage.Concurrency = &ConcurrencyPolicy{Workspace: WorkspaceBinding{
		Mode: mode, Key: "candidate/main", SnapshotArtifact: "candidate_snapshot",
	}}
	return stage
}

func randomSchedulingWorkflow(random *rand.Rand, trial int) WorkflowDescriptor {
	count := 1 + random.Intn(12)
	workflow := WorkflowDescriptor{ID: "random-scheduling", Version: "1", Stages: make([]StageDescriptor, 0, count)}
	for index := 0; index < count; index++ {
		key := StageKey("stage_" + string(rune('a'+index)))
		dependencies := make([]StageKey, 0, index)
		for predecessor := 0; predecessor < index; predecessor++ {
			if random.Intn(4) == 0 {
				dependencies = append(dependencies, StageKey("stage_"+string(rune('a'+predecessor))))
			}
		}
		reads := randomResourceSet(random)
		writes := randomResourceSet(random)
		stage := testStage(key, dependencies, EffectEvidenceOnly, reads, writes)
		if random.Intn(3) != 0 {
			stage.Inputs = []ArtifactSpec{{Name: "snapshot", SchemaVersion: "snapshot/v1", Required: true}}
			mode := WorkspaceReadOnlySnapshot
			if random.Intn(2) == 0 {
				mode = WorkspaceExclusiveWriter
			}
			stage.Concurrency = &ConcurrencyPolicy{Workspace: WorkspaceBinding{
				Mode: mode, Key: WorkspaceKey("workspace/" + string(rune('a'+random.Intn(3)))), SnapshotArtifact: "snapshot",
			}}
		}
		workflow.Stages = append(workflow.Stages, stage)
	}
	workflow.ID += string(rune('a' + trial%26))
	return workflow
}

func randomResourceSet(random *rand.Rand) []ResourceKey {
	resources := make(map[ResourceKey]struct{})
	for index := random.Intn(3); index > 0; index-- {
		resources[ResourceKey("resource/"+string(rune('a'+random.Intn(4))))] = struct{}{}
	}
	set := make([]ResourceKey, 0, len(resources))
	for resource := range resources {
		set = append(set, resource)
	}
	return set
}
