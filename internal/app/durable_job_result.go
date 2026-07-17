package app

import (
	"encoding/json"
	"errors"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// DurableJobResult is the terminal delivery fact returned by a durable job
// handler. A failed or in-doubt result always carries a bounded, operator-safe
// failure record; successful and intentionally canceled deliveries do not.
//
// The returned error remains useful to the controlled worker supervisor, but
// it is not used to infer the durable failure meaning.
type DurableJobResult struct {
	State         store.JobState
	Failure       *store.DurableJobFailure
	RunProjection *store.DurableJobRunProjection
}

// DurableJobFailureError transports a deliberately bounded diagnostic from a
// domain handler to the durable worker. It keeps the original cause available
// to in-process callers without allowing that raw error text into the record
// rendered by CLI or TUI surfaces.
type DurableJobFailureError struct {
	Failure store.DurableJobFailure
	cause   error
}

func (err *DurableJobFailureError) Error() string {
	if err == nil {
		return "durable job failure"
	}
	if err.Failure.Code != "" {
		// The cause can contain provider, filesystem, or model output. It is
		// intentionally reachable through Unwrap for in-process classification,
		// but never rendered through the error string returned to callers.
		return err.Failure.Code
	}
	return "durable job failure"
}

func (err *DurableJobFailureError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func newDurableJobFailureError(failure store.DurableJobFailure, cause error) error {
	return &DurableJobFailureError{Failure: failure, cause: cause}
}

// durableJobHandlerError preserves error identity for supervision and tests
// without permitting an arbitrary handler error string to reach an operator
// surface. A failure record is preferred when the result has one because its
// code is the stable public diagnosis.
func durableJobHandlerError(result DurableJobResult, cause error) error {
	if cause == nil {
		return nil
	}
	if result.Failure != nil {
		return newDurableJobFailureError(*result.Failure, cause)
	}
	return &durableJobHandlerFailure{cause: cause}
}

type durableJobHandlerFailure struct{ cause error }

func (err *durableJobHandlerFailure) Error() string {
	return "durable job handler failed"
}

func (err *durableJobHandlerFailure) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func durableJobResultForOutcome(job store.DurableJob, state store.JobState, cause error) DurableJobResult {
	if state != store.JobFailed && state != store.JobInDoubt {
		return DurableJobResult{State: state}
	}
	if failure := durableJobFailureFromError(cause); failure != nil {
		return DurableJobResult{State: state, Failure: failure}
	}
	code, message := "job.execution_failed", "The durable job could not complete."
	if state == store.JobInDoubt {
		code, message = "job.execution_in_doubt", "The durable job outcome is unknown and requires an explicit recovery action."
	}
	return DurableJobResult{
		State:   state,
		Failure: newDurableJobFailure(code, message, durableJobFailureDetails(job, "execution")),
	}
}

func durableJobFailureFromError(cause error) *store.DurableJobFailure {
	var diagnostic *DurableJobFailureError
	if !errors.As(cause, &diagnostic) || diagnostic == nil {
		return nil
	}
	failure := diagnostic.Failure
	return &failure
}

func newDurableJobFailure(code, message string, details map[string]string) *store.DurableJobFailure {
	encoded, err := json.Marshal(details)
	if err != nil {
		encoded = []byte("{}")
	}
	return &store.DurableJobFailure{Code: code, Message: message, DetailsJSON: string(encoded)}
}

func durableJobFailureDetails(job store.DurableJob, check string) map[string]string {
	details := map[string]string{"job_id": job.ID, "check": check}
	if job.RunID != "" {
		details["run_id"] = job.RunID
	}
	if job.StageAttemptID != "" {
		details["stage_attempt_id"] = job.StageAttemptID
	}
	if job.EntityType == "artifact_ref" && job.EntityID != "" {
		details["artifact_id"] = job.EntityID
	}
	return details
}
