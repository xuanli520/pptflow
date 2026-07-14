package store

import "time"

// RunInputArtifact is one immutable, run-scoped input selected before any
// stage executes. It is deliberately not an ArtifactRef: it has no synthetic
// producer StageAttempt and is bound directly to the sealed TaskRevision.
type RunInputArtifact struct {
	ID             string
	RunID          string
	TaskID         string
	RevisionID     string
	RevisionDigest string
	Port           string
	ContentDigest  string
	SchemaVersion  string
	SizeBytes      int64
	IdempotencyKey string
	CreatedBy      string
	CreatedAt      time.Time
}

// CreateRunInputArtifactRequest creates or idempotently recovers one
// immutable input binding for a Run. The idempotency key is owned by the
// caller and must identify the exact run/port/content tuple.
type CreateRunInputArtifactRequest struct {
	ID             string
	RunID          string
	TaskID         string
	RevisionID     string
	RevisionDigest string
	Port           string
	ContentDigest  string
	SchemaVersion  string
	SizeBytes      int64
	IdempotencyKey string
	Actor          string
	Reason         string
}
