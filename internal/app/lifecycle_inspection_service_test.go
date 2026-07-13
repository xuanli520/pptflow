package app

import (
	"context"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestLifecycleInspectionJoinsDurableJobsAndLeases(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := NewLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "inspection-jobs", Actor: "tester", Reason: "fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "inspection\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), Trigger: "verify", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := dataStore.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("initial durable jobs = %+v, want one workflow dispatch", jobs)
	}
	job := jobs[0]
	lease, err := dataStore.AcquireLease(ctx, store.AcquireLeaseRequest{
		ResourceType: "job_dispatch", ResourceID: job.ID, Owner: "worker", JobID: job.ID, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := services.Inspection.ReadTaskDetail(ctx, TaskInspectionQuery{TaskID: task.ID, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Runs) != 1 || snapshot.Runs[0].Run.ID != run.ID || len(snapshot.Runs[0].Jobs) != 1 {
		t.Fatalf("inspection runs = %+v", snapshot.Runs)
	}
	inspected := snapshot.Runs[0].Jobs[0]
	if inspected.Job.ID != job.ID || len(inspected.Leases) != 1 || inspected.Leases[0].ID != lease.ID {
		t.Fatalf("inspection job/leases = %+v", inspected)
	}
}
