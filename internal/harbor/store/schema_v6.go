package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// migrationV6 makes UUIDv7 entity identities globally unique across the V2
// lifecycle control plane. The registry intentionally retains an ID after its
// source row is deleted so an identity can never be reused.
const migrationV6 = `
CREATE TABLE IF NOT EXISTS entity_id_registry (
    id            TEXT PRIMARY KEY,
    entity_type   TEXT NOT NULL,
    registered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

const globalIdentityCollisionMessage = "global entity identity collision"

type globalIdentitySource struct {
	table      string
	entityType string
}

// Every V2-V5 table that owns a UUIDv7 entity identity belongs here. Foreign
// keys and generic subject references are deliberately excluded: they point
// at an existing entity rather than allocate another identity.
var globalIdentitySources = []globalIdentitySource{
	{table: "tasks_v2", entityType: "task"},
	{table: "task_revisions", entityType: "task_revision"},
	{table: "workspaces_v2", entityType: "workspace"},
	{table: "workflow_runs", entityType: "workflow_run"},
	{table: "run_attempts", entityType: "run_attempt"},
	{table: "stage_attempts", entityType: "stage_attempt"},
	{table: "node_attempts", entityType: "node_attempt"},
	{table: "turn_checkpoints", entityType: "turn_checkpoint"},
	{table: "review_requests", entityType: "review_request"},
	{table: "review_decisions", entityType: "review_decision"},
	{table: "quota_policies", entityType: "quota_policy"},
	{table: "quota_accounts", entityType: "quota_account"},
	{table: "quota_ledger_entries", entityType: "quota_ledger_entry"},
	{table: "quota_leases", entityType: "quota_lease"},
	{table: "usage_events", entityType: "usage_event"},
	{table: "budget_settlements", entityType: "budget_settlement"},
	{table: "budget_grants", entityType: "budget_grant"},
	{table: "jobs", entityType: "job"},
	{table: "leases", entityType: "lease"},
	{table: "audit_events", entityType: "audit_event"},
	{table: "outbox_events", entityType: "outbox_event"},
	{table: "releases", entityType: "release"},
	{table: "deletion_records", entityType: "deletion_record"},
	{table: "artifact_manifests_v4", entityType: "artifact_manifest"},
	{table: "artifact_refs_v4", entityType: "artifact_ref"},
	{table: "continuation_commands_v4", entityType: "continuation_command"},
	{table: "repair_sessions_v4", entityType: "repair_session"},
	{table: "prepared_changes_v4", entityType: "prepared_change"},
	{table: "mutation_receipts_v4", entityType: "mutation_receipt"},
	{table: "frozen_plans_v4", entityType: "frozen_plan"},
	{table: "continuation_executions_v4", entityType: "continuation_execution"},
	{table: "quota_accounts_v5", entityType: "quota_account_v5"},
	{table: "quota_leases_v5", entityType: "quota_lease_v5"},
	{table: "quota_usage_events_v5", entityType: "quota_usage_event_v5"},
	{table: "quota_heartbeats_v5", entityType: "quota_heartbeat_v5"},
	{table: "quota_settlements_v5", entityType: "quota_settlement_v5"},
	{table: "quota_budget_grants_v5", entityType: "quota_budget_grant_v5"},
	{table: "admission_decisions_v5", entityType: "admission_decision_v5"},
	{table: "control_operations_v5", entityType: "control_operation"},
	{table: "control_operation_transitions_v5", entityType: "control_operation_transition"},
	{table: "runtime_termination_receipts_v5", entityType: "runtime_termination_receipt"},
	{table: "side_effect_operations_v5", entityType: "side_effect_operation"},
	{table: "reconciliation_attempts_v5", entityType: "reconciliation_attempt"},
	{table: "capacity_pools_v5", entityType: "capacity_pool"},
	{table: "job_dispatch_claims_v5", entityType: "job_dispatch_claim"},
}

var taskPurgeOperationIdentitySource = globalIdentitySource{
	table:      "task_purge_operations_v7",
	entityType: "task_purge_operation",
}

// globalIdentitySourcesCurrent includes tables introduced after V6. Migration
// V6 must use globalIdentitySources alone because later tables do not exist on
// a V5 database yet.
var globalIdentitySourcesCurrent = append(
	append(
		append([]globalIdentitySource(nil), globalIdentitySources...),
		taskPurgeOperationIdentitySource,
	),
	revisionCandidateIdentitySource,
	changeOperationIdentitySource,
	outboxDeliveryOperationIdentitySource,
	candidateGarbageCollectionOperationIdentitySource,
	releaseWithdrawOperationIdentitySource,
	releaseWithdrawReceiptIdentitySource,
)

func applyMigrationV6(tx *sql.Tx) error {
	if err := rejectExistingGlobalIdentityCollision(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(migrationV6); err != nil {
		return err
	}
	for _, source := range globalIdentitySources {
		statement := fmt.Sprintf(
			"INSERT INTO entity_id_registry (id, entity_type) SELECT id, '%s' FROM %s",
			source.entityType,
			source.table,
		)
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("register %s identities: %w", source.table, err)
		}
	}
	for _, source := range globalIdentitySources {
		if _, err := tx.Exec(globalIdentityInsertTrigger(source)); err != nil {
			return fmt.Errorf("create global identity trigger for %s: %w", source.table, err)
		}
		if _, err := tx.Exec(globalIdentityImmutableTrigger(source)); err != nil {
			return fmt.Errorf("create immutable identity trigger for %s: %w", source.table, err)
		}
	}
	return nil
}

func rejectExistingGlobalIdentityCollision(tx *sql.Tx) error {
	var queries []string
	for _, source := range globalIdentitySources {
		queries = append(queries, fmt.Sprintf("SELECT id, '%s' AS entity_type FROM %s", source.entityType, source.table))
	}
	query := `SELECT id, GROUP_CONCAT(entity_type, ',')
		FROM (` + strings.Join(queries, " UNION ALL ") + `)
		GROUP BY id
		HAVING COUNT(*) > 1
		ORDER BY id
		LIMIT 1`
	var id, entityTypes string
	err := tx.QueryRow(query).Scan(&id, &entityTypes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scan existing global identity collision: %w", err)
	}
	return fmt.Errorf("%w: UUIDv7 %s exists in %s", ErrIdentityCollision, id, entityTypes)
}

func globalIdentityInsertTrigger(source globalIdentitySource) string {
	return fmt.Sprintf(`
CREATE TRIGGER IF NOT EXISTS entity_id_registry_%s_insert
BEFORE INSERT ON %s
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, '%s')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, '%s');
END;`, source.table, source.table, globalIdentityCollisionMessage, source.entityType)
}

func globalIdentityImmutableTrigger(source globalIdentitySource) string {
	return fmt.Sprintf(`
CREATE TRIGGER IF NOT EXISTS entity_id_registry_%s_id_immutable
BEFORE UPDATE OF id ON %s
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;`, source.table, source.table)
}
