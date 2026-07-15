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
// the exact source provenance; the run itself still binds only this session.
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
	`, session.ID, session.SourceID, nullableString(session.TargetTaskID), session.WorkflowTemplateID,
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
	if session.WorkflowTemplateID != templateID || session.WorkflowTemplateVersion != templateVersion {
		return WorkflowRun{}, fmt.Errorf("%w: authoring session %s template differs from workflow run", ErrImmutable, session.ID)
	}
	run := WorkflowRun{
		ID:                      id,
		SubjectKind:             WorkflowRunSubjectAuthoringSession,
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
			id, subject_kind, task_id, revision_id, authoring_session_id,
			workflow_template_id, workflow_template_version, resolved_profile_hash,
			definition_hash, run_manifest_json, parent_run_id, trigger, execution_epoch,
			status, created_by, created_at, started_at, finished_at, version
		) VALUES (?, ?, NULL, NULL, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, run.ID, run.SubjectKind, run.AuthoringSessionID, run.WorkflowTemplateID,
		run.WorkflowTemplateVersion, run.ResolvedProfileHash, run.DefinitionHash,
		run.RunManifestJSON, run.Trigger, run.ExecutionEpoch, run.Status, run.CreatedBy,
		run.CreatedAt, run.Version); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return WorkflowRun{}, fmt.Errorf("%w: authoring workflow run %s", ErrIdentityCollision, run.ID)
		}
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
	repositoryURL, err := normalizeAuthoringRepositoryURL(request.RepositoryURL)
	if err != nil {
		return preparedAuthoringSource{}, err
	}
	commitSHA, err := normalizeAuthoringCommitSHA(request.CommitSHA)
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
	targetTaskID, err := optionalV4ID(request.TargetTaskID, "authoring session target task ID")
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
	if taskID == "" {
		return nil
	}
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

func normalizeAuthoringRepositoryURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil {
		return "", fmt.Errorf("authoring source repository URL is invalid")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && parsed.Scheme != "ssh" && parsed.Scheme != "git" {
		return "", fmt.Errorf("authoring source repository URL must use https, ssh, or git")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("authoring source repository URL must be an absolute credential-free repository URL")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Path == "" || parsed.Path == "/" {
		return "", fmt.Errorf("authoring source repository URL must identify a repository path")
	}
	parsed.RawPath = ""
	return parsed.String(), nil
}

func normalizeAuthoringCommitSHA(value string) (string, error) {
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
