package workflowruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var (
	// ErrInvalidRuntimeScope marks a run or stage scope that cannot own a
	// durable worker or cancellation token.
	ErrInvalidRuntimeScope = errors.New("workflowruntime: invalid runtime scope")
	// ErrInvalidRuntimeControl marks an invalid control request or receipt.
	ErrInvalidRuntimeControl = errors.New("workflowruntime: invalid runtime control")
	// ErrChildWorkerNotFound marks a control request whose scoped worker has not
	// been started by this runtime.
	ErrChildWorkerNotFound = errors.New("workflowruntime: child worker not found")
	// ErrChildWorkerStopped marks a new control request for a worker that has
	// already produced a terminal receipt.
	ErrChildWorkerStopped = errors.New("workflowruntime: child worker already stopped")
	// ErrRuntimeControlConflict marks reuse of an operation key for a different
	// target or action.
	ErrRuntimeControlConflict = errors.New("workflowruntime: runtime control conflict")
	// ErrCheckpointRejected marks a pause request for which a resumable
	// checkpoint was not acknowledged. The worker remains live in this case.
	ErrCheckpointRejected = errors.New("workflowruntime: checkpoint was not acknowledged")
	// ErrGracefulTermination marks a worker that did not finish after its
	// graceful stop request and could not be conclusively force-terminated.
	ErrGracefulTermination = errors.New("workflowruntime: graceful termination failed")
)

// RunScope is a typed identity for one runtime-owned workflow run. It is
// deliberately separate from an application task or workspace identity.
type RunScope struct {
	RunID string `json:"run_id"`
}

// Validate verifies that this scope can safely identify an in-memory runtime
// token and a durable control request.
func (scope RunScope) Validate() error {
	if err := validateIdentifier("run id", scope.RunID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRuntimeScope, err)
	}
	return nil
}

// StageScope is the independent cancellation and process scope for a single
// stage attempt. Stage attempts, rather than display-stage names alone, are
// used because retries and continuations must not share a cancellation token.
type StageScope struct {
	Run            RunScope              `json:"run"`
	StageAttemptID workflowkit.AttemptID `json:"stage_attempt_id"`
	StageKey       workflowkit.StageKey  `json:"stage_key"`
}

// Validate verifies a complete run/stage execution scope.
func (scope StageScope) Validate() error {
	if err := scope.Run.Validate(); err != nil {
		return err
	}
	if err := validateIdentifier("stage attempt id", string(scope.StageAttemptID)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRuntimeScope, err)
	}
	if err := validateIdentifier("stage key", string(scope.StageKey)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRuntimeScope, err)
	}
	return nil
}

// RuntimeControlAction identifies a scoped action propagated to one child
// worker. Pause is resumable only after a checkpoint acknowledgement;
// terminate ends the current execution regardless of checkpoint outcome.
type RuntimeControlAction string

const (
	RuntimeControlPause     RuntimeControlAction = "pause"
	RuntimeControlTerminate RuntimeControlAction = "terminate"
)

func (action RuntimeControlAction) valid() bool {
	return action == RuntimeControlPause || action == RuntimeControlTerminate
}

// CancellationIntent is observable through a typed cancellation token before
// its context is canceled. A worker can use this to distinguish an explicit
// pause from an ordinary process interruption while it is checkpointing.
type CancellationIntent string

const (
	CancellationNone      CancellationIntent = ""
	CancellationPause     CancellationIntent = "pause"
	CancellationTerminate CancellationIntent = "terminate"
)

func intentFor(action RuntimeControlAction) CancellationIntent {
	if action == RuntimeControlPause {
		return CancellationPause
	}
	return CancellationTerminate
}

type runTokenState struct {
	scope  RunScope
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.RWMutex
	intent CancellationIntent
}

type stageTokenState struct {
	scope  StageScope
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.RWMutex
	intent CancellationIntent
}

// RunCancellationToken is an opaque, runtime-owned cancellation token for a
// single run. It has no exported cancel method, preventing a stage or UI from
// accidentally canceling sibling runs.
type RunCancellationToken struct {
	state *runTokenState
}

// Valid reports whether this token came from ControlledChildRuntime.
func (token RunCancellationToken) Valid() bool { return token.state != nil }

// Scope returns the run this token can affect.
func (token RunCancellationToken) Scope() RunScope {
	if token.state == nil {
		return RunScope{}
	}
	return token.state.scope
}

// Context is canceled only by a run-scoped termination or runtime shutdown;
// it never derives from a TUI or scheduler root context.
func (token RunCancellationToken) Context() context.Context {
	if token.state == nil {
		return canceledRuntimeContext()
	}
	return token.state.ctx
}

// Done exposes this run's cancellation signal.
func (token RunCancellationToken) Done() <-chan struct{} { return token.Context().Done() }

// Err returns the run token's context error after cancellation.
func (token RunCancellationToken) Err() error { return token.Context().Err() }

// Intent reports the explicit control intent, if any, associated with this
// run token.
func (token RunCancellationToken) Intent() CancellationIntent {
	if token.state == nil {
		return CancellationNone
	}
	token.state.mu.RLock()
	defer token.state.mu.RUnlock()
	return token.state.intent
}

// StageCancellationToken is an opaque token for exactly one stage attempt. A
// stage cancellation cannot cancel its parent run or any sibling stage.
type StageCancellationToken struct {
	state *stageTokenState
}

// Valid reports whether this token came from ControlledChildRuntime.
func (token StageCancellationToken) Valid() bool { return token.state != nil }

// Scope returns the stage attempt this token can affect.
func (token StageCancellationToken) Scope() StageScope {
	if token.state == nil {
		return StageScope{}
	}
	return token.state.scope
}

// Context is independent from caller/UI contexts and is canceled by only the
// owning stage action or a parent run termination.
func (token StageCancellationToken) Context() context.Context {
	if token.state == nil {
		return canceledRuntimeContext()
	}
	return token.state.ctx
}

// Done exposes this stage's cancellation signal.
func (token StageCancellationToken) Done() <-chan struct{} { return token.Context().Done() }

// Err returns the stage token's context error after cancellation.
func (token StageCancellationToken) Err() error { return token.Context().Err() }

// Intent reports the explicit action being propagated to this stage.
func (token StageCancellationToken) Intent() CancellationIntent {
	if token.state == nil {
		return CancellationNone
	}
	token.state.mu.RLock()
	defer token.state.mu.RUnlock()
	return token.state.intent
}

// ChildWorkerCommand describes a local child-worker command. It contains no
// remote endpoint, upload target, or provider credential contract; the
// injected launcher determines how a local process is created.
type ChildWorkerCommand struct {
	Program          string   `json:"program"`
	Arguments        []string `json:"arguments"`
	WorkingDirectory string   `json:"working_directory,omitempty"`
	Environment      []string `json:"environment,omitempty"`
}

// Validate verifies that a command factory returned a runnable local command.
func (command ChildWorkerCommand) Validate() error {
	if err := validateIdentifier("child worker program", command.Program); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRuntimeControl, err)
	}
	for _, argument := range command.Arguments {
		if err := validateIdentifier("child worker argument", argument); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRuntimeControl, err)
		}
	}
	for _, value := range command.Environment {
		if err := validateIdentifier("child worker environment", value); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRuntimeControl, err)
		}
	}
	return nil
}

// Clone returns an independent command specification.
func (command ChildWorkerCommand) Clone() ChildWorkerCommand {
	command.Arguments = append([]string(nil), command.Arguments...)
	command.Environment = append([]string(nil), command.Environment...)
	return command
}

// ChildWorkerCommandRequest is passed to an injected command factory. Its
// Detached flag lets an implementation select an explicit durable child mode
// rather than inheriting a UI process lifetime.
type ChildWorkerCommandRequest struct {
	Scope    StageScope `json:"scope"`
	Detached bool       `json:"detached"`
}

// ChildWorkerCommandFactory derives a local child-worker command from an
// immutable stage scope. It is injected so workflowruntime does not choose an
// application binary or leak application-wide contexts into a worker.
type ChildWorkerCommandFactory interface {
	BuildChildWorkerCommand(context.Context, ChildWorkerCommandRequest) (ChildWorkerCommand, error)
}

// ChildWorkerLaunchRequest is passed to an injected launcher after a command
// has been validated. ExecutionToken remains valid after the caller that
// launched or detached the worker exits.
type ChildWorkerLaunchRequest struct {
	Scope          StageScope             `json:"scope"`
	Command        ChildWorkerCommand     `json:"command"`
	ExecutionToken StageCancellationToken `json:"-"`
	Detached       bool                   `json:"detached"`
}

// RuntimeCheckpointRequest asks a worker to persist durable progress before a
// pause or termination action. The worker must return a concrete checkpoint
// ID for a pause to be acknowledged.
type RuntimeCheckpointRequest struct {
	OperationKey string               `json:"operation_key"`
	Scope        StageScope           `json:"scope"`
	Action       RuntimeControlAction `json:"action"`
	RequestedAt  time.Time            `json:"requested_at"`
}

// RuntimeCheckpointAcknowledgement is the worker's response to a checkpoint
// request. Resumable must be true for a pause to become a successful paused
// state; termination may still retain a non-resumable partial checkpoint.
type RuntimeCheckpointAcknowledgement struct {
	CheckpointID string `json:"checkpoint_id,omitempty"`
	Acknowledged bool   `json:"acknowledged"`
	Resumable    bool   `json:"resumable"`
}

// RuntimeStopRequest identifies the termination phase being requested from a
// worker. It is scoped and idempotency-keyed instead of canceling a shared
// process context.
type RuntimeStopRequest struct {
	OperationKey string               `json:"operation_key"`
	Scope        StageScope           `json:"scope"`
	Action       RuntimeControlAction `json:"action"`
	RequestedAt  time.Time            `json:"requested_at"`
	Force        bool                 `json:"force"`
}

// ChildWorkerExit is a worker-provided terminal process receipt. The runtime
// stores it in an immutable RuntimeTerminationReceipt rather than inferring a
// result solely from a canceled context.
type ChildWorkerExit struct {
	ExitedAt time.Time `json:"exited_at"`
	ExitCode int       `json:"exit_code"`
	Reason   string    `json:"reason,omitempty"`
}

// Validate verifies that an exit receipt is complete enough for audit.
func (exit ChildWorkerExit) Validate() error {
	if exit.ExitedAt.IsZero() {
		return fmt.Errorf("%w: child worker exit time is required", ErrInvalidRuntimeControl)
	}
	if err := validateIdentifierOptional("child worker exit reason", exit.Reason); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRuntimeControl, err)
	}
	return nil
}

// ControlledChildWorker is the local process boundary used by
// ControlledChildRuntime. Implementations may use signals, pipes, or an
// in-process fake, but must not rely on a global cancellation context.
type ControlledChildWorker interface {
	RequestCheckpoint(context.Context, RuntimeCheckpointRequest) (RuntimeCheckpointAcknowledgement, error)
	RequestGracefulStop(context.Context, RuntimeStopRequest) error
	ForceTerminate(context.Context, RuntimeStopRequest) error
	Done() <-chan ChildWorkerExit
}

// ChildWorkerLauncher starts a local controlled worker. It is injected so
// detaching does not hard-code application command names, process groups, or
// remote transport behavior in the generic workflow runtime.
type ChildWorkerLauncher interface {
	LaunchChildWorker(context.Context, ChildWorkerLaunchRequest) (ControlledChildWorker, error)
}

// ChildWorkerHandle is returned for an active worker. Its token is typed to a
// stage and cannot be used to cancel a run or sibling stage.
type ChildWorkerHandle struct {
	Scope          StageScope
	ExecutionToken StageCancellationToken
	Detached       bool
}

// RuntimeControlRequest carries one scoped, idempotent process-control
// operation. GracePeriod is deliberately required rather than defaulted: it
// is a policy decision that must be frozen by the caller's execution profile.
type RuntimeControlRequest struct {
	OperationKey string               `json:"operation_key"`
	Scope        StageScope           `json:"scope"`
	Action       RuntimeControlAction `json:"action"`
	GracePeriod  time.Duration        `json:"grace_period"`
}

func (request RuntimeControlRequest) validate() error {
	if err := validateIdentifier("runtime control operation key", request.OperationKey); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRuntimeControl, err)
	}
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if !request.Action.valid() {
		return fmt.Errorf("%w: unsupported action %q", ErrInvalidRuntimeControl, request.Action)
	}
	if request.GracePeriod <= 0 {
		return fmt.Errorf("%w: grace period must be positive", ErrInvalidRuntimeControl)
	}
	return nil
}

// RuntimeRunTerminationRequest terminates every active stage in one run using
// one caller-visible operation key. Per-stage operation keys are derived
// deterministically, so a repeated request does not repeat child actions.
type RuntimeRunTerminationRequest struct {
	OperationKey string        `json:"operation_key"`
	Run          RunScope      `json:"run"`
	GracePeriod  time.Duration `json:"grace_period"`
}

func (request RuntimeRunTerminationRequest) validate() error {
	if err := validateIdentifier("runtime control operation key", request.OperationKey); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRuntimeControl, err)
	}
	if err := request.Run.Validate(); err != nil {
		return err
	}
	if request.GracePeriod <= 0 {
		return fmt.Errorf("%w: grace period must be positive", ErrInvalidRuntimeControl)
	}
	return nil
}

// RuntimeTerminationMethod records whether the worker exited after the
// graceful stop request or only after escalation.
type RuntimeTerminationMethod string

const (
	TerminationMethodNone     RuntimeTerminationMethod = ""
	TerminationMethodGraceful RuntimeTerminationMethod = "graceful"
	TerminationMethodForced   RuntimeTerminationMethod = "forced"
)

// RuntimeTerminationResult records the semantic outcome of a scoped action.
// It deliberately distinguishes an unacknowledged pause checkpoint from a
// terminated process.
type RuntimeTerminationResult string

const (
	TerminationResultPaused             RuntimeTerminationResult = "paused"
	TerminationResultTerminated         RuntimeTerminationResult = "terminated"
	TerminationResultCheckpointRejected RuntimeTerminationResult = "checkpoint_rejected"
	TerminationResultFailed             RuntimeTerminationResult = "failed"
)

type runtimeCheckpointReceipt struct {
	RequestedAt    time.Time `json:"requested_at"`
	AcknowledgedAt time.Time `json:"acknowledged_at,omitempty"`
	CheckpointID   string    `json:"checkpoint_id,omitempty"`
	Acknowledged   bool      `json:"acknowledged"`
	Resumable      bool      `json:"resumable"`
}

// RuntimeCheckpointReceipt is an immutable value returned from a termination
// receipt. It has no mutable links to a live worker or checkpoint buffer.
type RuntimeCheckpointReceipt struct {
	RequestedAt    time.Time
	AcknowledgedAt time.Time
	CheckpointID   string
	Acknowledged   bool
	Resumable      bool
}

func checkpointReceiptValue(value runtimeCheckpointReceipt) RuntimeCheckpointReceipt {
	return RuntimeCheckpointReceipt{
		RequestedAt:    value.RequestedAt,
		AcknowledgedAt: value.AcknowledgedAt,
		CheckpointID:   value.CheckpointID,
		Acknowledged:   value.Acknowledged,
		Resumable:      value.Resumable,
	}
}

type runtimeTerminationReceiptData struct {
	Format                  string                   `json:"format"`
	OperationKey            string                   `json:"operation_key"`
	Action                  RuntimeControlAction     `json:"action"`
	Scope                   StageScope               `json:"scope"`
	RequestedAt             time.Time                `json:"requested_at"`
	Checkpoint              runtimeCheckpointReceipt `json:"checkpoint"`
	GracefulStopRequestedAt time.Time                `json:"graceful_stop_requested_at,omitempty"`
	EscalatedAt             time.Time                `json:"escalated_at,omitempty"`
	Method                  RuntimeTerminationMethod `json:"method,omitempty"`
	Result                  RuntimeTerminationResult `json:"result"`
	Exit                    ChildWorkerExit          `json:"exit,omitempty"`
	CompletedAt             time.Time                `json:"completed_at"`
}

const runtimeTerminationReceiptFormat = "workflowruntime.termination-receipt.v1"

// RuntimeTerminationReceipt is an immutable, serializable record of a
// checkpoint/stop protocol. Its internals are unexported; consumers receive
// copies through accessors and must create a new receipt rather than mutate a
// previously acknowledged operation.
type RuntimeTerminationReceipt struct {
	data runtimeTerminationReceiptData
}

// OperationKey returns the durable idempotency key of this control operation.
func (receipt RuntimeTerminationReceipt) OperationKey() string { return receipt.data.OperationKey }

// Action returns the action that produced this receipt.
func (receipt RuntimeTerminationReceipt) Action() RuntimeControlAction { return receipt.data.Action }

// Scope returns the stage attempt controlled by this receipt.
func (receipt RuntimeTerminationReceipt) Scope() StageScope { return receipt.data.Scope }

// RequestedAt returns when the runtime began the control protocol.
func (receipt RuntimeTerminationReceipt) RequestedAt() time.Time { return receipt.data.RequestedAt }

// Checkpoint returns a copied checkpoint acknowledgement projection.
func (receipt RuntimeTerminationReceipt) Checkpoint() RuntimeCheckpointReceipt {
	return checkpointReceiptValue(receipt.data.Checkpoint)
}

// GracefulStopRequestedAt returns when graceful termination was requested.
func (receipt RuntimeTerminationReceipt) GracefulStopRequestedAt() time.Time {
	return receipt.data.GracefulStopRequestedAt
}

// EscalatedAt returns when force termination was requested, or zero when the
// worker exited during the graceful period.
func (receipt RuntimeTerminationReceipt) EscalatedAt() time.Time { return receipt.data.EscalatedAt }

// Method reports whether the worker stopped gracefully or after escalation.
func (receipt RuntimeTerminationReceipt) Method() RuntimeTerminationMethod {
	return receipt.data.Method
}

// Result reports the semantic control result.
func (receipt RuntimeTerminationReceipt) Result() RuntimeTerminationResult {
	return receipt.data.Result
}

// Exit returns a copied process exit receipt.
func (receipt RuntimeTerminationReceipt) Exit() ChildWorkerExit { return receipt.data.Exit }

// CompletedAt returns when the runtime completed this protocol attempt.
func (receipt RuntimeTerminationReceipt) CompletedAt() time.Time { return receipt.data.CompletedAt }

// Validate verifies the immutable receipt's complete transition evidence.
func (receipt RuntimeTerminationReceipt) Validate() error {
	data := receipt.data
	if data.Format != runtimeTerminationReceiptFormat {
		return fmt.Errorf("%w: unsupported termination receipt format %q", ErrInvalidRuntimeControl, data.Format)
	}
	if err := validateIdentifier("runtime control operation key", data.OperationKey); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRuntimeControl, err)
	}
	if !data.Action.valid() {
		return fmt.Errorf("%w: unsupported action %q", ErrInvalidRuntimeControl, data.Action)
	}
	if err := data.Scope.Validate(); err != nil {
		return err
	}
	if data.RequestedAt.IsZero() || data.CompletedAt.IsZero() || data.CompletedAt.Before(data.RequestedAt) {
		return fmt.Errorf("%w: invalid receipt timestamps", ErrInvalidRuntimeControl)
	}
	if data.Checkpoint.RequestedAt.IsZero() || data.Checkpoint.RequestedAt.Before(data.RequestedAt) {
		return fmt.Errorf("%w: invalid checkpoint request timestamp", ErrInvalidRuntimeControl)
	}
	if data.Checkpoint.Acknowledged {
		if data.Checkpoint.AcknowledgedAt.IsZero() || data.Checkpoint.AcknowledgedAt.Before(data.Checkpoint.RequestedAt) {
			return fmt.Errorf("%w: invalid checkpoint acknowledgement timestamp", ErrInvalidRuntimeControl)
		}
		if err := validateIdentifier("checkpoint id", data.Checkpoint.CheckpointID); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRuntimeControl, err)
		}
	} else if !data.Checkpoint.AcknowledgedAt.IsZero() || data.Checkpoint.CheckpointID != "" || data.Checkpoint.Resumable {
		return fmt.Errorf("%w: unacknowledged checkpoint carries acknowledgement data", ErrInvalidRuntimeControl)
	}

	switch data.Result {
	case TerminationResultCheckpointRejected:
		if data.Action != RuntimeControlPause || (data.Checkpoint.Acknowledged && data.Checkpoint.Resumable) || data.Method != TerminationMethodNone || !data.Exit.ExitedAt.IsZero() || !data.GracefulStopRequestedAt.IsZero() || !data.EscalatedAt.IsZero() {
			return fmt.Errorf("%w: invalid checkpoint-rejected receipt", ErrInvalidRuntimeControl)
		}
	case TerminationResultPaused:
		if data.Action != RuntimeControlPause || !data.Checkpoint.Acknowledged || !data.Checkpoint.Resumable {
			return fmt.Errorf("%w: paused receipt requires a resumable checkpoint", ErrInvalidRuntimeControl)
		}
		if err := validateStoppedReceipt(data); err != nil {
			return err
		}
	case TerminationResultTerminated:
		if data.Action != RuntimeControlTerminate {
			return fmt.Errorf("%w: terminated receipt must represent terminate", ErrInvalidRuntimeControl)
		}
		if err := validateStoppedReceipt(data); err != nil {
			return err
		}
	case TerminationResultFailed:
		if data.Method != TerminationMethodNone && data.Method != TerminationMethodGraceful && data.Method != TerminationMethodForced {
			return fmt.Errorf("%w: invalid failed-receipt method", ErrInvalidRuntimeControl)
		}
	default:
		return fmt.Errorf("%w: unsupported termination result %q", ErrInvalidRuntimeControl, data.Result)
	}
	return nil
}

func validateStoppedReceipt(data runtimeTerminationReceiptData) error {
	if data.GracefulStopRequestedAt.IsZero() || data.GracefulStopRequestedAt.Before(data.Checkpoint.RequestedAt) {
		return fmt.Errorf("%w: stopped receipt lacks graceful-stop request", ErrInvalidRuntimeControl)
	}
	if data.Method != TerminationMethodGraceful && data.Method != TerminationMethodForced {
		return fmt.Errorf("%w: stopped receipt has invalid method", ErrInvalidRuntimeControl)
	}
	if data.Method == TerminationMethodGraceful && !data.EscalatedAt.IsZero() {
		return fmt.Errorf("%w: graceful receipt cannot have escalation", ErrInvalidRuntimeControl)
	}
	if data.Method == TerminationMethodForced && (data.EscalatedAt.IsZero() || data.EscalatedAt.Before(data.GracefulStopRequestedAt)) {
		return fmt.Errorf("%w: forced receipt lacks valid escalation", ErrInvalidRuntimeControl)
	}
	if err := data.Exit.Validate(); err != nil {
		return err
	}
	if data.Exit.ExitedAt.Before(data.GracefulStopRequestedAt) || data.Exit.ExitedAt.After(data.CompletedAt) {
		return fmt.Errorf("%w: invalid child worker exit timestamp", ErrInvalidRuntimeControl)
	}
	return nil
}

// MarshalJSON writes a validated immutable receipt for durable control-plane
// persistence or append-only event storage.
func (receipt RuntimeTerminationReceipt) MarshalJSON() ([]byte, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(receipt.data)
}

// ParseRuntimeTerminationReceipt decodes a strictly validated receipt. It is
// intentionally separate from mutation APIs so decoded historical receipts
// cannot be changed in place by callers.
func ParseRuntimeTerminationReceipt(encoded []byte) (RuntimeTerminationReceipt, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var data runtimeTerminationReceiptData
	if err := decoder.Decode(&data); err != nil {
		return RuntimeTerminationReceipt{}, fmt.Errorf("%w: decode termination receipt: %v", ErrInvalidRuntimeControl, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return RuntimeTerminationReceipt{}, fmt.Errorf("%w: decode trailing receipt JSON: %v", ErrInvalidRuntimeControl, err)
		}
		return RuntimeTerminationReceipt{}, fmt.Errorf("%w: unexpected trailing receipt JSON value", ErrInvalidRuntimeControl)
	}
	receipt := RuntimeTerminationReceipt{data: data}
	if err := receipt.Validate(); err != nil {
		return RuntimeTerminationReceipt{}, err
	}
	return receipt, nil
}

// ControlledChildRuntimeConfig supplies only local, injected process seams.
// No global application context, remote provider, or upload client is used.
type ControlledChildRuntimeConfig struct {
	CommandFactory ChildWorkerCommandFactory
	Launcher       ChildWorkerLauncher
	Now            func() time.Time
}

// ControlledChildRuntime owns independent run and stage cancellation tokens,
// child-worker handles, and idempotent scoped control operations.
type ControlledChildRuntime struct {
	commandFactory ChildWorkerCommandFactory
	launcher       ChildWorkerLauncher
	now            func() time.Time

	mu         sync.Mutex
	runs       map[RunScope]*managedRun
	workers    map[StageScope]*managedChildWorker
	operations map[string]*runtimeControlOperation
}

type managedRun struct {
	token RunCancellationToken
}

type managedChildWorker struct {
	scope    StageScope
	token    StageCancellationToken
	detached bool

	ready     chan struct{}
	launchErr error
	worker    ControlledChildWorker

	controlMu sync.Mutex
	stopped   bool
	receipt   RuntimeTerminationReceipt
}

func (worker *managedChildWorker) isStopped() bool {
	if worker == nil {
		return true
	}
	worker.controlMu.Lock()
	defer worker.controlMu.Unlock()
	return worker.stopped
}

type runtimeControlOperation struct {
	request RuntimeControlRequest
	done    chan struct{}
	receipt RuntimeTerminationReceipt
	err     error
}

// NewControlledChildRuntime constructs a local child-worker runtime. The
// factory and launcher are required so the generic package never chooses an
// application executable or inherits a shared cancellation root.
func NewControlledChildRuntime(config ControlledChildRuntimeConfig) (*ControlledChildRuntime, error) {
	if config.CommandFactory == nil {
		return nil, fmt.Errorf("%w: child worker command factory is required", ErrInvalidRuntimeControl)
	}
	if config.Launcher == nil {
		return nil, fmt.Errorf("%w: child worker launcher is required", ErrInvalidRuntimeControl)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &ControlledChildRuntime{
		commandFactory: config.CommandFactory,
		launcher:       config.Launcher,
		now:            config.Now,
		runs:           make(map[RunScope]*managedRun),
		workers:        make(map[StageScope]*managedChildWorker),
		operations:     make(map[string]*runtimeControlOperation),
	}, nil
}

// Start launches a foreground child worker with a dedicated stage token.
func (runtime *ControlledChildRuntime) Start(ctx context.Context, scope StageScope) (ChildWorkerHandle, error) {
	return runtime.start(ctx, scope, false)
}

// Detach starts a durable child-worker handoff through the injected launcher.
// The launch context is used only for factory/launcher setup; the child gets a
// dedicated stage token that survives cancellation of the caller's context.
func (runtime *ControlledChildRuntime) Detach(ctx context.Context, scope StageScope) (ChildWorkerHandle, error) {
	return runtime.start(ctx, scope, true)
}

func (runtime *ControlledChildRuntime) start(ctx context.Context, scope StageScope, detached bool) (ChildWorkerHandle, error) {
	if err := validateContext(ctx); err != nil {
		return ChildWorkerHandle{}, err
	}
	if err := scope.Validate(); err != nil {
		return ChildWorkerHandle{}, err
	}
	if runtime == nil {
		return ChildWorkerHandle{}, fmt.Errorf("%w: runtime is nil", ErrInvalidRuntimeControl)
	}

	runtime.mu.Lock()
	if existing := runtime.workers[scope]; existing != nil {
		if existing.isStopped() {
			runtime.mu.Unlock()
			return ChildWorkerHandle{}, fmt.Errorf("%w: stage attempt %s", ErrChildWorkerStopped, scope.StageAttemptID)
		}
		if existing.detached != detached {
			runtime.mu.Unlock()
			return ChildWorkerHandle{}, fmt.Errorf("%w: stage %s already launched with detached=%t", ErrRuntimeControlConflict, scope.StageAttemptID, existing.detached)
		}
		runtime.mu.Unlock()
		return waitForWorkerLaunch(ctx, existing)
	}
	run := runtime.runs[scope.Run]
	if run == nil {
		run = &managedRun{token: newRunCancellationToken(scope.Run)}
		runtime.runs[scope.Run] = run
	}
	worker := &managedChildWorker{
		scope:    scope,
		token:    newStageCancellationToken(scope, run.token),
		detached: detached,
		ready:    make(chan struct{}),
	}
	runtime.workers[scope] = worker
	runtime.mu.Unlock()

	command, err := runtime.commandFactory.BuildChildWorkerCommand(ctx, ChildWorkerCommandRequest{Scope: scope, Detached: detached})
	if err == nil {
		err = command.Validate()
	}
	var launched ControlledChildWorker
	if err == nil {
		launched, err = runtime.launcher.LaunchChildWorker(ctx, ChildWorkerLaunchRequest{
			Scope:          scope,
			Command:        command.Clone(),
			ExecutionToken: worker.token,
			Detached:       detached,
		})
		if launched == nil && err == nil {
			err = fmt.Errorf("%w: child worker launcher returned nil worker", ErrInvalidRuntimeControl)
		}
	}

	runtime.mu.Lock()
	worker.worker = launched
	worker.launchErr = err
	close(worker.ready)
	if err != nil {
		delete(runtime.workers, scope)
		if !runtime.hasRunWorkersLocked(scope.Run) {
			delete(runtime.runs, scope.Run)
		}
	}
	runtime.mu.Unlock()
	if err != nil {
		return ChildWorkerHandle{}, fmt.Errorf("launch child worker: %w", err)
	}
	return ChildWorkerHandle{Scope: scope, ExecutionToken: worker.token, Detached: detached}, nil
}

// RunToken returns the typed token for a live run without exposing a cancel
// function to callers.
func (runtime *ControlledChildRuntime) RunToken(scope RunScope) (RunCancellationToken, bool) {
	if runtime == nil {
		return RunCancellationToken{}, false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	run := runtime.runs[scope]
	if run == nil {
		return RunCancellationToken{}, false
	}
	return run.token, true
}

// StageToken returns the typed token for a live stage worker.
func (runtime *ControlledChildRuntime) StageToken(scope StageScope) (StageCancellationToken, bool) {
	if runtime == nil {
		return StageCancellationToken{}, false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	worker := runtime.workers[scope]
	if worker == nil {
		return StageCancellationToken{}, false
	}
	return worker.token, true
}

// RequestControl runs one idempotent stage-scoped checkpoint/termination
// protocol. Equal operation keys wait for and return the original receipt;
// mismatched reuse is rejected before any child-worker call is repeated.
func (runtime *ControlledChildRuntime) RequestControl(ctx context.Context, request RuntimeControlRequest) (RuntimeTerminationReceipt, error) {
	if err := validateContext(ctx); err != nil {
		return RuntimeTerminationReceipt{}, err
	}
	if err := request.validate(); err != nil {
		return RuntimeTerminationReceipt{}, err
	}
	if runtime == nil {
		return RuntimeTerminationReceipt{}, fmt.Errorf("%w: runtime is nil", ErrInvalidRuntimeControl)
	}

	runtime.mu.Lock()
	if existing := runtime.operations[request.OperationKey]; existing != nil {
		if existing.request.Scope != request.Scope || existing.request.Action != request.Action || existing.request.GracePeriod != request.GracePeriod {
			runtime.mu.Unlock()
			return RuntimeTerminationReceipt{}, fmt.Errorf("%w: operation key %q was already used for another control request", ErrRuntimeControlConflict, request.OperationKey)
		}
		runtime.mu.Unlock()
		return waitForControlOperation(ctx, existing)
	}
	worker := runtime.workers[request.Scope]
	if worker == nil {
		runtime.mu.Unlock()
		return RuntimeTerminationReceipt{}, fmt.Errorf("%w: stage attempt %s", ErrChildWorkerNotFound, request.Scope.StageAttemptID)
	}
	operation := &runtimeControlOperation{request: request, done: make(chan struct{})}
	runtime.operations[request.OperationKey] = operation
	runtime.mu.Unlock()

	receipt, err := runtime.requestWorkerControl(ctx, worker, request)
	runtime.mu.Lock()
	operation.receipt = receipt
	operation.err = err
	close(operation.done)
	runtime.mu.Unlock()
	return receipt, err
}

// TerminateRun applies a run-scoped termination intent to every active child
// stage. Each stage receives a deterministic derived operation key, preserving
// per-stage receipts and idempotency while the parent RunCancellationToken is
// canceled only after the scoped protocols have been attempted.
func (runtime *ControlledChildRuntime) TerminateRun(ctx context.Context, request RuntimeRunTerminationRequest) ([]RuntimeTerminationReceipt, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	if err := request.validate(); err != nil {
		return nil, err
	}
	if runtime == nil {
		return nil, fmt.Errorf("%w: runtime is nil", ErrInvalidRuntimeControl)
	}

	runtime.mu.Lock()
	run := runtime.runs[request.Run]
	if run == nil {
		runtime.mu.Unlock()
		return nil, fmt.Errorf("%w: run %s", ErrChildWorkerNotFound, request.Run.RunID)
	}
	scopes := make([]StageScope, 0)
	for scope, worker := range runtime.workers {
		if scope.Run == request.Run && !worker.isStopped() {
			scopes = append(scopes, scope)
		}
	}
	runtime.mu.Unlock()
	sort.Slice(scopes, func(left, right int) bool {
		if scopes[left].StageAttemptID != scopes[right].StageAttemptID {
			return scopes[left].StageAttemptID < scopes[right].StageAttemptID
		}
		return scopes[left].StageKey < scopes[right].StageKey
	})

	receipts := make([]RuntimeTerminationReceipt, 0, len(scopes))
	errs := make([]error, 0)
	for _, scope := range scopes {
		operationKey, keyErr := derivedRunOperationKey(request.OperationKey, scope)
		if keyErr != nil {
			errs = append(errs, keyErr)
			continue
		}
		receipt, err := runtime.RequestControl(ctx, RuntimeControlRequest{
			OperationKey: operationKey,
			Scope:        scope,
			Action:       RuntimeControlTerminate,
			GracePeriod:  request.GracePeriod,
		})
		if receipt.Validate() == nil {
			receipts = append(receipts, receipt)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	markRunIntent(run.token, CancellationTerminate)
	cancelRunToken(run.token)
	return receipts, errors.Join(errs...)
}

func (runtime *ControlledChildRuntime) requestWorkerControl(ctx context.Context, managed *managedChildWorker, request RuntimeControlRequest) (RuntimeTerminationReceipt, error) {
	if _, err := waitForWorkerLaunch(ctx, managed); err != nil {
		return RuntimeTerminationReceipt{}, err
	}
	managed.controlMu.Lock()
	defer managed.controlMu.Unlock()
	if managed.stopped {
		return managed.receipt, fmt.Errorf("%w: stage attempt %s", ErrChildWorkerStopped, request.Scope.StageAttemptID)
	}
	worker := managed.worker
	if worker == nil {
		return RuntimeTerminationReceipt{}, fmt.Errorf("%w: stage attempt %s has no worker", ErrChildWorkerNotFound, request.Scope.StageAttemptID)
	}

	requestedAt := runtime.timestamp()
	markStageIntent(managed.token, intentFor(request.Action))
	checkpointAcknowledgement, checkpointErr := worker.RequestCheckpoint(ctx, RuntimeCheckpointRequest{
		OperationKey: request.OperationKey,
		Scope:        request.Scope,
		Action:       request.Action,
		RequestedAt:  requestedAt,
	})
	checkpoint := runtimeCheckpointReceipt{RequestedAt: requestedAt}
	if checkpointErr == nil && checkpointAcknowledgement.Acknowledged && strings.TrimSpace(checkpointAcknowledgement.CheckpointID) != "" {
		checkpoint.Acknowledged = true
		checkpoint.AcknowledgedAt = runtime.timestamp()
		checkpoint.CheckpointID = checkpointAcknowledgement.CheckpointID
		checkpoint.Resumable = checkpointAcknowledgement.Resumable
	}
	if request.Action == RuntimeControlPause && (!checkpoint.Acknowledged || !checkpoint.Resumable) {
		receipt := runtime.newReceipt(request, requestedAt, checkpoint, time.Time{}, time.Time{}, TerminationMethodNone, TerminationResultCheckpointRejected, ChildWorkerExit{})
		return receipt, fmt.Errorf("%w: stage attempt %s", ErrCheckpointRejected, request.Scope.StageAttemptID)
	}

	gracefulAt := runtime.timestamp()
	stopErr := worker.RequestGracefulStop(ctx, RuntimeStopRequest{
		OperationKey: request.OperationKey,
		Scope:        request.Scope,
		Action:       request.Action,
		RequestedAt:  gracefulAt,
		Force:        false,
	})
	cancelStageToken(managed.token)
	exit, timedOut, waitErr := waitForChildExit(ctx, worker.Done(), request.GracePeriod)
	if waitErr == nil && !timedOut {
		receipt := runtime.newReceipt(request, requestedAt, checkpoint, gracefulAt, time.Time{}, TerminationMethodGraceful, resultFor(request.Action), exit)
		managed.stopped = true
		managed.receipt = receipt
		return receipt, nil
	}

	escalatedAt := runtime.timestamp()
	forceErr := worker.ForceTerminate(ctx, RuntimeStopRequest{
		OperationKey: request.OperationKey,
		Scope:        request.Scope,
		Action:       request.Action,
		RequestedAt:  escalatedAt,
		Force:        true,
	})
	if forceErr == nil {
		exit, forcedTimedOut, forcedWaitErr := waitForChildExit(ctx, worker.Done(), request.GracePeriod)
		if forcedWaitErr == nil && !forcedTimedOut {
			receipt := runtime.newReceipt(request, requestedAt, checkpoint, gracefulAt, escalatedAt, TerminationMethodForced, resultFor(request.Action), exit)
			managed.stopped = true
			managed.receipt = receipt
			return receipt, nil
		}
		waitErr = forcedWaitErr
	}

	receipt := runtime.newReceipt(request, requestedAt, checkpoint, gracefulAt, escalatedAt, TerminationMethodNone, TerminationResultFailed, ChildWorkerExit{})
	if forceErr != nil {
		return receipt, fmt.Errorf("%w: force terminate stage attempt %s: %v", ErrGracefulTermination, request.Scope.StageAttemptID, forceErr)
	}
	if waitErr != nil {
		return receipt, fmt.Errorf("%w: wait for stage attempt %s: %v", ErrGracefulTermination, request.Scope.StageAttemptID, waitErr)
	}
	if stopErr != nil {
		return receipt, fmt.Errorf("%w: request graceful stop for stage attempt %s: %v", ErrGracefulTermination, request.Scope.StageAttemptID, stopErr)
	}
	return receipt, fmt.Errorf("%w: stage attempt %s exceeded both grace periods", ErrGracefulTermination, request.Scope.StageAttemptID)
}

func (runtime *ControlledChildRuntime) newReceipt(request RuntimeControlRequest, requestedAt time.Time, checkpoint runtimeCheckpointReceipt, gracefulAt, escalatedAt time.Time, method RuntimeTerminationMethod, result RuntimeTerminationResult, exit ChildWorkerExit) RuntimeTerminationReceipt {
	receipt := RuntimeTerminationReceipt{data: runtimeTerminationReceiptData{
		Format:                  runtimeTerminationReceiptFormat,
		OperationKey:            request.OperationKey,
		Action:                  request.Action,
		Scope:                   request.Scope,
		RequestedAt:             requestedAt,
		Checkpoint:              checkpoint,
		GracefulStopRequestedAt: gracefulAt,
		EscalatedAt:             escalatedAt,
		Method:                  method,
		Result:                  result,
		Exit:                    exit,
		CompletedAt:             runtime.timestamp(),
	}}
	return receipt
}

func (runtime *ControlledChildRuntime) timestamp() time.Time { return runtime.now().UTC() }

func (runtime *ControlledChildRuntime) hasRunWorkersLocked(scope RunScope) bool {
	for stage := range runtime.workers {
		if stage.Run == scope {
			return true
		}
	}
	return false
}

func derivedRunOperationKey(base string, scope StageScope) (string, error) {
	fingerprint, err := workflowkit.FingerprintParts("workflowruntime.run-termination.v1", []workflowkit.FingerprintPart{
		{Name: "run", Value: []byte(scope.Run.RunID)},
		{Name: "stage_attempt", Value: []byte(scope.StageAttemptID)},
		{Name: "stage", Value: []byte(scope.StageKey)},
	})
	if err != nil {
		return "", fmt.Errorf("%w: derive run control operation key: %v", ErrInvalidRuntimeControl, err)
	}
	return base + "." + strings.TrimPrefix(string(fingerprint), ObjectAlgorithm+":"), nil
}

func waitForWorkerLaunch(ctx context.Context, worker *managedChildWorker) (ChildWorkerHandle, error) {
	if worker == nil {
		return ChildWorkerHandle{}, fmt.Errorf("%w", ErrChildWorkerNotFound)
	}
	select {
	case <-worker.ready:
		if worker.launchErr != nil {
			return ChildWorkerHandle{}, fmt.Errorf("launch child worker: %w", worker.launchErr)
		}
		if worker.worker == nil {
			return ChildWorkerHandle{}, fmt.Errorf("%w: child worker launcher returned nil worker", ErrInvalidRuntimeControl)
		}
		return ChildWorkerHandle{Scope: worker.scope, ExecutionToken: worker.token, Detached: worker.detached}, nil
	case <-ctx.Done():
		return ChildWorkerHandle{}, ctx.Err()
	}
}

func waitForControlOperation(ctx context.Context, operation *runtimeControlOperation) (RuntimeTerminationReceipt, error) {
	select {
	case <-operation.done:
		return operation.receipt, operation.err
	case <-ctx.Done():
		return RuntimeTerminationReceipt{}, ctx.Err()
	}
}

func waitForChildExit(ctx context.Context, done <-chan ChildWorkerExit, grace time.Duration) (ChildWorkerExit, bool, error) {
	if done == nil {
		return ChildWorkerExit{}, false, fmt.Errorf("%w: child worker has no completion channel", ErrInvalidRuntimeControl)
	}
	timer := time.NewTimer(grace)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case exit, open := <-done:
		if !open {
			return ChildWorkerExit{}, false, fmt.Errorf("%w: child worker closed without an exit receipt", ErrInvalidRuntimeControl)
		}
		if err := exit.Validate(); err != nil {
			return ChildWorkerExit{}, false, err
		}
		return exit, false, nil
	case <-timer.C:
		return ChildWorkerExit{}, true, nil
	case <-ctx.Done():
		return ChildWorkerExit{}, false, ctx.Err()
	}
}

func resultFor(action RuntimeControlAction) RuntimeTerminationResult {
	if action == RuntimeControlPause {
		return TerminationResultPaused
	}
	return TerminationResultTerminated
}

func newRunCancellationToken(scope RunScope) RunCancellationToken {
	ctx, cancel := context.WithCancel(context.Background())
	return RunCancellationToken{state: &runTokenState{scope: scope, ctx: ctx, cancel: cancel}}
}

func newStageCancellationToken(scope StageScope, run RunCancellationToken) StageCancellationToken {
	ctx, cancel := context.WithCancel(run.Context())
	return StageCancellationToken{state: &stageTokenState{scope: scope, ctx: ctx, cancel: cancel}}
}

func markRunIntent(token RunCancellationToken, intent CancellationIntent) {
	if token.state == nil {
		return
	}
	token.state.mu.Lock()
	token.state.intent = intent
	token.state.mu.Unlock()
}

func markStageIntent(token StageCancellationToken, intent CancellationIntent) {
	if token.state == nil {
		return
	}
	token.state.mu.Lock()
	token.state.intent = intent
	token.state.mu.Unlock()
}

func cancelRunToken(token RunCancellationToken) {
	if token.state != nil {
		token.state.cancel()
	}
}

func cancelStageToken(token StageCancellationToken) {
	if token.state != nil {
		token.state.cancel()
	}
}

func canceledRuntimeContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func validateIdentifierOptional(label, value string) error {
	if value == "" {
		return nil
	}
	return validateIdentifier(label, value)
}
