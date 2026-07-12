package workflow

import "fmt"

type NodeError struct {
	Kind       FailureKind
	CanRetry   bool
	Operation  string
	Underlying error
}

func (e NodeError) Error() string {
	if e.Underlying == nil {
		if e.Operation == "" {
			return string(e.Kind)
		}
		return e.Operation
	}
	if e.Operation == "" {
		return e.Underlying.Error()
	}
	return fmt.Sprintf("%s: %v", e.Operation, e.Underlying)
}

func (e NodeError) Unwrap() error            { return e.Underlying }
func (e NodeError) FailureKind() FailureKind { return e.Kind }
func (e NodeError) Retryable() bool          { return e.CanRetry }

func NewNodeError(kind FailureKind, retryable bool, operation string, err error) error {
	if err == nil {
		return nil
	}
	return NodeError{Kind: kind, CanRetry: retryable, Operation: operation, Underlying: err}
}
