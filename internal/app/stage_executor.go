package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var (
	// ErrStageExecutorUnavailable is an infrastructure failure, not a
	// successful no-op. A frozen V2 stage can never be marked completed until
	// a concrete executor has produced its declared evidence.
	ErrStageExecutorUnavailable = errors.New("v2 executor: stage executor is unavailable")
	// ErrInvalidStageExecution marks an executor result that cannot be bound to
	// the frozen descriptor or durable lineage records.
	ErrInvalidStageExecution = errors.New("v2 executor: invalid stage execution")
)

// StageArtifact is one immutable output produced by a V2 stage executor.
// Bytes are published into the managed object store by FrozenPlanExecutor;
// executors never receive a writable revision snapshot or direct database
// handle.
type StageArtifact struct {
	Key           string
	SchemaVersion string
	Content       []byte
	TurnOrdinal   int
}

func (artifact StageArtifact) clone() StageArtifact {
	artifact.Content = append([]byte(nil), artifact.Content...)
	return artifact
}

// StageCheckpoint is a durable substep fact supplied by an executor. The
// executor must call the callback only after its represented work is safe to
// resume or diagnose; the application layer records it against the current
// NodeAttempt before acknowledging a pause.
type StageCheckpoint struct {
	Turn        int
	Substep     string
	InputDigest string
	PayloadJSON string
	ArtifactID  string
	Resumable   bool
}

// StageControlSignal is a durable, target-scoped request delivered to an
// active executor before its frozen grace period expires. Executors that can
// checkpoint cooperatively should persist that checkpoint and return; runtimes
// cancel the execution context only after the grace period if it remains live.
type StageControlSignal struct {
	Action      store.ControlAction
	GracePeriod time.Duration
}

// StageUsage is one immutable billable fact emitted by a stage. OperationKey
// must identify the logical turn, trial, or provider operation so replaying a
// worker after process loss cannot double-charge the task or actor ledger.
type StageUsage struct {
	Dimension    string
	Units        int64
	OperationKey string
	OccurredAt   time.Time
}

// StageExecutionRequest is the complete V2-only input for an execution. It
// deliberately carries the frozen stage descriptor and immutable object
// bindings rather than a legacy workflow.NodeRequest, workspace identity, or
// mutable task directory.
type StageExecutionRequest struct {
	Run          store.WorkflowRun
	Revision     store.TaskRevision
	StageAttempt store.StageAttempt
	NodeAttempt  store.NodeAttempt
	Stage        workflowkit.StageDescriptor
	Inputs       []workflowkit.ArtifactBinding

	// Checkpoint persists a durable stage substep. It is safe to call more than
	// once for different turn/substep pairs and returns the persisted checkpoint
	// identity that control acknowledgements may reference.
	Checkpoint func(context.Context, StageCheckpoint) (store.TurnCheckpoint, error)

	// Control receives at most one target-scoped signal for this execution.
	// It is nil only in narrow unit tests that construct a request directly.
	// The channel never carries UI or scheduler-root cancellation state.
	Control <-chan StageControlSignal

	// Charge records actual frozen-dimension consumption. An executor receives
	// no way to invent claims or scopes: it can only consume a reservation that
	// the durable runtime admitted from Stage.QuotaClaims.
	Charge func(context.Context, StageUsage) error
}

func (request StageExecutionRequest) clone() StageExecutionRequest {
	request.Stage = request.Stage.Clone()
	request.Inputs = append([]workflowkit.ArtifactBinding(nil), request.Inputs...)
	return request
}

// StageExecutionResult is an actual terminal result. A completed result must
// include a verdict allowed by the frozen descriptor plus every required
// output. Infrastructure, interrupted, canceled, and in-doubt outcomes must
// not carry a verdict or manufactured artifacts.
type StageExecutionResult struct {
	Outcome      workflowkit.Outcome
	Artifacts    []StageArtifact
	ErrorText    string
	FailureClass string
	CheckpointID string
}

func (result StageExecutionResult) clone() StageExecutionResult {
	result.Artifacts = make([]StageArtifact, len(result.Artifacts))
	for index, artifact := range result.Artifacts {
		result.Artifacts[index] = artifact.clone()
	}
	return result
}

// StageExecutor executes one concrete stage implementation. The V2 runtime
// resolves it only through the frozen plugin ID and version carried by the
// stage descriptor; a stage key is scheduling identity, never an executable
// implementation selector.
type StageExecutor interface {
	ExecuteStage(context.Context, StageExecutionRequest) (StageExecutionResult, error)
}

// StageQuotaPlanner supplies the complete task+actor resource demand before a
// stage is invoked. It is deliberately separate from StageExecutor because
// admission must happen before an Agent turn, command, or other billable
// effect starts. A caller that intentionally has no demand still provides an
// explicit planner returning an empty claim set.
type StageQuotaPlanner interface {
	PlanStageQuota(context.Context, StageExecutionRequest) ([]store.TaskActorQuotaClaim, error)
}

// StageQuotaPlannerFunc adapts a local policy function into an explicit
// admission contract.
type StageQuotaPlannerFunc func(context.Context, StageExecutionRequest) ([]store.TaskActorQuotaClaim, error)

func (function StageQuotaPlannerFunc) PlanStageQuota(ctx context.Context, request StageExecutionRequest) ([]store.TaskActorQuotaClaim, error) {
	claims, err := function(ctx, request.clone())
	if err != nil {
		return nil, err
	}
	return NormalizeQuotaClaims(claims)
}

// StageExecutorFunc adapts a function into a StageExecutor for local
// providers and tests.
type StageExecutorFunc func(context.Context, StageExecutionRequest) (StageExecutionResult, error)

func (function StageExecutorFunc) ExecuteStage(ctx context.Context, request StageExecutionRequest) (StageExecutionResult, error) {
	return function(ctx, request.clone())
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
