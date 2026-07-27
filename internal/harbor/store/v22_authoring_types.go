package store

import "time"

// AuthoringSource is the immutable, content-addressed source repository
// snapshot used before a task exists. Repository branch names, local paths,
// and mutable checkout state are intentionally absent.
type AuthoringSource struct {
	ID                    string
	RepositoryURL         string
	CommitSHA             string
	SnapshotArtifactRef   string
	SnapshotContentDigest string
	SnapshotSchemaVersion string
	SourceFingerprint     string
	IdempotencyKey        string
	CreatedBy             string
	CreatedAt             time.Time
}

// CreateAuthoringSourceRequest freezes one repository commit and its readonly
// content-addressed snapshot. SnapshotArtifactRef must be the canonical
// SHA-256 reference of SnapshotContentDigest; no mutable path is accepted.
type CreateAuthoringSourceRequest struct {
	ID                    string
	RepositoryURL         string
	CommitSHA             string
	SnapshotArtifactRef   string
	SnapshotContentDigest string
	SnapshotSchemaVersion string
	IdempotencyKey        string
	Actor                 string
	Reason                string
}

// AuthoringSession freezes a Standard authoring contract over one source. A
// TargetTaskID is the required, revision-free draft task that owns quota and
// TUI projection. It is never a workflow subject and never stands in for a
// task revision.
type AuthoringSession struct {
	ID                      string
	SourceID                string
	TargetTaskID            string
	WorkflowTemplateID      string
	WorkflowTemplateVersion string
	SessionManifestJSON     string
	SessionFingerprint      string
	IdempotencyKey          string
	CreatedBy               string
	CreatedAt               time.Time
}

// CreateAuthoringSessionRequest freezes the authoring manifest and binds one
// pre-existing draft task that has no revision yet.
type CreateAuthoringSessionRequest struct {
	ID                      string
	SourceID                string
	TargetTaskID            string
	WorkflowTemplateID      string
	WorkflowTemplateVersion string
	SessionManifestJSON     string
	IdempotencyKey          string
	Actor                   string
	Reason                  string
}

// CreateAuthoringWorkflowRunRequest creates a Standard authoring Run with the
// generic immutable subject coordinate (AuthoringSource ID, AuthoringSession
// ID, source snapshot content digest). It deliberately has no task/revision
// fields: ordinary task-revision runs continue to use CreateWorkflowRunRequest.
type CreateAuthoringWorkflowRunRequest struct {
	ID                      string
	AuthoringSessionID      string
	WorkflowTemplateID      string
	WorkflowTemplateVersion string
	ResolvedProfileHash     string
	DefinitionHash          string
	RunManifestJSON         string
	Trigger                 string
	ExecutionEpoch          int
	Actor                   string
	Reason                  string
	Dispatch                *WorkflowRunDispatchRequest
}

// AuthoringRunInputArtifact is the immutable source snapshot exposed to a
// pre-materialization authoring Run. It has no TaskRevision coordinate by
// design; SourceID, SessionID, and the snapshot digest are its generic subject
// lineage, while SourceFingerprint remains an additional provenance fact.
type AuthoringRunInputArtifact struct {
	ID                  string
	RunID               string
	SessionID           string
	SourceID            string
	SourceFingerprint   string
	Port                string
	SnapshotArtifactRef string
	ContentDigest       string
	SchemaVersion       string
	IdempotencyKey      string
	CreatedBy           string
	CreatedAt           time.Time
}

// AuthoringTaskMaterialization is the immutable receipt linking one
// AuthoringSession and its authoring Run to the first real TaskRevision. The
// write API is intentionally introduced only with the confirmed task-creation
// policy; read access is present so callers can inspect a completed handoff.
type AuthoringTaskMaterialization struct {
	ID                 string
	SessionID          string
	SourceID           string
	AuthoringRunID     string
	TaskID             string
	RevisionID         string
	SourceFingerprint  string
	TaskDigest         string
	RequestFingerprint string
	IdempotencyKey     string
	CreatedBy          string
	CreatedAt          time.Time
}

// MaterializeAuthoringTaskRequest atomically creates the first sealed,
// generated revision of the draft task pre-bound to AuthoringSession. The
// caller cannot select another task, parent revision, origin, or state.
type MaterializeAuthoringTaskRequest struct {
	ID                  string
	IdempotencyKey      string
	AuthoringSessionID  string
	AuthoringRunID      string
	ExpectedTaskVersion int64
	ExpectedRunVersion  int64
	RevisionID          string
	TaskDigest          string
	ProposalDigest      string
	ManifestID          string
	ChangeSummary       string
	MetadataJSON        string
	Actor               string
	Reason              string
}

// MaterializeAuthoringTaskResult contains the one committed draft task,
// initial revision, and immutable source-to-revision receipt.
type MaterializeAuthoringTaskResult struct {
	Task            TaskV2
	Revision        TaskRevision
	Materialization AuthoringTaskMaterialization
}
