package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestTransitionDurableJobPersistsFailureAndAuditAtomically(t *testing.T) {
	ctx := context.Background()
	s, running := createRunningFailureFixture(t)

	failed, err := s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
		JobID: running.ID, ExpectedVersion: running.Version, State: JobFailed, Actor: "worker", Reason: "lineage validation failed",
		Failure: &DurableJobFailure{
			Code:    "handoff.artifact_lineage_invalid",
			Message: "The handoff artifact lineage could not be verified.",
			DetailsJSON: `{
				"stage":"handoff",
				"check":"artifact_lineage",
				"expected_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"actual_digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != JobFailed || failed.Failure == nil || failed.Failure.Code != "handoff.artifact_lineage_invalid" || failed.FinishedAt == nil {
		t.Fatalf("failed durable job = %+v", failed)
	}
	if failed.Failure.DetailsJSON != `{"actual_digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","check":"artifact_lineage","expected_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","stage":"handoff"}` {
		t.Fatalf("canonical failure details = %s", failed.Failure.DetailsJSON)
	}

	persisted, err := s.GetDurableJob(ctx, running.ID)
	if err != nil || persisted == nil || persisted.Failure == nil {
		t.Fatalf("persisted failure job = %+v, %v", persisted, err)
	}
	if *persisted.Failure != *failed.Failure {
		t.Fatalf("persisted failure = %+v, want %+v", persisted.Failure, failed.Failure)
	}

	events, err := s.ListAuditEvents(ctx, ListAuditEventsRequest{EntityType: "job", EntityID: running.ID})
	if err != nil {
		t.Fatal(err)
	}
	var transitionPayload map[string]any
	for _, event := range events {
		if event.Action == "job.transitioned" {
			var payload map[string]any
			if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["state"] == string(JobFailed) {
				transitionPayload = payload
			}
		}
	}
	if transitionPayload["failure_code"] != "handoff.artifact_lineage_invalid" {
		t.Fatalf("failure transition audit = %+v", transitionPayload)
	}
	if _, found := transitionPayload["failure_message"]; found {
		t.Fatalf("failure transition audit leaked failure message: %+v", transitionPayload)
	}
}

func TestTransitionDurableJobRejectsInvalidFailureRecords(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		state   JobState
		failure *DurableJobFailure
	}{
		{name: "failed requires failure", state: JobFailed},
		{name: "successful transition cannot carry failure", state: JobSucceeded, failure: validDurableJobFailure()},
		{name: "unsafe path detail", state: JobFailed, failure: &DurableJobFailure{Code: "handoff.artifact_invalid", Message: "The artifact check failed.", DetailsJSON: `{"stage":"handoff","path":"/tmp/private"}`}},
		{name: "model output detail", state: JobFailed, failure: &DurableJobFailure{Code: "handoff.artifact_invalid", Message: "The artifact check failed.", DetailsJSON: `{"model_output":"untrusted response"}`}},
		{name: "unscoped actual detail", state: JobFailed, failure: &DurableJobFailure{Code: "handoff.artifact_invalid", Message: "The artifact check failed.", DetailsJSON: `{"actual":"untrusted response"}`}},
		{name: "nested detail object", state: JobFailed, failure: &DurableJobFailure{Code: "handoff.artifact_invalid", Message: "The artifact check failed.", DetailsJSON: `{"expected":{"digest":"sha256:abc"}}`}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s, running := createRunningFailureFixture(t)
			_, err := s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
				JobID: running.ID, ExpectedVersion: running.Version, State: testCase.state, Failure: testCase.failure, Actor: "worker",
			})
			if !errors.Is(err, ErrInvalidJobFailure) {
				t.Fatalf("transition error = %v, want ErrInvalidJobFailure", err)
			}
			persisted, err := s.GetDurableJob(ctx, running.ID)
			if err != nil || persisted == nil {
				t.Fatalf("read job after rejected transition = %+v, %v", persisted, err)
			}
			if persisted.State != JobRunning || persisted.Failure != nil || persisted.Version != running.Version {
				t.Fatalf("rejected failure changed durable job: %+v", persisted)
			}
		})
	}
}

func TestTransitionDurableJobAcceptsStructuredFailureDetails(t *testing.T) {
	ctx := context.Background()
	s, running := createRunningFailureFixture(t)
	details := map[string]string{
		"job_id":           running.ID,
		"artifact_id":      mustUUIDv7(t),
		"stage_attempt_id": mustUUIDv7(t),
		"stage":            "handoff",
		"check":            "artifact_lineage",
		"expected_digest":  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"actual_digest":    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}

	failed, err := s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
		JobID: running.ID, ExpectedVersion: running.Version, State: JobFailed, Actor: "worker",
		Failure: &DurableJobFailure{
			Code:        "handoff.artifact_lineage_invalid",
			Message:     "The handoff artifact lineage could not be verified.",
			DetailsJSON: string(encoded),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Failure == nil {
		t.Fatalf("failed job omitted structured failure details: %+v", failed)
	}
	var persisted map[string]string
	if err := json.Unmarshal([]byte(failed.Failure.DetailsJSON), &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != len(details) {
		t.Fatalf("persisted failure detail count = %d, want %d: %+v", len(persisted), len(details), persisted)
	}
	for key, want := range details {
		if got := persisted[key]; got != want {
			t.Fatalf("persisted failure detail %q = %q, want %q", key, got, want)
		}
	}
}

func TestTransitionDurableJobRejectsFreeTextInAllowedFailureDetailFields(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		details map[string]string
	}{
		{
			name:    "free text artifact ID",
			details: map[string]string{"artifact_id": "assistant says the handoff artifact is ready"},
		},
		{
			name:    "free text expected digest",
			details: map[string]string{"expected_digest": "assistant estimates that the checksum is valid"},
		},
		{
			name:    "free text stage",
			details: map[string]string{"stage": "assistant says this is the final handoff stage"},
		},
		{
			name:    "free text check",
			details: map[string]string{"check": "assistant says the lineage validation passed"},
		},
		{
			name:    "token shaped model output in artifact ID",
			details: map[string]string{"artifact_id": "ghp_000000000000000000000000000000000000"},
		},
		{
			name:    "token shaped model output in check",
			details: map[string]string{"check": "ghp_000000000000000000000000000000000000"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s, running := createRunningFailureFixture(t)
			encoded, err := json.Marshal(testCase.details)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
				JobID: running.ID, ExpectedVersion: running.Version, State: JobFailed, Actor: "worker",
				Failure: &DurableJobFailure{
					Code:        "handoff.artifact_lineage_invalid",
					Message:     "The handoff artifact lineage could not be verified.",
					DetailsJSON: string(encoded),
				},
			})
			if !errors.Is(err, ErrInvalidJobFailure) {
				t.Fatalf("transition error = %v, want ErrInvalidJobFailure", err)
			}

			persisted, err := s.GetDurableJob(ctx, running.ID)
			if err != nil || persisted == nil {
				t.Fatalf("read job after rejected details = %+v, %v", persisted, err)
			}
			if persisted.State != JobRunning || persisted.Failure != nil || persisted.Version != running.Version {
				t.Fatalf("rejected failure details changed durable job: %+v", persisted)
			}
		})
	}
}

func TestDurableJobFailureCannotBeOverwritten(t *testing.T) {
	ctx := context.Background()
	s, running := createRunningFailureFixture(t)
	if _, err := s.db.Exec(`UPDATE jobs SET state = 'failed' WHERE id = ?`, running.ID); err == nil {
		t.Fatal("schema accepted a failed job without a failure record")
	}
	originalFailure := &DurableJobFailure{
		Code: "handoff.definition_unavailable", Message: "The deployment definition is temporarily unavailable.", DetailsJSON: "",
	}
	inDoubt, err := s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
		JobID: running.ID, ExpectedVersion: running.Version, State: JobInDoubt, Failure: originalFailure, Actor: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if inDoubt.Failure == nil || inDoubt.Failure.DetailsJSON != "{}" {
		t.Fatalf("empty failure details were not canonicalized: %+v", inDoubt.Failure)
	}

	_, err = s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
		JobID: running.ID, ExpectedVersion: running.Version, State: JobInDoubt,
		Failure: &DurableJobFailure{Code: "handoff.definition_invalid", Message: "A different failure must not replace the original."}, Actor: "retrying-worker",
	})
	if !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale failure overwrite error = %v, want ErrOptimisticLock", err)
	}
	_, err = s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
		JobID: inDoubt.ID, ExpectedVersion: inDoubt.Version, State: JobInDoubt,
		Failure: &DurableJobFailure{Code: "handoff.definition_invalid", Message: "A different failure must not replace the original."}, Actor: "retrying-worker",
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal failure overwrite error = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET failure_message = ? WHERE id = ?`, "replacement", inDoubt.ID); err == nil {
		t.Fatal("direct durable failure overwrite was accepted")
	}

	persisted, err := s.GetDurableJob(ctx, inDoubt.ID)
	if err != nil || persisted == nil || persisted.Failure == nil {
		t.Fatalf("persisted in-doubt job = %+v, %v", persisted, err)
	}
	if *persisted.Failure != *inDoubt.Failure {
		t.Fatalf("failure record changed: got %+v want %+v", persisted.Failure, inDoubt.Failure)
	}
}

func TestTransitionDurableJobRunProjectionFollowsDeliveryOutcome(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		state       JobState
		status      WorkflowRunStatus
		withFailure bool
	}{
		{name: "success resumes parent run", state: JobSucceeded, status: WorkflowRunRunning},
		{name: "unknown delivery holds parent run", state: JobInDoubt, status: WorkflowRunInDoubt, withFailure: true},
		{name: "deterministic failure terminates parent run", state: JobFailed, status: WorkflowRunFailedTerminal, withFailure: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s, run, job := createRunningRunProjectionFixture(t)
			request := TransitionDurableJobRequest{
				JobID: job.ID, ExpectedVersion: job.Version, State: testCase.state,
				RunProjection: &DurableJobRunProjection{Status: testCase.status}, Actor: "worker", Reason: "project durable delivery outcome",
			}
			if testCase.withFailure {
				request.Failure = validDurableJobFailure()
			}
			transitioned, err := s.TransitionDurableJob(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if transitioned.State != testCase.state {
				t.Fatalf("durable job state = %s, want %s", transitioned.State, testCase.state)
			}
			projected, err := s.GetWorkflowRun(ctx, run.ID)
			if err != nil || projected == nil || projected.Status != testCase.status {
				t.Fatalf("projected workflow run = %+v, %v; want %s", projected, err, testCase.status)
			}
		})
	}
}

func TestTransitionDurableJobRejectsConflictingRunProjection(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		state       JobState
		status      WorkflowRunStatus
		withFailure bool
	}{
		{name: "success cannot fail parent", state: JobSucceeded, status: WorkflowRunFailedTerminal},
		{name: "unknown delivery cannot resume parent", state: JobInDoubt, status: WorkflowRunRunning, withFailure: true},
		{name: "failure cannot leave parent in doubt", state: JobFailed, status: WorkflowRunInDoubt, withFailure: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s, run, job := createRunningRunProjectionFixture(t)
			request := TransitionDurableJobRequest{
				JobID: job.ID, ExpectedVersion: job.Version, State: testCase.state,
				RunProjection: &DurableJobRunProjection{Status: testCase.status}, Actor: "worker", Reason: "reject conflicting Run projection",
			}
			if testCase.withFailure {
				request.Failure = validDurableJobFailure()
			}
			if _, err := s.TransitionDurableJob(ctx, request); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("transition error = %v, want ErrInvalidTransition", err)
			}
			persistedJob, err := s.GetDurableJob(ctx, job.ID)
			if err != nil || persistedJob == nil || persistedJob.State != JobRunning || persistedJob.Version != job.Version || persistedJob.Failure != nil {
				t.Fatalf("rejected projection changed durable job = %+v, %v", persistedJob, err)
			}
			persistedRun, err := s.GetWorkflowRun(ctx, run.ID)
			if err != nil || persistedRun == nil || persistedRun.Status != WorkflowRunRunning || persistedRun.Version != run.Version {
				t.Fatalf("rejected projection changed workflow run = %+v, %v", persistedRun, err)
			}
		})
	}
}

func TestTransitionDurableJobRunProjectionDoesNotRegressTerminalRun(t *testing.T) {
	ctx := context.Background()
	s, run, job := createRunningRunProjectionFixture(t)
	run, err := s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunSucceeded, Actor: "tester", Reason: "complete parent before late recovery",
	})
	if err != nil {
		t.Fatal(err)
	}
	transitioned, err := s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
		JobID: job.ID, ExpectedVersion: job.Version, State: JobInDoubt, Failure: validDurableJobFailure(),
		RunProjection: &DurableJobRunProjection{Status: WorkflowRunInDoubt}, Actor: "worker", Reason: "record late unknown delivery",
	})
	if err != nil {
		t.Fatal(err)
	}
	if transitioned.State != JobInDoubt || transitioned.Failure == nil {
		t.Fatalf("late durable job = %+v", transitioned)
	}
	persistedRun, err := s.GetWorkflowRun(ctx, run.ID)
	if err != nil || persistedRun == nil || persistedRun.Status != WorkflowRunSucceeded || persistedRun.Version != run.Version {
		t.Fatalf("late projection regressed terminal Run = %+v, %v", persistedRun, err)
	}
}

func createRunningFailureFixture(t *testing.T) (*Store, DurableJob) {
	t.Helper()
	ctx := context.Background()
	s := tempDB(t)
	job, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "failure.fixture", EntityType: "fixture", EntityID: t.Name(), PayloadJSON: `{}`,
		IdempotencyKey: "failure-fixture-" + t.Name(), Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err = s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
		JobID: job.ID, ExpectedVersion: job.Version, State: JobRunning, Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, job
}

func createRunningRunProjectionFixture(t *testing.T) (*Store, WorkflowRun, DurableJob) {
	t.Helper()
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "harbor.standard", WorkflowTemplateVersion: "v2",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`, Trigger: "run-projection", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunRunning, Actor: "tester", Reason: "start projection fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "handoff.fixture", EntityType: "workflow_run", EntityID: run.ID, RunID: run.ID,
		PayloadJSON: `{}`, IdempotencyKey: "run-projection-fixture-" + t.Name(), Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err = s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
		JobID: job.ID, ExpectedVersion: job.Version, State: JobRunning, Actor: "tester", Reason: "claim projection fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, run, job
}

func validDurableJobFailure() *DurableJobFailure {
	return &DurableJobFailure{Code: "handoff.fixture_failed", Message: "The fixture check failed.", DetailsJSON: `{"check":"fixture"}`}
}
