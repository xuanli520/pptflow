package workflowkit

import (
	"fmt"
	"time"
)

// AttemptKind keeps run, stage, node, and turn accounting distinct. A domain
// may add its own logical work-item identity in ScopeID without conflating it
// with a workflow stage attempt.
type AttemptKind string

const (
	AttemptRun   AttemptKind = "run"
	AttemptStage AttemptKind = "stage"
	AttemptNode  AttemptKind = "node"
	AttemptTurn  AttemptKind = "turn"
)

func (kind AttemptKind) valid() bool {
	switch kind {
	case AttemptRun, AttemptStage, AttemptNode, AttemptTurn:
		return true
	default:
		return false
	}
}

// AttemptRecordEvent identifies an immutable event in an attempt's event
// stream. A terminal event closes an attempt; later work requires another
// opened attempt with a new identity.
type AttemptRecordEvent string

const (
	AttemptOpened     AttemptRecordEvent = "opened"
	AttemptCheckpoint AttemptRecordEvent = "checkpoint"
	AttemptTerminal   AttemptRecordEvent = "terminal"
)

func (event AttemptRecordEvent) valid() bool {
	switch event {
	case AttemptOpened, AttemptCheckpoint, AttemptTerminal:
		return true
	default:
		return false
	}
}

// AttemptIdentity is stable across all append-only records for one attempt.
type AttemptIdentity struct {
	ID               AttemptID   `json:"id"`
	Kind             AttemptKind `json:"kind"`
	ScopeID          string      `json:"scope_id"`
	ParentAttemptID  AttemptID   `json:"parent_attempt_id,omitempty"`
	RetryOfAttemptID AttemptID   `json:"retry_of_attempt_id,omitempty"`
	Ordinal          int         `json:"ordinal"`
}

func (identity AttemptIdentity) validate() error {
	if err := validateRequired("attempt id", string(identity.ID), ErrInvalidAttemptRecord); err != nil {
		return err
	}
	if !identity.Kind.valid() {
		return fmt.Errorf("%w: unsupported attempt kind %q", ErrInvalidAttemptRecord, identity.Kind)
	}
	if err := validateRequired("attempt scope id", identity.ScopeID, ErrInvalidAttemptRecord); err != nil {
		return err
	}
	if identity.Ordinal < 1 {
		return fmt.Errorf("%w: attempt ordinal must be at least one", ErrInvalidAttemptRecord)
	}
	if identity.ParentAttemptID == identity.ID || identity.RetryOfAttemptID == identity.ID {
		return fmt.Errorf("%w: attempt cannot reference itself", ErrInvalidAttemptRecord)
	}
	return nil
}

// AttemptRecord is one immutable fact about an attempt. Progress is populated
// for opened/checkpoint records. Terminal records instead carry a complete
// Outcome. Every artifact listed here remains visible even when the terminal
// outcome is an infrastructure failure.
type AttemptRecord struct {
	RecordID   string             `json:"record_id"`
	Sequence   uint64             `json:"sequence"`
	Identity   AttemptIdentity    `json:"identity"`
	Event      AttemptRecordEvent `json:"event"`
	Progress   ExecutionStatus    `json:"progress,omitempty"`
	Outcome    *Outcome           `json:"outcome,omitempty"`
	Artifacts  []ArtifactRef      `json:"artifacts,omitempty"`
	OccurredAt time.Time          `json:"occurred_at"`
}

// Clone returns a deep copy that cannot mutate a log's stored record.
func (record AttemptRecord) Clone() AttemptRecord {
	if record.Outcome != nil {
		outcome := *record.Outcome
		record.Outcome = &outcome
	}
	if len(record.Artifacts) > 0 {
		artifacts := record.Artifacts
		record.Artifacts = make([]ArtifactRef, len(artifacts))
		for index, artifact := range artifacts {
			record.Artifacts[index] = artifact.Clone()
		}
	}
	return record
}

// Status returns the status represented by this record.
func (record AttemptRecord) Status() ExecutionStatus {
	if record.Outcome != nil {
		return record.Outcome.Status
	}
	return record.Progress
}

// NewOpenedAttemptRecord creates the first record for an attempt. The initial
// durable state may be queued or running, allowing a scheduler to record an
// admission before the runtime starts.
func NewOpenedAttemptRecord(recordID string, sequence uint64, identity AttemptIdentity, initial ExecutionStatus, occurredAt time.Time) (AttemptRecord, error) {
	record := AttemptRecord{
		RecordID:   recordID,
		Sequence:   sequence,
		Identity:   identity,
		Event:      AttemptOpened,
		Progress:   initial,
		OccurredAt: occurredAt,
	}
	if err := record.validateLocal(); err != nil {
		return AttemptRecord{}, err
	}
	return record, nil
}

// NewCheckpointAttemptRecord appends a non-terminal status snapshot for the
// same immutable attempt identity. It may retain partial evidence.
func NewCheckpointAttemptRecord(recordID string, sequence uint64, identity AttemptIdentity, status ExecutionStatus, artifacts []ArtifactRef, occurredAt time.Time) (AttemptRecord, error) {
	record := AttemptRecord{
		RecordID:   recordID,
		Sequence:   sequence,
		Identity:   identity,
		Event:      AttemptCheckpoint,
		Progress:   status,
		Artifacts:  cloneArtifacts(artifacts),
		OccurredAt: occurredAt,
	}
	if err := record.validateLocal(); err != nil {
		return AttemptRecord{}, err
	}
	return record, nil
}

// NewTerminalAttemptRecord appends the final immutable outcome for the same
// attempt identity. Failed evidence is intentionally accepted and retained.
func NewTerminalAttemptRecord(recordID string, sequence uint64, identity AttemptIdentity, outcome Outcome, artifacts []ArtifactRef, occurredAt time.Time) (AttemptRecord, error) {
	outcomeCopy := outcome
	record := AttemptRecord{
		RecordID:   recordID,
		Sequence:   sequence,
		Identity:   identity,
		Event:      AttemptTerminal,
		Outcome:    &outcomeCopy,
		Artifacts:  cloneArtifacts(artifacts),
		OccurredAt: occurredAt,
	}
	if err := record.validateLocal(); err != nil {
		return AttemptRecord{}, err
	}
	return record, nil
}

func (record AttemptRecord) validateLocal() error {
	if err := validateRequired("attempt record id", record.RecordID, ErrInvalidAttemptRecord); err != nil {
		return err
	}
	if record.Sequence == 0 {
		return fmt.Errorf("%w: attempt record sequence must be positive", ErrInvalidAttemptRecord)
	}
	if err := record.Identity.validate(); err != nil {
		return err
	}
	if !record.Event.valid() {
		return fmt.Errorf("%w: unsupported attempt record event %q", ErrInvalidAttemptRecord, record.Event)
	}
	if record.OccurredAt.IsZero() {
		return fmt.Errorf("%w: attempt record occurred at is required", ErrInvalidAttemptRecord)
	}
	if err := validateRecordArtifacts(record.Artifacts); err != nil {
		return err
	}
	switch record.Event {
	case AttemptOpened:
		if record.Outcome != nil || (record.Progress != StatusQueued && record.Progress != StatusRunning) {
			return fmt.Errorf("%w: opened record must use queued or running progress and no outcome", ErrInvalidAttemptRecord)
		}
	case AttemptCheckpoint:
		if record.Outcome != nil || !record.Progress.valid() || record.Progress.IsTerminal() {
			return fmt.Errorf("%w: checkpoint record must use a non-terminal progress status and no outcome", ErrInvalidAttemptRecord)
		}
	case AttemptTerminal:
		if record.Outcome == nil || record.Progress != "" {
			return fmt.Errorf("%w: terminal record must carry only an outcome", ErrInvalidAttemptRecord)
		}
		if err := record.Outcome.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateRecordArtifacts(artifacts []ArtifactRef) error {
	seen := make(map[ArtifactID]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if err := artifact.Validate(); err != nil {
			return err
		}
		if _, exists := seen[artifact.ID]; exists {
			return fmt.Errorf("%w: duplicate artifact %q in attempt record", ErrInvalidAttemptRecord, artifact.ID)
		}
		seen[artifact.ID] = struct{}{}
	}
	return nil
}

func cloneArtifacts(artifacts []ArtifactRef) []ArtifactRef {
	if len(artifacts) == 0 {
		return nil
	}
	copyArtifacts := make([]ArtifactRef, len(artifacts))
	for index, artifact := range artifacts {
		copyArtifacts[index] = artifact.Clone()
	}
	return copyArtifacts
}

// AttemptLog is a persistent-style append-only log. Its records are private;
// Append returns a new log and Records returns deep copies, so callers cannot
// mutate already accepted historical facts.
type AttemptLog struct {
	records []AttemptRecord
}

// NewAttemptLog validates an existing ordered event stream and copies it into
// an immutable log value.
func NewAttemptLog(records ...AttemptRecord) (AttemptLog, error) {
	log := AttemptLog{}
	for _, record := range records {
		var err error
		log, err = log.Append(record)
		if err != nil {
			return AttemptLog{}, err
		}
	}
	return log, nil
}

// Append validates and appends one new fact without mutating the original log.
func (log AttemptLog) Append(record AttemptRecord) (AttemptLog, error) {
	if err := record.validateLocal(); err != nil {
		return log, err
	}
	if err := validateAppend(log.records, record); err != nil {
		return log, err
	}
	copyRecords := make([]AttemptRecord, len(log.records)+1)
	for index, current := range log.records {
		copyRecords[index] = current.Clone()
	}
	copyRecords[len(log.records)] = record.Clone()
	return AttemptLog{records: copyRecords}, nil
}

func validateAppend(records []AttemptRecord, next AttemptRecord) error {
	if len(records) == 0 {
		if next.Event != AttemptOpened {
			return fmt.Errorf("%w: first attempt record must open an attempt", ErrInvalidAttemptRecord)
		}
		if next.Identity.Ordinal != 1 {
			return fmt.Errorf("%w: first attempt in a stream must have ordinal one", ErrInvalidAttemptRecord)
		}
		return nil
	}
	if next.Sequence <= records[len(records)-1].Sequence {
		return fmt.Errorf("%w: attempt record sequence must strictly increase", ErrInvalidAttemptRecord)
	}
	opened := make(map[AttemptID]AttemptRecord)
	last := make(map[AttemptID]AttemptRecord)
	terminal := make(map[AttemptID]AttemptRecord)
	streamMaxOrdinal := make(map[string]int)
	recordIDs := make(map[string]struct{}, len(records))
	for _, current := range records {
		recordIDs[current.RecordID] = struct{}{}
		if current.Event == AttemptOpened {
			opened[current.Identity.ID] = current
			stream := attemptStreamKey(current.Identity)
			if current.Identity.Ordinal > streamMaxOrdinal[stream] {
				streamMaxOrdinal[stream] = current.Identity.Ordinal
			}
		}
		last[current.Identity.ID] = current
		if current.Event == AttemptTerminal {
			terminal[current.Identity.ID] = current
		}
	}
	if _, exists := recordIDs[next.RecordID]; exists {
		return fmt.Errorf("%w: duplicate attempt record id %q", ErrInvalidAttemptRecord, next.RecordID)
	}
	if next.Event == AttemptOpened {
		if _, exists := opened[next.Identity.ID]; exists {
			return fmt.Errorf("%w: attempt %q has already been opened", ErrInvalidAttemptRecord, next.Identity.ID)
		}
		if next.Identity.Ordinal != streamMaxOrdinal[attemptStreamKey(next.Identity)]+1 {
			return fmt.Errorf("%w: attempt %q ordinal %d is not the next ordinal in its stream", ErrInvalidAttemptRecord, next.Identity.ID, next.Identity.Ordinal)
		}
		if next.Identity.ParentAttemptID != "" {
			if _, exists := opened[next.Identity.ParentAttemptID]; !exists {
				return fmt.Errorf("%w: parent attempt %q does not exist", ErrInvalidAttemptRecord, next.Identity.ParentAttemptID)
			}
		}
		if next.Identity.RetryOfAttemptID != "" {
			retried, exists := terminal[next.Identity.RetryOfAttemptID]
			if !exists {
				return fmt.Errorf("%w: retry target attempt %q is not terminal", ErrInvalidAttemptRecord, next.Identity.RetryOfAttemptID)
			}
			if retried.Identity.Kind != next.Identity.Kind || retried.Identity.ScopeID != next.Identity.ScopeID {
				return fmt.Errorf("%w: retry target must have the same kind and scope", ErrInvalidAttemptRecord)
			}
		}
		return nil
	}

	openedRecord, exists := opened[next.Identity.ID]
	if !exists {
		return fmt.Errorf("%w: attempt %q was not opened", ErrInvalidAttemptRecord, next.Identity.ID)
	}
	if !sameAttemptIdentity(openedRecord.Identity, next.Identity) {
		return fmt.Errorf("%w: attempt identity cannot change after opening", ErrInvalidAttemptRecord)
	}
	if next.OccurredAt.Before(openedRecord.OccurredAt) {
		return fmt.Errorf("%w: attempt record precedes its opening record", ErrInvalidAttemptRecord)
	}
	previous := last[next.Identity.ID]
	if _, closed := terminal[next.Identity.ID]; closed {
		return fmt.Errorf("%w: terminal attempt %q cannot be reopened or updated", ErrInvalidAttemptRecord, next.Identity.ID)
	}
	if next.Event == AttemptCheckpoint && next.Progress == previous.Status() {
		return nil
	}
	if err := ValidateExecutionTransition(previous.Status(), next.Status()); err != nil {
		return err
	}
	return nil
}

func attemptStreamKey(identity AttemptIdentity) string {
	return string(identity.Kind) + "\x00" + identity.ScopeID
}

func sameAttemptIdentity(left, right AttemptIdentity) bool {
	return left == right
}

// Records returns a deep-copy snapshot in append order.
func (log AttemptLog) Records() []AttemptRecord {
	copyRecords := make([]AttemptRecord, len(log.records))
	for index, record := range log.records {
		copyRecords[index] = record.Clone()
	}
	return copyRecords
}

// AttemptSnapshot is the current projection of one append-only attempt stream.
type AttemptSnapshot struct {
	Identity   AttemptIdentity
	Status     ExecutionStatus
	Outcome    *Outcome
	StartedAt  time.Time
	FinishedAt time.Time
	Artifacts  []ArtifactRef
}

// Snapshot returns a deep-copy projection of attemptID.
func (log AttemptLog) Snapshot(attemptID AttemptID) (AttemptSnapshot, bool) {
	var snapshot AttemptSnapshot
	found := false
	for _, record := range log.records {
		if record.Identity.ID != attemptID {
			continue
		}
		if !found {
			snapshot.Identity = record.Identity
			snapshot.StartedAt = record.OccurredAt
			found = true
		}
		snapshot.Status = record.Status()
		if len(record.Artifacts) > 0 {
			snapshot.Artifacts = cloneArtifacts(record.Artifacts)
		}
		if record.Outcome != nil {
			outcome := *record.Outcome
			snapshot.Outcome = &outcome
			snapshot.FinishedAt = record.OccurredAt
		}
	}
	if !found {
		return AttemptSnapshot{}, false
	}
	return snapshot, true
}
