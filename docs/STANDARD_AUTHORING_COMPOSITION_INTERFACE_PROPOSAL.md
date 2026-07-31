# Standard Authoring Composition Contract

Status: implemented provider/attestation boundary. Application, CLI, and TUI
admission still have to install this exact composition; there is no fallback
from another template.

`harbor.standard-authoring@1.2.0` is the source-session half of task creation.
Its `materialize_task` stage ends that Run after atomically creating the first
TaskRevision and emitting a typed child handoff. It must not continue the
task-bound CodeEdge lifecycle inside the AuthoringSession Run.

## Sealed operation kinds

The closed deployment catalog permits only:

| Payload kind | Standard authoring use | Runtime proof |
| --- | --- | --- |
| `local.command` | `repo_prepare` Git snapshot | exact regular Git executable, SHA-256, version, no caller argv |
| `agent.turn` | analysis and content generation | pinned Codex JS launcher/Node/CODEX_HOME/sandbox, `deepseek-v4-flash` / `max`, plus locked prompt/schema assets |
| `durable.review` | task, content, and solution gates | versioned durable-review policy; normal workflow handling waits for a decision |
| `harbor.builtin` | `materialize_task` | exact handler ID/version and Harbor Flow build identity |

`container.command` is rejected. Standard authoring does not claim an image
or Docker execution ABI, so a generated task Dockerfile can never become an
ambient container capability.

## Prompt/schema lock extension

Each Standard lock record carries:

```json
"standard_authoring_contract": {
  "format": "harbor.standard-authoring-contract.v1",
  "version": "1",
  "prompt": {"id": "...", "version": "...", "relative_path": "prompts/..."},
  "schema": {"id": "...", "version": "...", "relative_path": "schemas/..."}
}
```

The extension maps canonical asset IDs/versions and safe slash-relative paths
to the enclosing record's existing raw prompt/schema SHA-256 fields. Standard
locks require it for every operation; non-Standard locks reject it. Runtime
attestation uses a deployment-owned `ContractRoot`, rejects containment
escapes and all symlink path components, and rehashes both assets before every
external effect. No asset path is returned to handlers.

The `contract-assets.v1.json` manifest has exact closed-stage coverage and is
the only input a final-lock generator may use to create these extensions.

## Codex bridge injection

`NewStandardAuthoringProviderComposition` keeps an explicit
`StandardAuthoringOperationHandlers.AgentTurn` test/deployment seam. When the
catalog has `agent.turn` operations and that handler is absent, it constructs
`NewStandardAuthoringAttestedAgentTurnBridgeFromDeployment` from the exact
lock verifier, runtime attestor, and managed Codex workspace.

The bridge deliberately loads prompt/schema assets at effect time, because a
lock record does not have a Run's checkout revision or artifact identities. It
then constructs a one-effect Codex executor and reattests the runtime just
before `OpenConversation`. It never caches a Codex invocation, environment,
prompt path, or raw provider response.

## Composition ownership

The stageprovider package owns catalog/lock parsing, asset validation, typed
provider dispatch, and runtime attestation. Application composition must own
the following injected handlers without widening the generic workflow engine:

- Git snapshot executor;
- atomic `standard-authoring.materialize-task` handler backed by the Store;
- durable review projection/decision service; and
- a managed non-symlink Codex workspace root.

The application must install the same exact resolver for StartRun, replay,
foreground worker, detached worker, CLI, and TUI. The later CodeEdge Phase-1
parent and evaluator-child resolver are separate template-keyed compositions;
they are reached only through the durable authoring handoff, never through a
Standard provider fallback.
