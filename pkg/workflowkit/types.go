// Package workflowkit contains domain-neutral building blocks for durable,
// resumable workflows. It deliberately has no knowledge of a particular
// subject domain, runtime, storage engine, or user interface.
package workflowkit

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

var (
	// ErrInvalidDescriptor marks invalid workflow and stage descriptors.
	ErrInvalidDescriptor = errors.New("workflowkit: invalid descriptor")
	// ErrPluginUnavailable marks a frozen plugin binding whose identifier is
	// not installed in the controlled process registry.
	ErrPluginUnavailable = errors.New("workflowkit: plugin binding is unavailable")
	// ErrPluginVersionMismatch marks a frozen plugin identifier that is known
	// to the registry but does not provide the requested immutable version.
	ErrPluginVersionMismatch = errors.New("workflowkit: plugin binding version mismatch")
	// ErrInvalidBudget marks an execution budget that cannot be safely run.
	ErrInvalidBudget = errors.New("workflowkit: invalid execution budget")
	// ErrInvalidAttemptRecord marks an invalid append-only attempt record.
	ErrInvalidAttemptRecord = errors.New("workflowkit: invalid attempt record")
	// ErrInvalidContinuationPlan marks an invalid frozen continuation plan.
	ErrInvalidContinuationPlan = errors.New("workflowkit: invalid continuation plan")
	// ErrInvalidArtifact marks an invalid artifact reference or binding.
	ErrInvalidArtifact = errors.New("workflowkit: invalid artifact")
)

// StageKey identifies one independently schedulable workflow stage. A stage
// can belong to a display group, but its key is the DAG identity.
type StageKey string

// NodeID is an alias for StageKey because a compiled workflow stage is the
// unit that receives a transition in a continuation plan.
type NodeID = StageKey

// ResourceKey identifies a declared logical resource. It is intentionally not
// a filesystem path; a domain adapter owns its vocabulary and matching rules.
type ResourceKey string

// ArtifactID identifies immutable artifact metadata in an artifact store.
type ArtifactID string

// AttemptID identifies one durable run, stage, node, or turn attempt.
type AttemptID string

// Fingerprint is a canonical SHA-256 value formatted as sha256:<hex>.
type Fingerprint string

// SubjectDigest is the immutable digest of a workflow subject revision. It is
// deliberately separate from Fingerprint: content objects and workflow
// definitions use a bare sha256:<hex>, while a domain may version its subject
// digest scheme (for example subject.v2:sha256:<hex>) without weakening
// object-addressing invariants.
type SubjectDigest string

func validateRequired(label, value string, marker error) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", marker, label)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s contains a control character", marker, label)
		}
	}
	return nil
}

func validateUniqueStrings[T ~string](label string, values []T, marker error) error {
	seen := make(map[T]struct{}, len(values))
	for _, value := range values {
		if err := validateRequired(label, string(value), marker); err != nil {
			return err
		}
		if _, ok := seen[value]; ok {
			return fmt.Errorf("%w: duplicate %s %q", marker, label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}
