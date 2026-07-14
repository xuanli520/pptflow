// Package stageprovider binds Harbor's typed, frozen execution specification
// to controlled generic workflowkit stage providers. It intentionally has no
// dependency on internal/app, CLI, TUI, or a persistence implementation.
package stageprovider

import "errors"

var (
	// ErrInvalidStageOperation marks an invalid static operation registration
	// or a request that violates the frozen Harbor stage contract.
	ErrInvalidStageOperation = errors.New("harbor stage provider: invalid stage operation")
	// ErrProviderUnavailable marks a frozen provider for which this worker has
	// no controlled registration.
	ErrProviderUnavailable = errors.New("harbor stage provider: provider is unavailable")
	// ErrProviderVersionMismatch distinguishes a known provider ID from an
	// unavailable exact provider version or kind.
	ErrProviderVersionMismatch = errors.New("harbor stage provider: provider version mismatch")
	// ErrStageOperationUnavailable marks an unavailable exact stage operation.
	ErrStageOperationUnavailable = errors.New("harbor stage provider: stage operation is unavailable")
	// ErrFrozenOperationPayloadMismatch marks an operation identity whose
	// installed typed payload differs from the canonical payload frozen in the
	// execution specification.
	ErrFrozenOperationPayloadMismatch = errors.New("harbor stage provider: frozen operation payload mismatch")
	// ErrFrozenExecutionSpec marks a missing, malformed, noncanonical, or
	// subject-mismatched opaque Harbor execution specification.
	ErrFrozenExecutionSpec = errors.New("harbor stage provider: invalid frozen execution specification")
)
