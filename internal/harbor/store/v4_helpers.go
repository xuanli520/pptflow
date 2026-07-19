package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

func normalizeV4JSON(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if !json.Valid([]byte(value)) {
		return "", fmt.Errorf("%s must contain valid JSON", field)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(value)); err != nil {
		return "", fmt.Errorf("compact %s: %w", field, err)
	}
	return compact.String(), nil
}

func v4PayloadDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func requireV4ID(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if !isUUIDv7(value) {
		return "", fmt.Errorf("%w: %s", ErrInvalidUUIDv7Identity, field)
	}
	return value, nil
}

func optionalV4ID(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	return requireV4ID(value, field)
}

func validRepairSessionState(state RepairSessionState) bool {
	switch state {
	case RepairSessionOpen, RepairSessionNeedsHuman, RepairSessionCompleted, RepairSessionCanceled:
		return true
	default:
		return false
	}
}

func validRepairSessionTransition(from, to RepairSessionState) bool {
	if from == to || from == RepairSessionCompleted || from == RepairSessionCanceled {
		return false
	}
	switch from {
	case RepairSessionOpen:
		return to == RepairSessionNeedsHuman || to == RepairSessionCompleted || to == RepairSessionCanceled
	case RepairSessionNeedsHuman:
		return to == RepairSessionOpen || to == RepairSessionCompleted || to == RepairSessionCanceled
	default:
		return false
	}
}

func validMutationReceiptOutcome(outcome MutationReceiptOutcome) bool {
	switch outcome {
	case MutationReceiptApplied, MutationReceiptNoOp, MutationReceiptUncertain, MutationReceiptFailed:
		return true
	default:
		return false
	}
}

func validContinuationExecutionState(state ContinuationExecutionState) bool {
	switch state {
	case ContinuationExecutionQueued, ContinuationExecutionRunning, ContinuationExecutionCompleted,
		ContinuationExecutionFailed, ContinuationExecutionCanceled, ContinuationExecutionReconcileRequired:
		return true
	default:
		return false
	}
}

func validContinuationExecutionTransition(from, to ContinuationExecutionState) bool {
	if from == to || isTerminalContinuationExecutionState(from) {
		return false
	}
	switch from {
	case ContinuationExecutionQueued:
		return to == ContinuationExecutionRunning || to == ContinuationExecutionCanceled || to == ContinuationExecutionReconcileRequired
	case ContinuationExecutionRunning:
		return to == ContinuationExecutionCompleted || to == ContinuationExecutionFailed ||
			to == ContinuationExecutionCanceled || to == ContinuationExecutionReconcileRequired
	case ContinuationExecutionReconcileRequired:
		return to == ContinuationExecutionCompleted || to == ContinuationExecutionFailed || to == ContinuationExecutionCanceled
	default:
		return false
	}
}

func isTerminalContinuationExecutionState(state ContinuationExecutionState) bool {
	switch state {
	case ContinuationExecutionCompleted, ContinuationExecutionFailed, ContinuationExecutionCanceled:
		return true
	default:
		return false
	}
}
