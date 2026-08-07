package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// A worker log is not a stream of log lines. Each detached run-worker process
// writes exactly one JSON result envelope to its stdout, which the launcher
// appends to runs/<run>/worker.log. A Run that hands off many times therefore
// produces many multi-kilobyte envelopes in one file.
//
// Reading a fixed byte window from the tail of such a file almost always starts
// and ends mid-envelope, which is why the terminal previously showed only a
// JSON fragment. The reader below finds envelope boundaries instead of byte
// offsets, so it can promise whole records, and projects each record to the few
// fields an operator actually decides on.
const (
	// RunWorkerLogRecordFormat identifies the compact record a current worker
	// writes. Older files hold the full session envelope instead; the reader
	// accepts both so upgrading a build never blanks an existing log.
	RunWorkerLogRecordFormat = "harbor.run-worker-log-record.v1"
	// taskBoardLogScanLimit bounds how much of the tail is examined for record
	// boundaries. One legacy envelope can exceed 64 KiB on its own, so the scan
	// window must be large enough to hold several of them.
	taskBoardLogScanLimit int64 = 4 * 1024 * 1024
	// taskBoardLogRecordLimit bounds how many trailing records are projected.
	taskBoardLogRecordLimit = 50
	// taskBoardLogRawLimit bounds the verbatim text retained for the raw
	// fallback view, so a large envelope cannot be pushed whole into a terminal.
	taskBoardLogRawLimit = 64 * 1024
)

// RunWorkerLogRecord is what a controlled run-worker process now prints when it
// exits. It replaces printing the whole RunWorkerSessionResult, whose
// RunManifestJSON and PayloadJSON fields repeated the same frozen plan on every
// handoff and made up the overwhelming majority of each record's bytes.
//
// RunWorkerSessionResult itself is unchanged: it remains the in-process return
// value and its Go structure is still what callers and tests observe. Only the
// bytes this command writes to stdout are projected.
type RunWorkerLogRecord struct {
	Format     string    `json:"format"`
	ObservedAt time.Time `json:"observed_at"`
	RunID      string    `json:"run_id"`
	RunStatus  string    `json:"run_status"`
	// StoppedFor is the durable status the supervisor exited on. It is empty
	// when the worker stopped for a reason other than Run quiescence.
	StoppedFor     string                     `json:"stopped_for,omitempty"`
	ExecutionEpoch int                        `json:"execution_epoch,omitempty"`
	DefinitionHash string                     `json:"definition_hash,omitempty"`
	WorkerLease    *RunWorkerLogLease         `json:"worker_lease,omitempty"`
	Handoff        *RunWorkerLogHandoff       `json:"handoff,omitempty"`
	LastCycle      *RunWorkerLogCycle         `json:"last_cycle,omitempty"`
	Recoveries     []RunWorkerLogRecoveryNote `json:"recoveries,omitempty"`
}

// RunWorkerLogLease records which supervisor fence this process held.
type RunWorkerLogLease struct {
	ID           string `json:"id,omitempty"`
	Owner        string `json:"owner,omitempty"`
	State        string `json:"state,omitempty"`
	FencingToken uint64 `json:"fencing_token,omitempty"`
}

// RunWorkerLogHandoff records the durable handoff this process ran under.
type RunWorkerLogHandoff struct {
	OperationID string `json:"operation_id,omitempty"`
	State       string `json:"state,omitempty"`
	ProcessID   int    `json:"pid,omitempty"`
}

// RunWorkerLogCycle records the last durable job cycle the worker executed.
type RunWorkerLogCycle struct {
	Empty          bool                 `json:"empty,omitempty"`
	CommandType    string               `json:"command_type,omitempty"`
	JobID          string               `json:"job_id,omitempty"`
	StageAttemptID string               `json:"stage_attempt_id,omitempty"`
	State          string               `json:"state,omitempty"`
	FinalState     string               `json:"final_state,omitempty"`
	StartedAt      *time.Time           `json:"started_at,omitempty"`
	FinishedAt     *time.Time           `json:"finished_at,omitempty"`
	Failure        *RunWorkerLogFailure `json:"failure,omitempty"`
}

// RunWorkerLogFailure is the compact diagnostic retained for a failed cycle.
type RunWorkerLogFailure struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// RunWorkerLogRecoveryNote names one expired-fence recovery the worker observed.
type RunWorkerLogRecoveryNote struct {
	JobID string `json:"job_id,omitempty"`
	State string `json:"state,omitempty"`
}

// NewRunWorkerLogRecord projects a session result into the compact record a
// worker prints. It reads only durable identifiers and states, never the frozen
// run manifest or a job payload.
func NewRunWorkerLogRecord(result RunWorkerSessionResult) RunWorkerLogRecord {
	record := RunWorkerLogRecord{
		Format:         RunWorkerLogRecordFormat,
		ObservedAt:     time.Now().UTC(),
		RunID:          result.Run.ID,
		RunStatus:      string(result.Run.Status),
		StoppedFor:     string(result.StoppedFor),
		ExecutionEpoch: result.Run.ExecutionEpoch,
		DefinitionHash: result.Run.DefinitionHash,
	}
	if result.WorkerLease.ID != "" {
		record.WorkerLease = &RunWorkerLogLease{
			ID: result.WorkerLease.ID, Owner: result.WorkerLease.Owner,
			State: string(result.WorkerLease.State), FencingToken: result.WorkerLease.FencingToken,
		}
	}
	if result.Handoff != nil {
		record.Handoff = &RunWorkerLogHandoff{
			OperationID: result.Handoff.ID, State: string(result.Handoff.State), ProcessID: result.Handoff.ProcessID,
		}
	}
	cycle := &RunWorkerLogCycle{
		Empty:      result.LastCycle.Empty,
		FinalState: string(result.LastCycle.FinalState),
	}
	if job := result.LastCycle.Job; job != nil {
		cycle.CommandType = job.CommandType
		cycle.JobID = job.ID
		cycle.StageAttemptID = job.StageAttemptID
		cycle.State = string(job.State)
		cycle.StartedAt = utcTimePointer(job.StartedAt)
		cycle.FinishedAt = utcTimePointer(job.FinishedAt)
		if job.Failure != nil {
			cycle.Failure = &RunWorkerLogFailure{Code: job.Failure.Code, Message: firstLogLine(job.Failure.Message)}
		}
	}
	record.LastCycle = cycle
	for _, recovery := range result.LastCycle.Recoveries {
		record.Recoveries = append(record.Recoveries, RunWorkerLogRecoveryNote{
			JobID: recovery.Job.ID, State: string(recovery.Job.State),
		})
	}
	return record
}

// TaskBoardLogRecord is the operator-facing projection of one worker record. It
// carries the Run's observed state, the durable job that last executed, and the
// failure or handoff facts needed to choose a next action.
//
// It deliberately omits RunManifestJSON and PayloadJSON when reading a legacy
// envelope: those two fields are the bulk of such a record and repeat the same
// frozen plan every time, which is what made the log unreadable.
type TaskBoardLogRecord struct {
	// Sequence is the 1-based position of this record within the returned tail.
	Sequence int `json:"sequence"`
	// ObservedAt is the most representative timestamp available on the record.
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	RunID      string     `json:"run_id,omitempty"`
	RunStatus  string     `json:"run_status,omitempty"`
	// StoppedFor is the durable status the worker exited on.
	StoppedFor string `json:"stopped_for,omitempty"`
	// JobCommandType and JobState describe the last durable cycle.
	JobCommandType string     `json:"job_command_type,omitempty"`
	JobState       string     `json:"job_state,omitempty"`
	StageAttemptID string     `json:"stage_attempt_id,omitempty"`
	JobStartedAt   *time.Time `json:"job_started_at,omitempty"`
	JobFinishedAt  *time.Time `json:"job_finished_at,omitempty"`
	// CycleEmpty marks a poll cycle that claimed no work.
	CycleEmpty     bool   `json:"cycle_empty,omitempty"`
	FailureCode    string `json:"failure_code,omitempty"`
	FailureSummary string `json:"failure_summary,omitempty"`
	// HandoffSummary describes the worker handoff this process ran under.
	HandoffSummary string `json:"handoff_summary,omitempty"`
	// Legacy marks a record read from the pre-projection envelope format.
	Legacy bool `json:"legacy,omitempty"`
	// Raw is the verbatim record text, retained so an operator can always fall
	// back to the original bytes when a projection omits something.
	Raw string `json:"-"`
	// ParseError is set when the record could not be decoded. The record is
	// still returned with its Raw text so a decode failure never hides a log.
	ParseError string `json:"parse_error,omitempty"`
}

// taskBoardLegacyWorkerEnvelope mirrors only the parts of a printed
// RunWorkerSessionResult this projection reads.
//
// store.WorkflowRun, DurableWorkerResult, and store.DurableJob carry no JSON
// tags, so their keys in an existing log are the Go field names. Only the
// session result and store.RunWorkerHandoff declare snake_case tags.
type taskBoardLegacyWorkerEnvelope struct {
	Run struct {
		ID         string     `json:"ID"`
		Status     string     `json:"Status"`
		StartedAt  *time.Time `json:"StartedAt"`
		FinishedAt *time.Time `json:"FinishedAt"`
		CreatedAt  *time.Time `json:"CreatedAt"`
	} `json:"run"`
	StoppedFor string `json:"stopped_for"`
	LastCycle  struct {
		Empty      bool   `json:"Empty"`
		FinalState string `json:"FinalState"`
		Job        *struct {
			ID             string     `json:"ID"`
			CommandType    string     `json:"CommandType"`
			State          string     `json:"State"`
			StageAttemptID string     `json:"StageAttemptID"`
			StartedAt      *time.Time `json:"StartedAt"`
			FinishedAt     *time.Time `json:"FinishedAt"`
			Failure        *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Failure"`
		} `json:"Job"`
	} `json:"last_cycle"`
	Handoff *store.RunWorkerHandoff `json:"handoff"`
}

// projectTaskBoardLogRecord converts one raw record into its operator
// projection. A record that cannot be decoded keeps its raw text and reports
// the decode failure rather than disappearing.
func projectTaskBoardLogRecord(sequence int, raw string) TaskBoardLogRecord {
	record := TaskBoardLogRecord{Sequence: sequence, Raw: raw}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return record
	}
	if !strings.HasPrefix(trimmed, "{") {
		// Non-JSON text in a worker log is a launcher diagnostic, not a result
		// record. Surface it verbatim instead of calling it a parse failure.
		record.FailureSummary = firstLogLine(trimmed)
		return record
	}
	var probe struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		record.ParseError = err.Error()
		return record
	}
	if probe.Format == RunWorkerLogRecordFormat {
		return projectCompactWorkerLogRecord(record, trimmed)
	}
	return projectLegacyWorkerLogRecord(record, trimmed)
}

func projectCompactWorkerLogRecord(record TaskBoardLogRecord, trimmed string) TaskBoardLogRecord {
	var compact RunWorkerLogRecord
	if err := json.Unmarshal([]byte(trimmed), &compact); err != nil {
		record.ParseError = err.Error()
		return record
	}
	record.RunID = compact.RunID
	record.RunStatus = compact.RunStatus
	record.StoppedFor = compact.StoppedFor
	if !compact.ObservedAt.IsZero() {
		observed := compact.ObservedAt.UTC()
		record.ObservedAt = &observed
	}
	if cycle := compact.LastCycle; cycle != nil {
		record.CycleEmpty = cycle.Empty
		record.JobCommandType = cycle.CommandType
		record.JobState = cycle.State
		if record.JobState == "" {
			record.JobState = cycle.FinalState
		}
		record.StageAttemptID = cycle.StageAttemptID
		record.JobStartedAt = utcTimePointer(cycle.StartedAt)
		record.JobFinishedAt = utcTimePointer(cycle.FinishedAt)
		if failure := cycle.Failure; failure != nil {
			record.FailureCode = failure.Code
			record.FailureSummary = firstLogLine(failure.Message)
		}
	}
	if handoff := compact.Handoff; handoff != nil {
		record.HandoffSummary = handoffLogSummary(handoff.State, handoff.ProcessID)
	}
	return record
}

func projectLegacyWorkerLogRecord(record TaskBoardLogRecord, trimmed string) TaskBoardLogRecord {
	var envelope taskBoardLegacyWorkerEnvelope
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		record.ParseError = err.Error()
		return record
	}
	record.Legacy = true
	record.RunID = envelope.Run.ID
	record.RunStatus = envelope.Run.Status
	record.StoppedFor = envelope.StoppedFor
	record.CycleEmpty = envelope.LastCycle.Empty
	record.ObservedAt = firstPresentTime(envelope.Run.FinishedAt, envelope.Run.StartedAt, envelope.Run.CreatedAt)
	if job := envelope.LastCycle.Job; job != nil {
		record.JobCommandType = job.CommandType
		record.JobState = job.State
		record.StageAttemptID = job.StageAttemptID
		record.JobStartedAt = utcTimePointer(job.StartedAt)
		record.JobFinishedAt = utcTimePointer(job.FinishedAt)
		if record.ObservedAt == nil {
			record.ObservedAt = firstPresentTime(job.FinishedAt, job.StartedAt)
		}
		if job.Failure != nil {
			record.FailureCode = job.Failure.Code
			record.FailureSummary = firstLogLine(job.Failure.Message)
		}
	}
	if record.JobState == "" {
		record.JobState = envelope.LastCycle.FinalState
	}
	if handoff := envelope.Handoff; handoff != nil {
		record.HandoffSummary = handoffLogSummary(string(handoff.State), handoff.ProcessID)
	}
	return record
}

func handoffLogSummary(state string, processID int) string {
	state = strings.TrimSpace(state)
	if state == "" && processID <= 0 {
		return ""
	}
	if processID > 0 {
		if state == "" {
			return fmt.Sprintf("pid %d", processID)
		}
		return fmt.Sprintf("%s · pid %d", state, processID)
	}
	return state
}

func utcTimePointer(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	moment := value.UTC()
	return &moment
}

func firstPresentTime(values ...*time.Time) *time.Time {
	for _, value := range values {
		if moment := utcTimePointer(value); moment != nil {
			return moment
		}
	}
	return nil
}

func firstLogLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return strings.TrimSpace(value[:index])
	}
	return value
}

// splitTaskBoardLogRecords splits worker-log text into whole records. Every
// record is written by json.MarshalIndent, so its object opens on a line that
// is exactly "{" and closes on a line that is exactly "}". A record therefore
// starts where such an opening line appears.
//
// leadingPartial reports whether the text began inside a record; the caller
// drops that fragment and marks the read truncated rather than rendering a
// record whose head is missing.
func splitTaskBoardLogRecords(text string) (records []string, leadingPartial bool) {
	if strings.TrimSpace(text) == "" {
		return nil, false
	}
	var current []string
	flush := func() {
		if len(current) == 0 {
			return
		}
		joined := strings.TrimRight(strings.Join(current, "\n"), "\n")
		current = nil
		if strings.TrimSpace(joined) == "" {
			return
		}
		if !strings.HasPrefix(joined, "{") {
			// Text that precedes the first object boundary is the tail of a record
			// whose head was cut off by the scan window.
			leadingPartial = true
			if len(records) > 0 {
				// A launcher diagnostic between records is still whole text.
				records = append(records, joined)
			}
			return
		}
		records = append(records, joined)
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "{" {
			flush()
		}
		current = append(current, line)
	}
	flush()
	return records, leadingPartial
}

// taskBoardLogTail is the result of parsing a worker log's tail. Records being
// dropped and the raw fallback text being clipped are reported separately: they
// are different facts, and a reader that conflates them would tell an operator
// records are missing whenever a single large record exceeded the raw budget.
type taskBoardLogTail struct {
	// Records are whole, in order, oldest first.
	Records []TaskBoardLogRecord
	// Raw is the verbatim text the records were parsed from.
	Raw string
	// RecordsDropped means older records exist in the file but are not here.
	RecordsDropped bool
	// RawTruncated means Raw is a clipped view of those same records.
	RawTruncated bool
}

// readTaskBoardLogTail returns the trailing whole records of a worker log along
// with the verbatim text they were parsed from.
func readTaskBoardLogTail(text string, limit int) taskBoardLogTail {
	split, leadingPartial := splitTaskBoardLogRecords(text)
	if len(split) == 0 {
		// Nothing had a record boundary. Return the text verbatim so a log written
		// by something other than a worker process is still readable.
		return taskBoardLogTail{Raw: text}
	}
	tail := taskBoardLogTail{RecordsDropped: leadingPartial}
	if limit > 0 && len(split) > limit {
		split = split[len(split)-limit:]
		tail.RecordsDropped = true
	}
	tail.Records = make([]TaskBoardLogRecord, 0, len(split))
	for index, entry := range split {
		tail.Records = append(tail.Records, projectTaskBoardLogRecord(index+1, entry))
	}
	tail.Raw = strings.Join(split, "\n")
	if len(tail.Raw) > taskBoardLogRawLimit {
		tail.Raw = tail.Raw[len(tail.Raw)-taskBoardLogRawLimit:]
		if boundary := strings.IndexByte(tail.Raw, '\n'); boundary >= 0 {
			tail.Raw = tail.Raw[boundary+1:]
		}
		tail.RawTruncated = true
	}
	return tail
}

// taskBoardLogHandoffSummary describes the durable handoffs behind a log file so
// the terminal can state how many worker processes appended to it. A log that
// mixes records from several handoffs is normal for a recovered Run.
func taskBoardLogHandoffSummary(handoffs []store.RunWorkerHandoff) string {
	if len(handoffs) == 0 {
		return ""
	}
	latest := handoffs[len(handoffs)-1]
	state := strings.TrimSpace(string(latest.State))
	if state == "" {
		return fmt.Sprintf("%d 次 worker 交接", len(handoffs))
	}
	return fmt.Sprintf("%d 次 worker 交接 · 最近状态 %s", len(handoffs), state)
}
