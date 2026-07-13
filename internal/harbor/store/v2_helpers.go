package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"
)

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) mutationPreflight(ctx context.Context) error {
	_, err := s.BackupIfDue(ctx)
	return err
}

func (s *Store) newV2ID(value string) (string, error) {
	return normalizeUUIDv7(value, s.now().UTC())
}

func normalizeRequired(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return value, nil
}

func normalizeJSON(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}", nil
	}
	if !json.Valid([]byte(value)) {
		return "", fmt.Errorf("%s must contain valid JSON", field)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(value)); err != nil {
		return "", fmt.Errorf("compact %s: %w", field, err)
	}
	return compact.String(), nil
}

func auditPayload(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		return `{"serialization_error":true}`
	}
	return string(payload)
}

func resolveActor(actor string) string {
	actor = strings.TrimSpace(actor)
	if actor != "" {
		return actor
	}
	if current, err := user.Current(); err == nil {
		if name := strings.TrimSpace(current.Username); name != "" {
			return name
		}
	}
	for _, key := range []string{"USER", "USERNAME", "LOGNAME"} {
		if name := strings.TrimSpace(os.Getenv(key)); name != "" {
			return name
		}
	}
	return "local"
}

func nullableString(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func nullableTimePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time.UTC()
	return &result
}

func nullableInt64Ptr(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func nullableStringValue(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint failed") ||
		strings.Contains(message, "constraint failed") && strings.Contains(message, "unique") ||
		isGlobalIdentityCollision(err)
}

func isGlobalIdentityCollision(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), globalIdentityCollisionMessage)
}

func isForeignKeyConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "foreign key constraint failed")
}

func (s *Store) appendAuditTx(ctx context.Context, tx *sql.Tx, event AuditEvent) (AuditEvent, error) {
	var err error
	event.ID, err = s.newV2ID(event.ID)
	if err != nil {
		return AuditEvent{}, err
	}
	event.Actor = resolveActor(event.Actor)
	event.EntityType, err = normalizeRequired(event.EntityType, "audit entity type")
	if err != nil {
		return AuditEvent{}, err
	}
	event.EntityID, err = normalizeRequired(event.EntityID, "audit entity ID")
	if err != nil {
		return AuditEvent{}, err
	}
	event.Action, err = normalizeRequired(event.Action, "audit action")
	if err != nil {
		return AuditEvent{}, err
	}
	event.Reason = strings.TrimSpace(event.Reason)
	event.OperationKey = strings.TrimSpace(event.OperationKey)
	event.PayloadJSON, err = normalizeJSON(event.PayloadJSON, "audit payload")
	if err != nil {
		return AuditEvent{}, err
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = s.now().UTC()
	} else {
		event.CreatedAt = event.CreatedAt.UTC()
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_events (id, actor, entity_type, entity_id, action, reason, payload_json, operation_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, event.ID, event.Actor, event.EntityType, event.EntityID, event.Action, event.Reason, event.PayloadJSON, event.OperationKey, event.CreatedAt)
	if err != nil {
		if isUniqueConstraint(err) {
			return AuditEvent{}, fmt.Errorf("%w: audit event %s", ErrIdentityCollision, event.ID)
		}
		return AuditEvent{}, err
	}
	return event, nil
}

func (s *Store) ListAuditEvents(ctx context.Context, request ListAuditEventsRequest) ([]AuditEvent, error) {
	query := `SELECT id, actor, entity_type, entity_id, action, reason, payload_json, operation_key, created_at FROM audit_events`
	var clauses []string
	var args []any
	if value := strings.TrimSpace(request.EntityType); value != "" {
		clauses = append(clauses, "entity_type = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(request.EntityID); value != "" {
		clauses = append(clauses, "entity_id = ?")
		args = append(args, value)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY created_at ASC, id ASC"
	if request.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, request.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []AuditEvent
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(&event.ID, &event.Actor, &event.EntityType, &event.EntityID, &event.Action, &event.Reason, &event.PayloadJSON, &event.OperationKey, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.CreatedAt = event.CreatedAt.UTC()
		events = append(events, event)
	}
	return events, rows.Err()
}
