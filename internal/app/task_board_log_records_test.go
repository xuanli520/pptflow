package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// legacyWorkerEnvelope builds the bytes an older worker actually wrote: the
// whole RunWorkerSessionResult, whose RunManifestJSON dominated the record.
func legacyWorkerEnvelope(t *testing.T, runID string, status store.WorkflowRunStatus, manifestPadding int) string {
	t.Helper()
	started := time.Date(2026, 8, 7, 7, 43, 25, 0, time.UTC)
	result := RunWorkerSessionResult{
		Run: store.WorkflowRun{
			ID: runID, Status: status, StartedAt: &started, CreatedAt: started,
			ExecutionEpoch: 2, DefinitionHash: "sha256:definition",
			// This is the field that made a single record exceed the old 64 KiB
			// read window on its own.
			RunManifestJSON: `{"pad":"` + strings.Repeat("m", manifestPadding) + `"}`,
		},
		WorkerLease: store.Lease{ID: "lease-1", Owner: "worker-1", State: store.LeaseActive, FencingToken: 7},
		LastCycle: DurableWorkerResult{
			FinalState: store.JobSucceeded,
			Job: &store.DurableJob{
				ID: "job-1", CommandType: "stage_attempt.execute", State: store.JobSucceeded,
				StageAttemptID: "stage-1", StartedAt: &started, FinishedAt: &started,
				PayloadJSON: `{"pad":"` + strings.Repeat("p", 256) + `"}`,
			},
		},
		StoppedFor: store.WorkflowRunWaitingReview,
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("encode legacy worker envelope: %v", err)
	}
	return string(encoded)
}

// TestReadTaskBoardLogTailReturnsWholeLegacyRecords is the regression guard for
// the defect where a fixed 64 KiB byte window started mid-record, so the
// terminal could only ever show a JSON fragment.
func TestReadTaskBoardLogTailReturnsWholeLegacyRecords(t *testing.T) {
	// Each record is larger than the old read limit by itself.
	first := legacyWorkerEnvelope(t, "019f65fb-7270-74f8-8a04-1a50c12c7cae", store.WorkflowRunRunning, 70*1024)
	second := legacyWorkerEnvelope(t, "019f65fb-7270-74f8-8a04-1a50c12c7cae", store.WorkflowRunWaitingReview, 70*1024)
	if len(first) < taskBoardLogRawLimit {
		t.Fatalf("fixture record is %d bytes, expected to exceed the %d byte window", len(first), taskBoardLogRawLimit)
	}

	tail := readTaskBoardLogTail(first+"\n"+second+"\n", taskBoardLogRecordLimit)
	records := tail.Records
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	// Both records are whole even though they jointly exceed the raw byte budget.
	// These two facts are reported separately on purpose: a clipped raw fallback
	// must not make the terminal claim that records are missing.
	if tail.RecordsDropped {
		t.Error("a complete two-record log reported dropped records")
	}
	if !tail.RawTruncated {
		t.Errorf("raw fallback of %d bytes was not reported clipped at the %d byte cap", len(first)+len(second), taskBoardLogRawLimit)
	}
	for _, record := range records {
		if record.ParseError != "" {
			t.Errorf("record %d parse error: %s", record.Sequence, record.ParseError)
		}
		if !record.Legacy {
			t.Errorf("record %d was not marked legacy", record.Sequence)
		}
		if record.RunID != "019f65fb-7270-74f8-8a04-1a50c12c7cae" {
			t.Errorf("record %d run ID = %q", record.Sequence, record.RunID)
		}
		if record.JobCommandType != "stage_attempt.execute" || record.JobState != string(store.JobSucceeded) {
			t.Errorf("record %d cycle = %q/%q", record.Sequence, record.JobCommandType, record.JobState)
		}
		if record.StageAttemptID != "stage-1" {
			t.Errorf("record %d stage attempt = %q", record.Sequence, record.StageAttemptID)
		}
		if record.ObservedAt == nil {
			t.Errorf("record %d has no observed time", record.Sequence)
		}
	}
	if records[1].RunStatus != string(store.WorkflowRunWaitingReview) {
		t.Errorf("last record run status = %q", records[1].RunStatus)
	}
	if records[1].StoppedFor != string(store.WorkflowRunWaitingReview) {
		t.Errorf("last record stopped_for = %q", records[1].StoppedFor)
	}
}

// TestReadTaskBoardLogTailDropsLeadingFragment pins that a scan window opening
// inside a record never yields a headless record.
func TestReadTaskBoardLogTailDropsLeadingFragment(t *testing.T) {
	whole := legacyWorkerEnvelope(t, "019f65fb-7270-74f8-8a04-1a50c12c7cae", store.WorkflowRunSucceeded, 128)
	fragment := "    \"Status\": \"running\"\n  }\n}\n"

	tail := readTaskBoardLogTail(fragment+whole+"\n", taskBoardLogRecordLimit)
	records, truncated := tail.Records, tail.RecordsDropped
	if !truncated {
		t.Error("a log whose head was cut was not reported truncated")
	}
	if len(records) != 1 {
		t.Fatalf("record count = %d, want only the whole record", len(records))
	}
	if records[0].ParseError != "" {
		t.Errorf("whole record reported a parse error: %s", records[0].ParseError)
	}
}

// TestReadTaskBoardLogTailProjectsCompactRecords covers the format a current
// worker writes, which carries no run manifest or job payload at all.
func TestReadTaskBoardLogTailProjectsCompactRecords(t *testing.T) {
	started := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	source := RunWorkerSessionResult{
		Run: store.WorkflowRun{
			ID: "019f65fb-7270-74f8-8a04-1a50c12c7cae", Status: store.WorkflowRunFailedRecoverable,
			ExecutionEpoch: 3, DefinitionHash: "sha256:definition",
			RunManifestJSON: `{"pad":"` + strings.Repeat("m", 70*1024) + `"}`,
		},
		WorkerLease: store.Lease{ID: "lease-1", Owner: "worker-1", State: store.LeaseActive},
		LastCycle: DurableWorkerResult{
			FinalState: store.JobFailed,
			Job: &store.DurableJob{
				ID: "job-9", CommandType: "stage_attempt.execute", State: store.JobFailed,
				StageAttemptID: "stage-9", StartedAt: &started, FinishedAt: &started,
				PayloadJSON: `{"pad":"` + strings.Repeat("p", 4096) + `"}`,
				Failure:     &store.DurableJobFailure{Code: "agent.protocol_rejected", Message: "declared claims did not match\nsecond line"},
			},
		},
		StoppedFor: store.WorkflowRunFailedRecoverable,
	}
	encoded, err := json.MarshalIndent(NewRunWorkerLogRecord(source), "", "  ")
	if err != nil {
		t.Fatalf("encode compact record: %v", err)
	}
	text := string(encoded)

	// The projection is what makes the log readable: it must not carry the two
	// fields that previously dominated every record.
	for _, forbidden := range []string{"RunManifestJSON", "PayloadJSON", strings.Repeat("m", 64), strings.Repeat("p", 64)} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("compact record leaked %q", forbidden)
		}
	}

	records := readTaskBoardLogTail(text+"\n", taskBoardLogRecordLimit).Records
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	record := records[0]
	if record.Legacy {
		t.Error("a compact record was marked legacy")
	}
	if record.ParseError != "" {
		t.Fatalf("compact record parse error: %s", record.ParseError)
	}
	if record.RunStatus != string(store.WorkflowRunFailedRecoverable) || record.StoppedFor != string(store.WorkflowRunFailedRecoverable) {
		t.Errorf("compact record status = %q / %q", record.RunStatus, record.StoppedFor)
	}
	if record.JobCommandType != "stage_attempt.execute" || record.JobState != string(store.JobFailed) {
		t.Errorf("compact record cycle = %q / %q", record.JobCommandType, record.JobState)
	}
	if record.FailureCode != "agent.protocol_rejected" {
		t.Errorf("compact record failure code = %q", record.FailureCode)
	}
	if record.FailureSummary != "declared claims did not match" {
		t.Errorf("compact record failure summary = %q, want only the first line", record.FailureSummary)
	}
}

// TestReadTaskBoardLogTailKeepsNonRecordTextReadable pins that a log holding
// launcher diagnostics rather than records is still returned verbatim.
func TestReadTaskBoardLogTailKeepsNonRecordTextReadable(t *testing.T) {
	text := "controlled child parent handle release warning: no such process\n"
	tail := readTaskBoardLogTail(text, taskBoardLogRecordLimit)
	records, raw, truncated := tail.Records, tail.Raw, tail.RecordsDropped
	if len(records) != 0 {
		t.Fatalf("record count = %d, want 0", len(records))
	}
	if raw != text {
		t.Errorf("raw text = %q, want the original bytes", raw)
	}
	if truncated {
		t.Error("short diagnostic text was reported truncated")
	}
}

// TestReadTaskBoardLogTailBoundsRecordCount pins the record cap so a long-lived
// Run cannot push an unbounded amount of text into a terminal.
func TestReadTaskBoardLogTailBoundsRecordCount(t *testing.T) {
	var builder strings.Builder
	for index := 0; index < taskBoardLogRecordLimit+5; index++ {
		builder.WriteString(legacyWorkerEnvelope(t, "019f65fb-7270-74f8-8a04-1a50c12c7cae", store.WorkflowRunRunning, 32))
		builder.WriteString("\n")
	}
	tail := readTaskBoardLogTail(builder.String(), taskBoardLogRecordLimit)
	records, raw, truncated := tail.Records, tail.Raw, tail.RecordsDropped
	if len(records) != taskBoardLogRecordLimit {
		t.Fatalf("record count = %d, want the %d record cap", len(records), taskBoardLogRecordLimit)
	}
	if !truncated {
		t.Error("a log beyond the record cap was not reported truncated")
	}
	if len(raw) > taskBoardLogRawLimit {
		t.Errorf("raw text = %d bytes, exceeds the %d byte cap", len(raw), taskBoardLogRawLimit)
	}
	if records[0].Sequence != 1 || records[len(records)-1].Sequence != taskBoardLogRecordLimit {
		t.Errorf("sequence range = %d..%d", records[0].Sequence, records[len(records)-1].Sequence)
	}
}

// TestProjectTaskBoardLogRecordReportsUndecodableRecord pins that a record which
// cannot be decoded still reaches the operator with its raw text.
func TestProjectTaskBoardLogRecordReportsUndecodableRecord(t *testing.T) {
	record := projectTaskBoardLogRecord(1, "{\n  \"run\": {\n")
	if record.ParseError == "" {
		t.Error("an undecodable record reported no parse error")
	}
	if record.Raw == "" {
		t.Error("an undecodable record dropped its raw text")
	}
}
