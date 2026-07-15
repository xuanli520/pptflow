package store

import "time"

// CodeEdgeEvaluatorEvidenceArtifact is one immutable child-owned artifact
// reference retained by a parent-owned evaluator evidence handoff. It carries
// identity and digest only; bytes remain in the existing artifact store.
type CodeEdgeEvaluatorEvidenceArtifact struct {
	ArtifactID    string
	ContentDigest string
	SchemaVersion string
}

// CodeEdgeEvaluatorEvidenceHandoff links one complete CodeEdge Phase-1 parent
// Run to exactly one closed evaluator child Run. The parent remains the sole
// final-compliance and package authority. The child retains ownership of its
// stages, artifacts, and logical trials; this record is provenance, not an
// artifact copy or a synthetic parent stage.
type CodeEdgeEvaluatorEvidenceHandoff struct {
	ID          string
	ParentRunID string
	ChildRunID  string
	TaskID      string
	RevisionID  string
	TaskDigest  string

	ParentCatalogFingerprint    string
	ParentLockFingerprint       string
	ParentManifestFingerprint   string
	ParentDefinitionFingerprint string
	ChildCatalogFingerprint     string
	ChildLockFingerprint        string
	ChildManifestFingerprint    string
	ChildDefinitionFingerprint  string

	QwenStageAttemptID      string
	QwenBundle              CodeEdgeEvaluatorEvidenceArtifact
	QwenScreenshot          CodeEdgeEvaluatorEvidenceArtifact
	QwenTrialSetFingerprint string

	OpusStageAttemptID      string
	OpusBundle              CodeEdgeEvaluatorEvidenceArtifact
	OpusScreenshot          CodeEdgeEvaluatorEvidenceArtifact
	OpusTrialSetFingerprint string
	HandoffJSON             string
	HandoffFingerprint      string

	IdempotencyKey string
	CreatedBy      string
	CreatedAt      time.Time
}

// CreateCodeEdgeEvaluatorEvidenceHandoffRequest creates one append-only
// provenance link. ID, when present, must equal IdempotencyKey so replaying a
// lost response cannot create a second parent-child evidence association.
type CreateCodeEdgeEvaluatorEvidenceHandoffRequest struct {
	ID          string
	ParentRunID string
	ChildRunID  string
	TaskID      string
	RevisionID  string
	TaskDigest  string

	ParentCatalogFingerprint    string
	ParentLockFingerprint       string
	ParentManifestFingerprint   string
	ParentDefinitionFingerprint string
	ChildCatalogFingerprint     string
	ChildLockFingerprint        string
	ChildManifestFingerprint    string
	ChildDefinitionFingerprint  string

	QwenStageAttemptID      string
	QwenBundle              CodeEdgeEvaluatorEvidenceArtifact
	QwenScreenshot          CodeEdgeEvaluatorEvidenceArtifact
	QwenTrialSetFingerprint string

	OpusStageAttemptID      string
	OpusBundle              CodeEdgeEvaluatorEvidenceArtifact
	OpusScreenshot          CodeEdgeEvaluatorEvidenceArtifact
	OpusTrialSetFingerprint string
	HandoffJSON             string
	HandoffFingerprint      string

	IdempotencyKey string
	Actor          string
	Reason         string
}
