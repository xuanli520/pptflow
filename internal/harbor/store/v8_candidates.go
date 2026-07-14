package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const revisionCandidateV8Select = `
	SELECT id, task_id, source_run_id, command_id, repair_session_id,
	       round_ordinal, base_revision_id, base_digest, target_revision_id,
	       target_run_id, expected_task_version, provider_id, checkout_relpath,
	       findings_json, state, after_digest, observed_changes_json,
	       prepared_change_id, mutation_receipt_id, frozen_plan_id,
	       final_manifest_id, child_run_manifest_json, lease_id, lease_owner,
	       lease_fencing_token, lease_version, created_by, reason, created_at,
	       updated_at, retain_until, checkout_tombstoned_at,
	       checkout_tombstoned_by, version
	FROM revision_candidates_v8`

const changeOperationV8Select = `
	SELECT id, candidate_id, provider_id, operation_key, payload_json,
	       payload_digest, state, receipt_id, created_by, created_at, updated_at,
	       version
	FROM change_operations_v8`

func validRevisionCandidateState(state RevisionCandidateState) bool {
	switch state {
	case RevisionCandidateReady, RevisionCandidateApplying, RevisionCandidatePrepared,
		RevisionCandidateNoOp, RevisionCandidateReconcileRequired, RevisionCandidateDiscarded,
		RevisionCandidateCommitting, RevisionCandidateCommitted:
		return true
	default:
		return false
	}
}

func validChangeOperationState(state ChangeOperationState) bool {
	switch state {
	case ChangeOperationPrepared, ChangeOperationRunning, ChangeOperationSucceeded,
		ChangeOperationFailed, ChangeOperationUnknown:
		return true
	default:
		return false
	}
}

func validRevisionCandidateTransition(from, to RevisionCandidateState) bool {
	if from == to || from == RevisionCandidateCommitted || from == RevisionCandidateDiscarded || from == RevisionCandidateNoOp {
		return false
	}
	switch from {
	case RevisionCandidateReady:
		return to == RevisionCandidateApplying || to == RevisionCandidateDiscarded
	case RevisionCandidateApplying:
		return to == RevisionCandidatePrepared || to == RevisionCandidateNoOp || to == RevisionCandidateReconcileRequired || to == RevisionCandidateDiscarded
	case RevisionCandidateReconcileRequired:
		return to == RevisionCandidatePrepared || to == RevisionCandidateNoOp || to == RevisionCandidateDiscarded
	case RevisionCandidatePrepared:
		return to == RevisionCandidateCommitting || to == RevisionCandidateDiscarded
	case RevisionCandidateCommitting:
		return to == RevisionCandidateCommitted || to == RevisionCandidatePrepared || to == RevisionCandidateReconcileRequired
	default:
		return false
	}
}

func validChangeOperationTransition(from, to ChangeOperationState) bool {
	if from == to || from == ChangeOperationSucceeded || from == ChangeOperationFailed {
		return false
	}
	switch from {
	case ChangeOperationPrepared:
		return to == ChangeOperationRunning || to == ChangeOperationFailed
	case ChangeOperationRunning:
		return to == ChangeOperationSucceeded || to == ChangeOperationFailed || to == ChangeOperationUnknown
	case ChangeOperationUnknown:
		return to == ChangeOperationSucceeded || to == ChangeOperationFailed
	default:
		return false
	}
}

// CreateRevisionCandidate records a mutable checkout only after the caller
// has materialized it from a sealed snapshot. The source run, task version,
// base digest, and candidate write lease are all checked in the same
// transaction that reserves the final revision/run identities.
func (s *Store) CreateRevisionCandidate(ctx context.Context, request CreateRevisionCandidateRequest) (RevisionCandidate, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RevisionCandidate{}, err
	}
	candidate, err := prepareRevisionCandidateCreate(s, request)
	if err != nil {
		return RevisionCandidate{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RevisionCandidate{}, err
	}
	defer tx.Rollback()

	if existing, err := getRevisionCandidateByCommandTx(ctx, tx, candidate.CommandID); err == nil {
		if sameRevisionCandidateCreate(existing, candidate) {
			if err := tx.Commit(); err != nil {
				return RevisionCandidate{}, err
			}
			return existing, nil
		}
		return RevisionCandidate{}, fmt.Errorf("%w: revision candidate command %s", ErrIdempotencyConflict, candidate.CommandID)
	} else if !isNotFound(err) {
		return RevisionCandidate{}, err
	}

	task, err := getTaskV2Tx(ctx, tx, candidate.TaskID)
	if err != nil {
		return RevisionCandidate{}, err
	}
	if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, candidate.CreatedBy, s.now().UTC()); err != nil {
		return RevisionCandidate{}, err
	}
	if task.Version != candidate.ExpectedTaskVersion {
		return RevisionCandidate{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	run, err := getWorkflowRunTx(ctx, tx, candidate.SourceRunID)
	if err != nil {
		return RevisionCandidate{}, err
	}
	if run.TaskID != task.ID || run.RevisionID != candidate.BaseRevisionID {
		return RevisionCandidate{}, fmt.Errorf("revision candidate source run does not bind the requested base revision")
	}
	base, err := getTaskRevisionTx(ctx, tx, candidate.BaseRevisionID)
	if err != nil {
		return RevisionCandidate{}, err
	}
	if base.TaskID != task.ID || base.TaskDigest != candidate.BaseDigest {
		return RevisionCandidate{}, fmt.Errorf("%w: candidate base revision changed", ErrOptimisticLock)
	}
	if _, err := getContinuationCommandTx(ctx, tx, candidate.CommandID); err != nil {
		return RevisionCandidate{}, err
	}
	if candidate.RepairSessionID != "" {
		session, err := getRepairSessionTx(ctx, tx, candidate.RepairSessionID)
		if err != nil {
			return RevisionCandidate{}, err
		}
		if session.Status != RepairSessionOpen || session.SubjectID != candidate.TaskID {
			return RevisionCandidate{}, fmt.Errorf("revision candidate repair session is not open for this task")
		}
		if candidate.RoundOrdinal <= 0 || candidate.RoundOrdinal > session.MaxRounds {
			return RevisionCandidate{}, fmt.Errorf("revision candidate repair round is outside session bounds")
		}
		if candidate.RoundOrdinal == 1 {
			if session.CommandID != candidate.CommandID || session.BaseRevisionID != candidate.BaseRevisionID {
				return RevisionCandidate{}, fmt.Errorf("initial repair candidate does not bind the session root command and revision")
			}
		} else {
			previous, err := getRevisionCandidateByRepairRoundTx(ctx, tx, session.ID, candidate.RoundOrdinal-1)
			if err != nil {
				return RevisionCandidate{}, err
			}
			if previous.State != RevisionCandidateCommitted || previous.TargetRevisionID != candidate.BaseRevisionID || previous.TargetRunID != candidate.SourceRunID {
				return RevisionCandidate{}, fmt.Errorf("repair candidate round %d does not continue the committed prior round", candidate.RoundOrdinal)
			}
		}
	} else if candidate.RoundOrdinal != 0 {
		return RevisionCandidate{}, fmt.Errorf("non-repair revision candidate must use round ordinal zero")
	}
	lease, err := s.validateRevisionCandidateLeaseTx(ctx, tx, candidate, s.now().UTC())
	if err != nil {
		return RevisionCandidate{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO revision_candidates_v8 (
			id, task_id, source_run_id, command_id, repair_session_id, round_ordinal,
			base_revision_id, base_digest, target_revision_id, target_run_id,
			expected_task_version, provider_id, checkout_relpath, findings_json, state,
			after_digest, observed_changes_json, prepared_change_id, mutation_receipt_id,
			frozen_plan_id, final_manifest_id, child_run_manifest_json, lease_id,
			lease_owner, lease_fencing_token, lease_version, created_by, reason,
			created_at, updated_at, retain_until, checkout_tombstoned_at,
			checkout_tombstoned_by, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '[]', NULL, NULL, NULL, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?)
	`, candidate.ID, candidate.TaskID, candidate.SourceRunID, candidate.CommandID, nullableString(candidate.RepairSessionID), candidate.RoundOrdinal,
		candidate.BaseRevisionID, candidate.BaseDigest, candidate.TargetRevisionID, candidate.TargetRunID,
		candidate.ExpectedTaskVersion, candidate.ProviderID, candidate.CheckoutRelpath, candidate.FindingsJSON, candidate.State,
		lease.ID, candidate.LeaseOwner, int64(candidate.LeaseFencingToken), candidate.LeaseVersion, candidate.CreatedBy, candidate.Reason,
		candidate.CreatedAt, candidate.UpdatedAt, candidate.RetainUntil, candidate.Version)
	if err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return RevisionCandidate{}, fmt.Errorf("%w: revision candidate %s", ErrIdentityCollision, candidate.ID)
		}
		return RevisionCandidate{}, err
	}
	if err := reserveCandidateIdentityTx(ctx, tx, candidate.TargetRevisionID, candidate.ID, "task_revision", candidate.CreatedAt); err != nil {
		return RevisionCandidate{}, err
	}
	if err := reserveCandidateIdentityTx(ctx, tx, candidate.TargetRunID, candidate.ID, "workflow_run", candidate.CreatedAt); err != nil {
		return RevisionCandidate{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: candidate.CreatedBy, EntityType: "revision_candidate", EntityID: candidate.ID,
		Action: "revision_candidate.created", Reason: candidate.Reason,
		PayloadJSON:  auditPayload(map[string]any{"task_id": candidate.TaskID, "base_revision_id": candidate.BaseRevisionID, "provider_id": candidate.ProviderID, "lease_id": lease.ID, "target_revision_id": candidate.TargetRevisionID, "target_run_id": candidate.TargetRunID}),
		OperationKey: candidate.CommandID, CreatedAt: candidate.CreatedAt,
	}); err != nil {
		return RevisionCandidate{}, err
	}
	if err := tx.Commit(); err != nil {
		return RevisionCandidate{}, err
	}
	return candidate, nil
}

func prepareRevisionCandidateCreate(s *Store, request CreateRevisionCandidateRequest) (RevisionCandidate, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return RevisionCandidate{}, err
	}
	for field, value := range map[string]string{
		"candidate task ID":            request.TaskID,
		"candidate source run ID":      request.SourceRunID,
		"candidate command ID":         request.CommandID,
		"candidate base revision ID":   request.BaseRevisionID,
		"candidate target revision ID": request.TargetRevisionID,
		"candidate target run ID":      request.TargetRunID,
		"candidate lease ID":           request.LeaseID,
	} {
		if !isUUIDv7(value) {
			return RevisionCandidate{}, fmt.Errorf("%w: %s", ErrInvalidUUIDv7Identity, field)
		}
	}
	if request.TargetRevisionID == request.TargetRunID || request.TargetRevisionID == id || request.TargetRunID == id {
		return RevisionCandidate{}, fmt.Errorf("revision candidate identities must be distinct")
	}
	baseDigest, err := normalizeRequired(request.BaseDigest, "candidate base digest")
	if err != nil {
		return RevisionCandidate{}, err
	}
	if err := ValidateTaskDigestV2(baseDigest); err != nil {
		return RevisionCandidate{}, err
	}
	providerID, err := normalizeRequired(request.ProviderID, "candidate provider ID")
	if err != nil {
		return RevisionCandidate{}, err
	}
	checkout, err := normalizeCandidateCheckoutRelpath(request.CheckoutRelpath)
	if err != nil {
		return RevisionCandidate{}, err
	}
	findings, err := normalizeV4JSON(request.FindingsJSON, "candidate findings")
	if err != nil {
		return RevisionCandidate{}, err
	}
	if request.ExpectedTaskVersion <= 0 || request.LeaseFencingToken == 0 || request.LeaseVersion <= 0 {
		return RevisionCandidate{}, fmt.Errorf("candidate expected task version and lease fence/version must be positive")
	}
	repairSessionID, err := optionalV4ID(request.RepairSessionID, "candidate repair session ID")
	if err != nil {
		return RevisionCandidate{}, err
	}
	now := s.now().UTC()
	return RevisionCandidate{
		ID: id, TaskID: request.TaskID, SourceRunID: request.SourceRunID, CommandID: request.CommandID,
		RepairSessionID: repairSessionID, RoundOrdinal: request.RoundOrdinal, BaseRevisionID: request.BaseRevisionID,
		BaseDigest: baseDigest, TargetRevisionID: request.TargetRevisionID, TargetRunID: request.TargetRunID,
		ExpectedTaskVersion: request.ExpectedTaskVersion, ProviderID: providerID, CheckoutRelpath: checkout,
		FindingsJSON: findings, State: RevisionCandidateReady, LeaseID: request.LeaseID,
		LeaseOwner: strings.TrimSpace(request.LeaseOwner), LeaseFencingToken: request.LeaseFencingToken,
		LeaseVersion: request.LeaseVersion, CreatedBy: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason),
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}, nil
}

func normalizeCandidateCheckoutRelpath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) {
		return "", fmt.Errorf("candidate checkout relative path is required")
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "\\") {
		return "", fmt.Errorf("candidate checkout path escapes managed root")
	}
	return clean, nil
}

func sameRevisionCandidateCreate(left, right RevisionCandidate) bool {
	return left.TaskID == right.TaskID && left.SourceRunID == right.SourceRunID && left.CommandID == right.CommandID &&
		left.RepairSessionID == right.RepairSessionID && left.RoundOrdinal == right.RoundOrdinal &&
		left.BaseRevisionID == right.BaseRevisionID && left.BaseDigest == right.BaseDigest &&
		left.TargetRevisionID == right.TargetRevisionID && left.TargetRunID == right.TargetRunID &&
		left.ExpectedTaskVersion == right.ExpectedTaskVersion && left.ProviderID == right.ProviderID &&
		left.CheckoutRelpath == right.CheckoutRelpath && left.FindingsJSON == right.FindingsJSON &&
		left.LeaseID == right.LeaseID && left.LeaseOwner == right.LeaseOwner &&
		left.LeaseFencingToken == right.LeaseFencingToken && left.LeaseVersion == right.LeaseVersion
}

func (s *Store) GetRevisionCandidate(ctx context.Context, candidateID string) (*RevisionCandidate, error) {
	if !isUUIDv7(candidateID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	candidate, err := scanRevisionCandidate(s.db.QueryRowContext(ctx, revisionCandidateV8Select+" WHERE id = ?", candidateID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &candidate, nil
}

func (s *Store) GetRevisionCandidateByCommand(ctx context.Context, commandID string) (*RevisionCandidate, error) {
	if !isUUIDv7(commandID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	candidate, err := scanRevisionCandidate(s.db.QueryRowContext(ctx, revisionCandidateV8Select+" WHERE command_id = ?", commandID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &candidate, nil
}

func (s *Store) GetRevisionCandidateByTargetRevision(ctx context.Context, revisionID string) (*RevisionCandidate, error) {
	if !isUUIDv7(revisionID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	candidate, err := scanRevisionCandidate(s.db.QueryRowContext(ctx, revisionCandidateV8Select+" WHERE target_revision_id = ?", revisionID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &candidate, nil
}

func (s *Store) ListRevisionCandidatesForTask(ctx context.Context, taskID string) ([]RevisionCandidate, error) {
	if !isUUIDv7(taskID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, revisionCandidateV8Select+" WHERE task_id = ? ORDER BY created_at DESC, id ASC", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []RevisionCandidate
	for rows.Next() {
		candidate, err := scanRevisionCandidate(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Store) CreateChangeOperation(ctx context.Context, request CreateChangeOperationRequest) (ChangeOperation, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ChangeOperation{}, err
	}
	operation, err := prepareChangeOperationCreate(s, request)
	if err != nil {
		return ChangeOperation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChangeOperation{}, err
	}
	defer tx.Rollback()
	candidate, err := getRevisionCandidateTx(ctx, tx, operation.CandidateID)
	if err != nil {
		return ChangeOperation{}, err
	}
	if candidate.ProviderID != operation.ProviderID {
		return ChangeOperation{}, fmt.Errorf("change operation provider does not match revision candidate")
	}
	if candidate.State != RevisionCandidateReady {
		return ChangeOperation{}, fmt.Errorf("%w: revision candidate %s is %s", ErrInvalidTransition, candidate.ID, candidate.State)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO change_operations_v8 (
			id, candidate_id, provider_id, operation_key, payload_json, payload_digest,
			state, receipt_id, created_by, created_at, updated_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?)
	`, operation.ID, operation.CandidateID, operation.ProviderID, operation.OperationKey, operation.PayloadJSON,
		operation.PayloadDigest, operation.State, operation.CreatedBy, operation.CreatedAt, operation.UpdatedAt, operation.Version)
	if err != nil {
		if !isUniqueConstraint(err) {
			return ChangeOperation{}, err
		}
		existing, lookupErr := getChangeOperationByKeyTx(ctx, tx, operation.OperationKey)
		if lookupErr == nil {
			if sameChangeOperationCreate(existing, operation) {
				if err := tx.Commit(); err != nil {
					return ChangeOperation{}, err
				}
				return existing, nil
			}
			return ChangeOperation{}, fmt.Errorf("%w: change operation key %s", ErrIdempotencyConflict, operation.OperationKey)
		}
		if !isNotFound(lookupErr) {
			return ChangeOperation{}, lookupErr
		}
		return ChangeOperation{}, fmt.Errorf("%w: change operation %s", ErrIdentityCollision, operation.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: operation.CreatedBy, EntityType: "change_operation", EntityID: operation.ID,
		Action: "change_operation.reserved", Reason: request.Reason,
		PayloadJSON:  auditPayload(map[string]any{"candidate_id": operation.CandidateID, "provider_id": operation.ProviderID, "operation_key": operation.OperationKey, "payload_digest": operation.PayloadDigest}),
		OperationKey: operation.OperationKey, CreatedAt: operation.CreatedAt}); err != nil {
		return ChangeOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return ChangeOperation{}, err
	}
	return operation, nil
}

func prepareChangeOperationCreate(s *Store, request CreateChangeOperationRequest) (ChangeOperation, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return ChangeOperation{}, err
	}
	if !isUUIDv7(request.CandidateID) {
		return ChangeOperation{}, ErrInvalidUUIDv7Identity
	}
	providerID, err := normalizeRequired(request.ProviderID, "change operation provider ID")
	if err != nil {
		return ChangeOperation{}, err
	}
	key, err := normalizeRequired(request.OperationKey, "change operation key")
	if err != nil {
		return ChangeOperation{}, err
	}
	payload, err := normalizeV4JSON(request.PayloadJSON, "change operation payload")
	if err != nil {
		return ChangeOperation{}, err
	}
	now := s.now().UTC()
	return ChangeOperation{ID: id, CandidateID: request.CandidateID, ProviderID: providerID, OperationKey: key,
		PayloadJSON: payload, PayloadDigest: v4PayloadDigest(payload), State: ChangeOperationPrepared,
		CreatedBy: resolveActor(request.Actor), CreatedAt: now, UpdatedAt: now, Version: 1}, nil
}

func sameChangeOperationCreate(left, right ChangeOperation) bool {
	return left.CandidateID == right.CandidateID && left.ProviderID == right.ProviderID && left.OperationKey == right.OperationKey && left.PayloadJSON == right.PayloadJSON
}

func (s *Store) GetChangeOperation(ctx context.Context, operationID string) (*ChangeOperation, error) {
	if !isUUIDv7(operationID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	operation, err := scanChangeOperation(s.db.QueryRowContext(ctx, changeOperationV8Select+" WHERE id = ?", operationID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func (s *Store) GetChangeOperationByKey(ctx context.Context, key string) (*ChangeOperation, error) {
	key, err := normalizeRequired(key, "change operation key")
	if err != nil {
		return nil, err
	}
	operation, err := scanChangeOperation(s.db.QueryRowContext(ctx, changeOperationV8Select+" WHERE operation_key = ?", key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

// StartChangeOperation moves the durable reservation to running before the
// provider is invoked. A restart observing this state must reconcile the
// candidate and never blindly invoke the provider a second time.
func (s *Store) StartChangeOperation(ctx context.Context, request StartChangeOperationRequest) (ChangeOperation, RevisionCandidate, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	if !isUUIDv7(request.OperationID) || !isUUIDv7(request.LeaseID) {
		return ChangeOperation{}, RevisionCandidate{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 || request.LeaseVersion <= 0 || request.LeaseFencingToken == 0 {
		return ChangeOperation{}, RevisionCandidate{}, fmt.Errorf("change operation expected version and lease fence/version must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	defer tx.Rollback()
	operation, err := getChangeOperationTx(ctx, tx, request.OperationID)
	if err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	candidate, err := getRevisionCandidateTx(ctx, tx, operation.CandidateID)
	if err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	if operation.State == ChangeOperationRunning && candidate.State == RevisionCandidateApplying {
		if err := tx.Commit(); err != nil {
			return ChangeOperation{}, RevisionCandidate{}, err
		}
		return operation, candidate, nil
	}
	if operation.Version != request.ExpectedVersion || operation.State != ChangeOperationPrepared || candidate.State != RevisionCandidateReady {
		return ChangeOperation{}, RevisionCandidate{}, fmt.Errorf("%w: change operation %s", ErrOptimisticLock, operation.ID)
	}
	if _, err := s.validateRevisionCandidateLeaseRequestTx(ctx, tx, candidate, request.LeaseID, request.LeaseOwner, request.LeaseFencingToken, request.LeaseVersion, s.now().UTC()); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	now := s.now().UTC()
	operation.State = ChangeOperationRunning
	operation.UpdatedAt = now
	operation.Version++
	candidate.State = RevisionCandidateApplying
	candidate.UpdatedAt = now
	candidate.Version++
	if err := updateChangeOperationTx(ctx, tx, operation, request.ExpectedVersion); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	if err := updateRevisionCandidateTx(ctx, tx, candidate, candidate.Version-1); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: resolveActor(request.Actor), EntityType: "change_operation", EntityID: operation.ID,
		Action: "change_operation.started", Reason: request.Reason,
		PayloadJSON:  auditPayload(map[string]any{"candidate_id": candidate.ID, "lease_id": request.LeaseID, "fencing_token": request.LeaseFencingToken}),
		OperationKey: operation.OperationKey, CreatedAt: now}); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	if err := tx.Commit(); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	return operation, candidate, nil
}

func (s *Store) MarkChangeOperationUnknown(ctx context.Context, operationID string, expectedVersion int64, actor, reason string) (ChangeOperation, RevisionCandidate, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	if !isUUIDv7(operationID) || expectedVersion <= 0 {
		return ChangeOperation{}, RevisionCandidate{}, ErrInvalidUUIDv7Identity
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	defer tx.Rollback()
	operation, err := getChangeOperationTx(ctx, tx, operationID)
	if err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	candidate, err := getRevisionCandidateTx(ctx, tx, operation.CandidateID)
	if err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	if operation.Version != expectedVersion || operation.State != ChangeOperationRunning || candidate.State != RevisionCandidateApplying {
		return ChangeOperation{}, RevisionCandidate{}, fmt.Errorf("%w: change operation %s", ErrOptimisticLock, operation.ID)
	}
	now := s.now().UTC()
	operation.State = ChangeOperationUnknown
	operation.UpdatedAt = now
	operation.Version++
	candidate.State = RevisionCandidateReconcileRequired
	candidate.UpdatedAt = now
	candidate.Version++
	if err := updateChangeOperationTx(ctx, tx, operation, expectedVersion); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	if err := updateRevisionCandidateTx(ctx, tx, candidate, candidate.Version-1); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: resolveActor(actor), EntityType: "change_operation", EntityID: operation.ID,
		Action: "change_operation.reconcile_required", Reason: reason,
		PayloadJSON: auditPayload(map[string]any{"candidate_id": candidate.ID}), OperationKey: operation.OperationKey, CreatedAt: now}); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	if err := tx.Commit(); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, err
	}
	return operation, candidate, nil
}

// FinalizeChangeOperation atomically turns a verified provider outcome into
// immutable PreparedChange and MutationReceipt facts. It is deliberately the
// first point at which an applied candidate can become plan-eligible.
func (s *Store) FinalizeChangeOperation(ctx context.Context, request FinalizeChangeOperationRequest) (ChangeOperation, RevisionCandidate, PreparedChange, MutationReceipt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	prepared, err := prepareChangeOperationFinalization(s, request)
	if err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	defer tx.Rollback()
	operation, err := getChangeOperationTx(ctx, tx, prepared.operationID)
	if err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	candidate, err := getRevisionCandidateTx(ctx, tx, operation.CandidateID)
	if err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	if operation.State == ChangeOperationSucceeded {
		if operation.ReceiptID != prepared.receiptID {
			return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, fmt.Errorf("%w: change operation %s", ErrIdempotencyConflict, operation.ID)
		}
		change, err := getPreparedChangeTx(ctx, tx, candidate.PreparedChangeID)
		if err != nil {
			return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
		}
		receipt, err := getMutationReceiptTx(ctx, tx, operation.ReceiptID)
		if err != nil {
			return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
		}
		if err := tx.Commit(); err != nil {
			return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
		}
		return operation, candidate, change, receipt, nil
	}
	if operation.Version != prepared.expectedVersion {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, fmt.Errorf("%w: change operation %s", ErrOptimisticLock, operation.ID)
	}
	if operation.State != ChangeOperationRunning && operation.State != ChangeOperationUnknown {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, fmt.Errorf("%w: change operation %s is %s", ErrInvalidTransition, operation.ID, operation.State)
	}
	if _, err := s.validateRevisionCandidateLeaseRequestTx(ctx, tx, candidate, prepared.leaseID, prepared.leaseOwner, prepared.leaseFencing, prepared.leaseVersion, s.now().UTC()); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	if candidate.State != RevisionCandidateApplying && candidate.State != RevisionCandidateReconcileRequired {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, fmt.Errorf("%w: revision candidate %s is %s", ErrInvalidTransition, candidate.ID, candidate.State)
	}
	if prepared.outcome == MutationReceiptApplied && prepared.afterDigest == candidate.BaseDigest {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, fmt.Errorf("applied candidate operation did not change the task digest")
	}
	if prepared.outcome == MutationReceiptNoOp && prepared.afterDigest != candidate.BaseDigest {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, fmt.Errorf("no-op candidate operation changed the task digest")
	}

	now := s.now().UTC()
	change := PreparedChange{
		ID: prepared.preparedChangeID, CommandID: candidate.CommandID, RepairSessionID: candidate.RepairSessionID,
		RoundOrdinal: candidate.RoundOrdinal, ProviderID: operation.ProviderID, OperationKey: operation.OperationKey,
		PayloadJSON: prepared.preparedPayloadJSON, ObservedChangesJSON: prepared.observedChangesJSON,
		BeforeDigest: candidate.BaseDigest, AfterDigest: prepared.afterDigest, CreatedBy: prepared.actor, CreatedAt: now,
	}
	receipt := MutationReceipt{
		ID: prepared.receiptID, PreparedChangeID: change.ID, OperationKey: operation.OperationKey, Outcome: prepared.outcome,
		ReceiptJSON: prepared.receiptJSON, ReceiptDigest: v4PayloadDigest(prepared.receiptJSON), IdempotencyKey: prepared.receiptKey,
		CreatedBy: prepared.actor, CreatedAt: now,
	}
	if err := insertPreparedChangeForCandidateTx(ctx, tx, change); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	if err := insertMutationReceiptForCandidateTx(ctx, tx, receipt); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	operation.State = changeOperationTerminalState(prepared.outcome)
	operation.ReceiptID = receipt.ID
	operation.UpdatedAt = now
	operation.Version++
	candidate.State = candidateStateForReceipt(prepared.outcome)
	candidate.AfterDigest = prepared.afterDigest
	candidate.ObservedChangesJSON = prepared.observedChangesJSON
	candidate.PreparedChangeID = change.ID
	candidate.MutationReceiptID = receipt.ID
	candidate.UpdatedAt = now
	if candidate.State == RevisionCandidateNoOp {
		candidate.RetainUntil = revisionCandidateRetentionDeadline(now)
	}
	candidate.Version++
	if err := updateChangeOperationTx(ctx, tx, operation, prepared.expectedVersion); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	if err := updateRevisionCandidateTx(ctx, tx, candidate, candidate.Version-1); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	if candidate.State == RevisionCandidateNoOp {
		if err := s.releaseRevisionCandidateLeaseTx(ctx, tx, candidate, prepared.actor, prepared.reason, now); err != nil {
			return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
		}
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: prepared.actor, EntityType: "prepared_change", EntityID: change.ID,
		Action: "prepared_change.created", Reason: prepared.reason,
		PayloadJSON:  auditPayload(map[string]any{"candidate_id": candidate.ID, "operation_key": change.OperationKey, "before_digest": change.BeforeDigest, "after_digest": change.AfterDigest}),
		OperationKey: change.OperationKey, CreatedAt: now}); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: prepared.actor, EntityType: "mutation_receipt", EntityID: receipt.ID,
		Action: "mutation_receipt.created", Reason: prepared.reason,
		PayloadJSON:  auditPayload(map[string]any{"candidate_id": candidate.ID, "prepared_change_id": change.ID, "outcome": receipt.Outcome, "receipt_digest": receipt.ReceiptDigest}),
		OperationKey: change.OperationKey, CreatedAt: now}); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: prepared.actor, EntityType: "revision_candidate", EntityID: candidate.ID,
		Action: "revision_candidate.provider_result_recorded", Reason: prepared.reason,
		PayloadJSON:  auditPayload(map[string]any{"operation_id": operation.ID, "state": candidate.State, "outcome": receipt.Outcome}),
		OperationKey: change.OperationKey, CreatedAt: now}); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return ChangeOperation{}, RevisionCandidate{}, PreparedChange{}, MutationReceipt{}, err
	}
	return operation, candidate, change, receipt, nil
}

type preparedChangeOperationFinalization struct {
	operationID         string
	expectedVersion     int64
	leaseID             string
	leaseOwner          string
	leaseFencing        uint64
	leaseVersion        int64
	outcome             MutationReceiptOutcome
	afterDigest         string
	observedChangesJSON string
	preparedChangeID    string
	preparedPayloadJSON string
	receiptID           string
	receiptJSON         string
	receiptKey          string
	actor               string
	reason              string
}

func prepareChangeOperationFinalization(s *Store, request FinalizeChangeOperationRequest) (preparedChangeOperationFinalization, error) {
	if !isUUIDv7(request.OperationID) || !isUUIDv7(request.PreparedChangeID) || !isUUIDv7(request.MutationReceiptID) {
		return preparedChangeOperationFinalization{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 || !validMutationReceiptOutcome(request.Outcome) {
		return preparedChangeOperationFinalization{}, fmt.Errorf("invalid change operation finalization")
	}
	if !isUUIDv7(request.LeaseID) || strings.TrimSpace(request.LeaseOwner) == "" || request.LeaseFencingToken == 0 || request.LeaseVersion <= 0 {
		return preparedChangeOperationFinalization{}, fmt.Errorf("candidate lease fence is incomplete")
	}
	afterDigest, err := normalizeRequired(request.AfterDigest, "candidate after digest")
	if err != nil {
		return preparedChangeOperationFinalization{}, err
	}
	if err := ValidateTaskDigestV2(afterDigest); err != nil {
		return preparedChangeOperationFinalization{}, err
	}
	observed, err := normalizeV4JSON(request.ObservedChangesJSON, "candidate observed changes")
	if err != nil {
		return preparedChangeOperationFinalization{}, err
	}
	payload, err := normalizeV4JSON(request.PreparedChangePayloadJSON, "prepared change payload")
	if err != nil {
		return preparedChangeOperationFinalization{}, err
	}
	receipt, err := normalizeV4JSON(request.MutationReceiptJSON, "mutation receipt payload")
	if err != nil {
		return preparedChangeOperationFinalization{}, err
	}
	key, err := normalizeRequired(request.MutationReceiptKey, "mutation receipt idempotency key")
	if err != nil {
		return preparedChangeOperationFinalization{}, err
	}
	return preparedChangeOperationFinalization{
		operationID: request.OperationID, expectedVersion: request.ExpectedVersion, leaseID: request.LeaseID,
		leaseOwner: strings.TrimSpace(request.LeaseOwner), leaseFencing: request.LeaseFencingToken, leaseVersion: request.LeaseVersion,
		outcome: request.Outcome, afterDigest: afterDigest, observedChangesJSON: observed,
		preparedChangeID: request.PreparedChangeID, preparedPayloadJSON: payload, receiptID: request.MutationReceiptID,
		receiptJSON: receipt, receiptKey: key, actor: resolveActor(request.Actor), reason: strings.TrimSpace(request.Reason),
	}, nil
}

func changeOperationTerminalState(outcome MutationReceiptOutcome) ChangeOperationState {
	if outcome == MutationReceiptUncertain {
		return ChangeOperationUnknown
	}
	if outcome == MutationReceiptFailed {
		return ChangeOperationFailed
	}
	return ChangeOperationSucceeded
}

func candidateStateForReceipt(outcome MutationReceiptOutcome) RevisionCandidateState {
	switch outcome {
	case MutationReceiptApplied:
		return RevisionCandidatePrepared
	case MutationReceiptNoOp:
		return RevisionCandidateNoOp
	default:
		return RevisionCandidateReconcileRequired
	}
}

func insertPreparedChangeForCandidateTx(ctx context.Context, tx *sql.Tx, change PreparedChange) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO prepared_changes_v4 (
			id, command_id, repair_session_id, round_ordinal, provider_id, operation_key,
			payload_json, observed_changes_json, before_digest, after_digest, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, change.ID, change.CommandID, nullableString(change.RepairSessionID), change.RoundOrdinal, change.ProviderID,
		change.OperationKey, change.PayloadJSON, change.ObservedChangesJSON, change.BeforeDigest, change.AfterDigest,
		change.CreatedBy, change.CreatedAt)
	if err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return fmt.Errorf("%w: prepared change %s", ErrIdentityCollision, change.ID)
		}
	}
	return err
}

func insertMutationReceiptForCandidateTx(ctx context.Context, tx *sql.Tx, receipt MutationReceipt) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO mutation_receipts_v4 (
			id, prepared_change_id, operation_key, outcome, receipt_json, receipt_digest,
			supersedes_receipt_id, idempotency_key, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?)
	`, receipt.ID, receipt.PreparedChangeID, receipt.OperationKey, receipt.Outcome, receipt.ReceiptJSON,
		receipt.ReceiptDigest, receipt.IdempotencyKey, receipt.CreatedBy, receipt.CreatedAt)
	if err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return fmt.Errorf("%w: mutation receipt %s", ErrIdentityCollision, receipt.ID)
		}
	}
	return err
}

// CreateAndBindRevisionCandidatePlan is the crash-safe boundary between a
// provider-verified candidate and an executable continuation. The plan row and
// candidate binding become visible together. An already-persisted incomplete
// binding is irreconcilable through this API: it must be investigated rather
// than silently repaired or made executable.
func (s *Store) CreateAndBindRevisionCandidatePlan(ctx context.Context, request CreateAndBindRevisionCandidatePlanRequest) (FrozenPlan, RevisionCandidate, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return FrozenPlan{}, RevisionCandidate{}, err
	}
	plan, err := s.prepareFrozenPlan(request.Plan)
	if err != nil {
		return FrozenPlan{}, RevisionCandidate{}, err
	}
	if !isUUIDv7(request.CandidateID) {
		return FrozenPlan{}, RevisionCandidate{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedCandidateVersion <= 0 {
		return FrozenPlan{}, RevisionCandidate{}, fmt.Errorf("expected candidate version must be positive")
	}
	manifestID, err := normalizeRequired(request.FinalManifestID, "candidate final manifest ID")
	if err != nil {
		return FrozenPlan{}, RevisionCandidate{}, err
	}
	childManifest, err := normalizeV4JSON(request.ChildRunManifestJSON, "candidate child run manifest")
	if err != nil {
		return FrozenPlan{}, RevisionCandidate{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FrozenPlan{}, RevisionCandidate{}, err
	}
	defer tx.Rollback()

	candidate, err := getRevisionCandidateTx(ctx, tx, request.CandidateID)
	if err != nil {
		return FrozenPlan{}, RevisionCandidate{}, err
	}
	if candidate.CommandID != plan.CommandID {
		return FrozenPlan{}, RevisionCandidate{}, fmt.Errorf("frozen plan command does not own revision candidate")
	}
	storedPlan, err := getFrozenPlanByCommandTx(ctx, tx, plan.CommandID)
	if err != nil {
		if !isNotFound(err) {
			return FrozenPlan{}, RevisionCandidate{}, err
		}
		if revisionCandidatePlanBindingStarted(candidate) {
			return FrozenPlan{}, RevisionCandidate{}, fmt.Errorf("%w: revision candidate %s has an incomplete persisted plan binding", ErrContinuationReconciliationRequired, candidate.ID)
		}
		if candidate.Version != request.ExpectedCandidateVersion || candidate.State != RevisionCandidatePrepared {
			return FrozenPlan{}, RevisionCandidate{}, fmt.Errorf("%w: revision candidate %s", ErrOptimisticLock, candidate.ID)
		}
		if err := validateFrozenPlanDependenciesTx(ctx, tx, plan); err != nil {
			return FrozenPlan{}, RevisionCandidate{}, err
		}
		if err := validateRevisionCandidatePlanFactsTx(ctx, tx, candidate, plan); err != nil {
			return FrozenPlan{}, RevisionCandidate{}, err
		}
		if err := insertFrozenPlanTx(ctx, tx, plan); err != nil {
			if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
				return FrozenPlan{}, RevisionCandidate{}, fmt.Errorf("%w: frozen plan %s", ErrIdentityCollision, plan.ID)
			}
			return FrozenPlan{}, RevisionCandidate{}, err
		}
		if err := s.appendFrozenPlanAuditTx(ctx, tx, plan, request.Actor, request.Reason); err != nil {
			return FrozenPlan{}, RevisionCandidate{}, err
		}
		storedPlan = plan
	} else {
		if !sameFrozenPlan(storedPlan, plan) {
			return FrozenPlan{}, RevisionCandidate{}, fmt.Errorf("%w: frozen plan command %s", ErrIdempotencyConflict, plan.CommandID)
		}
		if !revisionCandidatePlanBindingComplete(candidate) || candidate.FrozenPlanID != storedPlan.ID {
			return FrozenPlan{}, RevisionCandidate{}, fmt.Errorf("%w: frozen plan %s is not fully bound to revision candidate %s", ErrContinuationReconciliationRequired, storedPlan.ID, candidate.ID)
		}
		if err := validateRevisionCandidatePlanFactsTx(ctx, tx, candidate, storedPlan); err != nil {
			return FrozenPlan{}, RevisionCandidate{}, err
		}
		if candidate.FinalManifestID != manifestID || candidate.ChildRunManifestJSON != childManifest {
			return FrozenPlan{}, RevisionCandidate{}, fmt.Errorf("%w: revision candidate %s", ErrIdempotencyConflict, candidate.ID)
		}
		if err := tx.Commit(); err != nil {
			return FrozenPlan{}, RevisionCandidate{}, err
		}
		return storedPlan, candidate, nil
	}

	now := s.now().UTC()
	candidate.FrozenPlanID = storedPlan.ID
	candidate.FinalManifestID = manifestID
	candidate.ChildRunManifestJSON = childManifest
	candidate.UpdatedAt = now
	candidate.Version++
	if err := updateRevisionCandidateTx(ctx, tx, candidate, request.ExpectedCandidateVersion); err != nil {
		return FrozenPlan{}, RevisionCandidate{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: resolveActor(request.Actor), EntityType: "revision_candidate", EntityID: candidate.ID,
		Action: "revision_candidate.plan_bound", Reason: request.Reason,
		PayloadJSON:  auditPayload(map[string]any{"plan_id": storedPlan.ID, "manifest_id": manifestID, "target_run_id": candidate.TargetRunID}),
		OperationKey: storedPlan.ID, CreatedAt: now}); err != nil {
		return FrozenPlan{}, RevisionCandidate{}, err
	}
	if err := tx.Commit(); err != nil {
		return FrozenPlan{}, RevisionCandidate{}, err
	}
	return storedPlan, candidate, nil
}

func revisionCandidatePlanBindingComplete(candidate RevisionCandidate) bool {
	return candidate.FrozenPlanID != "" && candidate.FinalManifestID != "" && candidate.ChildRunManifestJSON != ""
}

func revisionCandidatePlanBindingStarted(candidate RevisionCandidate) bool {
	return candidate.FrozenPlanID != "" || candidate.FinalManifestID != "" || candidate.ChildRunManifestJSON != ""
}

func validateRevisionCandidatePlanFactsTx(ctx context.Context, tx *sql.Tx, candidate RevisionCandidate, plan FrozenPlan) error {
	if plan.CommandID != candidate.CommandID || plan.PreparedChangeID != candidate.PreparedChangeID ||
		plan.SubjectID != candidate.TaskID || plan.SubjectRevisionID != candidate.TargetRevisionID || plan.SubjectDigest != candidate.AfterDigest {
		return fmt.Errorf("frozen plan does not bind revision candidate facts")
	}
	sourceRun, err := getWorkflowRunTx(ctx, tx, candidate.SourceRunID)
	if err != nil {
		return err
	}
	if sourceRun.DefinitionHash != plan.WorkflowFingerprint {
		return fmt.Errorf("frozen plan workflow does not bind revision candidate source run")
	}
	return nil
}

// DiscardRevisionCandidate ends a noncommitted candidate under CAS and
// releases its task-write lease. Its identity reservations intentionally stay
// registered forever: discarding a candidate must not make a UUIDv7 identity
// available to a different lifecycle entity later.
func (s *Store) DiscardRevisionCandidate(ctx context.Context, request DiscardRevisionCandidateRequest) (RevisionCandidate, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RevisionCandidate{}, err
	}
	if !isUUIDv7(request.CandidateID) {
		return RevisionCandidate{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return RevisionCandidate{}, fmt.Errorf("expected candidate version must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RevisionCandidate{}, err
	}
	defer tx.Rollback()
	candidate, err := getRevisionCandidateTx(ctx, tx, request.CandidateID)
	if err != nil {
		return RevisionCandidate{}, err
	}
	if candidate.State == RevisionCandidateDiscarded {
		if err := tx.Commit(); err != nil {
			return RevisionCandidate{}, err
		}
		return candidate, nil
	}
	if candidate.Version != request.ExpectedVersion {
		return RevisionCandidate{}, fmt.Errorf("%w: revision candidate %s", ErrOptimisticLock, candidate.ID)
	}
	switch candidate.State {
	case RevisionCandidateReady, RevisionCandidatePrepared, RevisionCandidateNoOp, RevisionCandidateReconcileRequired:
	default:
		return RevisionCandidate{}, fmt.Errorf("%w: revision candidate %s is %s", ErrInvalidTransition, candidate.ID, candidate.State)
	}
	now := s.now().UTC()
	candidate.State = RevisionCandidateDiscarded
	candidate.UpdatedAt = now
	candidate.RetainUntil = revisionCandidateRetentionDeadline(now)
	candidate.Version++
	if err := updateRevisionCandidateTx(ctx, tx, candidate, request.ExpectedVersion); err != nil {
		return RevisionCandidate{}, err
	}
	if err := s.releaseRevisionCandidateLeaseTx(ctx, tx, candidate, resolveActor(request.Actor), request.Reason, now); err != nil {
		return RevisionCandidate{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: resolveActor(request.Actor), EntityType: "revision_candidate", EntityID: candidate.ID,
		Action: "revision_candidate.discarded", Reason: request.Reason,
		PayloadJSON: auditPayload(map[string]any{"target_revision_id": candidate.TargetRevisionID, "target_run_id": candidate.TargetRunID}), CreatedAt: now}); err != nil {
		return RevisionCandidate{}, err
	}
	if err := tx.Commit(); err != nil {
		return RevisionCandidate{}, err
	}
	return candidate, nil
}

// ExpireRevisionCandidate releases a prepared plan whose immutable TTL has
// elapsed. The candidate/evidence remains queryable for retention, but it no
// longer occupies the one active revision-write slot for its Task.
func (s *Store) ExpireRevisionCandidate(ctx context.Context, request ExpireRevisionCandidateRequest) (RevisionCandidate, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RevisionCandidate{}, err
	}
	if !isUUIDv7(request.CandidateID) {
		return RevisionCandidate{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return RevisionCandidate{}, fmt.Errorf("expected candidate version must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RevisionCandidate{}, err
	}
	defer tx.Rollback()
	candidate, err := getRevisionCandidateTx(ctx, tx, request.CandidateID)
	if err != nil {
		return RevisionCandidate{}, err
	}
	if candidate.State == RevisionCandidateDiscarded {
		if err := tx.Commit(); err != nil {
			return RevisionCandidate{}, err
		}
		return candidate, nil
	}
	if candidate.Version != request.ExpectedVersion || candidate.State != RevisionCandidatePrepared {
		return RevisionCandidate{}, fmt.Errorf("%w: revision candidate %s", ErrOptimisticLock, candidate.ID)
	}
	plan, err := getFrozenPlanByCommandTx(ctx, tx, candidate.CommandID)
	if err != nil {
		return RevisionCandidate{}, err
	}
	if candidate.FrozenPlanID != "" && candidate.FrozenPlanID != plan.ID {
		return RevisionCandidate{}, fmt.Errorf("%w: revision candidate %s plan binding conflicts with command", ErrInvalidTransition, candidate.ID)
	}
	if err := validateRevisionCandidatePlanFactsTx(ctx, tx, candidate, plan); err != nil {
		return RevisionCandidate{}, err
	}
	now := s.now().UTC()
	if now.Before(plan.ExpiresAt) {
		return RevisionCandidate{}, fmt.Errorf("%w: frozen plan %s has not expired", ErrInvalidTransition, plan.ID)
	}
	candidate.State = RevisionCandidateDiscarded
	candidate.UpdatedAt = now
	candidate.RetainUntil = revisionCandidateRetentionDeadline(now)
	candidate.Version++
	if err := updateRevisionCandidateTx(ctx, tx, candidate, request.ExpectedVersion); err != nil {
		return RevisionCandidate{}, err
	}
	if err := s.releaseRevisionCandidateLeaseTx(ctx, tx, candidate, resolveActor(request.Actor), request.Reason, now); err != nil {
		return RevisionCandidate{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: resolveActor(request.Actor), EntityType: "revision_candidate", EntityID: candidate.ID,
		Action: "revision_candidate.expired", Reason: request.Reason,
		PayloadJSON: auditPayload(map[string]any{"plan_id": plan.ID, "expires_at": plan.ExpiresAt, "state": RevisionCandidateDiscarded}), CreatedAt: now}); err != nil {
		return RevisionCandidate{}, err
	}
	if err := tx.Commit(); err != nil {
		return RevisionCandidate{}, err
	}
	return candidate, nil
}

// CommitRevisionCandidateContinuation consumes one prepared candidate into a
// sealed child revision and queues its child workflow run. The filesystem
// snapshot and run manifest are deliberately prewritten by the application;
// this transaction is the authoritative point at which either becomes live.
func (s *Store) CommitRevisionCandidateContinuation(ctx context.Context, request CommitRevisionCandidateContinuationRequest) (RevisionCandidateContinuationCommit, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	prepared, err := prepareRevisionCandidateContinuationCommit(s, request)
	if err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	defer tx.Rollback()

	if existing, err := getContinuationExecutionByKeyTx(ctx, tx, prepared.idempotencyKey); err == nil {
		if existing.PlanID != prepared.planID || existing.PayloadJSON != prepared.payloadJSON {
			return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: revision candidate execution key %s", ErrIdempotencyConflict, prepared.idempotencyKey)
		}
		candidate, err := getRevisionCandidateTx(ctx, tx, prepared.candidateID)
		if err != nil {
			return RevisionCandidateContinuationCommit{}, err
		}
		revision, err := getTaskRevisionTx(ctx, tx, candidate.TargetRevisionID)
		if err != nil {
			return RevisionCandidateContinuationCommit{}, err
		}
		run, err := getWorkflowRunTx(ctx, tx, existing.RunID)
		if err != nil {
			return RevisionCandidateContinuationCommit{}, err
		}
		job, err := getDurableJobByIdempotencyTx(ctx, tx, prepared.jobKey)
		if err != nil {
			return RevisionCandidateContinuationCommit{}, err
		}
		if err := s.verifyCandidateChildRunInputsTx(ctx, tx, prepared.childRunInputs, run, revision); err != nil {
			return RevisionCandidateContinuationCommit{}, err
		}
		if err := tx.Commit(); err != nil {
			return RevisionCandidateContinuationCommit{}, err
		}
		return RevisionCandidateContinuationCommit{Candidate: candidate, Revision: revision, Run: run, Execution: existing, Job: job}, nil
	} else if !isNotFound(err) {
		return RevisionCandidateContinuationCommit{}, err
	}

	plan, err := getFrozenPlanTx(ctx, tx, prepared.planID)
	if err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	now := s.now().UTC()
	if !plan.ExpiresAt.After(now) {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: %s", ErrContinuationPlanExpired, plan.ID)
	}
	var snapshot workflowkit.ContinuationPlanSnapshot
	if err := decodeStrictContinuationSnapshot(plan.PayloadJSON, &snapshot); err != nil {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("decode candidate continuation plan %s: %w", plan.ID, err)
	}
	candidate, err := getRevisionCandidateTx(ctx, tx, prepared.candidateID)
	if err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if candidate.State != RevisionCandidatePrepared || candidate.FrozenPlanID != plan.ID || candidate.PreparedChangeID == "" || candidate.FinalManifestID == "" || candidate.ChildRunManifestJSON == "" {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: revision candidate %s is not commit-ready", ErrInvalidTransition, candidate.ID)
	}
	if snapshot.PlanID != plan.ID || snapshot.Strategy != workflowkit.StrategyReviseSubject || snapshot.TargetRunRelation != workflowkit.RelationChildRun ||
		snapshot.PreparedChangeID != candidate.PreparedChangeID || snapshot.SubjectRevisionID != candidate.TargetRevisionID ||
		string(snapshot.SubjectDigest) != candidate.AfterDigest || snapshot.SourceRunID != candidate.SourceRunID {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("frozen plan does not match candidate commit facts")
	}
	if plan.PreparedChangeID != candidate.PreparedChangeID || plan.SubjectRevisionID != candidate.TargetRevisionID || plan.SubjectDigest != candidate.AfterDigest {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("stored frozen plan fields do not match candidate")
	}
	sourceRun, err := getWorkflowRunTx(ctx, tx, candidate.SourceRunID)
	if err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if err := verifyControlCheckpointTx(ctx, tx, sourceRun, prepared.expected); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if sourceRun.Status == WorkflowRunInDoubt {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: workflow run %s is in_doubt", ErrContinuationReconciliationRequired, sourceRun.ID)
	}
	if err := continuationRunIsReconciledTx(ctx, tx, sourceRun.ID); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if snapshot.NextExecutionEpoch != sourceRun.ExecutionEpoch+1 {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: revision candidate plan execution epoch is stale", ErrOptimisticLock)
	}
	task, err := getTaskV2Tx(ctx, tx, candidate.TaskID)
	if err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, prepared.actor, now); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if task.Version != candidate.ExpectedTaskVersion || prepared.expected.SubjectVersion != task.Version {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	base, err := getTaskRevisionTx(ctx, tx, candidate.BaseRevisionID)
	if err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if base.TaskID != task.ID || base.TaskDigest != candidate.BaseDigest {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: candidate base revision changed", ErrOptimisticLock)
	}
	change, err := getPreparedChangeTx(ctx, tx, candidate.PreparedChangeID)
	if err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if change.CommandID != candidate.CommandID || change.BeforeDigest != candidate.BaseDigest || change.AfterDigest != candidate.AfterDigest {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("prepared change does not match candidate digests")
	}
	receipt, err := getMutationReceiptTx(ctx, tx, candidate.MutationReceiptID)
	if err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if receipt.PreparedChangeID != change.ID || receipt.Outcome != MutationReceiptApplied {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("candidate requires an applied mutation receipt")
	}
	if err := consumeCandidateIdentityReservationTx(ctx, tx, candidate.TargetRevisionID, candidate.ID, "task_revision"); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if err := consumeCandidateIdentityReservationTx(ctx, tx, candidate.TargetRunID, candidate.ID, "workflow_run"); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	var revisionNumber int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_number), 0) + 1 FROM task_revisions WHERE task_id = ?`, task.ID).Scan(&revisionNumber); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	origin := RevisionOriginManual
	if candidate.RepairSessionID != "" {
		origin = RevisionOriginRepair
	}
	revision := TaskRevision{
		ID: candidate.TargetRevisionID, TaskID: task.ID, VersionNumber: revisionNumber, ParentRevisionID: base.ID,
		Origin: origin, TaskDigest: candidate.AfterDigest, ProposalDigest: base.ProposalDigest, ManifestID: candidate.FinalManifestID,
		State: RevisionStateSealed, StateVersion: 1, StateUpdatedBy: prepared.actor, StateUpdatedAt: now,
		ChangeSummary: "candidate " + candidate.ID + " via " + candidate.ProviderID, MetadataJSON: base.MetadataJSON,
		CreatedBy: prepared.actor, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_revisions (
			id, task_id, version_number, parent_revision_id, origin, task_digest, proposal_digest,
			manifest_id, state, validation_evidence_manifest, state_version, state_updated_by,
			state_updated_at, change_summary, metadata_json, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?)
	`, revision.ID, revision.TaskID, revision.VersionNumber, revision.ParentRevisionID, revision.Origin, revision.TaskDigest,
		revision.ProposalDigest, revision.ManifestID, revision.State, revision.StateVersion, revision.StateUpdatedBy,
		revision.StateUpdatedAt, revision.ChangeSummary, revision.MetadataJSON, revision.CreatedBy, revision.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: candidate task revision %s", ErrIdentityCollision, revision.ID)
		}
		return RevisionCandidateContinuationCommit{}, err
	}
	run := WorkflowRun{
		ID: candidate.TargetRunID, TaskID: task.ID, RevisionID: revision.ID,
		WorkflowTemplateID: sourceRun.WorkflowTemplateID, WorkflowTemplateVersion: sourceRun.WorkflowTemplateVersion,
		ResolvedProfileHash: sourceRun.ResolvedProfileHash, DefinitionHash: sourceRun.DefinitionHash,
		RunManifestJSON: candidate.ChildRunManifestJSON, ParentRunID: sourceRun.ID, Trigger: "continue",
		ExecutionEpoch: snapshot.NextExecutionEpoch, Status: WorkflowRunQueued, CreatedBy: prepared.actor, CreatedAt: now, Version: 1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_runs (
			id, task_id, revision_id, workflow_template_id, workflow_template_version, resolved_profile_hash,
			definition_hash, run_manifest_json, parent_run_id, trigger, execution_epoch, status, created_by,
			created_at, started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, run.ID, run.TaskID, run.RevisionID, run.WorkflowTemplateID, run.WorkflowTemplateVersion, run.ResolvedProfileHash,
		run.DefinitionHash, run.RunManifestJSON, run.ParentRunID, run.Trigger, run.ExecutionEpoch, run.Status,
		run.CreatedBy, run.CreatedAt, run.Version); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: candidate workflow run %s", ErrIdentityCollision, run.ID)
		}
		return RevisionCandidateContinuationCommit{}, err
	}
	if err := s.insertCandidateChildRunInputsTx(ctx, tx, prepared.childRunInputs, run, revision, now, prepared.actor, prepared.reason); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	previousRunVersion := sourceRun.Version
	sourceRun.ExecutionEpoch = snapshot.NextExecutionEpoch
	sourceRun.Version++
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET execution_epoch = ?, version = ? WHERE id = ? AND version = ? AND execution_epoch = ?`,
		sourceRun.ExecutionEpoch, sourceRun.Version, sourceRun.ID, previousRunVersion, prepared.expected.ExecutionEpoch)
	if err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return RevisionCandidateContinuationCommit{}, err
		}
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, sourceRun.ID)
	}
	if _, err := getDurableJobByIdempotencyTx(ctx, tx, prepared.jobKey); err == nil {
		return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: durable job key %s", ErrIdempotencyConflict, prepared.jobKey)
	} else if !isNotFound(err) {
		return RevisionCandidateContinuationCommit{}, err
	}
	execution := ContinuationExecution{ID: prepared.executionID, PlanID: plan.ID, RunID: run.ID, IdempotencyKey: prepared.idempotencyKey,
		State: ContinuationExecutionQueued, PayloadJSON: prepared.payloadJSON, CreatedBy: prepared.actor, CreatedAt: now, UpdatedAt: now, Version: 1}
	job := DurableJob{ID: prepared.jobID, CommandType: "task_continuation.execute", EntityType: "continuation_execution", EntityID: execution.ID,
		RunID: run.ID, State: JobQueued, Priority: prepared.priority, PayloadJSON: execution.PayloadJSON, IdempotencyKey: prepared.jobKey,
		CreatedBy: prepared.actor, CreatedAt: now, UpdatedAt: now, Version: 1}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO continuation_executions_v4 (
			id, plan_id, parent_execution_id, run_id, idempotency_key, state, payload_json,
			created_by, created_at, updated_at, finished_at, version
		) VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
	`, execution.ID, execution.PlanID, execution.RunID, execution.IdempotencyKey, execution.State, execution.PayloadJSON,
		execution.CreatedBy, execution.CreatedAt, execution.UpdatedAt, execution.Version); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: candidate continuation execution %s", ErrIdentityCollision, execution.ID)
		}
		return RevisionCandidateContinuationCommit{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (
			id, command_type, entity_type, entity_id, run_id, stage_attempt_id, state, priority,
			payload_json, idempotency_key, created_by, created_at, updated_at, started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, job.ID, job.CommandType, job.EntityType, job.EntityID, job.RunID, job.State, job.Priority, job.PayloadJSON,
		job.IdempotencyKey, job.CreatedBy, job.CreatedAt, job.UpdatedAt, job.Version); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return RevisionCandidateContinuationCommit{}, fmt.Errorf("%w: candidate continuation job %s", ErrIdentityCollision, job.ID)
		}
		return RevisionCandidateContinuationCommit{}, err
	}
	candidate.State = RevisionCandidateCommitted
	candidate.UpdatedAt = now
	candidate.Version++
	if err := updateRevisionCandidateTx(ctx, tx, candidate, candidate.Version-1); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if err := s.releaseRevisionCandidateLeaseTx(ctx, tx, candidate, prepared.actor, prepared.reason, now); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	if err := s.appendV5OutboxTx(ctx, tx, "revision_candidate.continuation_queued", "continuation_execution", execution.ID,
		prepared.idempotencyKey+":queued", auditPayload(map[string]any{"plan_id": plan.ID, "candidate_id": candidate.ID, "run_id": run.ID, "job_id": job.ID}), now); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	for _, event := range []AuditEvent{
		{Actor: prepared.actor, EntityType: "task_revision", EntityID: revision.ID, Action: "task_revision.created_from_candidate", Reason: prepared.reason, PayloadJSON: auditPayload(map[string]any{"candidate_id": candidate.ID, "task_id": task.ID, "task_digest": revision.TaskDigest}), OperationKey: prepared.idempotencyKey, CreatedAt: now},
		{Actor: prepared.actor, EntityType: "workflow_run", EntityID: run.ID, Action: "workflow_run.created_from_candidate", Reason: prepared.reason, PayloadJSON: auditPayload(map[string]any{"candidate_id": candidate.ID, "parent_run_id": sourceRun.ID}), OperationKey: prepared.idempotencyKey, CreatedAt: now},
		{Actor: prepared.actor, EntityType: "continuation_execution", EntityID: execution.ID, Action: "continuation_execution.committed", Reason: prepared.reason, PayloadJSON: auditPayload(map[string]any{"plan_id": plan.ID, "candidate_id": candidate.ID, "job_id": job.ID}), OperationKey: prepared.idempotencyKey, CreatedAt: now},
	} {
		if _, err := s.appendAuditTx(ctx, tx, event); err != nil {
			return RevisionCandidateContinuationCommit{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RevisionCandidateContinuationCommit{}, err
	}
	return RevisionCandidateContinuationCommit{Candidate: candidate, Revision: revision, Run: run, Execution: execution, Job: job}, nil
}

// insertCandidateChildRunInputsTx makes intrinsic subject inputs live at the
// same durable boundary as the target revision and Run. Object publication is
// intentionally completed by the application before this transaction; this
// method never fabricates an object or a synthetic stage producer.
func (s *Store) insertCandidateChildRunInputsTx(ctx context.Context, tx *sql.Tx, requests []CreateRunInputArtifactRequest, run WorkflowRun, revision TaskRevision, now time.Time, actor, reason string) error {
	inputs, err := prepareInitialWorkflowRunInputArtifacts(s, requests, run, now)
	if err != nil {
		return err
	}
	for _, input := range inputs {
		if err := validateRunInputArtifactSubjectTx(ctx, tx, input); err != nil {
			return err
		}
		if _, _, err := s.insertRunInputArtifactTx(ctx, tx, input, actor, reason); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) verifyCandidateChildRunInputsTx(ctx context.Context, tx *sql.Tx, requests []CreateRunInputArtifactRequest, run WorkflowRun, revision TaskRevision) error {
	expectedInputs, err := prepareInitialWorkflowRunInputArtifacts(s, requests, run, run.CreatedAt)
	if err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_input_artifacts WHERE run_id = ?`, run.ID).Scan(&count); err != nil {
		return err
	}
	if count != len(expectedInputs) {
		return fmt.Errorf("%w: candidate child Run input count does not match replay", ErrIdempotencyConflict)
	}
	for _, input := range expectedInputs {
		stored, err := getRunInputArtifactForPortTx(ctx, tx, run.ID, input.Port)
		if err != nil {
			return err
		}
		if stored.ID != input.ID || stored.TaskID != run.TaskID || stored.RevisionID != revision.ID || stored.RevisionDigest != revision.TaskDigest ||
			stored.ContentDigest != input.ContentDigest || stored.SchemaVersion != input.SchemaVersion || stored.SizeBytes != input.SizeBytes {
			return fmt.Errorf("%w: candidate child Run input %q does not match replay", ErrIdempotencyConflict, input.Port)
		}
	}
	return nil
}

type preparedRevisionCandidateContinuationCommit struct {
	executionID    string
	planID         string
	candidateID    string
	idempotencyKey string
	payloadJSON    string
	expected       ControlCheckpointRef
	childRunInputs []CreateRunInputArtifactRequest
	actor          string
	reason         string
	priority       int
	jobID          string
	jobKey         string
}

func prepareRevisionCandidateContinuationCommit(s *Store, request CommitRevisionCandidateContinuationRequest) (preparedRevisionCandidateContinuationCommit, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedRevisionCandidateContinuationCommit{}, err
	}
	if !isUUIDv7(request.PlanID) || !isUUIDv7(request.CandidateID) {
		return preparedRevisionCandidateContinuationCommit{}, ErrInvalidUUIDv7Identity
	}
	key, err := normalizeRequired(request.IdempotencyKey, "candidate continuation execution idempotency key")
	if err != nil {
		return preparedRevisionCandidateContinuationCommit{}, err
	}
	payload, err := normalizeV4JSON(request.PayloadJSON, "candidate continuation execution payload")
	if err != nil {
		return preparedRevisionCandidateContinuationCommit{}, err
	}
	if err := validateControlCheckpoint(request.Expected); err != nil {
		return preparedRevisionCandidateContinuationCommit{}, err
	}
	actor, err := normalizeRequired(request.Actor, "candidate continuation execution actor")
	if err != nil {
		return preparedRevisionCandidateContinuationCommit{}, err
	}
	reason, err := normalizeRequired(request.Reason, "candidate continuation execution reason")
	if err != nil {
		return preparedRevisionCandidateContinuationCommit{}, err
	}
	jobID, err := s.newV2ID("")
	if err != nil {
		return preparedRevisionCandidateContinuationCommit{}, err
	}
	childRunInputs := append([]CreateRunInputArtifactRequest(nil), request.ChildRunInputs...)
	return preparedRevisionCandidateContinuationCommit{executionID: id, planID: request.PlanID, candidateID: request.CandidateID,
		idempotencyKey: key, payloadJSON: payload, expected: request.Expected, actor: actor, reason: reason,
		childRunInputs: childRunInputs, priority: request.Priority, jobID: jobID, jobKey: "candidate-continuation-job:" + key}, nil
}

func decodeStrictContinuationSnapshot(raw string, destination any) error {
	// Frozen plan payloads are validated by workflowkit before storage. This
	// narrow decoder is intentionally strict as a second protection at the
	// store's cross-entity commit boundary.
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func consumeCandidateIdentityReservationTx(ctx context.Context, tx *sql.Tx, reservedID, candidateID, intendedType string) error {
	var owner, actualType string
	err := tx.QueryRowContext(ctx, `
		SELECT reservation.candidate_id, reservation.intended_type
		FROM revision_candidate_identity_reservations_v8 AS reservation
		WHERE reservation.reserved_id = ?
	`, reservedID).Scan(&owner, &actualType)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: candidate identity reservation %s", ErrNotFound, reservedID)
	}
	if err != nil {
		return err
	}
	if owner != candidateID || actualType != intendedType {
		return fmt.Errorf("%w: candidate identity reservation %s", ErrFencingToken, reservedID)
	}
	registryType := "reserved_" + intendedType
	result, err := tx.ExecContext(ctx, `DELETE FROM entity_id_registry WHERE id = ? AND entity_type = ?`, reservedID, registryType)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: candidate identity registry reservation %s", ErrIdentityCollision, reservedID)
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM revision_candidate_identity_reservations_v8 WHERE reserved_id = ? AND candidate_id = ? AND intended_type = ?`, reservedID, candidateID, intendedType)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: candidate identity reservation %s", ErrOptimisticLock, reservedID)
	}
	return nil
}

func (s *Store) releaseRevisionCandidateLeaseTx(ctx context.Context, tx *sql.Tx, candidate RevisionCandidate, actor, reason string, now time.Time) error {
	if candidate.LeaseID == "" {
		return nil
	}
	lease, err := getLeaseTx(ctx, tx, candidate.LeaseID)
	if err != nil {
		return err
	}
	if lease.State != LeaseActive {
		return nil
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE leases SET state = 'released', updated_at = ?, version = version + 1
		WHERE id = ? AND state = 'active' AND fencing_token = ?
	`, now, lease.ID, int64(candidate.LeaseFencingToken))
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: candidate lease %s", ErrOptimisticLock, lease.ID)
	}
	_, err = s.appendAuditTx(ctx, tx, AuditEvent{Actor: actor, EntityType: "lease", EntityID: lease.ID,
		Action: "lease.released_for_revision_candidate", Reason: reason,
		PayloadJSON: auditPayload(map[string]any{"candidate_id": candidate.ID, "task_id": candidate.TaskID, "fencing_token": candidate.LeaseFencingToken}), CreatedAt: now})
	return err
}

func scanRevisionCandidate(scanner rowScanner) (RevisionCandidate, error) {
	var candidate RevisionCandidate
	var repairSessionID, preparedChangeID, receiptID, planID, leaseID sql.NullString
	var retainUntil, checkoutTombstonedAt sql.NullTime
	var fencing int64
	if err := scanner.Scan(
		&candidate.ID, &candidate.TaskID, &candidate.SourceRunID, &candidate.CommandID, &repairSessionID,
		&candidate.RoundOrdinal, &candidate.BaseRevisionID, &candidate.BaseDigest, &candidate.TargetRevisionID,
		&candidate.TargetRunID, &candidate.ExpectedTaskVersion, &candidate.ProviderID, &candidate.CheckoutRelpath,
		&candidate.FindingsJSON, &candidate.State, &candidate.AfterDigest, &candidate.ObservedChangesJSON,
		&preparedChangeID, &receiptID, &planID, &candidate.FinalManifestID, &candidate.ChildRunManifestJSON,
		&leaseID, &candidate.LeaseOwner, &fencing, &candidate.LeaseVersion, &candidate.CreatedBy, &candidate.Reason,
		&candidate.CreatedAt, &candidate.UpdatedAt, &retainUntil, &checkoutTombstonedAt,
		&candidate.CheckoutTombstonedBy, &candidate.Version,
	); err != nil {
		return RevisionCandidate{}, err
	}
	if !validRevisionCandidateState(candidate.State) || fencing < 0 {
		return RevisionCandidate{}, fmt.Errorf("invalid revision candidate %s", candidate.ID)
	}
	candidate.RepairSessionID = nullableStringValue(repairSessionID)
	candidate.PreparedChangeID = nullableStringValue(preparedChangeID)
	candidate.MutationReceiptID = nullableStringValue(receiptID)
	candidate.FrozenPlanID = nullableStringValue(planID)
	candidate.LeaseID = nullableStringValue(leaseID)
	candidate.LeaseFencingToken = uint64(fencing)
	candidate.CreatedAt = candidate.CreatedAt.UTC()
	candidate.UpdatedAt = candidate.UpdatedAt.UTC()
	candidate.RetainUntil = nullableTimePtr(retainUntil)
	candidate.CheckoutTombstonedAt = nullableTimePtr(checkoutTombstonedAt)
	return candidate, nil
}

func scanChangeOperation(scanner rowScanner) (ChangeOperation, error) {
	var operation ChangeOperation
	var receiptID sql.NullString
	if err := scanner.Scan(&operation.ID, &operation.CandidateID, &operation.ProviderID, &operation.OperationKey,
		&operation.PayloadJSON, &operation.PayloadDigest, &operation.State, &receiptID, &operation.CreatedBy,
		&operation.CreatedAt, &operation.UpdatedAt, &operation.Version); err != nil {
		return ChangeOperation{}, err
	}
	if !validChangeOperationState(operation.State) {
		return ChangeOperation{}, fmt.Errorf("invalid change operation %s", operation.ID)
	}
	operation.ReceiptID = nullableStringValue(receiptID)
	operation.CreatedAt = operation.CreatedAt.UTC()
	operation.UpdatedAt = operation.UpdatedAt.UTC()
	return operation, nil
}

func getRevisionCandidateTx(ctx context.Context, tx *sql.Tx, candidateID string) (RevisionCandidate, error) {
	candidate, err := scanRevisionCandidate(tx.QueryRowContext(ctx, revisionCandidateV8Select+" WHERE id = ?", candidateID))
	if err == sql.ErrNoRows {
		return RevisionCandidate{}, fmt.Errorf("%w: revision candidate %s", ErrNotFound, candidateID)
	}
	return candidate, err
}

func getRevisionCandidateByCommandTx(ctx context.Context, tx *sql.Tx, commandID string) (RevisionCandidate, error) {
	candidate, err := scanRevisionCandidate(tx.QueryRowContext(ctx, revisionCandidateV8Select+" WHERE command_id = ?", commandID))
	if err == sql.ErrNoRows {
		return RevisionCandidate{}, fmt.Errorf("%w: revision candidate command %s", ErrNotFound, commandID)
	}
	return candidate, err
}

func getRevisionCandidateByRepairRoundTx(ctx context.Context, tx *sql.Tx, repairSessionID string, roundOrdinal int) (RevisionCandidate, error) {
	candidate, err := scanRevisionCandidate(tx.QueryRowContext(ctx, revisionCandidateV8Select+" WHERE repair_session_id = ? AND round_ordinal = ?", repairSessionID, roundOrdinal))
	if err == sql.ErrNoRows {
		return RevisionCandidate{}, fmt.Errorf("%w: repair session %s round %d candidate", ErrNotFound, repairSessionID, roundOrdinal)
	}
	return candidate, err
}

func getChangeOperationTx(ctx context.Context, tx *sql.Tx, operationID string) (ChangeOperation, error) {
	operation, err := scanChangeOperation(tx.QueryRowContext(ctx, changeOperationV8Select+" WHERE id = ?", operationID))
	if err == sql.ErrNoRows {
		return ChangeOperation{}, fmt.Errorf("%w: change operation %s", ErrNotFound, operationID)
	}
	return operation, err
}

func getChangeOperationByKeyTx(ctx context.Context, tx *sql.Tx, key string) (ChangeOperation, error) {
	operation, err := scanChangeOperation(tx.QueryRowContext(ctx, changeOperationV8Select+" WHERE operation_key = ?", key))
	if err == sql.ErrNoRows {
		return ChangeOperation{}, fmt.Errorf("%w: change operation key %s", ErrNotFound, key)
	}
	return operation, err
}

func reserveCandidateIdentityTx(ctx context.Context, tx *sql.Tx, reservedID, candidateID, intendedType string, now time.Time) error {
	if !isUUIDv7(reservedID) {
		return ErrInvalidUUIDv7Identity
	}
	registryType := "reserved_" + intendedType
	if _, err := tx.ExecContext(ctx, `INSERT INTO entity_id_registry (id, entity_type) VALUES (?, ?)`, reservedID, registryType); err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: reserved %s %s", ErrIdentityCollision, intendedType, reservedID)
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO revision_candidate_identity_reservations_v8 (reserved_id, candidate_id, intended_type, created_at)
		VALUES (?, ?, ?, ?)
	`, reservedID, candidateID, intendedType, now); err != nil {
		return err
	}
	return nil
}

func (s *Store) validateRevisionCandidateLeaseTx(ctx context.Context, tx *sql.Tx, candidate RevisionCandidate, now time.Time) (Lease, error) {
	return s.validateRevisionCandidateLeaseRequestTx(ctx, tx, candidate, candidate.LeaseID, candidate.LeaseOwner, candidate.LeaseFencingToken, candidate.LeaseVersion, now)
}

func (s *Store) validateRevisionCandidateLeaseRequestTx(ctx context.Context, tx *sql.Tx, candidate RevisionCandidate, leaseID, owner string, fencing uint64, version int64, now time.Time) (Lease, error) {
	lease, err := getLeaseTx(ctx, tx, leaseID)
	if err != nil {
		return Lease{}, err
	}
	if lease.ID != candidate.LeaseID || lease.Owner != candidate.LeaseOwner || lease.FencingToken != candidate.LeaseFencingToken {
		return Lease{}, fmt.Errorf("%w: candidate lease identity %s", ErrFencingToken, candidate.LeaseID)
	}
	if lease.ResourceType != "task_revision_candidate" || lease.ResourceID != candidate.TaskID || lease.State != LeaseActive ||
		lease.Owner != strings.TrimSpace(owner) || lease.FencingToken != fencing || lease.Version != version {
		return Lease{}, fmt.Errorf("%w: candidate lease %s", ErrFencingToken, lease.ID)
	}
	if !lease.ExpiresAt.After(now) {
		if err := s.expireLeaseTx(ctx, tx, lease, candidate.CreatedBy, now, "candidate operation observed expired lease"); err != nil {
			return Lease{}, err
		}
		return Lease{}, fmt.Errorf("%w: candidate lease %s expired", ErrLeaseHeld, lease.ID)
	}
	return lease, nil
}

func updateRevisionCandidateTx(ctx context.Context, tx *sql.Tx, candidate RevisionCandidate, expectedVersion int64) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE revision_candidates_v8
		SET state = ?, after_digest = ?, observed_changes_json = ?, prepared_change_id = ?,
			mutation_receipt_id = ?, frozen_plan_id = ?, final_manifest_id = ?, child_run_manifest_json = ?,
			updated_at = ?, retain_until = ?, checkout_tombstoned_at = ?,
			checkout_tombstoned_by = ?, version = ?
		WHERE id = ? AND version = ?
	`, candidate.State, candidate.AfterDigest, candidate.ObservedChangesJSON, nullableString(candidate.PreparedChangeID),
		nullableString(candidate.MutationReceiptID), nullableString(candidate.FrozenPlanID), candidate.FinalManifestID,
		candidate.ChildRunManifestJSON, candidate.UpdatedAt, candidate.RetainUntil,
		candidate.CheckoutTombstonedAt, candidate.CheckoutTombstonedBy, candidate.Version,
		candidate.ID, expectedVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: revision candidate %s", ErrOptimisticLock, candidate.ID)
	}
	return nil
}

func updateChangeOperationTx(ctx context.Context, tx *sql.Tx, operation ChangeOperation, expectedVersion int64) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE change_operations_v8 SET state = ?, receipt_id = ?, updated_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, operation.State, nullableString(operation.ReceiptID), operation.UpdatedAt, operation.Version, operation.ID, expectedVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: change operation %s", ErrOptimisticLock, operation.ID)
	}
	return nil
}
