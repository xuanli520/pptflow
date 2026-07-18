package store

import (
	"context"
	"database/sql"
	"fmt"
)

const artifactManifestV4Select = `
	SELECT id, subject_revision_id, subject_digest, workflow_fingerprint,
	       manifest_json, manifest_fingerprint, idempotency_key, created_by, created_at
	FROM artifact_manifests_v4`

const artifactRefV4Select = `
	SELECT id, manifest_id, artifact_key, content_digest, schema_version, run_id,
	       stage_key, attempt_id, turn_ordinal, subject_revision_id, subject_digest,
	       workflow_fingerprint, input_bindings_json, input_fingerprint,
	       producer_version, idempotency_key, created_at
	FROM artifact_refs_v4`

// CreateArtifactManifest writes a canonical JSON manifest exactly once. A
// replay of the same idempotency key returns the original immutable record;
// a changed payload is an explicit conflict rather than an overwrite.
func (s *Store) CreateArtifactManifest(ctx context.Context, request CreateArtifactManifestRequest) (ArtifactManifest, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ArtifactManifest{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return ArtifactManifest{}, err
	}
	subjectRevisionID, err := normalizeRequired(request.SubjectRevisionID, "artifact manifest subject revision ID")
	if err != nil {
		return ArtifactManifest{}, err
	}
	subjectDigest, err := normalizeRequired(request.SubjectDigest, "artifact manifest subject digest")
	if err != nil {
		return ArtifactManifest{}, err
	}
	workflowFingerprint, err := normalizeRequired(request.WorkflowFingerprint, "artifact manifest workflow fingerprint")
	if err != nil {
		return ArtifactManifest{}, err
	}
	manifestJSON, err := normalizeV4JSON(request.ManifestJSON, "artifact manifest payload")
	if err != nil {
		return ArtifactManifest{}, err
	}
	manifestFingerprint, err := normalizeRequired(request.ManifestFingerprint, "artifact manifest fingerprint")
	if err != nil {
		return ArtifactManifest{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "artifact manifest idempotency key")
	if err != nil {
		return ArtifactManifest{}, err
	}
	now := s.now().UTC()
	manifest := ArtifactManifest{
		ID:                  id,
		SubjectRevisionID:   subjectRevisionID,
		SubjectDigest:       subjectDigest,
		WorkflowFingerprint: workflowFingerprint,
		ManifestJSON:        manifestJSON,
		ManifestFingerprint: manifestFingerprint,
		IdempotencyKey:      key,
		CreatedBy:           resolveActor(request.Actor),
		CreatedAt:           now,
	}
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return ArtifactManifest{}, err
	}
	defer tx.Rollback()
	defer releaseFence()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO artifact_manifests_v4 (
			id, subject_revision_id, subject_digest, workflow_fingerprint,
			manifest_json, manifest_fingerprint, idempotency_key, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, manifest.ID, manifest.SubjectRevisionID, manifest.SubjectDigest, manifest.WorkflowFingerprint,
		manifest.ManifestJSON, manifest.ManifestFingerprint, manifest.IdempotencyKey, manifest.CreatedBy, manifest.CreatedAt)
	if err != nil {
		if !isUniqueConstraint(err) {
			return ArtifactManifest{}, err
		}
		existing, existingErr := getArtifactManifestByKeyTx(ctx, tx, manifest.IdempotencyKey)
		if existingErr == nil {
			if sameArtifactManifest(existing, manifest) {
				if err := tx.Commit(); err != nil {
					return ArtifactManifest{}, err
				}
				return existing, nil
			}
			return ArtifactManifest{}, fmt.Errorf("%w: artifact manifest key %s", ErrIdempotencyConflict, manifest.IdempotencyKey)
		}
		if !isNotFound(existingErr) {
			return ArtifactManifest{}, existingErr
		}
		return ArtifactManifest{}, fmt.Errorf("%w: artifact manifest %s", ErrIdentityCollision, manifest.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "artifact_manifest",
		EntityID:    manifest.ID,
		Action:      "artifact_manifest.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"subject_revision_id": manifest.SubjectRevisionID, "manifest_fingerprint": manifest.ManifestFingerprint, "idempotency_key": manifest.IdempotencyKey}),
		CreatedAt:   now,
	}); err != nil {
		return ArtifactManifest{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArtifactManifest{}, err
	}
	return manifest, nil
}

func (s *Store) GetArtifactManifest(ctx context.Context, manifestID string) (*ArtifactManifest, error) {
	if _, err := requireV4ID(manifestID, "artifact manifest ID"); err != nil {
		return nil, err
	}
	manifest, err := scanArtifactManifest(s.db.QueryRowContext(ctx, artifactManifestV4Select+" WHERE id = ?", manifestID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &manifest, nil
}

// ListArtifactManifestsForRevision returns immutable evidence manifests bound
// to a revision. Callers still need ListArtifactRefs to inspect the lineage
// entries within each manifest.
func (s *Store) ListArtifactManifestsForRevision(ctx context.Context, revisionID string) ([]ArtifactManifest, error) {
	if _, err := requireV4ID(revisionID, "artifact manifest subject revision ID"); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, artifactManifestV4Select+" WHERE subject_revision_id = ? ORDER BY created_at DESC, id ASC", revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	manifests := make([]ArtifactManifest, 0)
	for rows.Next() {
		manifest, err := scanArtifactManifest(rows)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, manifest)
	}
	return manifests, rows.Err()
}

// CreateArtifactRef appends immutable artifact lineage to a manifest.
func (s *Store) CreateArtifactRef(ctx context.Context, request CreateArtifactRefRequest) (ArtifactRef, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ArtifactRef{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return ArtifactRef{}, err
	}
	manifestID, err := requireV4ID(request.ManifestID, "artifact ref manifest ID")
	if err != nil {
		return ArtifactRef{}, err
	}
	artifactKey, err := normalizeRequired(request.ArtifactKey, "artifact ref key")
	if err != nil {
		return ArtifactRef{}, err
	}
	contentDigest, err := normalizeRequired(request.ContentDigest, "artifact ref content digest")
	if err != nil {
		return ArtifactRef{}, err
	}
	schemaVersion, err := normalizeRequired(request.SchemaVersion, "artifact ref schema version")
	if err != nil {
		return ArtifactRef{}, err
	}
	runID, err := normalizeRequired(request.RunID, "artifact ref run ID")
	if err != nil {
		return ArtifactRef{}, err
	}
	stageKey, err := normalizeRequired(request.StageKey, "artifact ref stage key")
	if err != nil {
		return ArtifactRef{}, err
	}
	attemptID, err := normalizeRequired(request.AttemptID, "artifact ref attempt ID")
	if err != nil {
		return ArtifactRef{}, err
	}
	if request.TurnOrdinal < 0 {
		return ArtifactRef{}, fmt.Errorf("artifact ref turn ordinal cannot be negative")
	}
	subjectRevisionID, err := normalizeRequired(request.SubjectRevisionID, "artifact ref subject revision ID")
	if err != nil {
		return ArtifactRef{}, err
	}
	subjectDigest, err := normalizeRequired(request.SubjectDigest, "artifact ref subject digest")
	if err != nil {
		return ArtifactRef{}, err
	}
	workflowFingerprint, err := normalizeRequired(request.WorkflowFingerprint, "artifact ref workflow fingerprint")
	if err != nil {
		return ArtifactRef{}, err
	}
	bindingsJSON, err := normalizeV4JSON(request.InputBindingsJSON, "artifact ref input bindings")
	if err != nil {
		return ArtifactRef{}, err
	}
	inputFingerprint, err := normalizeRequired(request.InputFingerprint, "artifact ref input fingerprint")
	if err != nil {
		return ArtifactRef{}, err
	}
	producerVersion, err := normalizeRequired(request.ProducerVersion, "artifact ref producer version")
	if err != nil {
		return ArtifactRef{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "artifact ref idempotency key")
	if err != nil {
		return ArtifactRef{}, err
	}
	now := s.now().UTC()
	reference := ArtifactRef{
		ID:                  id,
		ManifestID:          manifestID,
		ArtifactKey:         artifactKey,
		ContentDigest:       contentDigest,
		SchemaVersion:       schemaVersion,
		RunID:               runID,
		StageKey:            stageKey,
		AttemptID:           attemptID,
		TurnOrdinal:         request.TurnOrdinal,
		SubjectRevisionID:   subjectRevisionID,
		SubjectDigest:       subjectDigest,
		WorkflowFingerprint: workflowFingerprint,
		InputBindingsJSON:   bindingsJSON,
		InputFingerprint:    inputFingerprint,
		ProducerVersion:     producerVersion,
		IdempotencyKey:      key,
		CreatedAt:           now,
	}
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return ArtifactRef{}, err
	}
	defer tx.Rollback()
	defer releaseFence()
	if _, err := getArtifactManifestTx(ctx, tx, reference.ManifestID); err != nil {
		return ArtifactRef{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO artifact_refs_v4 (
			id, manifest_id, artifact_key, content_digest, schema_version, run_id,
			stage_key, attempt_id, turn_ordinal, subject_revision_id, subject_digest,
			workflow_fingerprint, input_bindings_json, input_fingerprint,
			producer_version, idempotency_key, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, reference.ID, reference.ManifestID, reference.ArtifactKey, reference.ContentDigest, reference.SchemaVersion, reference.RunID,
		reference.StageKey, reference.AttemptID, reference.TurnOrdinal, reference.SubjectRevisionID, reference.SubjectDigest,
		reference.WorkflowFingerprint, reference.InputBindingsJSON, reference.InputFingerprint,
		reference.ProducerVersion, reference.IdempotencyKey, reference.CreatedAt)
	if err != nil {
		if !isUniqueConstraint(err) {
			return ArtifactRef{}, err
		}
		existing, existingErr := getArtifactRefByKeyTx(ctx, tx, reference.IdempotencyKey)
		if existingErr == nil {
			if sameArtifactRef(existing, reference) {
				if err := tx.Commit(); err != nil {
					return ArtifactRef{}, err
				}
				return existing, nil
			}
			return ArtifactRef{}, fmt.Errorf("%w: artifact ref key %s", ErrIdempotencyConflict, reference.IdempotencyKey)
		}
		if !isNotFound(existingErr) {
			return ArtifactRef{}, existingErr
		}
		return ArtifactRef{}, fmt.Errorf("%w: artifact ref %s", ErrIdentityCollision, reference.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "artifact_ref",
		EntityID:    reference.ID,
		Action:      "artifact_ref.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"manifest_id": reference.ManifestID, "artifact_key": reference.ArtifactKey, "content_digest": reference.ContentDigest, "idempotency_key": reference.IdempotencyKey}),
		CreatedAt:   now,
	}); err != nil {
		return ArtifactRef{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArtifactRef{}, err
	}
	return reference, nil
}

func (s *Store) GetArtifactRef(ctx context.Context, artifactRefID string) (*ArtifactRef, error) {
	if _, err := requireV4ID(artifactRefID, "artifact ref ID"); err != nil {
		return nil, err
	}
	reference, err := scanArtifactRef(s.db.QueryRowContext(ctx, artifactRefV4Select+" WHERE id = ?", artifactRefID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &reference, nil
}

func (s *Store) ListArtifactRefs(ctx context.Context, manifestID string) ([]ArtifactRef, error) {
	if _, err := requireV4ID(manifestID, "artifact manifest ID"); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, artifactRefV4Select+" WHERE manifest_id = ? ORDER BY artifact_key ASC, id ASC", manifestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var references []ArtifactRef
	for rows.Next() {
		reference, err := scanArtifactRef(rows)
		if err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	return references, rows.Err()
}

// ListArtifactRefsForAttempt exposes immutable output/evidence lineage for one
// StageAttempt, including manifests that were persisted before a worker lost
// its dispatch fence and could update the StageAttempt projection.
func (s *Store) ListArtifactRefsForAttempt(ctx context.Context, stageAttemptID string) ([]ArtifactRef, error) {
	if _, err := requireV4ID(stageAttemptID, "artifact ref attempt ID"); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, artifactRefV4Select+" WHERE attempt_id = ? ORDER BY created_at ASC, id ASC", stageAttemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	references := make([]ArtifactRef, 0)
	for rows.Next() {
		reference, err := scanArtifactRef(rows)
		if err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	return references, rows.Err()
}

func getArtifactManifestTx(ctx context.Context, tx *sql.Tx, manifestID string) (ArtifactManifest, error) {
	manifest, err := scanArtifactManifest(tx.QueryRowContext(ctx, artifactManifestV4Select+" WHERE id = ?", manifestID))
	if err == sql.ErrNoRows {
		return ArtifactManifest{}, fmt.Errorf("%w: artifact manifest %s", ErrNotFound, manifestID)
	}
	return manifest, err
}

func getArtifactManifestByKeyTx(ctx context.Context, tx *sql.Tx, key string) (ArtifactManifest, error) {
	manifest, err := scanArtifactManifest(tx.QueryRowContext(ctx, artifactManifestV4Select+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return ArtifactManifest{}, fmt.Errorf("%w: artifact manifest idempotency key %s", ErrNotFound, key)
	}
	return manifest, err
}

func getArtifactRefByKeyTx(ctx context.Context, tx *sql.Tx, key string) (ArtifactRef, error) {
	reference, err := scanArtifactRef(tx.QueryRowContext(ctx, artifactRefV4Select+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return ArtifactRef{}, fmt.Errorf("%w: artifact ref idempotency key %s", ErrNotFound, key)
	}
	return reference, err
}

func scanArtifactManifest(scanner rowScanner) (ArtifactManifest, error) {
	var manifest ArtifactManifest
	if err := scanner.Scan(
		&manifest.ID, &manifest.SubjectRevisionID, &manifest.SubjectDigest, &manifest.WorkflowFingerprint,
		&manifest.ManifestJSON, &manifest.ManifestFingerprint, &manifest.IdempotencyKey, &manifest.CreatedBy, &manifest.CreatedAt,
	); err != nil {
		return ArtifactManifest{}, err
	}
	manifest.CreatedAt = manifest.CreatedAt.UTC()
	return manifest, nil
}

func scanArtifactRef(scanner rowScanner) (ArtifactRef, error) {
	var reference ArtifactRef
	if err := scanner.Scan(
		&reference.ID, &reference.ManifestID, &reference.ArtifactKey, &reference.ContentDigest, &reference.SchemaVersion, &reference.RunID,
		&reference.StageKey, &reference.AttemptID, &reference.TurnOrdinal, &reference.SubjectRevisionID, &reference.SubjectDigest,
		&reference.WorkflowFingerprint, &reference.InputBindingsJSON, &reference.InputFingerprint,
		&reference.ProducerVersion, &reference.IdempotencyKey, &reference.CreatedAt,
	); err != nil {
		return ArtifactRef{}, err
	}
	reference.CreatedAt = reference.CreatedAt.UTC()
	return reference, nil
}

func sameArtifactManifest(left, right ArtifactManifest) bool {
	return left.SubjectRevisionID == right.SubjectRevisionID &&
		left.SubjectDigest == right.SubjectDigest &&
		left.WorkflowFingerprint == right.WorkflowFingerprint &&
		left.ManifestJSON == right.ManifestJSON &&
		left.ManifestFingerprint == right.ManifestFingerprint
}

func sameArtifactRef(left, right ArtifactRef) bool {
	return left.ManifestID == right.ManifestID &&
		left.ArtifactKey == right.ArtifactKey &&
		left.ContentDigest == right.ContentDigest &&
		left.SchemaVersion == right.SchemaVersion &&
		left.RunID == right.RunID &&
		left.StageKey == right.StageKey &&
		left.AttemptID == right.AttemptID &&
		left.TurnOrdinal == right.TurnOrdinal &&
		left.SubjectRevisionID == right.SubjectRevisionID &&
		left.SubjectDigest == right.SubjectDigest &&
		left.WorkflowFingerprint == right.WorkflowFingerprint &&
		left.InputBindingsJSON == right.InputBindingsJSON &&
		left.InputFingerprint == right.InputFingerprint &&
		left.ProducerVersion == right.ProducerVersion
}
