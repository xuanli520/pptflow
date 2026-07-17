package store

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
)

const (
	maxDurableJobFailureCodeLength    = 160
	maxDurableJobFailureMessageLength = 512
	maxFailureDetailTokenLength       = 160
)

var (
	durableJobFailureCodePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	durableJobFailureDigestPattern = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9._-]*:)?sha256:[a-f0-9]{64}$`)

	// Details are an inspectable diagnostic index, not an error dump. Keep the
	// vocabulary intentionally small so the store cannot become a sink for
	// paths, credentials, logs, or model output.
	durableJobFailureDetailKeys = map[string]struct{}{
		"actual_digest": {}, "artifact_digest": {}, "artifact_id": {},
		"attempt_id": {}, "check": {}, "check_id": {}, "check_name": {},
		"child_run_id": {}, "definition_digest": {}, "definition_id": {}, "digest": {},
		"expected_digest": {}, "job_id": {}, "manifest_digest": {},
		"manifest_id": {}, "node_attempt_id": {}, "node_id": {}, "operation_id": {},
		"parent_job_id": {}, "parent_run_id": {}, "receipt_digest": {}, "receipt_id": {},
		"revision_id": {}, "run_id": {}, "snapshot_digest": {}, "snapshot_id": {},
		"source_digest": {}, "source_id": {}, "stage": {}, "stage_attempt_id": {},
		"stage_id": {}, "stage_key": {}, "task_id": {},
	}

	// These fields name durable control-plane records, whose identifiers are
	// canonical UUIDv7 values. Keeping them typed prevents a caller from
	// disguising arbitrary diagnostic prose as an otherwise allowed ID.
	durableJobFailureUUIDDetailKeys = map[string]struct{}{
		"artifact_id": {}, "attempt_id": {}, "child_run_id": {}, "job_id": {},
		"manifest_id": {}, "node_attempt_id": {}, "operation_id": {},
		"parent_job_id": {}, "parent_run_id": {}, "receipt_id": {},
		"revision_id": {}, "run_id": {}, "snapshot_id": {}, "source_id": {},
		"stage_attempt_id": {}, "task_id": {},
	}

	durableJobFailureSensitiveKeyParts = []string{
		"api_key", "apikey", "authorization", "command", "content", "cookie", "credential",
		"directory", "env", "file", "log", "model", "output", "password", "path", "payload",
		"private", "prompt", "response", "secret", "stack", "stderr", "stdout", "token", "trace", "url", "uri",
	}
)

func normalizeDurableJobFailure(failure *DurableJobFailure) (*DurableJobFailure, error) {
	if failure == nil {
		return nil, nil
	}
	code := strings.TrimSpace(failure.Code)
	if code == "" || len(code) > maxDurableJobFailureCodeLength || !durableJobFailureCodePattern.MatchString(code) {
		return nil, fmt.Errorf("%w: failure code must be a lowercase machine code", ErrInvalidJobFailure)
	}
	message := strings.TrimSpace(failure.Message)
	if message == "" || len(message) > maxDurableJobFailureMessageLength || containsUnsafeDiagnosticText(message) {
		return nil, fmt.Errorf("%w: failure message must be a short redacted summary", ErrInvalidJobFailure)
	}
	detailsJSON, err := normalizeDurableJobFailureDetails(failure.DetailsJSON)
	if err != nil {
		return nil, err
	}
	return &DurableJobFailure{Code: code, Message: message, DetailsJSON: detailsJSON}, nil
}

func normalizeDurableJobFailureDetails(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}", nil
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var details map[string]any
	if err := decoder.Decode(&details); err != nil {
		return "", fmt.Errorf("%w: failure details must be a JSON object", ErrInvalidJobFailure)
	}
	if details == nil {
		return "", fmt.Errorf("%w: failure details must be a JSON object", ErrInvalidJobFailure)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", fmt.Errorf("%w: failure details must contain one JSON object", ErrInvalidJobFailure)
	}
	for key, detail := range details {
		if err := validateDurableJobFailureDetail(key, detail); err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return "", fmt.Errorf("%w: serialize failure details", ErrInvalidJobFailure)
	}
	return string(encoded), nil
}

func validateDurableJobFailureDetail(key string, value any) error {
	normalizedKey := strings.ToLower(strings.TrimSpace(key))
	if normalizedKey != key || containsSensitiveDiagnosticKey(normalizedKey) {
		return fmt.Errorf("%w: failure details key is not permitted", ErrInvalidJobFailure)
	}
	if _, ok := durableJobFailureDetailKeys[normalizedKey]; !ok {
		return fmt.Errorf("%w: failure details key is not permitted", ErrInvalidJobFailure)
	}
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("%w: failure details values must be machine-readable strings", ErrInvalidJobFailure)
	}
	if text == "" || strings.TrimSpace(text) != text || len(text) > maxFailureDetailTokenLength || containsUnsafeDiagnosticText(text) {
		return fmt.Errorf("%w: failure details contain unsafe diagnostic text", ErrInvalidJobFailure)
	}
	if durableJobFailureDigestKey(normalizedKey) {
		if !durableJobFailureDigestPattern.MatchString(text) {
			return fmt.Errorf("%w: failure digest must be canonical", ErrInvalidJobFailure)
		}
		return nil
	}
	if _, requiresUUID := durableJobFailureUUIDDetailKeys[normalizedKey]; requiresUUID {
		if !isUUIDv7(text) {
			return fmt.Errorf("%w: failure identifier must be UUIDv7", ErrInvalidJobFailure)
		}
		return nil
	}
	if !durableJobFailureCodePattern.MatchString(text) {
		return fmt.Errorf("%w: failure detail must be a compact identifier", ErrInvalidJobFailure)
	}
	return nil
}

func durableJobFailureDigestKey(key string) bool {
	return key == "digest" || strings.HasSuffix(key, "_digest")
}

func containsSensitiveDiagnosticKey(key string) bool {
	for _, part := range durableJobFailureSensitiveKeyParts {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}

func containsUnsafeDiagnosticText(value string) bool {
	if strings.Contains(value, "/") || strings.Contains(value, `\`) {
		return true
	}
	for _, runeValue := range value {
		if unicode.IsControl(runeValue) {
			return true
		}
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"api_key", "apikey", "authorization", "bearer ", "credential", "password", "private key", "secret", "sk-", "ghp_", "github_pat_", "akia", "asia", "eyj"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func durableJobFailureFromColumns(code, message, detailsJSON string) (*DurableJobFailure, error) {
	if code == "" && message == "" && detailsJSON == "{}" {
		return nil, nil
	}
	return normalizeDurableJobFailure(&DurableJobFailure{Code: code, Message: message, DetailsJSON: detailsJSON})
}

func durableJobFailureColumns(failure *DurableJobFailure) (code, message, detailsJSON string) {
	if failure == nil {
		return "", "", "{}"
	}
	return failure.Code, failure.Message, failure.DetailsJSON
}
