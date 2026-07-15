package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const authoringSourceSelect = `
	SELECT id, repository_url, commit_sha, snapshot_artifact_ref,
	       snapshot_content_digest, snapshot_schema_version, source_fingerprint,
	       idempotency_key, created_by, created_at
	FROM authoring_sources_v2`

const authoringSessionSelect = `
	SELECT id, source_id, target_task_id, workflow_template_id,
	       workflow_template_version, session_manifest_json, session_fingerprint,
	       idempotency_key, created_by, created_at
	FROM authoring_sessions_v2`

const authoringTaskMaterializationSelect = `
	SELECT id, session_id, source_id, authoring_run_id, task_id, revision_id,
	       source_fingerprint, task_digest, request_fingerprint, idempotency_key,
	       created_by, created_at
	FROM authoring_task_materializations_v2`

const authoringRunInputArtifactSelect = `
	SELECT id, run_id, session_id, source_id, source_fingerprint, port,
	       snapshot_artifact_ref, content_digest, schema_version,
	       idempotency_key, created_by, created_at
	FROM authoring_run_input_artifacts_v2`

const authoringSourceSnapshotInputPort = "source_snapshot"

type preparedAuthoringSource struct {
	repositoryURL         string
	commitSHA             string
	snapshotArtifactRef   string
	snapshotContentDigest string
	snapshotSchemaVersion string
	sourceFingerprint     string
	idempotencyKey        string
	requestedID           string
}

// CreateAuthoringSource persists a content-addressed repository snapshot.
// Replays by idempotency key return the exact same immutable source; they can
// omit ID after a lost response, but an explicit different ID is rejected.
func (s *Store) CreateAuthoringSource(ctx context.Context, request CreateAuthoringSourceRequest) (AuthoringSource, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return AuthoringSource{}, err
	}
	prepared, err := prepareAuthoringSource(request)
	if err != nil {
		return AuthoringSource{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthoringSource{}, err
	}
	defer tx.Rollback()
	if existing, err := getAuthoringSourceByKeyTx(ctx, tx, prepared.idempotencyKey); err == nil {
		if !sameAuthoringSourceRequest(existing, prepared) || (prepared.requestedID != "" && prepared.requestedID != existing.ID) {
			return AuthoringSource{}, fmt.Errorf("%w: authoring source key %s", ErrIdempotencyConflict, prepared.idempotencyKey)
		}
		if err := tx.Commit(); err != nil {
			return AuthoringSource{}, err
		}
		return existing, nil
	} else if !isNotFound(err) {
		return AuthoringSource{}, err
	}
	if existing, err := getAuthoringSourceByFingerprintTx(ctx, tx, prepared.sourceFingerprint); err == nil {
		if prepared.requestedID != "" && prepared.requestedID != existing.ID {
			return AuthoringSource{}, fmt.Errorf("%w: authoring source %s", ErrIdentityCollision, prepared.requestedID)
		}
		if err := tx.Commit(); err != nil {
			return AuthoringSource{}, err
		}
		return existing, nil
	} else if !isNotFound(err) {
		return AuthoringSource{}, err
	}
	id, err := s.newV2ID(prepared.requestedID)
	if err != nil {
		return AuthoringSource{}, err
	}
	now := s.now().UTC()
	source := AuthoringSource{
		ID:                    id,
		RepositoryURL:         prepared.repositoryURL,
		CommitSHA:             prepared.commitSHA,
		SnapshotArtifactRef:   prepared.snapshotArtifactRef,
		SnapshotContentDigest: prepared.snapshotContentDigest,
		SnapshotSchemaVersion: prepared.snapshotSchemaVersion,
		SourceFingerprint:     prepared.sourceFingerprint,
		IdempotencyKey:        prepared.idempotencyKey,
		CreatedBy:             resolveActor(request.Actor),
		CreatedAt:             now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO authoring_sources_v2 (
			id, repository_url, commit_sha, snapshot_artifact_ref,
			snapshot_content_digest, snapshot_schema_version, source_fingerprint,
			idempotency_key, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, source.ID, source.RepositoryURL, source.CommitSHA, source.SnapshotArtifactRef,
		source.SnapshotContentDigest, source.SnapshotSchemaVersion, source.SourceFingerprint,
		source.IdempotencyKey, source.CreatedBy, source.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return AuthoringSource{}, fmt.Errorf("%w: authoring source %s", ErrIdentityCollision, source.ID)
		}
		return AuthoringSource{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "authoring_source",
		EntityID:    source.ID,
		Action:      "authoring_source.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"repository_url": source.RepositoryURL, "commit_sha": source.CommitSHA, "source_fingerprint": source.SourceFingerprint}),
		CreatedAt:   now,
	}); err != nil {
		return AuthoringSource{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthoringSource{}, err
	}
	return source, nil
}

func (s *Store) GetAuthoringSource(ctx context.Context, sourceID string) (*AuthoringSource, error) {
	if _, err := requireV4ID(sourceID, "authoring source ID"); err != nil {
		return nil, err
	}
	source, err := scanAuthoringSource(s.db.QueryRowContext(ctx, authoringSourceSelect+" WHERE id = ?", sourceID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &source, nil
}

func (s *Store) GetAuthoringSourceByFingerprint(ctx context.Context, sourceFingerprint string) (*AuthoringSource, error) {
	fingerprint, err := normalizeAuthoringSHA256(sourceFingerprint, "authoring source fingerprint")
	if err != nil {
		return nil, err
	}
	source, err := scanAuthoringSource(s.db.QueryRowContext(ctx, authoringSourceSelect+" WHERE source_fingerprint = ?", fingerprint))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &source, nil
}

// GetAuthoringSourceByCoordinateAndSnapshot finds the immutable source whose
// canonical remote coordinate and content-addressed snapshot are exact. The
// Store owns normalization and fingerprint construction so callers cannot
// duplicate the source identity format when safely reusing a snapshot.
func (s *Store) GetAuthoringSourceByCoordinateAndSnapshot(ctx context.Context, repositoryURL, commitSHA, contentDigest, schemaVersion string) (*AuthoringSource, error) {
	repositoryURL, err := NormalizeAuthoringRepositoryURL(repositoryURL)
	if err != nil {
		return nil, err
	}
	commitSHA, err = NormalizeAuthoringCommitSHA(commitSHA)
	if err != nil {
		return nil, err
	}
	contentDigest, err = normalizeAuthoringSHA256(contentDigest, "authoring source snapshot content digest")
	if err != nil {
		return nil, err
	}
	schemaVersion, err = normalizeRequired(schemaVersion, "authoring source snapshot schema version")
	if err != nil {
		return nil, err
	}
	return s.GetAuthoringSourceByFingerprint(ctx, authoringSourceFingerprint(repositoryURL, commitSHA, contentDigest, contentDigest, schemaVersion))
}

type preparedAuthoringSession struct {
	sourceID                string
	targetTaskID            string
	workflowTemplateID      string
	workflowTemplateVersion string
	sessionManifestJSON     string
	sessionFingerprint      string
	idempotencyKey          string
	requestedID             string
}

// CreateAuthoringSession freezes a Standard authoring contract. If a draft
// task is attached, the method validates it is still revision-free and has
// the exact source provenance. Its Run projects the source/session snapshot
// through WorkflowRun's generic subject coordinate.
func (s *Store) CreateAuthoringSession(ctx context.Context, request CreateAuthoringSessionRequest) (AuthoringSession, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return AuthoringSession{}, err
	}
	prepared, err := prepareAuthoringSession(request)
	if err != nil {
		return AuthoringSession{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthoringSession{}, err
	}
	defer tx.Rollback()
	source, err := getAuthoringSourceTx(ctx, tx, prepared.sourceID)
	if err != nil {
		return AuthoringSession{}, err
	}
	if existing, err := getAuthoringSessionByKeyTx(ctx, tx, prepared.idempotencyKey); err == nil {
		if !sameAuthoringSessionRequest(existing, prepared) || (prepared.requestedID != "" && prepared.requestedID != existing.ID) {
			return AuthoringSession{}, fmt.Errorf("%w: authoring session key %s", ErrIdempotencyConflict, prepared.idempotencyKey)
		}
		if err := tx.Commit(); err != nil {
			return AuthoringSession{}, err
		}
		return existing, nil
	} else if !isNotFound(err) {
		return AuthoringSession{}, err
	}
	if existing, err := getAuthoringSessionByFingerprintTx(ctx, tx, prepared.sessionFingerprint); err == nil {
		if prepared.requestedID != "" && prepared.requestedID != existing.ID {
			return AuthoringSession{}, fmt.Errorf("%w: authoring session %s", ErrIdentityCollision, prepared.requestedID)
		}
		if err := tx.Commit(); err != nil {
			return AuthoringSession{}, err
		}
		return existing, nil
	} else if !isNotFound(err) {
		return AuthoringSession{}, err
	}
	if err := validateAuthoringSessionTargetTaskTx(ctx, tx, source, prepared.targetTaskID); err != nil {
		return AuthoringSession{}, err
	}
	id, err := s.newV2ID(prepared.requestedID)
	if err != nil {
		return AuthoringSession{}, err
	}
	now := s.now().UTC()
	session := AuthoringSession{
		ID:                      id,
		SourceID:                source.ID,
		TargetTaskID:            prepared.targetTaskID,
		WorkflowTemplateID:      prepared.workflowTemplateID,
		WorkflowTemplateVersion: prepared.workflowTemplateVersion,
		SessionManifestJSON:     prepared.sessionManifestJSON,
		SessionFingerprint:      prepared.sessionFingerprint,
		IdempotencyKey:          prepared.idempotencyKey,
		CreatedBy:               resolveActor(request.Actor),
		CreatedAt:               now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO authoring_sessions_v2 (
			id, source_id, target_task_id, workflow_template_id,
			workflow_template_version, session_manifest_json, session_fingerprint,
			idempotency_key, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, session.ID, session.SourceID, session.TargetTaskID, session.WorkflowTemplateID,
		session.WorkflowTemplateVersion, session.SessionManifestJSON, session.SessionFingerprint,
		session.IdempotencyKey, session.CreatedBy, session.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return AuthoringSession{}, fmt.Errorf("%w: authoring session %s", ErrIdentityCollision, session.ID)
		}
		return AuthoringSession{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "authoring_session",
		EntityID:    session.ID,
		Action:      "authoring_session.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"source_id": session.SourceID, "target_task_id": session.TargetTaskID, "session_fingerprint": session.SessionFingerprint}),
		CreatedAt:   now,
	}); err != nil {
		return AuthoringSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthoringSession{}, err
	}
	return session, nil
}

func (s *Store) GetAuthoringSession(ctx context.Context, sessionID string) (*AuthoringSession, error) {
	if _, err := requireV4ID(sessionID, "authoring session ID"); err != nil {
		return nil, err
	}
	session, err := scanAuthoringSession(s.db.QueryRowContext(ctx, authoringSessionSelect+" WHERE id = ?", sessionID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *Store) GetAuthoringSessionForRun(ctx context.Context, runID string) (*AuthoringSession, error) {
	if _, err := requireV4ID(runID, "authoring workflow run ID"); err != nil {
		return nil, err
	}
	run, err := s.GetWorkflowRun(ctx, runID)
	if err != nil || run == nil {
		return nil, err
	}
	if run.SubjectKind != WorkflowRunSubjectAuthoringSession || run.AuthoringSessionID == "" {
		return nil, fmt.Errorf("%w: workflow run %s is not an authoring-session run", ErrNotFound, run.ID)
	}
	return s.GetAuthoringSession(ctx, run.AuthoringSessionID)
}

// CreateAuthoringWorkflowRun creates the pre-materialization Standard Run.
// Its task and revision columns remain NULL by construction, preventing the
// old empty-revision workaround at the durable boundary.
func (s *Store) CreateAuthoringWorkflowRun(ctx context.Context, request CreateAuthoringWorkflowRunRequest) (WorkflowRun, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return WorkflowRun{}, err
	}
	sessionID, err := requireV4ID(request.AuthoringSessionID, "authoring session ID")
	if err != nil {
		return WorkflowRun{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return WorkflowRun{}, err
	}
	templateID, err := normalizeRequired(request.WorkflowTemplateID, "workflow template ID")
	if err != nil {
		return WorkflowRun{}, err
	}
	templateVersion, err := normalizeRequired(request.WorkflowTemplateVersion, "workflow template version")
	if err != nil {
		return WorkflowRun{}, err
	}
	profileHash, err := normalizeRequired(request.ResolvedProfileHash, "resolved profile hash")
	if err != nil {
		return WorkflowRun{}, err
	}
	definitionHash, err := normalizeRequired(request.DefinitionHash, "workflow definition hash")
	if err != nil {
		return WorkflowRun{}, err
	}
	manifest, err := normalizeJSON(request.RunManifestJSON, "run manifest")
	if err != nil {
		return WorkflowRun{}, err
	}
	trigger, err := normalizeRequired(request.Trigger, "run trigger")
	if err != nil {
		return WorkflowRun{}, err
	}
	if request.ExecutionEpoch < 0 {
		return WorkflowRun{}, fmt.Errorf("execution epoch cannot be negative")
	}
	dispatch, err := prepareWorkflowRunDispatch(s, request.Dispatch, id, request.Actor)
	if err != nil {
		return WorkflowRun{}, err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowRun{}, err
	}
	defer tx.Rollback()
	session, err := getAuthoringSessionTx(ctx, tx, sessionID)
	if err != nil {
		return WorkflowRun{}, err
	}
	source, err := getAuthoringSourceTx(ctx, tx, session.SourceID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if session.WorkflowTemplateID != templateID || session.WorkflowTemplateVersion != templateVersion {
		return WorkflowRun{}, fmt.Errorf("%w: authoring session %s template differs from workflow run", ErrImmutable, session.ID)
	}
	run := WorkflowRun{
		ID:                      id,
		SubjectKind:             WorkflowRunSubjectAuthoringSession,
		SubjectID:               source.ID,
		SubjectRevisionID:       session.ID,
		SubjectDigest:           source.SnapshotContentDigest,
		AuthoringSessionID:      session.ID,
		WorkflowTemplateID:      templateID,
		WorkflowTemplateVersion: templateVersion,
		ResolvedProfileHash:     profileHash,
		DefinitionHash:          definitionHash,
		RunManifestJSON:         manifest,
		Trigger:                 trigger,
		ExecutionEpoch:          request.ExecutionEpoch,
		Status:                  WorkflowRunQueued,
		CreatedBy:               resolveActor(request.Actor),
		CreatedAt:               now,
		Version:                 1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_runs (
			id, subject_kind, subject_id, subject_revision_id, subject_digest,
			task_id, revision_id, authoring_session_id,
			workflow_template_id, workflow_template_version, resolved_profile_hash,
			definition_hash, run_manifest_json, parent_run_id, trigger, execution_epoch,
			status, created_by, created_at, started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, run.ID, run.SubjectKind, run.SubjectID, run.SubjectRevisionID, run.SubjectDigest,
		run.AuthoringSessionID, run.WorkflowTemplateID,
		run.WorkflowTemplateVersion, run.ResolvedProfileHash, run.DefinitionHash,
		run.RunManifestJSON, run.Trigger, run.ExecutionEpoch, run.Status, run.CreatedBy,
		run.CreatedAt, run.Version); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return WorkflowRun{}, fmt.Errorf("%w: authoring workflow run %s", ErrIdentityCollision, run.ID)
		}
		return WorkflowRun{}, err
	}
	if _, _, err := s.insertAuthoringRunSourceInputTx(ctx, tx, run, session, source, request.Actor, request.Reason, now); err != nil {
		return WorkflowRun{}, err
	}
	if dispatch != nil {
		dispatch.CreatedAt = now
		dispatch.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (
				id, command_type, entity_type, entity_id, run_id, stage_attempt_id, state,
				priority, payload_json, idempotency_key, created_by, created_at, updated_at,
				started_at, finished_at, version
			) VALUES (?, ?, 'workflow_run', ?, ?, NULL, 'queued', ?, ?, ?, ?, ?, ?, NULL, NULL, 1)
		`, dispatch.ID, dispatch.CommandType, run.ID, run.ID, dispatch.Priority, dispatch.PayloadJSON,
			dispatch.IdempotencyKey, dispatch.CreatedBy, dispatch.CreatedAt, dispatch.UpdatedAt); err != nil {
			if isGlobalIdentityCollision(err) {
				return WorkflowRun{}, fmt.Errorf("%w: initial authoring workflow job %s", ErrIdentityCollision, dispatch.ID)
			}
			if isUniqueConstraint(err) {
				return WorkflowRun{}, fmt.Errorf("%w: initial authoring workflow job key %s", ErrIdempotencyConflict, dispatch.IdempotencyKey)
			}
			return WorkflowRun{}, err
		}
		if err := s.appendV5OutboxTx(ctx, tx, "workflow_run.queued", "workflow_run", run.ID,
			dispatch.IdempotencyKey+":queued", dispatch.PayloadJSON, now); err != nil {
			return WorkflowRun{}, err
		}
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
			Actor:       request.Actor,
			EntityType:  "job",
			EntityID:    dispatch.ID,
			Action:      "job.created_for_workflow_run",
			Reason:      request.Reason,
			PayloadJSON: auditPayload(map[string]any{"run_id": run.ID, "command_type": dispatch.CommandType, "idempotency_key": dispatch.IdempotencyKey}),
			CreatedAt:   now,
		}); err != nil {
			return WorkflowRun{}, err
		}
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "workflow_run",
		EntityID:    run.ID,
		Action:      "workflow_run.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"subject_kind": run.SubjectKind, "authoring_session_id": run.AuthoringSessionID, "definition_hash": run.DefinitionHash}),
		CreatedAt:   now,
	}); err != nil {
		return WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return run, nil
}

// GetAuthoringRunInputArtifactForPort returns a frozen source/session input
// for an authoring Run. Unlike GetRunInputArtifactForPort it never exposes a
// TaskRevision because one does not exist on this side of materialization.
func (s *Store) GetAuthoringRunInputArtifactForPort(ctx context.Context, runID, port string) (*AuthoringRunInputArtifact, error) {
	if _, err := requireV4ID(runID, "authoring workflow run ID"); err != nil {
		return nil, err
	}
	port, err := normalizeRequired(port, "authoring run input port")
	if err != nil {
		return nil, err
	}
	input, err := scanAuthoringRunInputArtifact(s.db.QueryRowContext(ctx, authoringRunInputArtifactSelect+" WHERE run_id = ? AND port = ?", runID, port))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &input, nil
}

func (s *Store) ListAuthoringRunInputArtifacts(ctx context.Context, runID string) ([]AuthoringRunInputArtifact, error) {
	if _, err := requireV4ID(runID, "authoring workflow run ID"); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, authoringRunInputArtifactSelect+" WHERE run_id = ? ORDER BY port ASC", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	inputs := make([]AuthoringRunInputArtifact, 0)
	for rows.Next() {
		input, err := scanAuthoringRunInputArtifact(rows)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
	}
	return inputs, rows.Err()
}

// insertAuthoringRunSourceInputTx derives the only pre-materialization input
// from the persisted AuthoringSource. It is deliberately not public: callers
// cannot substitute a path, branch, altered digest, or unrelated artifact for
// the run's source_snapshot port.
func (s *Store) insertAuthoringRunSourceInputTx(ctx context.Context, tx *sql.Tx, run WorkflowRun, session AuthoringSession, source AuthoringSource, actor, reason string, now time.Time) (AuthoringRunInputArtifact, bool, error) {
	if run.SubjectKind != WorkflowRunSubjectAuthoringSession || run.AuthoringSessionID != session.ID ||
		run.SubjectID != source.ID || run.SubjectRevisionID != session.ID || run.SubjectDigest != source.SnapshotContentDigest ||
		session.SourceID != source.ID {
		return AuthoringRunInputArtifact{}, false, fmt.Errorf("authoring source input has inconsistent run/session/source binding")
	}
	key := "authoring-run-source-input:" + run.ID + ":" + authoringSourceSnapshotInputPort
	if existing, err := getAuthoringRunInputArtifactByKeyTx(ctx, tx, key); err == nil {
		if sameAuthoringRunInputArtifact(existing, AuthoringRunInputArtifact{
			RunID: run.ID, SessionID: session.ID, SourceID: source.ID, SourceFingerprint: source.SourceFingerprint,
			Port: authoringSourceSnapshotInputPort, SnapshotArtifactRef: source.SnapshotArtifactRef,
			ContentDigest: source.SnapshotContentDigest, SchemaVersion: source.SnapshotSchemaVersion,
			IdempotencyKey: key,
		}) {
			return existing, false, nil
		}
		return AuthoringRunInputArtifact{}, false, fmt.Errorf("%w: authoring run input key %s", ErrIdempotencyConflict, key)
	} else if !isNotFound(err) {
		return AuthoringRunInputArtifact{}, false, err
	}
	id, err := s.newV2ID("")
	if err != nil {
		return AuthoringRunInputArtifact{}, false, err
	}
	input := AuthoringRunInputArtifact{
		ID: id, RunID: run.ID, SessionID: session.ID, SourceID: source.ID,
		SourceFingerprint: source.SourceFingerprint, Port: authoringSourceSnapshotInputPort,
		SnapshotArtifactRef: source.SnapshotArtifactRef, ContentDigest: source.SnapshotContentDigest,
		SchemaVersion: source.SnapshotSchemaVersion, IdempotencyKey: key,
		CreatedBy: resolveActor(actor), CreatedAt: now.UTC(),
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO authoring_run_input_artifacts_v2 (
			id, run_id, session_id, source_id, source_fingerprint, port,
			snapshot_artifact_ref, content_digest, schema_version, idempotency_key,
			created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, input.ID, input.RunID, input.SessionID, input.SourceID, input.SourceFingerprint,
		input.Port, input.SnapshotArtifactRef, input.ContentDigest, input.SchemaVersion,
		input.IdempotencyKey, input.CreatedBy, input.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) {
			return AuthoringRunInputArtifact{}, false, fmt.Errorf("%w: authoring run input %s", ErrIdentityCollision, input.ID)
		}
		if isUniqueConstraint(err) {
			return AuthoringRunInputArtifact{}, false, fmt.Errorf("%w: authoring run input %s", ErrIdentityCollision, input.ID)
		}
		return AuthoringRunInputArtifact{}, false, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       actor,
		EntityType:  "authoring_run_input_artifact",
		EntityID:    input.ID,
		Action:      "authoring_run_input_artifact.created",
		Reason:      reason,
		PayloadJSON: auditPayload(map[string]any{"run_id": input.RunID, "session_id": input.SessionID, "source_id": input.SourceID, "port": input.Port, "content_digest": input.ContentDigest}),
		CreatedAt:   now,
	}); err != nil {
		return AuthoringRunInputArtifact{}, false, err
	}
	return input, true, nil
}

func (s *Store) GetAuthoringTaskMaterializationForSession(ctx context.Context, sessionID string) (*AuthoringTaskMaterialization, error) {
	if _, err := requireV4ID(sessionID, "authoring session ID"); err != nil {
		return nil, err
	}
	return getAuthoringTaskMaterialization(ctx, s.db, "session_id", sessionID)
}

func (s *Store) GetAuthoringTaskMaterializationForRun(ctx context.Context, runID string) (*AuthoringTaskMaterialization, error) {
	if _, err := requireV4ID(runID, "authoring workflow run ID"); err != nil {
		return nil, err
	}
	return getAuthoringTaskMaterialization(ctx, s.db, "authoring_run_id", runID)
}

func (s *Store) GetAuthoringTaskMaterializationForRevision(ctx context.Context, revisionID string) (*AuthoringTaskMaterialization, error) {
	if _, err := requireV4ID(revisionID, "authoring task revision ID"); err != nil {
		return nil, err
	}
	return getAuthoringTaskMaterialization(ctx, s.db, "revision_id", revisionID)
}

type preparedAuthoringTaskMaterialization struct {
	requestedID         string
	idempotencyKey      string
	sessionID           string
	runID               string
	expectedTaskVersion int64
	expectedRunVersion  int64
	requestedRevisionID string
	taskDigest          string
	proposalDigest      string
	manifestID          string
	changeSummary       string
	metadataJSON        string
	requestFingerprint  string
	actor               string
	reason              string
}

// MaterializeAuthoringTask is the sole durable boundary that turns a
// source/session authoring result into a first real TaskRevision. It does not
// accept a task ID because the session's mandatory draft Task is the only
// eligible target, and it forces generated/sealed/version-1 provenance.
func (s *Store) MaterializeAuthoringTask(ctx context.Context, request MaterializeAuthoringTaskRequest) (MaterializeAuthoringTaskResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	prepared, err := prepareAuthoringTaskMaterialization(request)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	defer tx.Rollback()
	if existing, err := getAuthoringTaskMaterializationByKeyTx(ctx, tx, prepared.idempotencyKey); err == nil {
		result, err := replayAuthoringTaskMaterializationTx(ctx, tx, existing, prepared)
		if err != nil {
			return MaterializeAuthoringTaskResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return MaterializeAuthoringTaskResult{}, err
		}
		return result, nil
	} else if !isNotFound(err) {
		return MaterializeAuthoringTaskResult{}, err
	}
	if existing, err := getAuthoringTaskMaterializationBySessionTx(ctx, tx, prepared.sessionID); err == nil {
		result, replayErr := replayAuthoringTaskMaterializationTx(ctx, tx, existing, prepared)
		if replayErr != nil {
			return MaterializeAuthoringTaskResult{}, replayErr
		}
		if err := tx.Commit(); err != nil {
			return MaterializeAuthoringTaskResult{}, err
		}
		return result, nil
	} else if !isNotFound(err) {
		return MaterializeAuthoringTaskResult{}, err
	}
	if existing, err := getAuthoringTaskMaterializationByRunTx(ctx, tx, prepared.runID); err == nil {
		result, replayErr := replayAuthoringTaskMaterializationTx(ctx, tx, existing, prepared)
		if replayErr != nil {
			return MaterializeAuthoringTaskResult{}, replayErr
		}
		if err := tx.Commit(); err != nil {
			return MaterializeAuthoringTaskResult{}, err
		}
		return result, nil
	} else if !isNotFound(err) {
		return MaterializeAuthoringTaskResult{}, err
	}

	session, err := getAuthoringSessionTx(ctx, tx, prepared.sessionID)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	source, err := getAuthoringSourceTx(ctx, tx, session.SourceID)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	run, err := getWorkflowRunTx(ctx, tx, prepared.runID)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	if run.SubjectKind != WorkflowRunSubjectAuthoringSession || run.AuthoringSessionID != session.ID ||
		run.SubjectID != source.ID || run.SubjectRevisionID != session.ID || run.SubjectDigest != source.SnapshotContentDigest {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring run is not bound to the requested session/source", ErrImmutable)
	}
	if run.Version != prepared.expectedRunVersion {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring workflow run %s", ErrOptimisticLock, run.ID)
	}
	if run.Status != WorkflowRunRunning {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring workflow run %s is %s, not running", ErrInvalidTransition, run.ID, run.Status)
	}
	input, err := getAuthoringRunInputArtifactForPortTx(ctx, tx, run.ID, authoringSourceSnapshotInputPort)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	if input.SessionID != session.ID || input.SourceID != source.ID || input.SourceFingerprint != source.SourceFingerprint ||
		input.SnapshotArtifactRef != source.SnapshotArtifactRef || input.ContentDigest != source.SnapshotContentDigest || input.SchemaVersion != source.SnapshotSchemaVersion {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring run source_snapshot input does not match frozen source", ErrImmutable)
	}
	task, err := getTaskV2Tx(ctx, tx, session.TargetTaskID)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	now := s.now().UTC()
	if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, prepared.actor, now); err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	if task.Version != prepared.expectedTaskVersion {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring draft task %s", ErrOptimisticLock, task.ID)
	}
	if task.LifecycleState != TaskLifecycleDraft || task.CurrentRevisionID != "" || task.SourceRepo != source.RepositoryURL || task.SourceCommit != source.CommitSHA {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: session target task no longer matches its frozen authoring source", ErrImmutable)
	}
	var revisionCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_revisions WHERE task_id = ?`, task.ID).Scan(&revisionCount); err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	if revisionCount != 0 {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: session target task already has a task revision", ErrImmutable)
	}
	revisionID, err := s.newV2ID(prepared.requestedRevisionID)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	materializationID, err := s.newV2ID(prepared.requestedID)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	revision := TaskRevision{
		ID: revisionID, TaskID: task.ID, VersionNumber: 1, Origin: RevisionOriginGenerated,
		TaskDigest: prepared.taskDigest, ProposalDigest: prepared.proposalDigest, ManifestID: prepared.manifestID,
		State: RevisionStateSealed, StateVersion: 1, StateUpdatedBy: prepared.actor, StateUpdatedAt: now,
		ChangeSummary: prepared.changeSummary, MetadataJSON: prepared.metadataJSON, CreatedBy: prepared.actor, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_revisions (
			id, task_id, version_number, parent_revision_id, origin, task_digest,
			proposal_digest, manifest_id, state, validation_evidence_manifest, state_version, state_updated_by,
			state_updated_at, change_summary, metadata_json, created_by, created_at
		) VALUES (?, ?, 1, NULL, ?, ?, ?, ?, ?, '', 1, ?, ?, ?, ?, ?, ?)
	`, revision.ID, revision.TaskID, revision.Origin, revision.TaskDigest, revision.ProposalDigest,
		revision.ManifestID, revision.State, revision.StateUpdatedBy, revision.StateUpdatedAt,
		revision.ChangeSummary, revision.MetadataJSON, revision.CreatedBy, revision.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring task revision %s", ErrIdentityCollision, revision.ID)
		}
		return MaterializeAuthoringTaskResult{}, err
	}
	previousTaskVersion := task.Version
	// Materialization is the first and only atomic transition that gives the
	// reserved draft a real TaskRevision. Keep the Task's current-revision
	// projection in the same transaction; leaving it empty would make the
	// generated sealed revision unreachable to ordinary lifecycle/TUI callers
	// and would incorrectly keep a post-materialization authoring subject
	// looking like a revision-free draft.
	task.CurrentRevisionID = revision.ID
	task.Version++
	task.UpdatedAt = now
	updated, err := tx.ExecContext(ctx, `
		UPDATE tasks_v2 SET current_revision_id = ?, updated_at = ?, version = ? WHERE id = ? AND version = ?
	`, task.CurrentRevisionID, task.UpdatedAt, task.Version, task.ID, previousTaskVersion)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	if changed, err := updated.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return MaterializeAuthoringTaskResult{}, err
		}
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring draft task %s", ErrOptimisticLock, task.ID)
	}
	materialization := AuthoringTaskMaterialization{
		ID: materializationID, SessionID: session.ID, SourceID: source.ID, AuthoringRunID: run.ID,
		TaskID: task.ID, RevisionID: revision.ID, SourceFingerprint: source.SourceFingerprint,
		TaskDigest: revision.TaskDigest, RequestFingerprint: prepared.requestFingerprint,
		IdempotencyKey: prepared.idempotencyKey, CreatedBy: prepared.actor, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO authoring_task_materializations_v2 (
			id, session_id, source_id, authoring_run_id, task_id, revision_id,
			source_fingerprint, task_digest, request_fingerprint, idempotency_key,
			created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, materialization.ID, materialization.SessionID, materialization.SourceID, materialization.AuthoringRunID,
		materialization.TaskID, materialization.RevisionID, materialization.SourceFingerprint,
		materialization.TaskDigest, materialization.RequestFingerprint, materialization.IdempotencyKey,
		materialization.CreatedBy, materialization.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring task materialization %s", ErrIdentityCollision, materialization.ID)
		}
		return MaterializeAuthoringTaskResult{}, err
	}
	for _, event := range []AuditEvent{
		{Actor: prepared.actor, EntityType: "task_revision", EntityID: revision.ID, Action: "task_revision.materialized_from_authoring_session", Reason: prepared.reason, PayloadJSON: auditPayload(map[string]any{"task_id": task.ID, "session_id": session.ID, "authoring_run_id": run.ID, "source_fingerprint": source.SourceFingerprint, "task_digest": revision.TaskDigest}), OperationKey: prepared.idempotencyKey, CreatedAt: now},
		{Actor: prepared.actor, EntityType: "task", EntityID: task.ID, Action: "task.initial_revision_materialized", Reason: prepared.reason, PayloadJSON: auditPayload(map[string]any{"revision_id": revision.ID, "session_id": session.ID, "version": task.Version}), OperationKey: prepared.idempotencyKey, CreatedAt: now},
		{Actor: prepared.actor, EntityType: "authoring_task_materialization", EntityID: materialization.ID, Action: "authoring_task_materialization.created", Reason: prepared.reason, PayloadJSON: auditPayload(map[string]any{"session_id": session.ID, "authoring_run_id": run.ID, "task_id": task.ID, "revision_id": revision.ID}), OperationKey: prepared.idempotencyKey, CreatedAt: now},
	} {
		if _, err := s.appendAuditTx(ctx, tx, event); err != nil {
			return MaterializeAuthoringTaskResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	return MaterializeAuthoringTaskResult{Task: task, Revision: revision, Materialization: materialization}, nil
}

func prepareAuthoringTaskMaterialization(request MaterializeAuthoringTaskRequest) (preparedAuthoringTaskMaterialization, error) {
	sessionID, err := requireV4ID(request.AuthoringSessionID, "authoring materialization session ID")
	if err != nil {
		return preparedAuthoringTaskMaterialization{}, err
	}
	runID, err := requireV4ID(request.AuthoringRunID, "authoring materialization run ID")
	if err != nil {
		return preparedAuthoringTaskMaterialization{}, err
	}
	if request.ExpectedTaskVersion <= 0 || request.ExpectedRunVersion <= 0 {
		return preparedAuthoringTaskMaterialization{}, fmt.Errorf("authoring materialization expected task and run versions must be positive")
	}
	requestedID := strings.TrimSpace(request.ID)
	if requestedID != "" && !isUUIDv7(requestedID) {
		return preparedAuthoringTaskMaterialization{}, ErrInvalidUUIDv7Identity
	}
	requestedRevisionID := strings.TrimSpace(request.RevisionID)
	if requestedRevisionID != "" && !isUUIDv7(requestedRevisionID) {
		return preparedAuthoringTaskMaterialization{}, ErrInvalidUUIDv7Identity
	}
	key, err := normalizeRequired(request.IdempotencyKey, "authoring materialization idempotency key")
	if err != nil {
		return preparedAuthoringTaskMaterialization{}, err
	}
	taskDigest, err := normalizeRequired(request.TaskDigest, "authoring materialization task digest")
	if err != nil {
		return preparedAuthoringTaskMaterialization{}, err
	}
	if err := ValidateTaskDigestV2(taskDigest); err != nil {
		return preparedAuthoringTaskMaterialization{}, err
	}
	manifestID, err := normalizeRequired(request.ManifestID, "authoring materialization manifest ID")
	if err != nil {
		return preparedAuthoringTaskMaterialization{}, err
	}
	metadata, err := normalizeJSON(request.MetadataJSON, "authoring materialization revision metadata")
	if err != nil {
		return preparedAuthoringTaskMaterialization{}, err
	}
	prepared := preparedAuthoringTaskMaterialization{
		requestedID: requestedID, idempotencyKey: key, sessionID: sessionID, runID: runID,
		expectedTaskVersion: request.ExpectedTaskVersion, expectedRunVersion: request.ExpectedRunVersion,
		requestedRevisionID: requestedRevisionID, taskDigest: taskDigest,
		proposalDigest: strings.TrimSpace(request.ProposalDigest), manifestID: manifestID,
		changeSummary: strings.TrimSpace(request.ChangeSummary), metadataJSON: metadata,
		actor: resolveActor(request.Actor), reason: strings.TrimSpace(request.Reason),
	}
	prepared.requestFingerprint = authoringTaskMaterializationFingerprint(prepared)
	return prepared, nil
}

func replayAuthoringTaskMaterializationTx(ctx context.Context, tx *sql.Tx, materialization AuthoringTaskMaterialization, request preparedAuthoringTaskMaterialization) (MaterializeAuthoringTaskResult, error) {
	if materialization.SessionID != request.sessionID || materialization.AuthoringRunID != request.runID ||
		materialization.RequestFingerprint != request.requestFingerprint {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring task materialization request differs from committed receipt", ErrIdempotencyConflict)
	}
	if request.requestedID != "" && request.requestedID != materialization.ID {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring task materialization %s", ErrIdentityCollision, request.requestedID)
	}
	if request.requestedRevisionID != "" && request.requestedRevisionID != materialization.RevisionID {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: authoring task revision %s", ErrIdentityCollision, request.requestedRevisionID)
	}
	task, err := getTaskV2Tx(ctx, tx, materialization.TaskID)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	revision, err := getTaskRevisionTx(ctx, tx, materialization.RevisionID)
	if err != nil {
		return MaterializeAuthoringTaskResult{}, err
	}
	if revision.TaskID != task.ID || revision.TaskDigest != materialization.TaskDigest ||
		revision.Origin != RevisionOriginGenerated || revision.State != RevisionStateSealed || revision.VersionNumber != 1 ||
		revision.ParentRevisionID != "" {
		return MaterializeAuthoringTaskResult{}, fmt.Errorf("%w: committed authoring task materialization lineage is inconsistent", ErrImmutable)
	}
	return MaterializeAuthoringTaskResult{Task: task, Revision: revision, Materialization: materialization}, nil
}

func getAuthoringTaskMaterializationByKeyTx(ctx context.Context, tx *sql.Tx, key string) (AuthoringTaskMaterialization, error) {
	materialization, err := scanAuthoringTaskMaterialization(tx.QueryRowContext(ctx, authoringTaskMaterializationSelect+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return AuthoringTaskMaterialization{}, fmt.Errorf("%w: authoring task materialization idempotency key %s", ErrNotFound, key)
	}
	return materialization, err
}

func getAuthoringTaskMaterializationBySessionTx(ctx context.Context, tx *sql.Tx, sessionID string) (AuthoringTaskMaterialization, error) {
	materialization, err := scanAuthoringTaskMaterialization(tx.QueryRowContext(ctx, authoringTaskMaterializationSelect+" WHERE session_id = ?", sessionID))
	if err == sql.ErrNoRows {
		return AuthoringTaskMaterialization{}, fmt.Errorf("%w: authoring task materialization session %s", ErrNotFound, sessionID)
	}
	return materialization, err
}

func getAuthoringTaskMaterializationByRunTx(ctx context.Context, tx *sql.Tx, runID string) (AuthoringTaskMaterialization, error) {
	materialization, err := scanAuthoringTaskMaterialization(tx.QueryRowContext(ctx, authoringTaskMaterializationSelect+" WHERE authoring_run_id = ?", runID))
	if err == sql.ErrNoRows {
		return AuthoringTaskMaterialization{}, fmt.Errorf("%w: authoring task materialization run %s", ErrNotFound, runID)
	}
	return materialization, err
}

func getAuthoringRunInputArtifactForPortTx(ctx context.Context, tx *sql.Tx, runID, port string) (AuthoringRunInputArtifact, error) {
	input, err := scanAuthoringRunInputArtifact(tx.QueryRowContext(ctx, authoringRunInputArtifactSelect+" WHERE run_id = ? AND port = ?", runID, port))
	if err == sql.ErrNoRows {
		return AuthoringRunInputArtifact{}, fmt.Errorf("%w: authoring run input %s/%s", ErrNotFound, runID, port)
	}
	return input, err
}

func authoringTaskMaterializationFingerprint(request preparedAuthoringTaskMaterialization) string {
	payload, _ := json.Marshal(struct {
		Domain              string `json:"domain"`
		SessionID           string `json:"session_id"`
		RunID               string `json:"authoring_run_id"`
		ExpectedTaskVersion int64  `json:"expected_task_version"`
		ExpectedRunVersion  int64  `json:"expected_run_version"`
		TaskDigest          string `json:"task_digest"`
		ProposalDigest      string `json:"proposal_digest"`
		ManifestID          string `json:"manifest_id"`
		ChangeSummary       string `json:"change_summary"`
		MetadataJSON        string `json:"metadata_json"`
	}{
		"harbor.authoring-task-materialization.v2", request.sessionID, request.runID,
		request.expectedTaskVersion, request.expectedRunVersion, request.taskDigest,
		request.proposalDigest, request.manifestID, request.changeSummary, request.metadataJSON,
	})
	return sha256Fingerprint(payload)
}

func getAuthoringTaskMaterialization(ctx context.Context, querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, column, value string) (*AuthoringTaskMaterialization, error) {
	// column is selected only by this package's fixed accessor methods.
	materialization, err := scanAuthoringTaskMaterialization(querier.QueryRowContext(ctx, authoringTaskMaterializationSelect+" WHERE "+column+" = ?", value))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &materialization, nil
}

func prepareAuthoringSource(request CreateAuthoringSourceRequest) (preparedAuthoringSource, error) {
	repositoryURL, err := NormalizeAuthoringRepositoryURL(request.RepositoryURL)
	if err != nil {
		return preparedAuthoringSource{}, err
	}
	commitSHA, err := NormalizeAuthoringCommitSHA(request.CommitSHA)
	if err != nil {
		return preparedAuthoringSource{}, err
	}
	artifactRef, err := normalizeAuthoringSHA256(request.SnapshotArtifactRef, "authoring source snapshot artifact reference")
	if err != nil {
		return preparedAuthoringSource{}, err
	}
	contentDigest, err := normalizeAuthoringSHA256(request.SnapshotContentDigest, "authoring source snapshot content digest")
	if err != nil {
		return preparedAuthoringSource{}, err
	}
	if artifactRef != contentDigest {
		return preparedAuthoringSource{}, fmt.Errorf("authoring source snapshot artifact reference must equal its content digest")
	}
	schemaVersion, err := normalizeRequired(request.SnapshotSchemaVersion, "authoring source snapshot schema version")
	if err != nil {
		return preparedAuthoringSource{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "authoring source idempotency key")
	if err != nil {
		return preparedAuthoringSource{}, err
	}
	requestedID := strings.TrimSpace(request.ID)
	if requestedID != "" && !isUUIDv7(requestedID) {
		return preparedAuthoringSource{}, ErrInvalidUUIDv7Identity
	}
	return preparedAuthoringSource{
		repositoryURL: repositoryURL, commitSHA: commitSHA, snapshotArtifactRef: artifactRef,
		snapshotContentDigest: contentDigest, snapshotSchemaVersion: schemaVersion,
		sourceFingerprint: authoringSourceFingerprint(repositoryURL, commitSHA, artifactRef, contentDigest, schemaVersion),
		idempotencyKey:    key, requestedID: requestedID,
	}, nil
}

func prepareAuthoringSession(request CreateAuthoringSessionRequest) (preparedAuthoringSession, error) {
	sourceID, err := requireV4ID(request.SourceID, "authoring session source ID")
	if err != nil {
		return preparedAuthoringSession{}, err
	}
	targetTaskID, err := requireV4ID(request.TargetTaskID, "authoring session target task ID")
	if err != nil {
		return preparedAuthoringSession{}, err
	}
	templateID, err := normalizeRequired(request.WorkflowTemplateID, "authoring session workflow template ID")
	if err != nil {
		return preparedAuthoringSession{}, err
	}
	templateVersion, err := normalizeRequired(request.WorkflowTemplateVersion, "authoring session workflow template version")
	if err != nil {
		return preparedAuthoringSession{}, err
	}
	manifest, err := normalizeJSON(request.SessionManifestJSON, "authoring session manifest")
	if err != nil {
		return preparedAuthoringSession{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "authoring session idempotency key")
	if err != nil {
		return preparedAuthoringSession{}, err
	}
	requestedID := strings.TrimSpace(request.ID)
	if requestedID != "" && !isUUIDv7(requestedID) {
		return preparedAuthoringSession{}, ErrInvalidUUIDv7Identity
	}
	return preparedAuthoringSession{
		sourceID: sourceID, targetTaskID: targetTaskID, workflowTemplateID: templateID,
		workflowTemplateVersion: templateVersion, sessionManifestJSON: manifest,
		sessionFingerprint: authoringSessionFingerprint(sourceID, targetTaskID, templateID, templateVersion, manifest),
		idempotencyKey:     key, requestedID: requestedID,
	}, nil
}

func validateAuthoringSessionTargetTaskTx(ctx context.Context, tx *sql.Tx, source AuthoringSource, taskID string) error {
	task, err := getTaskV2Tx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if task.LifecycleState != TaskLifecycleDraft || task.CurrentRevisionID != "" {
		return fmt.Errorf("authoring session target task must be a revision-free draft")
	}
	var revisionCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_revisions WHERE task_id = ?`, task.ID).Scan(&revisionCount); err != nil {
		return err
	}
	if revisionCount != 0 {
		return fmt.Errorf("authoring session target task already has a revision")
	}
	if task.SourceRepo != source.RepositoryURL || task.SourceCommit != source.CommitSHA {
		return fmt.Errorf("authoring session target task source does not match frozen authoring source")
	}
	return nil
}

func getAuthoringSourceTx(ctx context.Context, tx *sql.Tx, sourceID string) (AuthoringSource, error) {
	source, err := scanAuthoringSource(tx.QueryRowContext(ctx, authoringSourceSelect+" WHERE id = ?", sourceID))
	if err == sql.ErrNoRows {
		return AuthoringSource{}, fmt.Errorf("%w: authoring source %s", ErrNotFound, sourceID)
	}
	return source, err
}

func getAuthoringSourceByKeyTx(ctx context.Context, tx *sql.Tx, key string) (AuthoringSource, error) {
	source, err := scanAuthoringSource(tx.QueryRowContext(ctx, authoringSourceSelect+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return AuthoringSource{}, fmt.Errorf("%w: authoring source idempotency key %s", ErrNotFound, key)
	}
	return source, err
}

func getAuthoringSourceByFingerprintTx(ctx context.Context, tx *sql.Tx, fingerprint string) (AuthoringSource, error) {
	source, err := scanAuthoringSource(tx.QueryRowContext(ctx, authoringSourceSelect+" WHERE source_fingerprint = ?", fingerprint))
	if err == sql.ErrNoRows {
		return AuthoringSource{}, fmt.Errorf("%w: authoring source fingerprint %s", ErrNotFound, fingerprint)
	}
	return source, err
}

func getAuthoringSessionTx(ctx context.Context, tx *sql.Tx, sessionID string) (AuthoringSession, error) {
	session, err := scanAuthoringSession(tx.QueryRowContext(ctx, authoringSessionSelect+" WHERE id = ?", sessionID))
	if err == sql.ErrNoRows {
		return AuthoringSession{}, fmt.Errorf("%w: authoring session %s", ErrNotFound, sessionID)
	}
	return session, err
}

func getAuthoringSessionByKeyTx(ctx context.Context, tx *sql.Tx, key string) (AuthoringSession, error) {
	session, err := scanAuthoringSession(tx.QueryRowContext(ctx, authoringSessionSelect+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return AuthoringSession{}, fmt.Errorf("%w: authoring session idempotency key %s", ErrNotFound, key)
	}
	return session, err
}

func getAuthoringSessionByFingerprintTx(ctx context.Context, tx *sql.Tx, fingerprint string) (AuthoringSession, error) {
	session, err := scanAuthoringSession(tx.QueryRowContext(ctx, authoringSessionSelect+" WHERE session_fingerprint = ?", fingerprint))
	if err == sql.ErrNoRows {
		return AuthoringSession{}, fmt.Errorf("%w: authoring session fingerprint %s", ErrNotFound, fingerprint)
	}
	return session, err
}

func getAuthoringRunInputArtifactByKeyTx(ctx context.Context, tx *sql.Tx, key string) (AuthoringRunInputArtifact, error) {
	input, err := scanAuthoringRunInputArtifact(tx.QueryRowContext(ctx, authoringRunInputArtifactSelect+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return AuthoringRunInputArtifact{}, fmt.Errorf("%w: authoring run input idempotency key %s", ErrNotFound, key)
	}
	return input, err
}

func scanAuthoringSource(scanner rowScanner) (AuthoringSource, error) {
	var source AuthoringSource
	if err := scanner.Scan(
		&source.ID, &source.RepositoryURL, &source.CommitSHA, &source.SnapshotArtifactRef,
		&source.SnapshotContentDigest, &source.SnapshotSchemaVersion, &source.SourceFingerprint,
		&source.IdempotencyKey, &source.CreatedBy, &source.CreatedAt,
	); err != nil {
		return AuthoringSource{}, err
	}
	source.CreatedAt = source.CreatedAt.UTC()
	return source, nil
}

func scanAuthoringSession(scanner rowScanner) (AuthoringSession, error) {
	var session AuthoringSession
	var targetTaskID sql.NullString
	if err := scanner.Scan(
		&session.ID, &session.SourceID, &targetTaskID, &session.WorkflowTemplateID,
		&session.WorkflowTemplateVersion, &session.SessionManifestJSON, &session.SessionFingerprint,
		&session.IdempotencyKey, &session.CreatedBy, &session.CreatedAt,
	); err != nil {
		return AuthoringSession{}, err
	}
	session.TargetTaskID = nullableStringValue(targetTaskID)
	session.CreatedAt = session.CreatedAt.UTC()
	return session, nil
}

func scanAuthoringTaskMaterialization(scanner rowScanner) (AuthoringTaskMaterialization, error) {
	var materialization AuthoringTaskMaterialization
	if err := scanner.Scan(
		&materialization.ID, &materialization.SessionID, &materialization.SourceID,
		&materialization.AuthoringRunID, &materialization.TaskID, &materialization.RevisionID,
		&materialization.SourceFingerprint, &materialization.TaskDigest,
		&materialization.RequestFingerprint, &materialization.IdempotencyKey,
		&materialization.CreatedBy, &materialization.CreatedAt,
	); err != nil {
		return AuthoringTaskMaterialization{}, err
	}
	materialization.CreatedAt = materialization.CreatedAt.UTC()
	return materialization, nil
}

func scanAuthoringRunInputArtifact(scanner rowScanner) (AuthoringRunInputArtifact, error) {
	var input AuthoringRunInputArtifact
	if err := scanner.Scan(
		&input.ID, &input.RunID, &input.SessionID, &input.SourceID, &input.SourceFingerprint,
		&input.Port, &input.SnapshotArtifactRef, &input.ContentDigest, &input.SchemaVersion,
		&input.IdempotencyKey, &input.CreatedBy, &input.CreatedAt,
	); err != nil {
		return AuthoringRunInputArtifact{}, err
	}
	input.CreatedAt = input.CreatedAt.UTC()
	return input, nil
}

func sameAuthoringSourceRequest(source AuthoringSource, request preparedAuthoringSource) bool {
	return source.RepositoryURL == request.repositoryURL && source.CommitSHA == request.commitSHA &&
		source.SnapshotArtifactRef == request.snapshotArtifactRef &&
		source.SnapshotContentDigest == request.snapshotContentDigest &&
		source.SnapshotSchemaVersion == request.snapshotSchemaVersion &&
		source.SourceFingerprint == request.sourceFingerprint
}

func sameAuthoringSessionRequest(session AuthoringSession, request preparedAuthoringSession) bool {
	return session.SourceID == request.sourceID && session.TargetTaskID == request.targetTaskID &&
		session.WorkflowTemplateID == request.workflowTemplateID &&
		session.WorkflowTemplateVersion == request.workflowTemplateVersion &&
		session.SessionManifestJSON == request.sessionManifestJSON &&
		session.SessionFingerprint == request.sessionFingerprint
}

func sameAuthoringRunInputArtifact(left, right AuthoringRunInputArtifact) bool {
	return left.RunID == right.RunID && left.SessionID == right.SessionID && left.SourceID == right.SourceID &&
		left.SourceFingerprint == right.SourceFingerprint && left.Port == right.Port &&
		left.SnapshotArtifactRef == right.SnapshotArtifactRef && left.ContentDigest == right.ContentDigest &&
		left.SchemaVersion == right.SchemaVersion && left.IdempotencyKey == right.IdempotencyKey
}

// NormalizeAuthoringRepositoryURL accepts only immutable remote Git source
// coordinates. HTTPS repositories must be credential-free. SSH repositories
// may name a login user, but never a password, and support both URI and the
// common scp-like Git spelling. The result is a canonical URL suitable for a
// durable AuthoringSource fingerprint.
func NormalizeAuthoringRepositoryURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, "?#") {
		return "", fmt.Errorf("authoring source repository URL must not contain query or fragment")
	}
	if normalized, recognized, err := normalizeAuthoringScpLikeSSHURL(value); recognized {
		return normalized, err
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed == nil {
		return "", fmt.Errorf("authoring source repository URL is invalid")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && parsed.Scheme != "ssh" {
		return "", fmt.Errorf("authoring source repository URL must use https or ssh")
	}
	if parsed.Host == "" || parsed.Hostname() == "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("authoring source repository URL must be an absolute repository URL without query or fragment")
	}
	if parsed.Scheme == "https" && parsed.User != nil {
		return "", fmt.Errorf("authoring source HTTPS URL must not contain credentials")
	}
	if parsed.Scheme == "ssh" {
		hasPassword := false
		if parsed.User != nil {
			_, hasPassword = parsed.User.Password()
		}
		if parsed.User == nil || parsed.User.Username() == "" || strings.Contains(parsed.User.Username(), ":") || hasPassword {
			return "", fmt.Errorf("authoring source SSH URL must contain a username but no password")
		}
	}
	path, err := normalizeAuthoringRepositoryPath(parsed.Path)
	if err != nil {
		return "", err
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = path
	parsed.RawPath = ""
	return parsed.String(), nil
}

func normalizeAuthoringScpLikeSSHURL(value string) (string, bool, error) {
	if value == "" || strings.Contains(value, "://") {
		return "", false, nil
	}
	at := strings.IndexByte(value, '@')
	colon := strings.IndexByte(value, ':')
	if at <= 0 || colon <= at+1 || strings.Count(value, "@") != 1 || strings.Count(value, ":") != 1 {
		return "", false, nil
	}
	user, host, repositoryPath := value[:at], value[at+1:colon], value[colon+1:]
	if strings.ContainsAny(user, "\t\r\n :/") || strings.ContainsAny(host, "\t\r\n /@") || host == "" || strings.Contains(user, ":") {
		return "", true, fmt.Errorf("authoring source SSH URL is invalid")
	}
	path, err := normalizeAuthoringRepositoryPath("/" + repositoryPath)
	if err != nil {
		return "", true, err
	}
	return (&url.URL{Scheme: "ssh", User: url.User(user), Host: strings.ToLower(host), Path: path}).String(), true, nil
}

func normalizeAuthoringRepositoryPath(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" || value == "/" || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\t\r\n\x00") {
		return "", fmt.Errorf("authoring source repository URL must identify a repository path")
	}
	for _, segment := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("authoring source repository URL has an unsafe repository path")
		}
	}
	return value, nil
}

// NormalizeAuthoringCommitSHA accepts a full immutable Git object ID. Both
// SHA-1 and SHA-256 repository formats are supported, but abbreviated refs,
// branches, and tags are not.
func NormalizeAuthoringCommitSHA(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return "", fmt.Errorf("authoring source commit must be a full 40- or 64-hex object ID")
	}
	if value != strings.ToLower(value) || strings.Trim(value, "0123456789abcdef") != "" {
		return "", fmt.Errorf("authoring source commit must be canonical lowercase hexadecimal")
	}
	return value, nil
}

func normalizeAuthoringSHA256(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") ||
		strings.Trim(value[len("sha256:"):], "0123456789abcdef") != "" {
		return "", fmt.Errorf("%s must be a canonical sha256 fingerprint", field)
	}
	return value, nil
}

func authoringSourceFingerprint(repositoryURL, commitSHA, artifactRef, contentDigest, schemaVersion string) string {
	payload, _ := json.Marshal(struct {
		Domain        string `json:"domain"`
		RepositoryURL string `json:"repository_url"`
		CommitSHA     string `json:"commit_sha"`
		ArtifactRef   string `json:"snapshot_artifact_ref"`
		ContentDigest string `json:"snapshot_content_digest"`
		SchemaVersion string `json:"snapshot_schema_version"`
	}{"harbor.authoring-source.v2", repositoryURL, commitSHA, artifactRef, contentDigest, schemaVersion})
	return sha256Fingerprint(payload)
}

func authoringSessionFingerprint(sourceID, targetTaskID, templateID, templateVersion, manifest string) string {
	payload, _ := json.Marshal(struct {
		Domain          string `json:"domain"`
		SourceID        string `json:"source_id"`
		TargetTaskID    string `json:"target_task_id"`
		TemplateID      string `json:"workflow_template_id"`
		TemplateVersion string `json:"workflow_template_version"`
		Manifest        string `json:"session_manifest_json"`
	}{"harbor.authoring-session.v2", sourceID, targetTaskID, templateID, templateVersion, manifest})
	return sha256Fingerprint(payload)
}

func sha256Fingerprint(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}
