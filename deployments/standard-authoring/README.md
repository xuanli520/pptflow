# Standard Authoring Deployment Contract — Pending Enablement

This directory is deliberately **not an enabled production deployment**.
It contains the binding boundary and the observed host facts needed to make
`harbor.task-lifecycle@2.2.0` author a first Harbor task from an immutable
repository snapshot.  It contains no operation catalog or lock because the
current sealed operation-payload union has no truthful representation for a
Harbor Flow built-in, Go-controlled stage.

Treat the absence of `operation-catalog.v1.json` and
`operation-catalog.lock.json` as an intentional deny-all state.  A CLI, TUI,
foreground worker, or detached worker must reject a Standard authoring Run
rather than fall back to a local command, ambient model, image tag, or another
template's catalog.

## Fixed authoring subject

The intended first task is authored from this immutable source identity:

- repository: `https://github.com/tower-rs/tower-http.git`
- commit: `f066e10ebc07ea9050a2ce4576315abfa568edf4`
- template: `harbor.task-lifecycle@2.2.0`

The Run must bind an `AuthoringSource` / `AuthoringSession`, not a fabricated
empty `TaskRevision`.  The source record owns the canonical URL, full commit,
read-only snapshot artifact and snapshot fingerprint.  `materialize_task` is
the only operation allowed to atomically create the first real Task and
TaskRevision; every later verification, CodeEdge parent/child Run, and local
package must use that generated revision.

## Why this deployment is not enabled yet

The current sealed payload union permits only:

- `local.command`, which must lock an actual external executable;
- `container.command`, which must lock an image digest;
- `agent.turn`, which locks an agent/model runtime; and
- `durable.review`, which is a human decision wait.

Several Standard stages are Go-controlled Harbor Flow operations, not external
commands or model turns.  In particular, `materialize_task` must call the
atomic application service, and the evaluator handoff/package paths must use
their durable lifecycle services.  Assigning one of those stages an unrelated
`git` or `docker` executable simply to satisfy `local.command` would falsely
attest an operation that is not invoked.  Treating it as `agent.turn` or
`durable.review` would be equally inaccurate.

Before an operation catalog can be created, the deployment needs an explicitly
approved, versioned built-in operation contract.  The minimal proposed shape
is a new sealed payload kind such as `harbor.builtin`, paired with a typed lock
record that binds:

- an exact built-in handler ID and version;
- the linker-bound Harbor Flow build identity;
- the Standard stage/plugin/operation coordinates; and
- the existing prompt/schema fingerprints, checkout, runtime, and secret
  references.

Its attestor must prove the linked build identity before dispatching the
Go-controlled handler.  It must not substitute a PATH command or accept an
unlocked handler.  This is a design proposal, not an enabled format: the
strict parser intentionally rejects it until the user approves the new public
contract.

## Proposed routing after approval

The final catalog must list every Standard stage exactly once and bind only
the matching `harbor.task-lifecycle@2.2.0` stage/plugin/type/group.  The
following is an implementation routing plan, not a whitelist:

| Category | Standard stages | Required proof |
| --- | --- | --- |
| Pinned host executable | `repo_prepare` | exact Git executable bytes/version; source URL/commit and snapshot fingerprint |
| Pinned Codex App Server | authoring analysis/generation stages | `gpt-5.5`, pinned Codex launcher/Node/CODEX_HOME/sandbox, immutable prompt and schema fingerprints |
| Harbor Flow built-in | `materialize_task`, lifecycle-aware checks, evaluator handoff, package integration | approved built-in payload/lock and linker-bound Harbor Flow build |
| Pinned Docker executable | build/initial/oracle verification only | exact Docker client bytes/version plus a task-specific, digest-pinned image policy; no `latest` or unknown base image |
| Durable review | five Standard review gates | versioned durable-review policy only |
| CodeEdge evaluation | Qwen and Opus pass@4 | separately launch the existing `harbor.codeedge-evaluator@1.0.0` child through an explicit child-handoff contract; never reuse its catalog for a Standard Run |

Prompt text, output schemas, task-image digests, and child handoff ABI are not
present in the current deployment materials.  They must be frozen as files
and fingerprints in the approved catalog/lock; they must not be guessed from
the training document.

## Required composition boundary

When enabled, application composition needs one template-keyed Standard
catalog/lock resolver in addition to the existing CodeEdge evaluator resolver.
The Standard resolver must be selected only for
`harbor.task-lifecycle@2.2.0`; it may not be a fallback for either CodeEdge
template.  Its provider composition needs exact handlers for pinned host
commands, Codex agent turns, built-in operations, and durable review waits.
The worker registry must install the exact same template set and resolvers as
StartRun admission.

The implementation must fail closed when any handler, prompt/schema artifact,
source snapshot, executable hash, Docker image policy, catalog receipt, lock
identity, or child catalog receipt is missing or differs from the frozen Run.

## Secret handling

This directory contains no secret value, endpoint value, credential file, or
environment dump.  A future Codex provider may use the already authenticated
Codex runtime through its locked `CODEX_HOME`; it must not serialize or print
its credentials.  Qwen/Opus endpoint and token handling remains solely in the
separate CodeEdge evaluator deployment contract.

See [the host-attestation observation](../../docs/STANDARD_AUTHORING_ENVIRONMENT_ATTESTATION_OBSERVATION.md)
for non-secret facts observed on this host.  That observation is evidence for
a future reviewed lock, not an approval to execute an operation.
