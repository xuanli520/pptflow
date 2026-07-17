package app

import (
	"errors"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

const (
	handoffDefinitionUnavailableCode   = "handoff.definition_unavailable"
	handoffDefinitionInvalidCode       = "handoff.definition_invalid"
	handoffStorageUnavailableCode      = "handoff.storage_unavailable"
	handoffArtifactLineageInvalidCode  = "handoff.artifact_lineage_invalid"
	handoffAdmissionReceiptMissingCode = "handoff.admission_receipt_missing"
	handoffSnapshotDigestMismatchCode  = "handoff.snapshot_digest_mismatch"
	handoffMaterializationInvalidCode  = "handoff.materialization_invalid"
)

// standardAuthoringHandoffFailure is a typed, safe diagnosis for the durable
// handoff boundary. Its cause remains available to in-process callers through
// Unwrap, while only Code, Message, and Details are persisted on the job.
type standardAuthoringHandoffFailure struct {
	state   store.JobState
	code    string
	message string
	details map[string]string
	cause   error
}

func (failure *standardAuthoringHandoffFailure) Error() string {
	if failure == nil {
		return "Standard authoring handoff failed"
	}
	return failure.code + ": " + failure.message
}

func (failure *standardAuthoringHandoffFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

func newStandardAuthoringHandoffFailure(state store.JobState, code, message string, details map[string]string, cause error) error {
	return &standardAuthoringHandoffFailure{
		state: state, code: code, message: message, details: details, cause: cause,
	}
}

func handoffFailureDetails(request StandardAuthoringHandoffRequest, check string) map[string]string {
	details := map[string]string{"check": check}
	if request.AuthoringRunID != "" {
		details["run_id"] = request.AuthoringRunID
	}
	if request.StageAttemptID != "" {
		details["stage_attempt_id"] = request.StageAttemptID
	}
	if request.HandoffArtifactID != "" {
		details["artifact_id"] = request.HandoffArtifactID
	}
	if request.ChildRunID != "" {
		details["child_run_id"] = request.ChildRunID
	}
	return details
}

func handoffStorageFailure(request StandardAuthoringHandoffRequest, check string, cause error) error {
	return newStandardAuthoringHandoffFailure(
		store.JobInDoubt,
		handoffStorageUnavailableCode,
		"The persisted Standard authoring handoff could not be read safely.",
		handoffFailureDetails(request, check),
		cause,
	)
}

func handoffDeterministicFailure(request StandardAuthoringHandoffRequest, code, message, check string, cause error) error {
	return newStandardAuthoringHandoffFailure(
		store.JobFailed,
		code,
		message,
		handoffFailureDetails(request, check),
		cause,
	)
}

func handoffDefinitionFailure(request StandardAuthoringHandoffRequest, code, message string, cause error) error {
	return newStandardAuthoringHandoffFailure(
		store.JobInDoubt,
		code,
		message,
		handoffFailureDetails(request, "definition"),
		cause,
	)
}

func handoffFailureResult(job store.DurableJob, cause error) (DurableJobResult, bool) {
	var diagnostic *standardAuthoringHandoffFailure
	if !errors.As(cause, &diagnostic) || diagnostic == nil {
		return DurableJobResult{}, false
	}
	details := durableJobFailureDetails(job, "handoff")
	for key, value := range diagnostic.details {
		if value != "" {
			details[key] = value
		}
	}
	failure := newDurableJobFailure(diagnostic.code, diagnostic.message, details)
	return DurableJobResult{
		State:   diagnostic.state,
		Failure: failure,
	}, true
}

func handoffFailureErrorForWorker(job store.DurableJob, cause error) (store.JobState, error, bool) {
	result, ok := handoffFailureResult(job, cause)
	if !ok {
		return "", nil, false
	}
	return result.State, newDurableJobFailureError(*result.Failure, cause), true
}

func isRecoverableHandoffFailure(failure *store.DurableJobFailure) bool {
	if failure == nil {
		return false
	}
	switch failure.Code {
	case handoffDefinitionUnavailableCode, handoffDefinitionInvalidCode, handoffStorageUnavailableCode:
		return true
	default:
		return false
	}
}

func durableJobFailureCode(failure *store.DurableJobFailure) string {
	if failure == nil {
		return ""
	}
	return failure.Code
}
