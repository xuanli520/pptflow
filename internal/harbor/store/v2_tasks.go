package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const taskV2Select = `
	SELECT id, slug, title, metadata_json, source_repo, source_commit,
	       lifecycle_state, current_revision_id, created_at, updated_at, deleted_at, version
	FROM tasks_v2`

const reviewRequestSelect = `
	SELECT id, revision_id, evidence_manifest_digest, state, created_by, created_at, closed_at
	FROM review_requests`

const reviewDecisionSelect = `
	SELECT id, review_request_id, revision_id, action, expected_revision_digest, actor, reason, created_at
	FROM review_decisions`

// CreateTaskV2 inserts a stable UUIDv7 task identity. It never upserts: an ID
// collision is an explicit error rather than an implicit identity merge.
func (s *Store) CreateTaskV2(ctx context.Context, request CreateTaskV2Request) (TaskV2, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TaskV2{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return TaskV2{}, err
	}
	slug, err := normalizeRequired(request.Slug, "task slug")
	if err != nil {
		return TaskV2{}, err
	}
	metadata, err := normalizeJSON(request.MetadataJSON, "task metadata")
	if err != nil {
		return TaskV2{}, err
	}
	state := request.LifecycleState
	if state == "" {
		state = TaskLifecycleDraft
	}
	if !validTaskLifecycleState(state) {
		return TaskV2{}, fmt.Errorf("invalid task lifecycle state %q", state)
	}
	now := s.now().UTC()
	task := TaskV2{
		ID:             id,
		Slug:           slug,
		Title:          strings.TrimSpace(request.Title),
		MetadataJSON:   metadata,
		SourceRepo:     strings.TrimSpace(request.SourceRepo),
		SourceCommit:   strings.TrimSpace(request.SourceCommit),
		LifecycleState: state,
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        1,
	}
	if state == TaskLifecycleDeleted {
		task.DeletedAt = &now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskV2{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tasks_v2 (
			id, slug, title, metadata_json, source_repo, source_commit,
			lifecycle_state, current_revision_id, created_at, updated_at, deleted_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?)
	`, task.ID, task.Slug, task.Title, task.MetadataJSON, task.SourceRepo, task.SourceCommit,
		task.LifecycleState, task.CreatedAt, task.UpdatedAt, task.DeletedAt, task.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return TaskV2{}, fmt.Errorf("%w: task %s", ErrIdentityCollision, task.ID)
		}
		return TaskV2{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "task",
		EntityID:    task.ID,
		Action:      "task.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"slug": task.Slug}),
		CreatedAt:   now,
	}); err != nil {
		return TaskV2{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskV2{}, err
	}
	return task, nil
}

// CreateTaskWithRevision writes the task and its first sealed revision in one
// transaction. It is the import/fork boundary after a managed snapshot has
// already been materialized and its V2 digest computed by task policy.
func (s *Store) CreateTaskWithRevision(ctx context.Context, request CreateTaskWithRevisionRequest) (CreateTaskWithRevisionResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	taskID, err := s.newV2ID(request.Task.ID)
	if err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	slug, err := normalizeRequired(request.Task.Slug, "task slug")
	if err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	taskMetadata, err := normalizeJSON(request.Task.MetadataJSON, "task metadata")
	if err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	taskState := request.Task.LifecycleState
	if taskState == "" {
		taskState = TaskLifecycleDraft
	}
	if !validTaskLifecycleState(taskState) {
		return CreateTaskWithRevisionResult{}, fmt.Errorf("invalid task lifecycle state %q", taskState)
	}
	if request.Revision.TaskID != "" && request.Revision.TaskID != taskID {
		return CreateTaskWithRevisionResult{}, fmt.Errorf("initial revision task ID must match the created task")
	}
	if request.Revision.ParentRevisionID != "" {
		return CreateTaskWithRevisionResult{}, fmt.Errorf("initial atomic revision cannot reference a parent revision")
	}
	revisionID, err := s.newV2ID(request.Revision.ID)
	if err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	if request.Revision.Origin == "" || !validRevisionOrigin(request.Revision.Origin) {
		return CreateTaskWithRevisionResult{}, fmt.Errorf("valid revision origin is required")
	}
	if request.Revision.State != "" && request.Revision.State != RevisionStateSealed {
		return CreateTaskWithRevisionResult{}, fmt.Errorf("initial atomic revision must be sealed")
	}
	taskDigest, err := normalizeRequired(request.Revision.TaskDigest, "task digest")
	if err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	if err := ValidateTaskDigestV2(taskDigest); err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	revisionMetadata, err := normalizeJSON(request.Revision.MetadataJSON, "revision metadata")
	if err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	now := s.now().UTC()
	task := TaskV2{
		ID:             taskID,
		Slug:           slug,
		Title:          strings.TrimSpace(request.Task.Title),
		MetadataJSON:   taskMetadata,
		SourceRepo:     strings.TrimSpace(request.Task.SourceRepo),
		SourceCommit:   strings.TrimSpace(request.Task.SourceCommit),
		LifecycleState: taskState,
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        1,
	}
	if task.LifecycleState == TaskLifecycleDeleted {
		task.DeletedAt = &now
	}
	revisionActor := resolveActor(request.Revision.Actor)
	if strings.TrimSpace(request.Revision.Actor) == "" && strings.TrimSpace(request.Task.Actor) != "" {
		revisionActor = resolveActor(request.Task.Actor)
	}
	revision := TaskRevision{
		ID:             revisionID,
		TaskID:         task.ID,
		VersionNumber:  1,
		Origin:         request.Revision.Origin,
		TaskDigest:     taskDigest,
		ProposalDigest: strings.TrimSpace(request.Revision.ProposalDigest),
		ManifestID:     strings.TrimSpace(request.Revision.ManifestID),
		State:          RevisionStateSealed,
		StateVersion:   1,
		StateUpdatedBy: revisionActor,
		StateUpdatedAt: now,
		ChangeSummary:  strings.TrimSpace(request.Revision.ChangeSummary),
		MetadataJSON:   revisionMetadata,
		CreatedBy:      revisionActor,
		CreatedAt:      now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tasks_v2 (
			id, slug, title, metadata_json, source_repo, source_commit,
			lifecycle_state, current_revision_id, created_at, updated_at, deleted_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?)
	`, task.ID, task.Slug, task.Title, task.MetadataJSON, task.SourceRepo, task.SourceCommit,
		task.LifecycleState, task.CreatedAt, task.UpdatedAt, task.DeletedAt, task.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return CreateTaskWithRevisionResult{}, fmt.Errorf("%w: task %s", ErrIdentityCollision, task.ID)
		}
		return CreateTaskWithRevisionResult{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO task_revisions (
			id, task_id, version_number, parent_revision_id, origin, task_digest,
			proposal_digest, manifest_id, state, validation_evidence_manifest, state_version, state_updated_by,
			state_updated_at, change_summary, metadata_json, created_by, created_at
		) VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?)
	`, revision.ID, revision.TaskID, revision.VersionNumber, revision.Origin, revision.TaskDigest,
		revision.ProposalDigest, revision.ManifestID, revision.State, revision.StateVersion, revision.StateUpdatedBy,
		revision.StateUpdatedAt, revision.ChangeSummary, revision.MetadataJSON, revision.CreatedBy, revision.CreatedAt)
	if err != nil {
		if isUniqueConstraint(err) {
			return CreateTaskWithRevisionResult{}, fmt.Errorf("%w: revision %s", ErrIdentityCollision, revision.ID)
		}
		return CreateTaskWithRevisionResult{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Task.Actor,
		EntityType:  "task",
		EntityID:    task.ID,
		Action:      "task.created",
		Reason:      request.Task.Reason,
		PayloadJSON: auditPayload(map[string]any{"slug": task.Slug, "atomic_initial_revision_id": revision.ID}),
		CreatedAt:   now,
	}); err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       revisionActor,
		EntityType:  "task_revision",
		EntityID:    revision.ID,
		Action:      "task_revision.created",
		Reason:      request.Revision.Reason,
		PayloadJSON: auditPayload(map[string]any{"task_id": revision.TaskID, "version_number": revision.VersionNumber, "task_digest": revision.TaskDigest, "state": revision.State}),
		CreatedAt:   now,
	}); err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CreateTaskWithRevisionResult{}, err
	}
	return CreateTaskWithRevisionResult{Task: task, Revision: revision}, nil
}

func (s *Store) GetTaskV2(ctx context.Context, taskID string) (*TaskV2, error) {
	if !isUUIDv7(taskID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	task, err := scanTaskV2(s.db.QueryRowContext(ctx, taskV2Select+" WHERE id = ?", taskID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &task, nil
}

func (s *Store) ListTasksV2(ctx context.Context, includeDeleted bool) ([]TaskV2, error) {
	query := taskV2Select
	if !includeDeleted {
		query += " WHERE lifecycle_state <> 'deleted'"
	}
	query += " ORDER BY updated_at DESC, id ASC"
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []TaskV2
	for rows.Next() {
		task, err := scanTaskV2(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// UpdateTaskV2 applies a compare-and-swap patch. Empty patch fields preserve
// their stored value, which prevents accidental metadata loss in callers that
// only intend to change one field.
func (s *Store) UpdateTaskV2(ctx context.Context, request UpdateTaskV2Request) (TaskV2, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TaskV2{}, err
	}
	if !isUUIDv7(request.TaskID) {
		return TaskV2{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return TaskV2{}, fmt.Errorf("expected task version must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskV2{}, err
	}
	defer tx.Rollback()
	task, err := getTaskV2Tx(ctx, tx, request.TaskID)
	if err != nil {
		return TaskV2{}, err
	}
	now := s.now().UTC()
	if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, resolveActor(request.Actor), now); err != nil {
		return TaskV2{}, err
	}
	if task.Version != request.ExpectedVersion {
		return TaskV2{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	if value := strings.TrimSpace(request.Slug); value != "" {
		task.Slug = value
	}
	if value := strings.TrimSpace(request.Title); value != "" {
		task.Title = value
	}
	if strings.TrimSpace(request.MetadataJSON) != "" {
		metadata, err := normalizeJSON(request.MetadataJSON, "task metadata")
		if err != nil {
			return TaskV2{}, err
		}
		task.MetadataJSON = metadata
	}
	if request.LifecycleState != "" {
		if !validTaskLifecycleState(request.LifecycleState) {
			return TaskV2{}, fmt.Errorf("invalid task lifecycle state %q", request.LifecycleState)
		}
		if !validTaskLifecycleTransition(task.LifecycleState, request.LifecycleState) {
			return TaskV2{}, fmt.Errorf("%w: task %s from %s to %s", ErrInvalidTransition, task.ID, task.LifecycleState, request.LifecycleState)
		}
		task.LifecycleState = request.LifecycleState
	}
	if task.LifecycleState == TaskLifecycleDeleted {
		if task.DeletedAt == nil {
			task.DeletedAt = &now
		}
	} else {
		task.DeletedAt = nil
	}
	task.UpdatedAt = now
	task.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks_v2
		SET slug = ?, title = ?, metadata_json = ?, lifecycle_state = ?, updated_at = ?, deleted_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, task.Slug, task.Title, task.MetadataJSON, task.LifecycleState, task.UpdatedAt, task.DeletedAt, task.Version, task.ID, request.ExpectedVersion)
	if err != nil {
		return TaskV2{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return TaskV2{}, err
	}
	if changed != 1 {
		return TaskV2{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "task",
		EntityID:    task.ID,
		Action:      "task.updated",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"version": task.Version, "lifecycle_state": task.LifecycleState}),
		CreatedAt:   now,
	}); err != nil {
		return TaskV2{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskV2{}, err
	}
	return task, nil
}

// guardTaskPurgeMutationTx is the shared task mutation fence. It prevents
// task state or content writes while a prepared purge awaits recovery, and it
// turns a completed irreversible purge into a durable tombstone. An expired
// orphan lease is resolved in the same transaction so it cannot block a later
// operation indefinitely.
func (s *Store) guardTaskPurgeMutationTx(ctx context.Context, tx *sql.Tx, taskID, actor string, now time.Time) error {
	completed, err := getCompletedTaskPurgeForTaskTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if completed != nil {
		return fmt.Errorf("%w: task %s operation %s", ErrTaskPurged, taskID, completed.ID)
	}
	inProgress, err := getInProgressTaskPurgeForTaskTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if inProgress != nil {
		return fmt.Errorf("%w: task %s operation %s", ErrTaskPurgeInProgress, taskID, inProgress.ID)
	}
	lease, err := getActiveLeaseForResourceTx(ctx, tx, "task_purge", taskID)
	if err != nil {
		return err
	}
	if lease == nil {
		return nil
	}
	if lease.ExpiresAt.After(now) {
		return fmt.Errorf("%w: task %s", ErrTaskPurgeInProgress, taskID)
	}
	return s.expireLeaseTx(ctx, tx, *lease, actor, now, "task mutation observed expired task purge lease")
}

func (s *Store) CreateTaskRevision(ctx context.Context, request CreateTaskRevisionRequest) (TaskRevision, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TaskRevision{}, err
	}
	if !isUUIDv7(request.TaskID) {
		return TaskRevision{}, ErrInvalidUUIDv7Identity
	}
	if request.ParentRevisionID != "" && !isUUIDv7(request.ParentRevisionID) {
		return TaskRevision{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return TaskRevision{}, err
	}
	if request.Origin == "" || !validRevisionOrigin(request.Origin) {
		return TaskRevision{}, fmt.Errorf("valid revision origin is required")
	}
	if request.State == "" {
		request.State = RevisionStateSealed
	}
	if request.State != RevisionStateSealed {
		return TaskRevision{}, fmt.Errorf("new task revisions must be sealed; use TransitionTaskRevisionState for lifecycle changes")
	}
	taskDigest, err := normalizeRequired(request.TaskDigest, "task digest")
	if err != nil {
		return TaskRevision{}, err
	}
	if err := ValidateTaskDigestV2(taskDigest); err != nil {
		return TaskRevision{}, err
	}
	metadata, err := normalizeJSON(request.MetadataJSON, "revision metadata")
	if err != nil {
		return TaskRevision{}, err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskRevision{}, err
	}
	defer tx.Rollback()
	task, err := getTaskV2Tx(ctx, tx, request.TaskID)
	if err != nil {
		return TaskRevision{}, err
	}
	if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, resolveActor(request.Actor), now); err != nil {
		return TaskRevision{}, err
	}
	if request.ParentRevisionID != "" {
		parent, err := getTaskRevisionTx(ctx, tx, request.ParentRevisionID)
		if err != nil {
			return TaskRevision{}, err
		}
		if parent.TaskID != request.TaskID {
			return TaskRevision{}, fmt.Errorf("parent revision belongs to another task")
		}
	}
	var versionNumber int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_number), 0) + 1 FROM task_revisions WHERE task_id = ?`, request.TaskID).Scan(&versionNumber); err != nil {
		return TaskRevision{}, err
	}
	revision := TaskRevision{
		ID:               id,
		TaskID:           request.TaskID,
		VersionNumber:    versionNumber,
		ParentRevisionID: strings.TrimSpace(request.ParentRevisionID),
		Origin:           request.Origin,
		TaskDigest:       taskDigest,
		ProposalDigest:   strings.TrimSpace(request.ProposalDigest),
		ManifestID:       strings.TrimSpace(request.ManifestID),
		State:            request.State,
		StateVersion:     1,
		StateUpdatedBy:   resolveActor(request.Actor),
		StateUpdatedAt:   now,
		ChangeSummary:    strings.TrimSpace(request.ChangeSummary),
		MetadataJSON:     metadata,
		CreatedBy:        resolveActor(request.Actor),
		CreatedAt:        now,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO task_revisions (
			id, task_id, version_number, parent_revision_id, origin, task_digest,
			proposal_digest, manifest_id, state, validation_evidence_manifest, state_version, state_updated_by,
			state_updated_at, change_summary, metadata_json, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?)
	`, revision.ID, revision.TaskID, revision.VersionNumber, nullableString(revision.ParentRevisionID), revision.Origin,
		revision.TaskDigest, revision.ProposalDigest, revision.ManifestID, revision.State, revision.StateVersion,
		revision.StateUpdatedBy, revision.StateUpdatedAt, revision.ChangeSummary, revision.MetadataJSON, revision.CreatedBy, revision.CreatedAt)
	if err != nil {
		if isUniqueConstraint(err) {
			return TaskRevision{}, fmt.Errorf("%w: revision %s", ErrIdentityCollision, revision.ID)
		}
		return TaskRevision{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "task_revision",
		EntityID:    revision.ID,
		Action:      "task_revision.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"task_id": revision.TaskID, "version_number": revision.VersionNumber, "task_digest": revision.TaskDigest, "state": revision.State}),
		CreatedAt:   now,
	}); err != nil {
		return TaskRevision{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskRevision{}, err
	}
	return revision, nil
}

// TransitionTaskRevisionState advances only lifecycle metadata under CAS. The
// SQL triggers independently reject any update to revision content fields.
func (s *Store) TransitionTaskRevisionState(ctx context.Context, request TransitionTaskRevisionStateRequest) (TaskRevision, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TaskRevision{}, err
	}
	if !isUUIDv7(request.RevisionID) {
		return TaskRevision{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedStateVersion <= 0 {
		return TaskRevision{}, fmt.Errorf("expected revision state version must be positive")
	}
	if !validRevisionState(request.State) {
		return TaskRevision{}, fmt.Errorf("invalid revision state %q", request.State)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskRevision{}, err
	}
	defer tx.Rollback()
	revision, err := getTaskRevisionTx(ctx, tx, request.RevisionID)
	if err != nil {
		return TaskRevision{}, err
	}
	task, err := getTaskV2Tx(ctx, tx, revision.TaskID)
	if err != nil {
		return TaskRevision{}, err
	}
	if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, resolveActor(request.Actor), s.now().UTC()); err != nil {
		return TaskRevision{}, err
	}
	if revision.StateVersion != request.ExpectedStateVersion {
		return TaskRevision{}, fmt.Errorf("%w: revision %s", ErrOptimisticLock, revision.ID)
	}
	if !validRevisionStateTransition(revision.State, request.State) {
		return TaskRevision{}, fmt.Errorf("%w: revision %s from %s to %s", ErrInvalidTransition, revision.ID, revision.State, request.State)
	}
	evidence := revision.ValidationEvidenceManifest
	if request.State == RevisionStateValidated {
		value, err := normalizeRequired(request.ValidationEvidenceManifest, "validation evidence manifest")
		if err != nil {
			return TaskRevision{}, err
		}
		evidence = value
	} else if value := strings.TrimSpace(request.ValidationEvidenceManifest); value != "" && value != evidence {
		return TaskRevision{}, fmt.Errorf("validation evidence manifest is immutable after validation")
	}
	now := s.now().UTC()
	revision.State = request.State
	revision.ValidationEvidenceManifest = evidence
	revision.StateVersion++
	revision.StateUpdatedBy = resolveActor(request.Actor)
	revision.StateUpdatedAt = now
	result, err := tx.ExecContext(ctx, `
		UPDATE task_revisions
		SET state = ?, validation_evidence_manifest = ?, state_version = ?, state_updated_by = ?, state_updated_at = ?
		WHERE id = ? AND state_version = ?
	`, revision.State, revision.ValidationEvidenceManifest, revision.StateVersion, revision.StateUpdatedBy, revision.StateUpdatedAt,
		revision.ID, request.ExpectedStateVersion)
	if err != nil {
		return TaskRevision{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return TaskRevision{}, err
	}
	if changed != 1 {
		return TaskRevision{}, fmt.Errorf("%w: revision %s", ErrOptimisticLock, revision.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "task_revision",
		EntityID:    revision.ID,
		Action:      "task_revision.state_transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"state": revision.State, "state_version": revision.StateVersion, "validation_evidence_manifest": revision.ValidationEvidenceManifest}),
		CreatedAt:   now,
	}); err != nil {
		return TaskRevision{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskRevision{}, err
	}
	return revision, nil
}

func (s *Store) GetTaskRevision(ctx context.Context, revisionID string) (*TaskRevision, error) {
	if !isUUIDv7(revisionID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	revision, err := scanTaskRevision(s.db.QueryRowContext(ctx, taskRevisionSelect+" WHERE id = ?", revisionID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &revision, nil
}

func (s *Store) ListTaskRevisions(ctx context.Context, taskID string) ([]TaskRevision, error) {
	if !isUUIDv7(taskID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, taskRevisionSelect+" WHERE task_id = ? ORDER BY version_number DESC", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []TaskRevision
	for rows.Next() {
		revision, err := scanTaskRevision(rows)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

func (s *Store) CreateReviewRequest(ctx context.Context, request CreateReviewRequest) (ReviewRequest, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ReviewRequest{}, err
	}
	if !isUUIDv7(request.RevisionID) {
		return ReviewRequest{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return ReviewRequest{}, err
	}
	evidence, err := normalizeRequired(request.EvidenceManifestDigest, "review evidence manifest digest")
	if err != nil {
		return ReviewRequest{}, err
	}
	now := s.now().UTC()
	review := ReviewRequest{ID: id, RevisionID: request.RevisionID, EvidenceManifestDigest: evidence, State: "open", CreatedBy: resolveActor(request.Actor), CreatedAt: now}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRequest{}, err
	}
	defer tx.Rollback()
	if _, err := getTaskRevisionTx(ctx, tx, request.RevisionID); err != nil {
		return ReviewRequest{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO review_requests (id, revision_id, evidence_manifest_digest, state, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, review.ID, review.RevisionID, review.EvidenceManifestDigest, review.State, review.CreatedBy, review.CreatedAt)
	if err != nil {
		if isUniqueConstraint(err) {
			return ReviewRequest{}, fmt.Errorf("%w: review request %s", ErrIdentityCollision, review.ID)
		}
		return ReviewRequest{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "review_request",
		EntityID:    review.ID,
		Action:      "review.requested",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"revision_id": review.RevisionID, "evidence_manifest_digest": review.EvidenceManifestDigest}),
		CreatedAt:   now,
	}); err != nil {
		return ReviewRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewRequest{}, err
	}
	return review, nil
}

// ListReviewRequestsForRevision returns the durable review envelopes for one
// immutable revision. It is deliberately read-only so projection adapters do
// not need direct database access to explain approval state.
func (s *Store) ListReviewRequestsForRevision(ctx context.Context, revisionID string) ([]ReviewRequest, error) {
	if !isUUIDv7(revisionID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, reviewRequestSelect+" WHERE revision_id = ? ORDER BY created_at DESC, id ASC", revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	requests := make([]ReviewRequest, 0)
	for rows.Next() {
		request, err := scanReviewRequest(rows)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

// GetReviewRequest returns one durable review envelope by its stable identity.
// Lifecycle mutation checkpoints use this narrow read to bind an operator's
// confirmation to the exact review state and evidence it displayed.
func (s *Store) GetReviewRequest(ctx context.Context, reviewRequestID string) (*ReviewRequest, error) {
	if !isUUIDv7(reviewRequestID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	review, err := scanReviewRequest(s.db.QueryRowContext(ctx, reviewRequestSelect+" WHERE id = ?", reviewRequestID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &review, nil
}

// ListReviewDecisionsForRequest returns immutable decisions attached to a
// review envelope. A normal V2 review has at most one decision, but returning
// a slice preserves the audit model without projecting a mutable shortcut.
func (s *Store) ListReviewDecisionsForRequest(ctx context.Context, reviewRequestID string) ([]ReviewDecision, error) {
	if !isUUIDv7(reviewRequestID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, reviewDecisionSelect+" WHERE review_request_id = ? ORDER BY created_at DESC, id ASC", reviewRequestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	decisions := make([]ReviewDecision, 0)
	for rows.Next() {
		decision, err := scanReviewDecision(rows)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

func (s *Store) RecordReviewDecision(ctx context.Context, request RecordReviewDecisionRequest) (ReviewDecision, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ReviewDecision{}, err
	}
	if !isUUIDv7(request.ReviewRequestID) || !isUUIDv7(request.RevisionID) {
		return ReviewDecision{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return ReviewDecision{}, err
	}
	if !validReviewDecisionAction(request.Action) {
		return ReviewDecision{}, fmt.Errorf("invalid review decision action %q", request.Action)
	}
	expectedDigest, err := normalizeRequired(request.ExpectedRevisionDigest, "expected revision digest")
	if err != nil {
		return ReviewDecision{}, err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewDecision{}, err
	}
	defer tx.Rollback()
	gateBinding, err := getReviewGateBindingByReviewRequestTx(ctx, tx, request.ReviewRequestID)
	if err != nil {
		return ReviewDecision{}, err
	}
	if gateBinding != nil {
		return ReviewDecision{}, fmt.Errorf("%w: review request %s is a workflow review gate", ErrInvalidTransition, request.ReviewRequestID)
	}
	var reviewRevisionID, reviewState string
	err = tx.QueryRowContext(ctx, `SELECT revision_id, state FROM review_requests WHERE id = ?`, request.ReviewRequestID).Scan(&reviewRevisionID, &reviewState)
	if err == sql.ErrNoRows {
		return ReviewDecision{}, fmt.Errorf("%w: review request %s", ErrNotFound, request.ReviewRequestID)
	}
	if err != nil {
		return ReviewDecision{}, err
	}
	if reviewRevisionID != request.RevisionID || reviewState != "open" {
		return ReviewDecision{}, fmt.Errorf("%w: review request is closed or targets another revision", ErrImmutable)
	}
	revision, err := getTaskRevisionTx(ctx, tx, request.RevisionID)
	if err != nil {
		return ReviewDecision{}, err
	}
	if revision.TaskDigest != expectedDigest {
		return ReviewDecision{}, fmt.Errorf("%w: review decision digest does not match revision", ErrInvalidTransition)
	}
	decision := ReviewDecision{
		ID:                     id,
		ReviewRequestID:        request.ReviewRequestID,
		RevisionID:             request.RevisionID,
		Action:                 request.Action,
		ExpectedRevisionDigest: expectedDigest,
		Actor:                  resolveActor(request.Actor),
		Reason:                 strings.TrimSpace(request.Reason),
		CreatedAt:              now,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO review_decisions (id, review_request_id, revision_id, action, expected_revision_digest, actor, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, decision.ID, decision.ReviewRequestID, decision.RevisionID, decision.Action, decision.ExpectedRevisionDigest, decision.Actor, decision.Reason, decision.CreatedAt)
	if err != nil {
		if isUniqueConstraint(err) {
			return ReviewDecision{}, fmt.Errorf("%w: review decision %s", ErrIdentityCollision, decision.ID)
		}
		return ReviewDecision{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_requests SET state = 'closed', closed_at = ? WHERE id = ? AND state = 'open'`, now, request.ReviewRequestID); err != nil {
		return ReviewDecision{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       decision.Actor,
		EntityType:  "review_decision",
		EntityID:    decision.ID,
		Action:      "review.decided",
		Reason:      decision.Reason,
		PayloadJSON: auditPayload(map[string]any{"review_request_id": decision.ReviewRequestID, "revision_id": decision.RevisionID, "action": decision.Action}),
		CreatedAt:   now,
	}); err != nil {
		return ReviewDecision{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewDecision{}, err
	}
	return decision, nil
}

// PromoteTaskCurrentRevision enforces the review-gated current-revision rule:
// a revision must be durable-validated and have a matching approve decision.
func (s *Store) PromoteTaskCurrentRevision(ctx context.Context, request PromoteCurrentRevisionRequest) (TaskV2, error) {
	if !isUUIDv7(request.TaskID) || !isUUIDv7(request.RevisionID) {
		return TaskV2{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return TaskV2{}, fmt.Errorf("expected task version must be positive")
	}
	if _, err := s.BackupBeforeCriticalOperation(ctx, "current_revision_promotion"); err != nil {
		return TaskV2{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskV2{}, err
	}
	defer tx.Rollback()
	task, err := getTaskV2Tx(ctx, tx, request.TaskID)
	if err != nil {
		return TaskV2{}, err
	}
	now := s.now().UTC()
	if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, resolveActor(request.Actor), now); err != nil {
		return TaskV2{}, err
	}
	if task.Version != request.ExpectedVersion {
		return TaskV2{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	revision, err := getTaskRevisionTx(ctx, tx, request.RevisionID)
	if err != nil {
		return TaskV2{}, err
	}
	if revision.TaskID != task.ID {
		return TaskV2{}, fmt.Errorf("revision belongs to another task")
	}
	if revision.State != RevisionStateValidated && revision.State != RevisionStateReleased {
		return TaskV2{}, fmt.Errorf("%w: revision %s is %s", ErrRevisionNotValidated, revision.ID, revision.State)
	}
	var approved int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM review_decisions
		WHERE revision_id = ? AND action = 'approve' AND expected_revision_digest = ?
		  AND NOT EXISTS (
			  SELECT 1
			  FROM review_gate_bindings_v15 gate
			  WHERE gate.review_request_id = review_decisions.review_request_id
		  )
	`, revision.ID, revision.TaskDigest).Scan(&approved)
	if err != nil {
		return TaskV2{}, err
	}
	if approved == 0 {
		return TaskV2{}, fmt.Errorf("%w: revision %s", ErrReviewApprovalNeeded, revision.ID)
	}
	task.CurrentRevisionID = revision.ID
	task.UpdatedAt = now
	task.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks_v2 SET current_revision_id = ?, updated_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, task.CurrentRevisionID, task.UpdatedAt, task.Version, task.ID, request.ExpectedVersion)
	if err != nil {
		return TaskV2{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return TaskV2{}, err
	}
	if changed != 1 {
		return TaskV2{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "task",
		EntityID:    task.ID,
		Action:      "task.current_revision_promoted",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"revision_id": revision.ID, "task_digest": revision.TaskDigest, "version": task.Version}),
		CreatedAt:   now,
	}); err != nil {
		return TaskV2{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskV2{}, err
	}
	return task, nil
}

const taskRevisionSelect = `
	SELECT id, task_id, version_number, parent_revision_id, origin, task_digest,
	       proposal_digest, manifest_id, state, validation_evidence_manifest, state_version, state_updated_by,
	       state_updated_at, change_summary, metadata_json, created_by, created_at
	FROM task_revisions`

func getTaskV2Tx(ctx context.Context, tx *sql.Tx, taskID string) (TaskV2, error) {
	task, err := scanTaskV2(tx.QueryRowContext(ctx, taskV2Select+" WHERE id = ?", taskID))
	if err == sql.ErrNoRows {
		return TaskV2{}, fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	return task, err
}

func getTaskRevisionTx(ctx context.Context, tx *sql.Tx, revisionID string) (TaskRevision, error) {
	revision, err := scanTaskRevision(tx.QueryRowContext(ctx, taskRevisionSelect+" WHERE id = ?", revisionID))
	if err == sql.ErrNoRows {
		return TaskRevision{}, fmt.Errorf("%w: revision %s", ErrNotFound, revisionID)
	}
	return revision, err
}

func scanTaskV2(scanner rowScanner) (TaskV2, error) {
	var task TaskV2
	var deletedAt sql.NullTime
	if err := scanner.Scan(
		&task.ID, &task.Slug, &task.Title, &task.MetadataJSON, &task.SourceRepo, &task.SourceCommit,
		&task.LifecycleState, &task.CurrentRevisionID, &task.CreatedAt, &task.UpdatedAt, &deletedAt, &task.Version,
	); err != nil {
		return TaskV2{}, err
	}
	task.CreatedAt = task.CreatedAt.UTC()
	task.UpdatedAt = task.UpdatedAt.UTC()
	task.DeletedAt = nullableTimePtr(deletedAt)
	return task, nil
}

func scanTaskRevision(scanner rowScanner) (TaskRevision, error) {
	var revision TaskRevision
	var parent sql.NullString
	if err := scanner.Scan(
		&revision.ID, &revision.TaskID, &revision.VersionNumber, &parent, &revision.Origin, &revision.TaskDigest,
		&revision.ProposalDigest, &revision.ManifestID, &revision.State, &revision.ValidationEvidenceManifest,
		&revision.StateVersion, &revision.StateUpdatedBy, &revision.StateUpdatedAt, &revision.ChangeSummary,
		&revision.MetadataJSON, &revision.CreatedBy, &revision.CreatedAt,
	); err != nil {
		return TaskRevision{}, err
	}
	revision.ParentRevisionID = nullableStringValue(parent)
	revision.StateUpdatedAt = revision.StateUpdatedAt.UTC()
	revision.CreatedAt = revision.CreatedAt.UTC()
	return revision, nil
}

func scanReviewRequest(scanner rowScanner) (ReviewRequest, error) {
	var request ReviewRequest
	var closedAt sql.NullTime
	if err := scanner.Scan(
		&request.ID, &request.RevisionID, &request.EvidenceManifestDigest, &request.State,
		&request.CreatedBy, &request.CreatedAt, &closedAt,
	); err != nil {
		return ReviewRequest{}, err
	}
	request.CreatedAt = request.CreatedAt.UTC()
	request.ClosedAt = nullableTimePtr(closedAt)
	return request, nil
}

func scanReviewDecision(scanner rowScanner) (ReviewDecision, error) {
	var decision ReviewDecision
	if err := scanner.Scan(
		&decision.ID, &decision.ReviewRequestID, &decision.RevisionID, &decision.Action,
		&decision.ExpectedRevisionDigest, &decision.Actor, &decision.Reason, &decision.CreatedAt,
	); err != nil {
		return ReviewDecision{}, err
	}
	decision.CreatedAt = decision.CreatedAt.UTC()
	return decision, nil
}

func validTaskLifecycleState(state TaskLifecycleState) bool {
	switch state {
	case TaskLifecycleDraft, TaskLifecycleReady, TaskLifecyclePublished, TaskLifecycleArchived, TaskLifecycleDeleted:
		return true
	default:
		return false
	}
}

func validTaskLifecycleTransition(from, to TaskLifecycleState) bool {
	if from == to {
		return true
	}
	switch from {
	case TaskLifecycleDraft:
		return to == TaskLifecycleReady || to == TaskLifecycleDeleted
	case TaskLifecycleReady:
		return to == TaskLifecyclePublished || to == TaskLifecycleDeleted
	case TaskLifecyclePublished:
		return to == TaskLifecycleArchived || to == TaskLifecycleDeleted
	case TaskLifecycleArchived:
		return to == TaskLifecycleDeleted
	case TaskLifecycleDeleted:
		return to == TaskLifecycleDraft || to == TaskLifecycleReady || to == TaskLifecyclePublished || to == TaskLifecycleArchived
	default:
		return false
	}
}

func validRevisionOrigin(origin RevisionOrigin) bool {
	switch origin {
	case RevisionOriginGenerated, RevisionOriginImported, RevisionOriginManual, RevisionOriginRepair, RevisionOriginFork, RevisionOriginRollback:
		return true
	default:
		return false
	}
}

func validRevisionState(state RevisionState) bool {
	switch state {
	case RevisionStateSealed, RevisionStateValidated, RevisionStateReleased, RevisionStateSuperseded:
		return true
	default:
		return false
	}
}

func validRevisionStateTransition(from, to RevisionState) bool {
	switch from {
	case RevisionStateSealed:
		return to == RevisionStateValidated || to == RevisionStateSuperseded
	case RevisionStateValidated:
		return to == RevisionStateReleased || to == RevisionStateSuperseded
	case RevisionStateReleased:
		return to == RevisionStateSuperseded
	default:
		return false
	}
}

func validReviewDecisionAction(action ReviewDecisionAction) bool {
	switch action {
	case ReviewDecisionApprove, ReviewDecisionRequestChanges, ReviewDecisionRejectTerminal:
		return true
	default:
		return false
	}
}
