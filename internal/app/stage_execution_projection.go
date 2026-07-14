package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var (
// ErrInvalidStageExecution marks a malformed Harbor-side durable projection
	// (artifact, checkpoint, usage, or control fact). Concrete executable
	// results are validated by workflowkit.Engine before this adapter receives
	// them.
	ErrInvalidStageExecution = errors.New("v2 executor: invalid stage execution")
)

// StageArtifact is one immutable Harbor persistence projection of a public
// workflowkit stage output. Bytes are published into the managed object store
// by the durable adapter; public executors never receive a writable revision
// snapshot or direct database handle.
type StageArtifact struct {
	Key           string
	SchemaVersion string
	Content       []byte
	TurnOrdinal   int
}

// StageCheckpoint is a Harbor durable substep projection converted from a
// public workflowkit checkpoint callback.
type StageCheckpoint struct {
	Turn        int
	Substep     string
	InputDigest string
	PayloadJSON string
	ArtifactID  string
	Resumable   bool
}

// StageControlSignal is a Harbor durable, target-scoped control projection.
// workflowkitControlSignals adapts it to the public Engine request contract.
type StageControlSignal struct {
	Action      store.ControlAction
	GracePeriod time.Duration
}

// StageUsage is one immutable Harbor billable projection converted from a
// public workflowkit usage callback.
type StageUsage struct {
	Dimension    string
	Units        int64
	OperationKey string
	OccurredAt   time.Time
}

// StageExecutionResult is the Harbor durable projection of a public
// workflowkit terminal result. The public Engine validates executor output;
// the application layer adds only persistence-oriented failure/checkpoint
// fields used by its state machine.
type StageExecutionResult struct {
	Outcome      workflowkit.Outcome
	Artifacts    []StageArtifact
	ErrorText    string
	FailureClass string
	CheckpointID string
}

// RequiredStageArtifacts validates an actual completed result against the
// frozen output contract. Optional outputs may be omitted; duplicate output
// keys or schema drift are always rejected.
func RequiredStageArtifacts(stage workflowkit.StageDescriptor, artifacts []StageArtifact) error {
	declared := make(map[string]workflowkit.ArtifactSpec, len(stage.Outputs))
	for _, output := range stage.Outputs {
		declared[output.Name] = output
	}
	seen := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		key := strings.TrimSpace(artifact.Key)
		if key == "" {
			return fmt.Errorf("%w: output artifact key is required", ErrInvalidStageExecution)
		}
		specification, exists := declared[key]
		if !exists {
			return fmt.Errorf("%w: stage %q returned undeclared output %q", ErrInvalidStageExecution, stage.Key, key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: stage %q returned duplicate output %q", ErrInvalidStageExecution, stage.Key, key)
		}
		if strings.TrimSpace(artifact.SchemaVersion) != specification.SchemaVersion {
			return fmt.Errorf("%w: stage %q output %q schema %q, want %q", ErrInvalidStageExecution, stage.Key, key, artifact.SchemaVersion, specification.SchemaVersion)
		}
		if artifact.TurnOrdinal < 0 {
			return fmt.Errorf("%w: stage %q output %q has negative turn ordinal", ErrInvalidStageExecution, stage.Key, key)
		}
		seen[key] = struct{}{}
	}
	for key, specification := range declared {
		if specification.Required {
			if _, exists := seen[key]; !exists {
				return fmt.Errorf("%w: stage %q omitted required output %q", ErrInvalidStageExecution, stage.Key, key)
			}
		}
	}
	return nil
}

// NormalizeQuotaClaims validates a stable order for the task+actor claims an
// executor wants admitted before work begins. The executor does not set scope:
// the V2 worker derives task and local actor scopes from the frozen run.
func NormalizeQuotaClaims(claims []store.TaskActorQuotaClaim) ([]store.TaskActorQuotaClaim, error) {
	copyClaims := append([]store.TaskActorQuotaClaim(nil), claims...)
	sort.Slice(copyClaims, func(left, right int) bool {
		if copyClaims[left].Dimension != copyClaims[right].Dimension {
			return copyClaims[left].Dimension < copyClaims[right].Dimension
		}
		if copyClaims[left].Units != copyClaims[right].Units {
			return copyClaims[left].Units < copyClaims[right].Units
		}
		return copyClaims[left].ReclaimPolicy < copyClaims[right].ReclaimPolicy
	})
	for index, claim := range copyClaims {
		if strings.TrimSpace(claim.Dimension) == "" || claim.Units <= 0 || (claim.ReclaimPolicy != store.QuotaReclaimUnused && claim.ReclaimPolicy != store.QuotaReclaimNever) {
			return nil, fmt.Errorf("%w: invalid quota claim", ErrInvalidStageExecution)
		}
		if index > 0 && copyClaims[index-1].Dimension == claim.Dimension {
			return nil, fmt.Errorf("%w: duplicate quota claim dimension %q", ErrInvalidStageExecution, claim.Dimension)
		}
	}
	return copyClaims, nil
}

// RetryDelay returns the explicit retry delay specified by the frozen stage
// budget. It is separate from a plugin's own retry policy so repeated worker
// processes cannot accidentally multiply retries or invent an exponential
// backoff that was not frozen into the run manifest.
func RetryDelay(stage workflowkit.StageDescriptor, attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	index := attempt - 2
	if index < 0 || index >= len(stage.Budget.Backoff.RetryDelays) {
		return 0
	}
	return stage.Budget.Backoff.RetryDelays[index]
}
