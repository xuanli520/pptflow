package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const authoringFixtureRepository = "https://github.com/tower-rs/tower-http.git"
const authoringFixtureCommit = "f066e10ebc07ea9050a2ce4576315abfa568edf4"

func TestAuthoringSourceSessionAndRunFreezePreRevisionSubject(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	source := createAuthoringSourceFixture(t, ctx, s, "source-a")
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{
		Slug: "tower-http-authoring", Title: "Tower HTTP authoring draft",
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA,
		Actor: "author", Reason: "reserve TUI task ownership",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateAuthoringSession(ctx, CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID,
		WorkflowTemplateID: "harbor.task-lifecycle", WorkflowTemplateVersion: "2.2.0",
		SessionManifestJSON: `{"mode":"standard","source_snapshot":"readonly"}`,
		IdempotencyKey:      "session-a", Actor: "author", Reason: "freeze Standard authoring intent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isUUIDv7(source.ID) || !isUUIDv7(session.ID) || session.TargetTaskID != task.ID {
		t.Fatalf("authoring identities or draft task binding are invalid: source=%+v session=%+v", source, session)
	}
	run, err := s.CreateAuthoringWorkflowRun(ctx, CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID,
		WorkflowTemplateID: "harbor.task-lifecycle", WorkflowTemplateVersion: "2.2.0",
		ResolvedProfileHash: "sha256:profile", DefinitionHash: "sha256:definition",
		RunManifestJSON: `{"template":"harbor.task-lifecycle","mode":"standard"}`,
		Trigger:         "task.generate", Actor: "author", Reason: "start source-bound authoring",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isUUIDv7(run.ID) || run.SubjectKind != WorkflowRunSubjectAuthoringSession || run.AuthoringSessionID != session.ID ||
		run.SubjectID != source.ID || run.SubjectRevisionID != session.ID || run.SubjectDigest != source.SnapshotContentDigest ||
		run.TaskID != "" || run.RevisionID != "" {
		t.Fatalf("authoring run has an invalid subject binding: %+v", run)
	}
	loadedRun, err := s.GetWorkflowRun(ctx, run.ID)
	if err != nil || loadedRun == nil || loadedRun.SubjectKind != WorkflowRunSubjectAuthoringSession || loadedRun.AuthoringSessionID != session.ID ||
		loadedRun.SubjectID != source.ID || loadedRun.SubjectRevisionID != session.ID || loadedRun.SubjectDigest != source.SnapshotContentDigest ||
		loadedRun.TaskID != "" || loadedRun.RevisionID != "" {
		t.Fatalf("loaded authoring run did not preserve nullable task/revision subject: %+v err=%v", loadedRun, err)
	}
	loadedSession, err := s.GetAuthoringSessionForRun(ctx, run.ID)
	if err != nil || loadedSession == nil || loadedSession.ID != session.ID || loadedSession.SourceID != source.ID {
		t.Fatalf("authoring run session lookup failed: session=%+v err=%v", loadedSession, err)
	}
	if materialization, err := s.GetAuthoringTaskMaterializationForSession(ctx, session.ID); err != nil || materialization != nil {
		t.Fatalf("pre-materialization session unexpectedly has a task revision receipt: %+v err=%v", materialization, err)
	}

	run, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunRunning, Actor: "author", Reason: "worker began authoring",
	})
	if err != nil || run.StartedAt == nil {
		t.Fatalf("authoring run cannot enter normal lifecycle: %+v err=%v", run, err)
	}
	stage, err := s.CreateStageAttempt(ctx, CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "repo_prepare", StageGroup: "authoring", Ordinal: 1,
		InputFingerprint: source.SourceFingerprint, BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "author", Reason: "prepare readonly source",
	})
	if err != nil || stage.RunID != run.ID {
		t.Fatalf("authoring stage did not attach to the source/session run: %+v err=%v", stage, err)
	}

	if _, err := s.db.Exec(`UPDATE authoring_sources_v2 SET commit_sha = ? WHERE id = ?`, strings.Repeat("a", 40), source.ID); err == nil {
		t.Fatal("direct authoring source mutation was accepted")
	}
	if _, err := s.db.Exec(`UPDATE authoring_sessions_v2 SET target_task_id = NULL WHERE id = ?`, session.ID); err == nil {
		t.Fatal("direct authoring session mutation was accepted")
	}
	if _, err := s.db.Exec(`UPDATE workflow_runs SET subject_kind = 'task_revision' WHERE id = ?`, run.ID); err == nil {
		t.Fatal("authoring workflow run was rebound to a task revision")
	}
}

func TestListAuthoringWorkflowRunsForTargetTaskUsesSessionOwnership(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	source := createAuthoringSourceFixture(t, ctx, s, "list-authoring-runs-source")
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{
		Slug: "list-authoring-runs", Title: "List authoring runs", SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA,
		Actor: "author", Reason: "reserve draft task ownership",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := s.CreateTaskV2(ctx, CreateTaskV2Request{
		Slug: "list-authoring-runs-other", Title: "Other draft", SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA,
		Actor: "author", Reason: "reserve other draft task",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateAuthoringSession(ctx, CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: "harbor.standard-authoring", WorkflowTemplateVersion: "1.0.0",
		SessionManifestJSON: `{"mode":"standard"}`, IdempotencyKey: "list-authoring-runs-session", Actor: "author", Reason: "freeze authoring session",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateAuthoringWorkflowRun(ctx, CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:list-authoring-profile", DefinitionHash: "sha256:list-authoring-definition",
		RunManifestJSON: `{}`, Trigger: "task.generate", Actor: "author", Reason: "create authoring Run",
	})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := s.ListAuthoringWorkflowRunsForTargetTask(ctx, task.ID)
	if err != nil || len(owned) != 1 || owned[0].ID != run.ID || owned[0].TaskID != "" || owned[0].RevisionID != "" {
		t.Fatalf("authoring runs by session-owned task=%+v err=%v", owned, err)
	}
	if generic, err := s.ListWorkflowRunsForTask(ctx, task.ID); err != nil || len(generic) != 0 {
		t.Fatalf("pre-materialization Run leaked into task-revision run listing: %+v err=%v", generic, err)
	}
	if other, err := s.ListAuthoringWorkflowRunsForTargetTask(ctx, otherTask.ID); err != nil || len(other) != 0 {
		t.Fatalf("authoring Run leaked to unrelated task ownership: %+v err=%v", other, err)
	}
}

func TestAuthoringSourceAndSessionReplayAndValidation(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	source := createAuthoringSourceFixture(t, ctx, s, "source-replay")
	replayed, err := s.CreateAuthoringSource(ctx, CreateAuthoringSourceRequest{
		RepositoryURL: source.RepositoryURL, CommitSHA: source.CommitSHA,
		SnapshotArtifactRef: source.SnapshotArtifactRef, SnapshotContentDigest: source.SnapshotContentDigest,
		SnapshotSchemaVersion: source.SnapshotSchemaVersion, IdempotencyKey: source.IdempotencyKey,
		Actor: "different-actor", Reason: "retry after lost response",
	})
	if err != nil || replayed.ID != source.ID || replayed.SourceFingerprint != source.SourceFingerprint {
		t.Fatalf("source replay did not recover the immutable record: %+v err=%v", replayed, err)
	}
	if _, err := s.CreateAuthoringSource(ctx, CreateAuthoringSourceRequest{
		RepositoryURL: source.RepositoryURL, CommitSHA: strings.Repeat("b", 40),
		SnapshotArtifactRef: source.SnapshotArtifactRef, SnapshotContentDigest: source.SnapshotContentDigest,
		SnapshotSchemaVersion: source.SnapshotSchemaVersion, IdempotencyKey: source.IdempotencyKey, Actor: "author",
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed source retry error = %v, want idempotency conflict", err)
	}
	for _, request := range []CreateAuthoringSourceRequest{
		{RepositoryURL: "https://github.com/tower-rs/tower-http.git", CommitSHA: "main", SnapshotArtifactRef: authoringDigest("a"), SnapshotContentDigest: authoringDigest("a"), SnapshotSchemaVersion: "harbor.source-snapshot.v1", IdempotencyKey: "invalid-commit"},
		{RepositoryURL: "file:///tmp/tower-http", CommitSHA: authoringFixtureCommit, SnapshotArtifactRef: authoringDigest("a"), SnapshotContentDigest: authoringDigest("a"), SnapshotSchemaVersion: "harbor.source-snapshot.v1", IdempotencyKey: "invalid-url"},
		{RepositoryURL: authoringFixtureRepository, CommitSHA: authoringFixtureCommit, SnapshotArtifactRef: authoringDigest("a"), SnapshotContentDigest: authoringDigest("b"), SnapshotSchemaVersion: "harbor.source-snapshot.v1", IdempotencyKey: "mutable-reference"},
	} {
		if _, err := s.CreateAuthoringSource(ctx, request); err == nil {
			t.Fatalf("invalid authoring source was accepted: %+v", request)
		}
	}

	task, revision := createValidatedTaskAndRevision(t, s)
	if _, err := s.CreateAuthoringSession(ctx, CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID,
		WorkflowTemplateID: "harbor.task-lifecycle", WorkflowTemplateVersion: "2.2.0",
		SessionManifestJSON: `{}`, IdempotencyKey: "revised-target", Actor: "author",
	}); err == nil {
		t.Fatalf("session accepted task %s with existing revision %s", task.ID, revision.ID)
	}

	draft, err := s.CreateTaskV2(ctx, CreateTaskV2Request{
		Slug: "source-bound-draft", SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateAuthoringSession(ctx, CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: draft.ID,
		WorkflowTemplateID: "harbor.task-lifecycle", WorkflowTemplateVersion: "2.2.0",
		SessionManifestJSON: `{"proposal":"v1"}`, IdempotencyKey: "session-replay", Actor: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	replayedSession, err := s.CreateAuthoringSession(ctx, CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: draft.ID,
		WorkflowTemplateID: "harbor.task-lifecycle", WorkflowTemplateVersion: "2.2.0",
		SessionManifestJSON: `{"proposal":"v1"}`, IdempotencyKey: "session-replay", Actor: "author",
	})
	if err != nil || replayedSession.ID != session.ID {
		t.Fatalf("session replay did not recover immutable record: %+v err=%v", replayedSession, err)
	}
	if _, err := s.CreateAuthoringWorkflowRun(ctx, CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID,
		WorkflowTemplateID: "harbor.other", WorkflowTemplateVersion: "1",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`, Trigger: "task.generate", Actor: "author",
	}); !errors.Is(err, ErrImmutable) {
		t.Fatalf("mismatched authoring run template error = %v, want immutable binding error", err)
	}
}

func TestAuthoringSourceGlobalIdentityAndRunSubjectExclusivity(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	draft, err := s.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "identity-draft", SourceRepo: authoringFixtureRepository, SourceCommit: authoringFixtureCommit, Actor: "author"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAuthoringSource(ctx, CreateAuthoringSourceRequest{
		ID: draft.ID, RepositoryURL: authoringFixtureRepository, CommitSHA: authoringFixtureCommit,
		SnapshotArtifactRef: authoringDigest("c"), SnapshotContentDigest: authoringDigest("c"),
		SnapshotSchemaVersion: "harbor.source-snapshot.v1", IdempotencyKey: "global-collision", Actor: "author",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("cross-entity UUID collision error = %v, want identity collision", err)
	}
	source := createAuthoringSourceFixture(t, ctx, s, "source-exclusive")
	session, err := s.CreateAuthoringSession(ctx, CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: draft.ID,
		WorkflowTemplateID: "harbor.task-lifecycle", WorkflowTemplateVersion: "2.2.0",
		SessionManifestJSON: `{}`, IdempotencyKey: "session-exclusive", Actor: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateAuthoringWorkflowRun(ctx, CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID,
		WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`, Trigger: "task.generate", Actor: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAuthoringWorkflowRun(ctx, CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID,
		WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "profile-2", DefinitionHash: "definition-2", RunManifestJSON: `{}`, Trigger: "task.generate", Actor: "author",
	}); err == nil {
		t.Fatal("a second run was allowed to re-execute the same immutable authoring session")
	}
	if _, err := s.db.Exec(`
		INSERT INTO workflow_runs (
			id, subject_kind, subject_id, subject_revision_id, subject_digest,
			task_id, revision_id, authoring_session_id,
			workflow_template_id, workflow_template_version, resolved_profile_hash,
			definition_hash, run_manifest_json, trigger, execution_epoch, status,
			created_by, created_at, version
		) VALUES (?, 'authoring_session', ?, ?, ?, NULL, NULL, ?, 'harbor.task-lifecycle', '2.2.0', 'profile', 'definition', '{}', 'forged', 0, 'queued', 'author', CURRENT_TIMESTAMP, 1)
	`, mustUUIDv7(t), session.ID, source.ID, source.SourceFingerprint, session.ID); err == nil || !strings.Contains(err.Error(), "workflow run subject binding") {
		t.Fatalf("swapped authoring generic subject error=%v, want workflow run subject binding rejection", err)
	}
	if loaded, err := s.GetAuthoringSessionForRun(ctx, run.ID); err != nil || loaded == nil || loaded.ID != session.ID {
		t.Fatalf("authoring session lookup lost exclusive run binding: %+v err=%v", loaded, err)
	}
}

func TestMaterializeAuthoringTaskAtomicallyBridgesTheGenericSourceSessionSubject(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	source := createAuthoringSourceFixture(t, ctx, s, "source-materialize")
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{
		Slug: "tower-http-materialization", Title: "Tower HTTP materialization draft",
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA,
		Actor: "author", Reason: "reserve revision-free draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateAuthoringSession(ctx, CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID,
		WorkflowTemplateID: "harbor.task-lifecycle", WorkflowTemplateVersion: "2.2.0",
		SessionManifestJSON: `{"mode":"standard"}`,
		IdempotencyKey:      "session-materialize", Actor: "author", Reason: "freeze materialization contract",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateAuthoringWorkflowRun(ctx, CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID,
		WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:profile", DefinitionHash: "sha256:definition",
		RunManifestJSON: `{}`, Trigger: "task.generate", Actor: "author", Reason: "start authoring",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.SubjectID != source.ID || run.SubjectRevisionID != session.ID || run.SubjectDigest != source.SnapshotContentDigest || run.SubjectDigest == source.SourceFingerprint {
		t.Fatalf("authoring run did not retain the generic source/session snapshot subject: %+v source=%+v", run, source)
	}
	run, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunRunning,
		Actor: "author", Reason: "materialize generated task",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := MaterializeAuthoringTaskRequest{
		IdempotencyKey: "materialize-authoring-task", AuthoringSessionID: session.ID, AuthoringRunID: run.ID,
		ExpectedTaskVersion: task.Version, ExpectedRunVersion: run.Version,
		TaskDigest: validTaskDigest("d"), ProposalDigest: "proposal-sha256", ManifestID: "authoring-manifest",
		ChangeSummary: "initial generated task", MetadataJSON: `{"source":"tower-http"}`,
		Actor: "author", Reason: "sealed authoring handoff",
	}
	result, err := s.MaterializeAuthoringTask(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.ID != task.ID || result.Task.Version != task.Version+1 || result.Revision.TaskID != task.ID ||
		result.Revision.VersionNumber != 1 || result.Revision.Origin != RevisionOriginGenerated || result.Revision.State != RevisionStateSealed ||
		result.Materialization.SessionID != session.ID || result.Materialization.SourceID != source.ID || result.Materialization.AuthoringRunID != run.ID ||
		result.Materialization.SourceFingerprint != source.SourceFingerprint {
		t.Fatalf("materialization did not preserve its immutable source/session/revision lineage: %+v", result)
	}
	input, err := s.GetAuthoringRunInputArtifactForPort(ctx, run.ID, authoringSourceSnapshotInputPort)
	if err != nil || input == nil || input.SourceID != source.ID || input.SessionID != session.ID || input.ContentDigest != source.SnapshotContentDigest {
		t.Fatalf("authoring source input was not retained as immutable evidence: input=%+v err=%v", input, err)
	}
	replayed, err := s.MaterializeAuthoringTask(ctx, request)
	if err != nil || replayed.Revision.ID != result.Revision.ID || replayed.Materialization.ID != result.Materialization.ID {
		t.Fatalf("materialization replay did not recover the committed receipt: %+v err=%v", replayed, err)
	}
	request.TaskDigest = validTaskDigest("e")
	if _, err := s.MaterializeAuthoringTask(ctx, request); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed materialization replay err=%v, want idempotency conflict", err)
	}
}

func createAuthoringSourceFixture(t *testing.T, ctx context.Context, s *Store, key string) AuthoringSource {
	t.Helper()
	source, err := s.CreateAuthoringSource(ctx, CreateAuthoringSourceRequest{
		RepositoryURL: authoringFixtureRepository, CommitSHA: authoringFixtureCommit,
		SnapshotArtifactRef: authoringDigest("a"), SnapshotContentDigest: authoringDigest("a"),
		SnapshotSchemaVersion: "harbor.source-snapshot.v1", IdempotencyKey: key, Actor: "author", Reason: "freeze source fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func authoringDigest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}
