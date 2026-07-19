package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const workflowRunSelect = `
	SELECT id, subject_kind, subject_id, subject_revision_id, subject_digest,
	       task_id, revision_id, authoring_session_id, workflow_template_id, workflow_template_version,
	       resolved_profile_hash, definition_hash, run_manifest_json, parent_run_id,
	       trigger, execution_epoch, status, created_by, created_at, started_at, finished_at, version
	FROM workflow_runs`

// These closed constants duplicate only the durable parent/child policy at
// the Store boundary. The richer descriptor remains in workflowadapter, but
// allowing an AuthoringSession parent for arbitrary task-bound templates here
// would let a direct Store caller bypass the application-level persisted
// handoff artifact verification.
const (
	standardAuthoringParentTemplateID                    = "harbor.standard-authoring"
	standardAuthoringParentTemplateVersion               = "1.2.0"
	standardAuthoringTaskAdmissionParentTemplateVersion  = "1.3.0"
	standardAuthoringBriefParentTemplateVersion          = "1.4.0"
	standardAuthoringRepairFeedbackParentTemplateVersion = "1.5.0"
	codeEdgePhase1ChildTemplateID                        = "harbor.codeedge-phase1"
	codeEdgePhase1ChildTemplateVersion                   = "2.2.0"
	standardAuthoringChildTrigger                        = "standard-authoring.materialized"
)

func isStandardAuthoringParentTemplateVersion(version string) bool {
	switch version {
	case standardAuthoringParentTemplateVersion,
		standardAuthoringTaskAdmissionParentTemplateVersion,
		standardAuthoringBriefParentTemplateVersion,
		standardAuthoringRepairFeedbackParentTemplateVersion:
		return true
	default:
		return false
	}
}

func (s *Store) CreateWorkflowRun(ctx context.Context, request CreateWorkflowRunRequest) (WorkflowRun, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return WorkflowRun{}, err
	}
	if !isUUIDv7(request.TaskID) || !isUUIDv7(request.RevisionID) {
		return WorkflowRun{}, ErrInvalidUUIDv7Identity
	}
	if request.ParentRunID != "" && !isUUIDv7(request.ParentRunID) {
		return WorkflowRun{}, ErrInvalidUUIDv7Identity
	}
	authoringHandoffID := strings.TrimSpace(request.AuthoringPhase1HandoffID)
	if authoringHandoffID != "" && !isUUIDv7(authoringHandoffID) {
		return WorkflowRun{}, ErrInvalidUUIDv7Identity
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
	run := WorkflowRun{
		ID:                      id,
		SubjectKind:             WorkflowRunSubjectTaskRevision,
		SubjectID:               request.TaskID,
		SubjectRevisionID:       request.RevisionID,
		TaskID:                  request.TaskID,
		RevisionID:              request.RevisionID,
		WorkflowTemplateID:      templateID,
		WorkflowTemplateVersion: templateVersion,
		ResolvedProfileHash:     profileHash,
		DefinitionHash:          definitionHash,
		RunManifestJSON:         manifest,
		ParentRunID:             strings.TrimSpace(request.ParentRunID),
		Trigger:                 trigger,
		ExecutionEpoch:          request.ExecutionEpoch,
		Status:                  WorkflowRunQueued,
		CreatedBy:               resolveActor(request.Actor),
		CreatedAt:               now,
		Version:                 1,
	}
	initialInputs, err := prepareInitialWorkflowRunInputArtifacts(s, request.InitialInputArtifacts, run, now)
	if err != nil {
		return WorkflowRun{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowRun{}, err
	}
	defer tx.Rollback()
	task, err := getTaskV2Tx(ctx, tx, run.TaskID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, resolveActor(request.Actor), now); err != nil {
		return WorkflowRun{}, err
	}
	revision, err := getTaskRevisionTx(ctx, tx, run.RevisionID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if revision.TaskID != run.TaskID {
		return WorkflowRun{}, fmt.Errorf("workflow run revision belongs to another task")
	}
	run.SubjectDigest = revision.TaskDigest
	if run.ParentRunID != "" {
		parent, err := getWorkflowRunTx(ctx, tx, run.ParentRunID)
		if err != nil {
			return WorkflowRun{}, err
		}
		if parent.TaskID == run.TaskID {
			if authoringHandoffID != "" {
				return WorkflowRun{}, fmt.Errorf("ordinary task-revision parent cannot use an authoring Phase-1 handoff")
			}
		} else {
			// A Standard authoring Run deliberately has no task/revision subject
			// before materialize_task. Its one permitted task-bound child is
			// nevertheless rooted in the AuthoringSession's target Task, proved
			// by the immutable materialization receipt. Do not force the child to
			// fabricate a TaskRevision parent just to satisfy this relationship.
			if parent.SubjectKind != WorkflowRunSubjectAuthoringSession || parent.TaskID != "" || parent.RevisionID != "" || parent.AuthoringSessionID == "" {
				return WorkflowRun{}, fmt.Errorf("parent workflow run belongs to another task")
			}
			if parent.WorkflowTemplateID != standardAuthoringParentTemplateID || !isStandardAuthoringParentTemplateVersion(parent.WorkflowTemplateVersion) ||
				run.WorkflowTemplateID != codeEdgePhase1ChildTemplateID || run.WorkflowTemplateVersion != codeEdgePhase1ChildTemplateVersion ||
				run.Trigger != standardAuthoringChildTrigger {
				return WorkflowRun{}, fmt.Errorf("authoring parent may create only its closed CodeEdge Phase-1 child")
			}
			if authoringHandoffID == "" {
				return WorkflowRun{}, fmt.Errorf("authoring parent requires a persisted Phase-1 handoff")
			}
			session, sessionErr := getAuthoringSessionTx(ctx, tx, parent.AuthoringSessionID)
			if sessionErr != nil || session.TargetTaskID != run.TaskID {
				if sessionErr != nil {
					return WorkflowRun{}, sessionErr
				}
				return WorkflowRun{}, fmt.Errorf("authoring parent workflow run does not own child task")
			}
			materialization, materializationErr := getAuthoringTaskMaterializationByRunTx(ctx, tx, parent.ID)
			if materializationErr != nil {
				return WorkflowRun{}, fmt.Errorf("authoring parent workflow run has no durable task materialization: %w", materializationErr)
			}
			if materialization.SessionID != session.ID || materialization.TaskID != run.TaskID || materialization.RevisionID != run.RevisionID || materialization.TaskDigest != revision.TaskDigest {
				return WorkflowRun{}, fmt.Errorf("authoring parent workflow run does not match child TaskRevision")
			}
			handoff, handoffErr := getAuthoringPhase1HandoffTx(ctx, tx, "id", authoringHandoffID)
			if handoffErr != nil {
				return WorkflowRun{}, fmt.Errorf("authoring parent has no persisted Phase-1 handoff: %w", handoffErr)
			}
			if handoff.AuthoringRunID != parent.ID || handoff.AuthoringSessionID != session.ID || handoff.TaskID != run.TaskID ||
				handoff.RevisionID != run.RevisionID || handoff.TaskDigest != revision.TaskDigest || handoff.ChildRunID != run.ID {
				return WorkflowRun{}, fmt.Errorf("authoring Phase-1 handoff does not match child Run")
			}
		}
	} else if authoringHandoffID != "" {
		return WorkflowRun{}, fmt.Errorf("authoring Phase-1 handoff requires an authoring parent Run")
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_runs (
			id, subject_kind, subject_id, subject_revision_id, subject_digest,
			task_id, revision_id, authoring_session_id, workflow_template_id, workflow_template_version,
			resolved_profile_hash, definition_hash, run_manifest_json, parent_run_id,
			trigger, execution_epoch, status, created_by, created_at, started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, run.ID, run.SubjectKind, run.SubjectID, run.SubjectRevisionID, run.SubjectDigest,
		run.TaskID, run.RevisionID, run.WorkflowTemplateID, run.WorkflowTemplateVersion,
		run.ResolvedProfileHash, run.DefinitionHash, run.RunManifestJSON, nullableString(run.ParentRunID),
		run.Trigger, run.ExecutionEpoch, run.Status, run.CreatedBy, run.CreatedAt, run.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return WorkflowRun{}, fmt.Errorf("%w: workflow run %s", ErrIdentityCollision, run.ID)
		}
		return WorkflowRun{}, err
	}
	// Run inputs precede the worker-visible job in this transaction. SQLite
	// exposes all rows only at commit, while this ordering makes the invariant
	// explicit and prevents future dispatch changes from publishing a job before
	// its immutable subject is durable.
	for _, input := range initialInputs {
		if err := validateRunInputArtifactSubjectTx(ctx, tx, input); err != nil {
			return WorkflowRun{}, err
		}
		if _, _, err := s.insertRunInputArtifactTx(ctx, tx, input, request.Actor, request.Reason); err != nil {
			return WorkflowRun{}, err
		}
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
				return WorkflowRun{}, fmt.Errorf("%w: initial workflow run job %s", ErrIdentityCollision, dispatch.ID)
			}
			if isUniqueConstraint(err) {
				return WorkflowRun{}, fmt.Errorf("%w: initial workflow run job key %s", ErrIdempotencyConflict, dispatch.IdempotencyKey)
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
		PayloadJSON: auditPayload(map[string]any{"task_id": run.TaskID, "revision_id": run.RevisionID, "definition_hash": run.DefinitionHash}),
		CreatedAt:   now,
	}); err != nil {
		return WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return run, nil
}

func prepareWorkflowRunDispatch(s *Store, request *WorkflowRunDispatchRequest, runID, actor string) (*DurableJob, error) {
	if request == nil {
		return nil, nil
	}
	commandType, err := normalizeRequired(request.CommandType, "workflow run dispatch command type")
	if err != nil {
		return nil, err
	}
	payload, err := normalizeJSON(request.PayloadJSON, "workflow run dispatch payload")
	if err != nil {
		return nil, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "workflow run dispatch idempotency key")
	if err != nil {
		return nil, err
	}
	jobID, err := s.newV2ID("")
	if err != nil {
		return nil, err
	}
	return &DurableJob{
		ID:             jobID,
		CommandType:    commandType,
		EntityType:     "workflow_run",
		EntityID:       runID,
		RunID:          runID,
		State:          JobQueued,
		Priority:       request.Priority,
		PayloadJSON:    payload,
		IdempotencyKey: key,
		CreatedBy:      resolveActor(actor),
		Version:        1,
	}, nil
}

func (s *Store) GetWorkflowRun(ctx context.Context, runID string) (*WorkflowRun, error) {
	if !isUUIDv7(runID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	run, err := scanWorkflowRun(s.db.QueryRowContext(ctx, workflowRunSelect+" WHERE id = ?", runID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *Store) ListWorkflowRunsForTask(ctx context.Context, taskID string) ([]WorkflowRun, error) {
	if !isUUIDv7(taskID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, workflowRunSelect+" WHERE task_id = ? ORDER BY created_at DESC, id DESC", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []WorkflowRun
	for rows.Next() {
		run, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// ListAuthoringWorkflowRunsForTargetTask returns pre-materialization Runs
// whose AuthoringSession is owned by the draft Task. These Runs intentionally
// have NULL task_id/revision_id and therefore must never be folded into the
// task-revision list above.
func (s *Store) ListAuthoringWorkflowRunsForTargetTask(ctx context.Context, taskID string) ([]WorkflowRun, error) {
	if !isUUIDv7(taskID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, workflowRunSelect+`
		WHERE subject_kind = ?
		  AND authoring_session_id IN (
			SELECT id FROM authoring_sessions_v2 WHERE target_task_id = ?
		  )
		ORDER BY created_at DESC, id DESC`, WorkflowRunSubjectAuthoringSession, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]WorkflowRun, 0)
	for rows.Next() {
		run, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) TransitionWorkflowRun(ctx context.Context, request TransitionWorkflowRunRequest) (WorkflowRun, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return WorkflowRun{}, err
	}
	if !isUUIDv7(request.RunID) {
		return WorkflowRun{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return WorkflowRun{}, fmt.Errorf("expected workflow run version must be positive")
	}
	if !validWorkflowRunStatus(request.Status) {
		return WorkflowRun{}, fmt.Errorf("invalid workflow run status %q", request.Status)
	}
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return WorkflowRun{}, err
	}
	defer tx.Rollback()
	defer releaseFence()
	run, err := getWorkflowRunTx(ctx, tx, request.RunID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if run.Version != request.ExpectedVersion {
		return WorkflowRun{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, run.ID)
	}
	if !validWorkflowRunTransition(run.Status, request.Status) {
		return WorkflowRun{}, fmt.Errorf("%w: workflow run %s from %s to %s", ErrInvalidTransition, run.ID, run.Status, request.Status)
	}
	now := s.now().UTC()
	run.Status = request.Status
	if (run.Status == WorkflowRunRunning || run.Status == WorkflowRunWaitingReview) && run.StartedAt == nil {
		run.StartedAt = &now
	}
	if marksWorkflowRunFinished(run.Status) {
		run.FinishedAt = &now
	} else if run.Status == WorkflowRunRunning || run.Status == WorkflowRunQueued || run.Status == WorkflowRunResumeRequested {
		// failed_recoverable remains an actionable terminal projection, but a
		// committed continuation is permitted to resume the same immutable Run.
		// Clear the previous completion timestamp only when execution becomes
		// active again; history remains in RunAttempt/StageAttempt records.
		run.FinishedAt = nil
	}
	run.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs SET status = ?, started_at = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, run.Status, run.StartedAt, run.FinishedAt, run.Version, run.ID, request.ExpectedVersion)
	if err != nil {
		return WorkflowRun{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return WorkflowRun{}, err
	}
	if changed != 1 {
		return WorkflowRun{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, run.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "workflow_run",
		EntityID:    run.ID,
		Action:      "workflow_run.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"status": run.Status, "version": run.Version}),
		CreatedAt:   now,
	}); err != nil {
		return WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowRun{}, err
	}
	return run, nil
}

const stageAttemptSelect = `
	SELECT id, run_id, retry_of_stage_attempt_id, stage_key, stage_group, ordinal,
	       input_fingerprint, execution_status, verdict, budget_snapshot_json,
	       retry_snapshot_json, artifact_manifest_id, error_text, failure_class,
	       created_at, started_at, finished_at, version
	FROM stage_attempts`

func (s *Store) CreateStageAttempt(ctx context.Context, request CreateStageAttemptRequest) (StageAttempt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return StageAttempt{}, err
	}
	if !isUUIDv7(request.RunID) {
		return StageAttempt{}, ErrInvalidUUIDv7Identity
	}
	if request.RetryOfStageAttemptID != "" && !isUUIDv7(request.RetryOfStageAttemptID) {
		return StageAttempt{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return StageAttempt{}, err
	}
	stageKey, err := normalizeRequired(request.StageKey, "stage key")
	if err != nil {
		return StageAttempt{}, err
	}
	stageGroup, err := normalizeRequired(request.StageGroup, "stage group")
	if err != nil {
		return StageAttempt{}, err
	}
	inputFingerprint, err := normalizeRequired(request.InputFingerprint, "stage input fingerprint")
	if err != nil {
		return StageAttempt{}, err
	}
	if request.Ordinal <= 0 {
		return StageAttempt{}, fmt.Errorf("stage attempt ordinal must be positive")
	}
	budget, err := normalizeJSON(request.BudgetSnapshotJSON, "stage budget snapshot")
	if err != nil {
		return StageAttempt{}, err
	}
	retry, err := normalizeJSON(request.RetrySnapshotJSON, "stage retry snapshot")
	if err != nil {
		return StageAttempt{}, err
	}
	now := s.now().UTC()
	attempt := StageAttempt{
		ID:                    id,
		RunID:                 request.RunID,
		RetryOfStageAttemptID: strings.TrimSpace(request.RetryOfStageAttemptID),
		StageKey:              stageKey,
		StageGroup:            stageGroup,
		Ordinal:               request.Ordinal,
		InputFingerprint:      inputFingerprint,
		ExecutionStatus:       StageExecutionQueued,
		BudgetSnapshotJSON:    budget,
		RetrySnapshotJSON:     retry,
		CreatedAt:             now,
		Version:               1,
	}
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return StageAttempt{}, err
	}
	defer tx.Rollback()
	defer releaseFence()
	if _, err := getWorkflowRunTx(ctx, tx, attempt.RunID); err != nil {
		return StageAttempt{}, err
	}
	if attempt.RetryOfStageAttemptID != "" {
		previous, err := getStageAttemptTx(ctx, tx, attempt.RetryOfStageAttemptID)
		if err != nil {
			return StageAttempt{}, err
		}
		if previous.RunID != attempt.RunID {
			return StageAttempt{}, fmt.Errorf("retry stage attempt belongs to another run")
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO stage_attempts (
			id, run_id, retry_of_stage_attempt_id, stage_key, stage_group, ordinal,
			input_fingerprint, execution_status, verdict, budget_snapshot_json, retry_snapshot_json,
			artifact_manifest_id, error_text, failure_class, created_at, started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, '', '', '', ?, NULL, NULL, ?)
	`, attempt.ID, attempt.RunID, nullableString(attempt.RetryOfStageAttemptID), attempt.StageKey, attempt.StageGroup,
		attempt.Ordinal, attempt.InputFingerprint, attempt.ExecutionStatus, attempt.BudgetSnapshotJSON, attempt.RetrySnapshotJSON,
		attempt.CreatedAt, attempt.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return StageAttempt{}, fmt.Errorf("%w: stage attempt %s", ErrIdentityCollision, attempt.ID)
		}
		return StageAttempt{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "stage_attempt",
		EntityID:    attempt.ID,
		Action:      "stage_attempt.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"run_id": attempt.RunID, "stage_key": attempt.StageKey, "ordinal": attempt.Ordinal}),
		CreatedAt:   now,
	}); err != nil {
		return StageAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return StageAttempt{}, err
	}
	return attempt, nil
}

func (s *Store) GetStageAttempt(ctx context.Context, stageAttemptID string) (*StageAttempt, error) {
	if !isUUIDv7(stageAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	attempt, err := scanStageAttempt(s.db.QueryRowContext(ctx, stageAttemptSelect+" WHERE id = ?", stageAttemptID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

func (s *Store) ListStageAttemptsForRun(ctx context.Context, runID string) ([]StageAttempt, error) {
	if !isUUIDv7(runID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, stageAttemptSelect+" WHERE run_id = ? ORDER BY ordinal ASC, created_at ASC", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []StageAttempt
	for rows.Next() {
		attempt, err := scanStageAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) TransitionStageAttempt(ctx context.Context, request TransitionStageAttemptRequest) (StageAttempt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return StageAttempt{}, err
	}
	if !isUUIDv7(request.StageAttemptID) {
		return StageAttempt{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return StageAttempt{}, fmt.Errorf("expected stage attempt version must be positive")
	}
	if !validStageExecutionStatus(request.ExecutionStatus) {
		return StageAttempt{}, fmt.Errorf("invalid stage execution status %q", request.ExecutionStatus)
	}
	if request.ExecutionStatus == StageExecutionCompleted {
		if !validVerdict(request.Verdict) {
			return StageAttempt{}, fmt.Errorf("completed stage attempts require a verdict")
		}
	} else if request.Verdict != "" {
		return StageAttempt{}, fmt.Errorf("only completed stage attempts may carry a verdict")
	}
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return StageAttempt{}, err
	}
	defer tx.Rollback()
	defer releaseFence()
	attempt, err := getStageAttemptTx(ctx, tx, request.StageAttemptID)
	if err != nil {
		return StageAttempt{}, err
	}
	if attempt.Version != request.ExpectedVersion {
		return StageAttempt{}, fmt.Errorf("%w: stage attempt %s", ErrOptimisticLock, attempt.ID)
	}
	if !validStageTransition(attempt.ExecutionStatus, request.ExecutionStatus) {
		return StageAttempt{}, fmt.Errorf("%w: stage attempt %s from %s to %s", ErrInvalidTransition, attempt.ID, attempt.ExecutionStatus, request.ExecutionStatus)
	}
	now := s.now().UTC()
	attempt.ExecutionStatus = request.ExecutionStatus
	attempt.Verdict = request.Verdict
	if value := strings.TrimSpace(request.ArtifactManifestID); value != "" {
		attempt.ArtifactManifestID = value
	}
	if value := strings.TrimSpace(request.ErrorText); value != "" {
		attempt.ErrorText = value
	}
	if value := strings.TrimSpace(request.FailureClass); value != "" {
		attempt.FailureClass = value
	}
	if (attempt.ExecutionStatus == StageExecutionRunning || attempt.ExecutionStatus == StageExecutionWaiting) && attempt.StartedAt == nil {
		attempt.StartedAt = &now
	}
	if isTerminalStageExecutionStatus(attempt.ExecutionStatus) {
		attempt.FinishedAt = &now
	}
	attempt.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE stage_attempts
		SET execution_status = ?, verdict = ?, artifact_manifest_id = ?, error_text = ?, failure_class = ?,
			started_at = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, attempt.ExecutionStatus, attempt.Verdict, attempt.ArtifactManifestID, attempt.ErrorText, attempt.FailureClass,
		attempt.StartedAt, attempt.FinishedAt, attempt.Version, attempt.ID, request.ExpectedVersion)
	if err != nil {
		return StageAttempt{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return StageAttempt{}, err
	}
	if changed != 1 {
		return StageAttempt{}, fmt.Errorf("%w: stage attempt %s", ErrOptimisticLock, attempt.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "stage_attempt",
		EntityID:    attempt.ID,
		Action:      "stage_attempt.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"execution_status": attempt.ExecutionStatus, "verdict": attempt.Verdict, "version": attempt.Version}),
		CreatedAt:   now,
	}); err != nil {
		return StageAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return StageAttempt{}, err
	}
	return attempt, nil
}

func getWorkflowRunTx(ctx context.Context, tx *sql.Tx, runID string) (WorkflowRun, error) {
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+" WHERE id = ?", runID))
	if err == sql.ErrNoRows {
		return WorkflowRun{}, fmt.Errorf("%w: workflow run %s", ErrNotFound, runID)
	}
	return run, err
}

func getStageAttemptTx(ctx context.Context, tx *sql.Tx, stageAttemptID string) (StageAttempt, error) {
	attempt, err := scanStageAttempt(tx.QueryRowContext(ctx, stageAttemptSelect+" WHERE id = ?", stageAttemptID))
	if err == sql.ErrNoRows {
		return StageAttempt{}, fmt.Errorf("%w: stage attempt %s", ErrNotFound, stageAttemptID)
	}
	return attempt, err
}

func scanWorkflowRun(scanner rowScanner) (WorkflowRun, error) {
	var run WorkflowRun
	var taskID, revisionID, authoringSessionID, parent sql.NullString
	var startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(
		&run.ID, &run.SubjectKind, &run.SubjectID, &run.SubjectRevisionID, &run.SubjectDigest,
		&taskID, &revisionID, &authoringSessionID, &run.WorkflowTemplateID, &run.WorkflowTemplateVersion,
		&run.ResolvedProfileHash, &run.DefinitionHash, &run.RunManifestJSON, &parent,
		&run.Trigger, &run.ExecutionEpoch, &run.Status, &run.CreatedBy, &run.CreatedAt, &startedAt, &finishedAt, &run.Version,
	); err != nil {
		return WorkflowRun{}, err
	}
	run.ParentRunID = nullableStringValue(parent)
	run.TaskID = nullableStringValue(taskID)
	run.RevisionID = nullableStringValue(revisionID)
	run.AuthoringSessionID = nullableStringValue(authoringSessionID)
	run.CreatedAt = run.CreatedAt.UTC()
	run.StartedAt = nullableTimePtr(startedAt)
	run.FinishedAt = nullableTimePtr(finishedAt)
	return run, nil
}

func scanStageAttempt(scanner rowScanner) (StageAttempt, error) {
	var attempt StageAttempt
	var retry sql.NullString
	var startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(
		&attempt.ID, &attempt.RunID, &retry, &attempt.StageKey, &attempt.StageGroup, &attempt.Ordinal,
		&attempt.InputFingerprint, &attempt.ExecutionStatus, &attempt.Verdict, &attempt.BudgetSnapshotJSON,
		&attempt.RetrySnapshotJSON, &attempt.ArtifactManifestID, &attempt.ErrorText, &attempt.FailureClass,
		&attempt.CreatedAt, &startedAt, &finishedAt, &attempt.Version,
	); err != nil {
		return StageAttempt{}, err
	}
	attempt.RetryOfStageAttemptID = nullableStringValue(retry)
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	attempt.StartedAt = nullableTimePtr(startedAt)
	attempt.FinishedAt = nullableTimePtr(finishedAt)
	return attempt, nil
}

func validWorkflowRunStatus(status WorkflowRunStatus) bool {
	switch status {
	case WorkflowRunQueued, WorkflowRunRunning, WorkflowRunPauseRequested, WorkflowRunPausing, WorkflowRunPaused,
		WorkflowRunResumeRequested, WorkflowRunWaitingReview, WorkflowRunWaitingContinuation, WorkflowRunSucceeded,
		WorkflowRunFailedRecoverable, WorkflowRunFailedTerminal, WorkflowRunCancelRequested, WorkflowRunStopRequested,
		WorkflowRunCanceling, WorkflowRunCanceled, WorkflowRunInterrupted, WorkflowRunInDoubt:
		return true
	default:
		return false
	}
}

func validWorkflowRunTransition(from, to WorkflowRunStatus) bool {
	if from == to || isTerminalWorkflowRunStatus(from) {
		return false
	}
	switch from {
	case WorkflowRunQueued:
		// A durable coordinator can discover a malformed frozen payload before
		// it has begun execution. Preserve that integrity fact as in_doubt
		// rather than leaving a queue entry that another worker would keep
		// claiming as though it were safe work.
		return to == WorkflowRunRunning || to == WorkflowRunCancelRequested || to == WorkflowRunCanceled || to == WorkflowRunInDoubt
	case WorkflowRunRunning:
		return to == WorkflowRunPauseRequested || to == WorkflowRunWaitingReview || to == WorkflowRunWaitingContinuation ||
			to == WorkflowRunSucceeded || to == WorkflowRunFailedRecoverable || to == WorkflowRunFailedTerminal ||
			to == WorkflowRunCancelRequested || to == WorkflowRunStopRequested || to == WorkflowRunCanceling ||
			to == WorkflowRunCanceled || to == WorkflowRunInterrupted || to == WorkflowRunInDoubt
	case WorkflowRunPauseRequested:
		return to == WorkflowRunPausing || to == WorkflowRunCancelRequested || to == WorkflowRunInterrupted || to == WorkflowRunInDoubt
	case WorkflowRunPausing:
		return to == WorkflowRunPaused || to == WorkflowRunInterrupted || to == WorkflowRunInDoubt
	case WorkflowRunPaused:
		return to == WorkflowRunResumeRequested || to == WorkflowRunCancelRequested || to == WorkflowRunCanceled
	case WorkflowRunResumeRequested:
		return to == WorkflowRunRunning || to == WorkflowRunCancelRequested || to == WorkflowRunInterrupted
	case WorkflowRunWaitingReview:
		return to == WorkflowRunRunning || to == WorkflowRunWaitingContinuation || to == WorkflowRunFailedTerminal ||
			to == WorkflowRunCancelRequested || to == WorkflowRunCanceled || to == WorkflowRunInDoubt
	case WorkflowRunWaitingContinuation:
		return to == WorkflowRunRunning || to == WorkflowRunCancelRequested || to == WorkflowRunCanceled || to == WorkflowRunInDoubt
	case WorkflowRunCancelRequested, WorkflowRunStopRequested:
		return to == WorkflowRunCanceling || to == WorkflowRunCanceled || to == WorkflowRunInDoubt || to == WorkflowRunInterrupted
	case WorkflowRunCanceling:
		return to == WorkflowRunCanceled || to == WorkflowRunInDoubt || to == WorkflowRunInterrupted
	case WorkflowRunInDoubt:
		return to == WorkflowRunRunning || to == WorkflowRunSucceeded || to == WorkflowRunFailedRecoverable || to == WorkflowRunFailedTerminal || to == WorkflowRunCanceled || to == WorkflowRunInterrupted
	case WorkflowRunFailedRecoverable:
		return to == WorkflowRunRunning || to == WorkflowRunResumeRequested || to == WorkflowRunCanceled || to == WorkflowRunInDoubt
	default:
		return false
	}
}

func isTerminalWorkflowRunStatus(status WorkflowRunStatus) bool {
	switch status {
	case WorkflowRunSucceeded, WorkflowRunFailedTerminal, WorkflowRunCanceled, WorkflowRunInterrupted:
		return true
	default:
		return false
	}
}

func marksWorkflowRunFinished(status WorkflowRunStatus) bool {
	switch status {
	case WorkflowRunSucceeded, WorkflowRunFailedRecoverable, WorkflowRunFailedTerminal, WorkflowRunCanceled, WorkflowRunInterrupted:
		return true
	default:
		return false
	}
}

func validStageExecutionStatus(status StageExecutionStatus) bool {
	switch status {
	case StageExecutionQueued, StageExecutionRunning, StageExecutionWaiting, StageExecutionCompleted, StageExecutionInfraFailed,
		StageExecutionInterrupted, StageExecutionInDoubt, StageExecutionReconciling, StageExecutionCanceled:
		return true
	default:
		return false
	}
}

func validVerdict(verdict Verdict) bool {
	switch verdict {
	case VerdictPass, VerdictNeedsRepair, VerdictReject, VerdictAdvisory:
		return true
	default:
		return false
	}
}

func validStageTransition(from, to StageExecutionStatus) bool {
	if from == to || isTerminalStageExecutionStatus(from) {
		return false
	}
	switch from {
	case StageExecutionQueued:
		return to == StageExecutionRunning || to == StageExecutionWaiting || to == StageExecutionCanceled
	case StageExecutionRunning:
		return to == StageExecutionWaiting || to == StageExecutionCompleted || to == StageExecutionInfraFailed || to == StageExecutionInterrupted || to == StageExecutionInDoubt || to == StageExecutionCanceled
	case StageExecutionWaiting:
		return to == StageExecutionRunning || to == StageExecutionCompleted || to == StageExecutionInfraFailed || to == StageExecutionInterrupted || to == StageExecutionInDoubt || to == StageExecutionCanceled
	case StageExecutionInDoubt:
		return to == StageExecutionReconciling
	case StageExecutionReconciling:
		return to == StageExecutionCompleted || to == StageExecutionInfraFailed || to == StageExecutionInterrupted || to == StageExecutionCanceled
	default:
		return false
	}
}

func isTerminalStageExecutionStatus(status StageExecutionStatus) bool {
	switch status {
	case StageExecutionCompleted, StageExecutionInfraFailed, StageExecutionInterrupted, StageExecutionCanceled:
		return true
	default:
		return false
	}
}
