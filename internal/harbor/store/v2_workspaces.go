package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const managedWorkspaceSelect = `
	SELECT id, root_uri, purpose, task_id, revision_id, run_id, state, created_at, updated_at, version
	FROM workspaces_v2`

// CreateManagedWorkspace records a disposable checkout after the application
// has created its directory. This repository never creates, removes, or moves
// workspace files itself.
func (s *Store) CreateManagedWorkspace(ctx context.Context, request CreateManagedWorkspaceRequest) (ManagedWorkspace, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ManagedWorkspace{}, err
	}
	if !isUUIDv7(request.TaskID) {
		return ManagedWorkspace{}, ErrInvalidUUIDv7Identity
	}
	if request.RevisionID != "" && !isUUIDv7(request.RevisionID) {
		return ManagedWorkspace{}, ErrInvalidUUIDv7Identity
	}
	if request.RunID != "" && !isUUIDv7(request.RunID) {
		return ManagedWorkspace{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return ManagedWorkspace{}, err
	}
	rootURI, err := normalizeRequired(request.RootURI, "workspace root URI")
	if err != nil {
		return ManagedWorkspace{}, err
	}
	purpose, err := normalizeRequired(request.Purpose, "workspace purpose")
	if err != nil {
		return ManagedWorkspace{}, err
	}
	now := s.now().UTC()
	workspace := ManagedWorkspace{
		ID:         id,
		RootURI:    rootURI,
		Purpose:    purpose,
		TaskID:     request.TaskID,
		RevisionID: strings.TrimSpace(request.RevisionID),
		RunID:      strings.TrimSpace(request.RunID),
		State:      WorkspaceActive,
		CreatedAt:  now,
		UpdatedAt:  now,
		Version:    1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ManagedWorkspace{}, err
	}
	defer tx.Rollback()
	task, err := getTaskV2Tx(ctx, tx, workspace.TaskID)
	if err != nil {
		return ManagedWorkspace{}, err
	}
	if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, resolveActor(request.Actor), now); err != nil {
		return ManagedWorkspace{}, err
	}
	if workspace.RevisionID != "" {
		revision, err := getTaskRevisionTx(ctx, tx, workspace.RevisionID)
		if err != nil {
			return ManagedWorkspace{}, err
		}
		if revision.TaskID != workspace.TaskID {
			return ManagedWorkspace{}, fmt.Errorf("workspace revision belongs to another task")
		}
	}
	if workspace.RunID != "" {
		run, err := getWorkflowRunTx(ctx, tx, workspace.RunID)
		if err != nil {
			return ManagedWorkspace{}, err
		}
		if run.TaskID != workspace.TaskID {
			return ManagedWorkspace{}, fmt.Errorf("workspace run belongs to another task")
		}
		if workspace.RevisionID != "" && run.RevisionID != workspace.RevisionID {
			return ManagedWorkspace{}, fmt.Errorf("workspace run belongs to another revision")
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workspaces_v2 (id, root_uri, purpose, task_id, revision_id, run_id, state, created_at, updated_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, workspace.ID, workspace.RootURI, workspace.Purpose, workspace.TaskID, nullableString(workspace.RevisionID),
		nullableString(workspace.RunID), workspace.State, workspace.CreatedAt, workspace.UpdatedAt, workspace.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return ManagedWorkspace{}, fmt.Errorf("%w: workspace %s or root %s", ErrIdentityCollision, workspace.ID, workspace.RootURI)
		}
		return ManagedWorkspace{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "workspace",
		EntityID:    workspace.ID,
		Action:      "workspace.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"task_id": workspace.TaskID, "revision_id": workspace.RevisionID, "run_id": workspace.RunID, "root_uri": workspace.RootURI, "purpose": workspace.Purpose}),
		CreatedAt:   now,
	}); err != nil {
		return ManagedWorkspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return ManagedWorkspace{}, err
	}
	return workspace, nil
}

func (s *Store) GetManagedWorkspace(ctx context.Context, workspaceID string) (*ManagedWorkspace, error) {
	if !isUUIDv7(workspaceID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	workspace, err := scanManagedWorkspace(s.db.QueryRowContext(ctx, managedWorkspaceSelect+" WHERE id = ?", workspaceID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &workspace, nil
}

func (s *Store) ListManagedWorkspacesForTask(ctx context.Context, taskID string, includePurged bool) ([]ManagedWorkspace, error) {
	if !isUUIDv7(taskID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	query := managedWorkspaceSelect + " WHERE task_id = ?"
	if !includePurged {
		query += " AND state <> 'purged'"
	}
	query += " ORDER BY updated_at DESC, id ASC"
	rows, err := s.db.QueryContext(ctx, query, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var workspaces []ManagedWorkspace
	for rows.Next() {
		workspace, err := scanManagedWorkspace(rows)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces, rows.Err()
}

func (s *Store) TransitionManagedWorkspace(ctx context.Context, request TransitionManagedWorkspaceRequest) (ManagedWorkspace, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ManagedWorkspace{}, err
	}
	if !isUUIDv7(request.WorkspaceID) {
		return ManagedWorkspace{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return ManagedWorkspace{}, fmt.Errorf("expected workspace version must be positive")
	}
	if !validWorkspaceState(request.State) {
		return ManagedWorkspace{}, fmt.Errorf("invalid workspace state %q", request.State)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ManagedWorkspace{}, err
	}
	defer tx.Rollback()
	workspace, err := getManagedWorkspaceTx(ctx, tx, request.WorkspaceID)
	if err != nil {
		return ManagedWorkspace{}, err
	}
	if workspace.Version != request.ExpectedVersion {
		return ManagedWorkspace{}, fmt.Errorf("%w: workspace %s", ErrOptimisticLock, workspace.ID)
	}
	if !validWorkspaceTransition(workspace.State, request.State) {
		return ManagedWorkspace{}, fmt.Errorf("%w: workspace %s from %s to %s", ErrInvalidTransition, workspace.ID, workspace.State, request.State)
	}
	now := s.now().UTC()
	workspace.State = request.State
	workspace.UpdatedAt = now
	workspace.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE workspaces_v2 SET state = ?, updated_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, workspace.State, workspace.UpdatedAt, workspace.Version, workspace.ID, request.ExpectedVersion)
	if err != nil {
		return ManagedWorkspace{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ManagedWorkspace{}, err
	}
	if changed != 1 {
		return ManagedWorkspace{}, fmt.Errorf("%w: workspace %s", ErrOptimisticLock, workspace.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "workspace",
		EntityID:    workspace.ID,
		Action:      "workspace.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"state": workspace.State, "version": workspace.Version}),
		CreatedAt:   now,
	}); err != nil {
		return ManagedWorkspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return ManagedWorkspace{}, err
	}
	return workspace, nil
}

func getManagedWorkspaceTx(ctx context.Context, tx *sql.Tx, workspaceID string) (ManagedWorkspace, error) {
	workspace, err := scanManagedWorkspace(tx.QueryRowContext(ctx, managedWorkspaceSelect+" WHERE id = ?", workspaceID))
	if err == sql.ErrNoRows {
		return ManagedWorkspace{}, fmt.Errorf("%w: workspace %s", ErrNotFound, workspaceID)
	}
	return workspace, err
}

func scanManagedWorkspace(scanner rowScanner) (ManagedWorkspace, error) {
	var workspace ManagedWorkspace
	var revisionID, runID sql.NullString
	if err := scanner.Scan(
		&workspace.ID, &workspace.RootURI, &workspace.Purpose, &workspace.TaskID, &revisionID, &runID,
		&workspace.State, &workspace.CreatedAt, &workspace.UpdatedAt, &workspace.Version,
	); err != nil {
		return ManagedWorkspace{}, err
	}
	workspace.RevisionID = nullableStringValue(revisionID)
	workspace.RunID = nullableStringValue(runID)
	workspace.CreatedAt = workspace.CreatedAt.UTC()
	workspace.UpdatedAt = workspace.UpdatedAt.UTC()
	return workspace, nil
}

func validWorkspaceState(state WorkspaceState) bool {
	switch state {
	case WorkspaceActive, WorkspaceReleased, WorkspaceTrash, WorkspacePurged:
		return true
	default:
		return false
	}
}

func validWorkspaceTransition(from, to WorkspaceState) bool {
	if from == to || from == WorkspacePurged {
		return false
	}
	switch from {
	case WorkspaceActive:
		return to == WorkspaceReleased || to == WorkspaceTrash
	case WorkspaceReleased:
		return to == WorkspaceTrash
	case WorkspaceTrash:
		return to == WorkspaceActive || to == WorkspaceReleased || to == WorkspacePurged
	default:
		return false
	}
}
