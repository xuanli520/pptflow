package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	authoringRepairLedgerAuditEntityType = "authoring_repair_ledger"
	authoringRepairLedgerOpenedAction    = "authoring_repair.opened"
	authoringRepairLedgerResolvedAction  = "authoring_repair.resolved"
	authoringRepairLedgerFormat          = "harbor.authoring-repair-ledger.v1"
	authoringRepairLedgerVersion         = "1"
)

// AuthoringRepairFindingKind is a closed classification for a repair that
// invalidates one producer under an immutable authoring contract.
type AuthoringRepairFindingKind string

const (
	AuthoringRepairFindingArtifactInvalid       AuthoringRepairFindingKind = "artifact_invalid"
	AuthoringRepairFindingTaskProposalInvalid   AuthoringRepairFindingKind = "task_proposal_invalid"
	AuthoringRepairFindingSourceAnalysisInvalid AuthoringRepairFindingKind = "source_analysis_invalid"
	AuthoringRepairFindingPackageInvalid        AuthoringRepairFindingKind = "package_invalid"
)

// AuthoringRepairLedgerState is derived from immutable open and resolution
// events. Ledger records are never updated in place.
type AuthoringRepairLedgerState string

const (
	AuthoringRepairLedgerOpen     AuthoringRepairLedgerState = "open"
	AuthoringRepairLedgerResolved AuthoringRepairLedgerState = "resolved"
)

// AuthoringRepairLedgerEntry is one typed repair requirement. EvidenceDigest
// identifies immutable reviewer or validator evidence, not untrusted prose.
type AuthoringRepairLedgerEntry struct {
	ID             string
	RunID          string
	ContractDigest string
	TargetProducer string
	FindingKind    AuthoringRepairFindingKind
	Reason         string
	EvidenceDigest string
	State          AuthoringRepairLedgerState
	CreatedBy      string
	CreatedAt      time.Time
	Resolution     *AuthoringRepairLedgerResolution
}

// AuthoringRepairLedgerResolution explicitly closes one repair against a
// later validated artifact from the affected producer.
type AuthoringRepairLedgerResolution struct {
	ID                    string
	RepairID              string
	RunID                 string
	ContractDigest        string
	Producer              string
	SupersedingArtifactID string
	SupersedingAttemptID  string
	Reason                string
	ResolvedBy            string
	ResolvedAt            time.Time
}

type OpenAuthoringRepairLedgerEntryRequest struct {
	ID             string
	RunID          string
	ContractDigest string
	TargetProducer string
	FindingKind    AuthoringRepairFindingKind
	Reason         string
	EvidenceDigest string
	Actor          string
}

type ResolveAuthoringRepairLedgerEntryRequest struct {
	ID                    string
	RepairID              string
	RunID                 string
	ContractDigest        string
	Producer              string
	SupersedingArtifactID string
	SupersedingAttemptID  string
	Reason                string
	Actor                 string
}

type authoringRepairLedgerOpenPayload struct {
	Format         string                     `json:"format"`
	Version        string                     `json:"version"`
	RepairID       string                     `json:"repair_id"`
	ContractDigest string                     `json:"contract_digest"`
	TargetProducer string                     `json:"target_producer"`
	FindingKind    AuthoringRepairFindingKind `json:"finding_kind"`
	EvidenceDigest string                     `json:"evidence_digest"`
}

type authoringRepairLedgerResolutionPayload struct {
	Format               string `json:"format"`
	Version              string `json:"version"`
	RepairID             string `json:"repair_id"`
	ContractDigest       string `json:"contract_digest"`
	Producer             string `json:"producer"`
	SupersedingArtifact  string `json:"superseding_artifact_id"`
	SupersedingAttemptID string `json:"superseding_attempt_id"`
}

type preparedAuthoringRepairLedgerOpen struct {
	id, runID, contractDigest, targetProducer, reason, evidenceDigest, actor string
	findingKind                                                              AuthoringRepairFindingKind
}

type preparedAuthoringRepairLedgerResolution struct {
	id, repairID, runID, contractDigest, producer, artifactID, attemptID, reason, actor string
}

// OpenAuthoringRepairLedgerEntry appends one immutable repair requirement.
// Reusing its ID is accepted only when every immutable fact is identical.
func (s *Store) OpenAuthoringRepairLedgerEntry(ctx context.Context, request OpenAuthoringRepairLedgerEntryRequest) (AuthoringRepairLedgerEntry, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	prepared, err := prepareAuthoringRepairLedgerOpen(s, request)
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	defer tx.Rollback()
	if _, err := getWorkflowRunTx(ctx, tx, prepared.runID); err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	entries, err := listAuthoringRepairLedgerEntriesTx(ctx, tx, prepared.runID)
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	for _, entry := range entries {
		if entry.ID != prepared.id {
			continue
		}
		if samePreparedAuthoringRepairLedgerOpen(entry, prepared) {
			if err := tx.Commit(); err != nil {
				return AuthoringRepairLedgerEntry{}, err
			}
			return entry, nil
		}
		return AuthoringRepairLedgerEntry{}, fmt.Errorf("%w: authoring repair ledger entry %s", ErrIdempotencyConflict, prepared.id)
	}
	now := s.now().UTC()
	payload := authoringRepairLedgerOpenPayload{
		Format: authoringRepairLedgerFormat, Version: authoringRepairLedgerVersion, RepairID: prepared.id,
		ContractDigest: prepared.contractDigest, TargetProducer: prepared.targetProducer,
		FindingKind: prepared.findingKind, EvidenceDigest: prepared.evidenceDigest,
	}
	event, err := s.appendAuditTx(ctx, tx, AuditEvent{
		ID: prepared.id, Actor: prepared.actor, EntityType: authoringRepairLedgerAuditEntityType, EntityID: prepared.runID,
		Action: authoringRepairLedgerOpenedAction, Reason: prepared.reason, PayloadJSON: auditPayload(payload),
		OperationKey: "authoring-repair:" + prepared.id, CreatedAt: now,
	})
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	return authoringRepairLedgerEntryFromOpenEvent(event, payload)
}

// ResolveAuthoringRepairLedgerEntry appends the sole resolution event for an
// open repair. The referenced artifact must belong to a completed pass attempt
// of the repair's target producer in the same Run and its frozen input
// manifest must explicitly carry the immutable evidence digest that opened the
// repair. A later, unrelated producer retry cannot close an entry.
func (s *Store) ResolveAuthoringRepairLedgerEntry(ctx context.Context, request ResolveAuthoringRepairLedgerEntryRequest) (AuthoringRepairLedgerEntry, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	prepared, err := prepareAuthoringRepairLedgerResolution(s, request)
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	defer tx.Rollback()
	entries, err := listAuthoringRepairLedgerEntriesTx(ctx, tx, prepared.runID)
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	var target *AuthoringRepairLedgerEntry
	for index := range entries {
		if entries[index].ID == prepared.repairID {
			target = &entries[index]
			break
		}
	}
	if target == nil {
		return AuthoringRepairLedgerEntry{}, fmt.Errorf("%w: authoring repair ledger entry %s", ErrNotFound, prepared.repairID)
	}
	if target.ContractDigest != prepared.contractDigest || target.TargetProducer != prepared.producer {
		return AuthoringRepairLedgerEntry{}, fmt.Errorf("%w: authoring repair resolution does not match the open repair", ErrImmutable)
	}
	if target.Resolution != nil {
		if samePreparedAuthoringRepairLedgerResolution(*target.Resolution, prepared) {
			if err := tx.Commit(); err != nil {
				return AuthoringRepairLedgerEntry{}, err
			}
			return *target, nil
		}
		return AuthoringRepairLedgerEntry{}, fmt.Errorf("%w: authoring repair ledger entry %s is already resolved", ErrImmutable, target.ID)
	}
	var accepted int
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM artifact_refs_v4 AS artifact
			JOIN stage_attempts AS attempt ON attempt.id = artifact.attempt_id
			WHERE artifact.id = ?
			  AND artifact.run_id = ?
			  AND artifact.stage_key = ?
			  AND artifact.attempt_id = ?
			  AND attempt.run_id = artifact.run_id
			  AND attempt.stage_key = artifact.stage_key
			  AND attempt.execution_status = ?
			  AND attempt.verdict = ?
			  AND EXISTS (
				  SELECT 1
				  FROM json_each(artifact.input_bindings_json) AS input
				  WHERE json_extract(input.value, '$.content_digest') = ?
			  )
		)
	`, prepared.artifactID, prepared.runID, prepared.producer, prepared.attemptID, StageExecutionCompleted, VerdictPass, target.EvidenceDigest).Scan(&accepted)
	if err != nil {
		return AuthoringRepairLedgerEntry{}, fmt.Errorf("verify authoring repair superseding artifact: %w", err)
	}
	if accepted == 0 {
		return AuthoringRepairLedgerEntry{}, fmt.Errorf("%w: authoring repair resolution requires a validated artifact from producer %s", ErrInvalidTransition, prepared.producer)
	}
	now := s.now().UTC()
	payload := authoringRepairLedgerResolutionPayload{
		Format: authoringRepairLedgerFormat, Version: authoringRepairLedgerVersion, RepairID: prepared.repairID,
		ContractDigest: prepared.contractDigest, Producer: prepared.producer,
		SupersedingArtifact: prepared.artifactID, SupersedingAttemptID: prepared.attemptID,
	}
	event, err := s.appendAuditTx(ctx, tx, AuditEvent{
		ID: prepared.id, Actor: prepared.actor, EntityType: authoringRepairLedgerAuditEntityType, EntityID: prepared.runID,
		Action: authoringRepairLedgerResolvedAction, Reason: prepared.reason, PayloadJSON: auditPayload(payload),
		OperationKey: "authoring-repair-resolution:" + prepared.repairID, CreatedAt: now,
	})
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	resolution, err := authoringRepairLedgerResolutionFromEvent(event, payload)
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	target.State = AuthoringRepairLedgerResolved
	target.Resolution = &resolution
	if err := tx.Commit(); err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	return *target, nil
}

// ListAuthoringRepairLedgerEntries replays the append-only repair ledger for
// one Run. Malformed or contradictory ledger events fail closed.
func (s *Store) ListAuthoringRepairLedgerEntries(ctx context.Context, runID string) ([]AuthoringRepairLedgerEntry, error) {
	if !isUUIDv7(runID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	return listAuthoringRepairLedgerEntriesTx(ctx, s.db, runID)
}

// ListOpenAuthoringRepairLedgerEntries returns only requirements that still
// block final materialization.
func (s *Store) ListOpenAuthoringRepairLedgerEntries(ctx context.Context, runID string) ([]AuthoringRepairLedgerEntry, error) {
	entries, err := s.ListAuthoringRepairLedgerEntries(ctx, runID)
	if err != nil {
		return nil, err
	}
	open := make([]AuthoringRepairLedgerEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.State == AuthoringRepairLedgerOpen {
			open = append(open, entry)
		}
	}
	return open, nil
}

func prepareAuthoringRepairLedgerOpen(s *Store, request OpenAuthoringRepairLedgerEntryRequest) (preparedAuthoringRepairLedgerOpen, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedAuthoringRepairLedgerOpen{}, err
	}
	if !isUUIDv7(request.RunID) {
		return preparedAuthoringRepairLedgerOpen{}, ErrInvalidUUIDv7Identity
	}
	contractDigest, err := normalizeAuthoringSHA256(request.ContractDigest, "authoring repair contract digest")
	if err != nil {
		return preparedAuthoringRepairLedgerOpen{}, err
	}
	targetProducer, err := normalizeAuthoringRepairProducer(request.TargetProducer)
	if err != nil {
		return preparedAuthoringRepairLedgerOpen{}, err
	}
	findingKind, err := normalizeAuthoringRepairFindingKind(request.FindingKind)
	if err != nil {
		return preparedAuthoringRepairLedgerOpen{}, err
	}
	reason, err := normalizeRequired(request.Reason, "authoring repair reason")
	if err != nil {
		return preparedAuthoringRepairLedgerOpen{}, err
	}
	evidenceDigest, err := normalizeAuthoringSHA256(request.EvidenceDigest, "authoring repair evidence digest")
	if err != nil {
		return preparedAuthoringRepairLedgerOpen{}, err
	}
	return preparedAuthoringRepairLedgerOpen{
		id: id, runID: request.RunID, contractDigest: contractDigest, targetProducer: targetProducer,
		findingKind: findingKind, reason: reason, evidenceDigest: evidenceDigest, actor: resolveActor(request.Actor),
	}, nil
}

func prepareAuthoringRepairLedgerResolution(s *Store, request ResolveAuthoringRepairLedgerEntryRequest) (preparedAuthoringRepairLedgerResolution, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedAuthoringRepairLedgerResolution{}, err
	}
	if !isUUIDv7(request.RepairID) || !isUUIDv7(request.RunID) || !isUUIDv7(request.SupersedingArtifactID) || !isUUIDv7(request.SupersedingAttemptID) {
		return preparedAuthoringRepairLedgerResolution{}, ErrInvalidUUIDv7Identity
	}
	contractDigest, err := normalizeAuthoringSHA256(request.ContractDigest, "authoring repair resolution contract digest")
	if err != nil {
		return preparedAuthoringRepairLedgerResolution{}, err
	}
	producer, err := normalizeAuthoringRepairProducer(request.Producer)
	if err != nil {
		return preparedAuthoringRepairLedgerResolution{}, err
	}
	reason, err := normalizeRequired(request.Reason, "authoring repair resolution reason")
	if err != nil {
		return preparedAuthoringRepairLedgerResolution{}, err
	}
	return preparedAuthoringRepairLedgerResolution{
		id: id, repairID: request.RepairID, runID: request.RunID, contractDigest: contractDigest,
		producer: producer, artifactID: request.SupersedingArtifactID, attemptID: request.SupersedingAttemptID,
		reason: reason, actor: resolveActor(request.Actor),
	}, nil
}

func listAuthoringRepairLedgerEntriesTx(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, runID string) ([]AuthoringRepairLedgerEntry, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT id, actor, entity_type, entity_id, action, reason, payload_json, operation_key, created_at
		FROM audit_events
		WHERE entity_type = ? AND entity_id = ?
		ORDER BY created_at ASC, id ASC
	`, authoringRepairLedgerAuditEntityType, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := make(map[string]AuthoringRepairLedgerEntry)
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(&event.ID, &event.Actor, &event.EntityType, &event.EntityID, &event.Action, &event.Reason, &event.PayloadJSON, &event.OperationKey, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.CreatedAt = event.CreatedAt.UTC()
		switch event.Action {
		case authoringRepairLedgerOpenedAction:
			var payload authoringRepairLedgerOpenPayload
			if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
				return nil, fmt.Errorf("decode authoring repair open event %s: %w", event.ID, err)
			}
			entry, err := authoringRepairLedgerEntryFromOpenEvent(event, payload)
			if err != nil {
				return nil, err
			}
			if _, duplicate := byID[entry.ID]; duplicate {
				return nil, fmt.Errorf("%w: duplicate authoring repair ledger entry %s", ErrImmutable, entry.ID)
			}
			byID[entry.ID] = entry
		case authoringRepairLedgerResolvedAction:
			var payload authoringRepairLedgerResolutionPayload
			if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
				return nil, fmt.Errorf("decode authoring repair resolution event %s: %w", event.ID, err)
			}
			resolution, err := authoringRepairLedgerResolutionFromEvent(event, payload)
			if err != nil {
				return nil, err
			}
			entry, present := byID[resolution.RepairID]
			if !present || entry.RunID != runID || entry.ContractDigest != resolution.ContractDigest || entry.TargetProducer != resolution.Producer || entry.Resolution != nil {
				return nil, fmt.Errorf("%w: invalid authoring repair resolution for %s", ErrImmutable, resolution.RepairID)
			}
			entry.State = AuthoringRepairLedgerResolved
			entry.Resolution = &resolution
			byID[entry.ID] = entry
		default:
			return nil, fmt.Errorf("%w: unsupported authoring repair ledger action %q", ErrImmutable, event.Action)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	entries := make([]AuthoringRepairLedgerEntry, 0, len(byID))
	for _, entry := range byID {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].CreatedAt.Equal(entries[right].CreatedAt) {
			return entries[left].ID < entries[right].ID
		}
		return entries[left].CreatedAt.Before(entries[right].CreatedAt)
	})
	return entries, nil
}

func authoringRepairLedgerEntryFromOpenEvent(event AuditEvent, payload authoringRepairLedgerOpenPayload) (AuthoringRepairLedgerEntry, error) {
	if event.EntityType != authoringRepairLedgerAuditEntityType || event.Action != authoringRepairLedgerOpenedAction ||
		payload.Format != authoringRepairLedgerFormat || payload.Version != authoringRepairLedgerVersion || payload.RepairID != event.ID || !isUUIDv7(event.ID) || !isUUIDv7(event.EntityID) {
		return AuthoringRepairLedgerEntry{}, fmt.Errorf("%w: invalid authoring repair open event", ErrImmutable)
	}
	contractDigest, err := normalizeAuthoringSHA256(payload.ContractDigest, "authoring repair contract digest")
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	targetProducer, err := normalizeAuthoringRepairProducer(payload.TargetProducer)
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	findingKind, err := normalizeAuthoringRepairFindingKind(payload.FindingKind)
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	evidenceDigest, err := normalizeAuthoringSHA256(payload.EvidenceDigest, "authoring repair evidence digest")
	if err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	if _, err := normalizeRequired(event.Reason, "authoring repair reason"); err != nil {
		return AuthoringRepairLedgerEntry{}, err
	}
	return AuthoringRepairLedgerEntry{
		ID: event.ID, RunID: event.EntityID, ContractDigest: contractDigest, TargetProducer: targetProducer,
		FindingKind: findingKind, Reason: event.Reason, EvidenceDigest: evidenceDigest,
		State: AuthoringRepairLedgerOpen, CreatedBy: event.Actor, CreatedAt: event.CreatedAt.UTC(),
	}, nil
}

func authoringRepairLedgerResolutionFromEvent(event AuditEvent, payload authoringRepairLedgerResolutionPayload) (AuthoringRepairLedgerResolution, error) {
	if event.EntityType != authoringRepairLedgerAuditEntityType || event.Action != authoringRepairLedgerResolvedAction ||
		payload.Format != authoringRepairLedgerFormat || payload.Version != authoringRepairLedgerVersion || !isUUIDv7(event.ID) ||
		!isUUIDv7(event.EntityID) || !isUUIDv7(payload.RepairID) || !isUUIDv7(payload.SupersedingArtifact) || !isUUIDv7(payload.SupersedingAttemptID) {
		return AuthoringRepairLedgerResolution{}, fmt.Errorf("%w: invalid authoring repair resolution event", ErrImmutable)
	}
	contractDigest, err := normalizeAuthoringSHA256(payload.ContractDigest, "authoring repair resolution contract digest")
	if err != nil {
		return AuthoringRepairLedgerResolution{}, err
	}
	producer, err := normalizeAuthoringRepairProducer(payload.Producer)
	if err != nil {
		return AuthoringRepairLedgerResolution{}, err
	}
	if _, err := normalizeRequired(event.Reason, "authoring repair resolution reason"); err != nil {
		return AuthoringRepairLedgerResolution{}, err
	}
	return AuthoringRepairLedgerResolution{
		ID: event.ID, RepairID: payload.RepairID, RunID: event.EntityID, ContractDigest: contractDigest,
		Producer: producer, SupersedingArtifactID: payload.SupersedingArtifact, SupersedingAttemptID: payload.SupersedingAttemptID,
		Reason: event.Reason, ResolvedBy: event.Actor, ResolvedAt: event.CreatedAt.UTC(),
	}, nil
}

func normalizeAuthoringRepairProducer(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 96 {
		return "", fmt.Errorf("authoring repair target producer is required")
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') && character != '_' && character != '-' {
			return "", fmt.Errorf("authoring repair target producer %q is invalid", value)
		}
	}
	return value, nil
}

func normalizeAuthoringRepairFindingKind(value AuthoringRepairFindingKind) (AuthoringRepairFindingKind, error) {
	switch value {
	case AuthoringRepairFindingArtifactInvalid, AuthoringRepairFindingTaskProposalInvalid,
		AuthoringRepairFindingSourceAnalysisInvalid, AuthoringRepairFindingPackageInvalid:
		return value, nil
	default:
		return "", fmt.Errorf("authoring repair finding kind %q is invalid", value)
	}
}

func samePreparedAuthoringRepairLedgerOpen(entry AuthoringRepairLedgerEntry, prepared preparedAuthoringRepairLedgerOpen) bool {
	return entry.ID == prepared.id && entry.RunID == prepared.runID && entry.ContractDigest == prepared.contractDigest &&
		entry.TargetProducer == prepared.targetProducer && entry.FindingKind == prepared.findingKind &&
		entry.Reason == prepared.reason && entry.EvidenceDigest == prepared.evidenceDigest && entry.CreatedBy == prepared.actor
}

func samePreparedAuthoringRepairLedgerResolution(resolution AuthoringRepairLedgerResolution, prepared preparedAuthoringRepairLedgerResolution) bool {
	return resolution.ID == prepared.id && resolution.RepairID == prepared.repairID && resolution.RunID == prepared.runID &&
		resolution.ContractDigest == prepared.contractDigest && resolution.Producer == prepared.producer &&
		resolution.SupersedingArtifactID == prepared.artifactID && resolution.SupersedingAttemptID == prepared.attemptID &&
		resolution.Reason == prepared.reason && resolution.ResolvedBy == prepared.actor
}
