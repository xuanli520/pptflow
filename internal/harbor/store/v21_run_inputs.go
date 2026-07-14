package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const runInputArtifactSelect = `
	SELECT id, run_id, task_id, revision_id, revision_digest, port,
	       content_digest, schema_version, size_bytes, idempotency_key,
	       created_by, created_at
	FROM run_input_artifacts`

// CreateRunInputArtifact persists a run-scoped immutable input after the Run
// row exists. A replay returns only the exact same immutable tuple; a changed
// port, revision, or object is an explicit conflict rather than an update.
func (s *Store) CreateRunInputArtifact(ctx context.Context, request CreateRunInputArtifactRequest) (RunInputArtifact, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RunInputArtifact{}, err
	}
	now := s.now().UTC()
	artifact, err := prepareRunInputArtifact(s, request, now)
	if err != nil {
		return RunInputArtifact{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunInputArtifact{}, err
	}
	defer tx.Rollback()
	if err := validateRunInputArtifactSubjectTx(ctx, tx, artifact); err != nil {
		return RunInputArtifact{}, err
	}
	persisted, _, err := s.insertRunInputArtifactTx(ctx, tx, artifact, request.Actor, request.Reason)
	if err != nil {
		return RunInputArtifact{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunInputArtifact{}, err
	}
	return persisted, nil
}

// prepareInitialWorkflowRunInputArtifacts derives the immutable subject
// coordinates from the new Run. Callers can provide the object identity, but
// cannot bind it to another Run, Task, or TaskRevision in this transaction.
func prepareInitialWorkflowRunInputArtifacts(s *Store, requests []CreateRunInputArtifactRequest, run WorkflowRun, now time.Time) ([]RunInputArtifact, error) {
	prepared := make([]RunInputArtifact, 0, len(requests))
	ports := make(map[string]struct{}, len(requests))
	ids := make(map[string]struct{}, len(requests))
	keys := make(map[string]struct{}, len(requests))
	for _, request := range requests {
		if request.RunID != "" && request.RunID != run.ID {
			return nil, fmt.Errorf("initial run input belongs to another workflow run")
		}
		if request.TaskID != "" && request.TaskID != run.TaskID {
			return nil, fmt.Errorf("initial run input belongs to another task")
		}
		if request.RevisionID != "" && request.RevisionID != run.RevisionID {
			return nil, fmt.Errorf("initial run input belongs to another task revision")
		}
		request.RunID = run.ID
		request.TaskID = run.TaskID
		request.RevisionID = run.RevisionID
		request.Actor = run.CreatedBy
		artifact, err := prepareRunInputArtifact(s, request, now)
		if err != nil {
			return nil, err
		}
		if _, duplicate := ports[artifact.Port]; duplicate {
			return nil, fmt.Errorf("workflow run has duplicate initial input port %q", artifact.Port)
		}
		if _, duplicate := ids[artifact.ID]; duplicate {
			return nil, fmt.Errorf("workflow run has duplicate initial input ID %q", artifact.ID)
		}
		if _, duplicate := keys[artifact.IdempotencyKey]; duplicate {
			return nil, fmt.Errorf("workflow run has duplicate initial input idempotency key %q", artifact.IdempotencyKey)
		}
		ports[artifact.Port] = struct{}{}
		ids[artifact.ID] = struct{}{}
		keys[artifact.IdempotencyKey] = struct{}{}
		prepared = append(prepared, artifact)
	}
	return prepared, nil
}

func prepareRunInputArtifact(s *Store, request CreateRunInputArtifactRequest, now time.Time) (RunInputArtifact, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return RunInputArtifact{}, err
	}
	runID, err := requireV4ID(request.RunID, "run input artifact run ID")
	if err != nil {
		return RunInputArtifact{}, err
	}
	taskID, err := requireV4ID(request.TaskID, "run input artifact task ID")
	if err != nil {
		return RunInputArtifact{}, err
	}
	revisionID, err := requireV4ID(request.RevisionID, "run input artifact revision ID")
	if err != nil {
		return RunInputArtifact{}, err
	}
	revisionDigest, err := normalizeRequired(request.RevisionDigest, "run input artifact revision digest")
	if err != nil {
		return RunInputArtifact{}, err
	}
	port, err := normalizeRequired(request.Port, "run input artifact port")
	if err != nil {
		return RunInputArtifact{}, err
	}
	contentDigest, err := normalizeRequired(request.ContentDigest, "run input artifact content digest")
	if err != nil {
		return RunInputArtifact{}, err
	}
	schemaVersion, err := normalizeRequired(request.SchemaVersion, "run input artifact schema version")
	if err != nil {
		return RunInputArtifact{}, err
	}
	if request.SizeBytes < 0 {
		return RunInputArtifact{}, fmt.Errorf("run input artifact size cannot be negative")
	}
	key, err := normalizeRequired(request.IdempotencyKey, "run input artifact idempotency key")
	if err != nil {
		return RunInputArtifact{}, err
	}
	return RunInputArtifact{
		ID: id, RunID: runID, TaskID: taskID, RevisionID: revisionID,
		RevisionDigest: revisionDigest, Port: port, ContentDigest: contentDigest,
		SchemaVersion: schemaVersion, SizeBytes: request.SizeBytes, IdempotencyKey: key,
		CreatedBy: resolveActor(request.Actor), CreatedAt: now.UTC(),
	}, nil
}

func validateRunInputArtifactSubjectTx(ctx context.Context, tx *sql.Tx, artifact RunInputArtifact) error {
	run, err := getWorkflowRunTx(ctx, tx, artifact.RunID)
	if err != nil {
		return err
	}
	if run.TaskID != artifact.TaskID || run.RevisionID != artifact.RevisionID {
		return fmt.Errorf("run input artifact does not match workflow run subject")
	}
	revision, err := getTaskRevisionTx(ctx, tx, artifact.RevisionID)
	if err != nil {
		return err
	}
	if revision.TaskID != artifact.TaskID || revision.TaskDigest != artifact.RevisionDigest {
		return fmt.Errorf("run input artifact does not match immutable task revision")
	}
	return nil
}

// insertRunInputArtifactTx owns the immutable insert and audit record. It is
// shared by standalone recovery and StartRun's Run/input/job/outbox
// transaction so the worker cannot observe a partially persisted subject.
func (s *Store) insertRunInputArtifactTx(ctx context.Context, tx *sql.Tx, artifact RunInputArtifact, actor, reason string) (RunInputArtifact, bool, error) {
	// Check the key before INSERT so a faithful replay with the same explicit
	// UUID reaches idempotency semantics rather than the global-ID trigger.
	if existing, existingErr := getRunInputArtifactByKeyTx(ctx, tx, artifact.IdempotencyKey); existingErr == nil {
		if sameRunInputArtifact(existing, artifact) {
			return existing, false, nil
		}
		return RunInputArtifact{}, false, fmt.Errorf("%w: run input artifact key %s", ErrIdempotencyConflict, artifact.IdempotencyKey)
	} else if !isNotFound(existingErr) {
		return RunInputArtifact{}, false, existingErr
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO run_input_artifacts (
			id, run_id, task_id, revision_id, revision_digest, port,
			content_digest, schema_version, size_bytes, idempotency_key,
			created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, artifact.ID, artifact.RunID, artifact.TaskID, artifact.RevisionID, artifact.RevisionDigest,
		artifact.Port, artifact.ContentDigest, artifact.SchemaVersion, artifact.SizeBytes,
		artifact.IdempotencyKey, artifact.CreatedBy, artifact.CreatedAt)
	if err != nil {
		if isGlobalIdentityCollision(err) {
			return RunInputArtifact{}, false, fmt.Errorf("%w: run input artifact %s", ErrIdentityCollision, artifact.ID)
		}
		if !isUniqueConstraint(err) {
			return RunInputArtifact{}, false, err
		}
		_, portErr := getRunInputArtifactForPortTx(ctx, tx, artifact.RunID, artifact.Port)
		if portErr == nil {
			return RunInputArtifact{}, false, fmt.Errorf("%w: run input artifact port %s", ErrIdempotencyConflict, artifact.Port)
		}
		if !isNotFound(portErr) {
			return RunInputArtifact{}, false, portErr
		}
		return RunInputArtifact{}, false, fmt.Errorf("%w: run input artifact %s", ErrIdentityCollision, artifact.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: actor, EntityType: "run_input_artifact", EntityID: artifact.ID,
		Action: "run_input_artifact.created", Reason: reason,
		PayloadJSON: auditPayload(map[string]any{
			"run_id": artifact.RunID, "port": artifact.Port,
			"content_digest": artifact.ContentDigest, "idempotency_key": artifact.IdempotencyKey,
		}),
		CreatedAt: artifact.CreatedAt,
	}); err != nil {
		return RunInputArtifact{}, false, err
	}
	return artifact, true, nil
}

// GetRunInputArtifact returns one immutable run input by its globally unique
// identity.
func (s *Store) GetRunInputArtifact(ctx context.Context, artifactID string) (*RunInputArtifact, error) {
	if _, err := requireV4ID(artifactID, "run input artifact ID"); err != nil {
		return nil, err
	}
	artifact, err := scanRunInputArtifact(s.db.QueryRowContext(ctx, runInputArtifactSelect+" WHERE id = ?", artifactID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &artifact, nil
}

// GetRunInputArtifactForPort returns the sole immutable input bound to a run
// port. It is used by recovery and by the stage input reader.
func (s *Store) GetRunInputArtifactForPort(ctx context.Context, runID, port string) (*RunInputArtifact, error) {
	if _, err := requireV4ID(runID, "run input artifact run ID"); err != nil {
		return nil, err
	}
	port, err := normalizeRequired(port, "run input artifact port")
	if err != nil {
		return nil, err
	}
	artifact, err := scanRunInputArtifact(s.db.QueryRowContext(ctx, runInputArtifactSelect+" WHERE run_id = ? AND port = ?", runID, port))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &artifact, nil
}

func getRunInputArtifactByKeyTx(ctx context.Context, tx *sql.Tx, key string) (RunInputArtifact, error) {
	artifact, err := scanRunInputArtifact(tx.QueryRowContext(ctx, runInputArtifactSelect+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return RunInputArtifact{}, fmt.Errorf("%w: run input artifact idempotency key %s", ErrNotFound, key)
	}
	return artifact, err
}

func getRunInputArtifactForPortTx(ctx context.Context, tx *sql.Tx, runID, port string) (RunInputArtifact, error) {
	artifact, err := scanRunInputArtifact(tx.QueryRowContext(ctx, runInputArtifactSelect+" WHERE run_id = ? AND port = ?", runID, port))
	if err == sql.ErrNoRows {
		return RunInputArtifact{}, fmt.Errorf("%w: run input artifact %s/%s", ErrNotFound, runID, port)
	}
	return artifact, err
}

func scanRunInputArtifact(scanner rowScanner) (RunInputArtifact, error) {
	var artifact RunInputArtifact
	if err := scanner.Scan(
		&artifact.ID, &artifact.RunID, &artifact.TaskID, &artifact.RevisionID,
		&artifact.RevisionDigest, &artifact.Port, &artifact.ContentDigest,
		&artifact.SchemaVersion, &artifact.SizeBytes, &artifact.IdempotencyKey,
		&artifact.CreatedBy, &artifact.CreatedAt,
	); err != nil {
		return RunInputArtifact{}, err
	}
	artifact.CreatedAt = artifact.CreatedAt.UTC()
	return artifact, nil
}

func sameRunInputArtifact(left, right RunInputArtifact) bool {
	return left.ID == right.ID && left.RunID == right.RunID && left.TaskID == right.TaskID &&
		left.RevisionID == right.RevisionID && left.RevisionDigest == right.RevisionDigest &&
		left.Port == right.Port && left.ContentDigest == right.ContentDigest &&
		left.SchemaVersion == right.SchemaVersion && left.SizeBytes == right.SizeBytes
}
