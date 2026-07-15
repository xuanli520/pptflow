# Standard Authoring Composition Interface Proposal

Status: proposal only.  It deliberately does not add a public payload kind,
operation lock field, provider, or executable deployment catalog before that
material API decision is confirmed.

## Problem boundary

`StandardWorkflowTemplate` contains both external host/model work and
Harbor-Flow-internal lifecycle work.  A production composition needs to admit
all exact stage bindings at StartRun, but must not claim that a Go handler is a
shell command or a model call.  It also needs a distinct template-keyed
resolver so a Standard Run cannot borrow CodeEdge evaluator capability.

## Proposed narrow public extension

The recommended extension is a fifth sealed operation payload and matching
lock specialization:

```go
// Proposed, not implemented.
type HarborBuiltinOperationPayload struct {
    HandlerID string `json:"handler_id"`
}

// Proposed, not implemented.
type HarborFlowBuiltinOperationLock struct {
    Format                string `json:"format"`  // harbor.flow.builtin-operation.v1
    Version               string `json:"version"` // 1
    HandlerID             string `json:"handler_id"`
    HandlerVersion        string `json:"handler_version"`
}

// Proposed, not implemented.
type BuiltinOperationExecutor interface {
    ExecuteBuiltin(context.Context, StageOperationInvocation,
        HarborBuiltinOperationPayload) (workflowkit.StageExecutionResult, error)
}
```

The enclosing deployment lock already carries `HarborFlowBuildIdentity`; the
runtime attestor would require exact equality to the linker-bound build before
dispatching a built-in handler.  The lock record would accept exactly one
attestation specialization: local executable, container runtime, agent/model,
durable review, or the new built-in handler.  This prevents an operation from
being both an opaque Go handler and an unverified host command.

## Proposed composition shape

```go
// Proposed composition contract; no implementation is installed yet.
type StandardAuthoringProviderHandlers struct {
    HostCommand stageprovider.LocalCommandOperationExecutor
    AgentTurn   stageprovider.AgentTurnOperationExecutor
    Builtin     BuiltinOperationExecutor
    Review      workflowkit.StageExecutor // resolution-only durable review adapter
}

type StandardAuthoringComposition struct {
    Template workflowadapter.TemplateReference // exactly harbor.task-lifecycle@2.2.0
    Resolver *stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver
    Registry *workflowkit.ControlledPluginRegistry[workflowkit.StageExecutor]
}
```

Construction would load only a colocated Standard catalog/lock, prove that the
catalog receipt names `harbor.task-lifecycle@2.2.0`, install the exact handler
inventory, and pass that resolver under the same template key to StartRun,
replay, foreground workers, and detached workers.  It would then be combined
with CodeEdge parent/evaluator bindings only through an explicit
multi-template registry; no resolver is a fallback for another template.

## Handler ownership

| Handler family | Owns | Must never do |
| --- | --- | --- |
| Host command | pinned Git/Docker invocation with direct argv | use PATH, use a shell, pick an image tag or arbitrary working directory |
| Codex agent turn | locked App Server invocation and immutable prompt/schema artifacts | inherit arbitrary environment values, switch model, or serialize credentials |
| Built-in | atomic AuthoringSession materialization, artifact/lifecycle handoff, package coordination | invent a TaskRevision, bypass store fencing, or call an unlocked external binary |
| Durable review | exact policy resolution and external-decision wait | auto-approve a gate |

## Required later tests

Once approved and implemented, the implementation must replace the current
negative parser guard with tests that prove:

1. strict parse/canonical round-trip and duplicate/unknown-field rejection for
   the new payload and lock;
2. a built-in handler cannot execute after build, handler-ID, catalog receipt,
   lock identity, or frozen stage-resolution drift;
3. Standard StartRun refuses missing operations and never uses a CodeEdge
   catalog as fallback;
4. the same Standard resolver is used by StartRun, replay, foreground worker,
   detached worker, and TUI-driven start;
5. a `materialize_task` handler atomically creates the first revision from an
   AuthoringSession and rejects duplicate/fenced invocation; and
6. Docker stages reject unpinned image references before calling Docker.
