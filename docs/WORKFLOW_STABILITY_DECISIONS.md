# Workflow Stability and Task Lifecycle Decision Record

Status: Confirmed. This record is the implementation contract for the
workflow-stability and lifecycle refactor.

Source: User confirmations for `WORKFLOW_STABILITY_AND_TASK_LIFECYCLE_REFACTOR_PLAN.md`.

## Confirmed Decisions

| Area | Decision | Binding Rule |
| --- | --- | --- |
| Delivery | One complete delivery | M0 through M5 ship as one coherent refactor. |
| Workflow Kernel | Direct public package | Create `pkg/workflowkit` in this delivery. |
| Generic execution engine | `workflowkit` only | The engine remains domain-neutral. Harbor stage operations and providers are selected only from a typed, frozen `RunExecutionSpec`; no legacy runner, stage-name fallback, dynamic payload, or compatibility execution path remains. |
| Control plane | Front-load V2 | Introduce the minimum SQLite V2 control plane before the M1 runtime work. |
| SQLite baseline | Destructive V2-only baseline (4A) | A new database bootstraps at V2 without V1 tables or schema-version row 1. `Open` and `OpenReadOnly` reject any database containing V1 `tasks`/`runs` tables or version-1 history. The process never reads V1 records, imports, migrates, deletes, or transforms that database; recovery accepts only a verified pure-V2 backup, otherwise a new control-plane root is required. |
| Task identity | UUIDv7 | Task IDs use UUIDv7. Database uniqueness rejects collisions; identities are never merged on ID collision. |
| UUIDv7 collision scope | Global lifecycle namespace | Every persistent V2 lifecycle entity `id` belongs to one local UUIDv7 namespace. SQLite rejects reuse across entity types and after deletion. Migration rejects a pre-existing cross-entity collision without merging, rewriting, or otherwise resolving either identity. |
| V2 task digest | `harbor.task.v2:sha256` | Digest only a managed revision snapshot with a versioned strict Harbor file policy. The five core files are required; at least one allowed environment file is required. Extra files, symlinks, and non-regular files fail validation. Hash a length-prefixed binary manifest of canonical path, fixed mode, raw byte length, and content SHA-256. Ignore source path, timestamps, uid/gid, ACLs, and source mode. V1 evidence is neither accepted nor retained. |
| Legacy import | Retired | `task import` accepts only managed V2 snapshots. No V1 workspace import, canonical-identity merge, legacy orphan, legacy evidence retention, or compatibility reader remains. |
| Legacy source retirement | Complete after verified V2 parity, with no exceptions | Remove all legacy source, command, TUI, runtime, compatibility, migration, and obsolete test paths. Deletion follows, rather than substitutes for, a verified V2-native replacement with unit, integration, and TUI coverage. |
| Current revision | Review-gated | Switch `current_revision_id` only after blocking validation passes and a `ReviewDecision=approve` is recorded. |
| Candidate retention | 7 days | Retain failed/no-op/discarded candidates and their evidence for exactly 7 days, then garbage collect only unreferenced candidate material through an idempotent audited operation. |
| Stage catalog | Standard groups | Use the 11 plan stage groups. Gates are durable `StageAttempt` records as well as review entities. |
| Template/Profile authority | Code-versioned | Workflow templates and execution-profile descriptors are versioned in code and their fingerprints are frozen into each run manifest. |
| Verdict policy | Repair-first | Deterministic content failures are `needs_repair`; infrastructure failures are `infra_failed`; security/policy violations are `reject`; warnings are `advisory`. |
| Budget configuration | Fully explicit | Every API/CLI execution request carries a complete persisted budget profile. Production has no implicit budget default. |
| Continuation-plan TTL | Frozen 24 hours | Every versioned ExecutionProfile must declare `continuation_plan_ttl=24h`; it participates in the profile fingerprint and is copied into the frozen Run manifest. `TaskContinuationService` derives plan expiry from that manifest. Callers cannot provide or override an expiry. |
| Quota scope | Task plus actor | Enforce task and actor quotas; the local installation is the single tenant. |
| Budget grants | Task owner | The Task owner may issue grants with actor, reason, optimistic version, and idempotency key. |
| Worker handoff | Controlled child worker | CLI/TUI may execute in-process, but detach hands work to a controlled child worker so durable jobs continue after the UI exits. |
| TUI exit handoff | Per active Run | Leaving the TUI enumerates active Runs and obtains a separate handoff decision for each one. It never cancels a shared root context or silently applies one exit decision to every Run. |
| Lease policy | Configurable | Default lease TTL is 90 seconds with a 20-second heartbeat; deployment profiles may override both values. |
| CLI signals | Pause / terminate | SIGINT requests pause; SIGTERM requests terminate. |
| External side effects | Local package only | Harbor Flow creates and records immutable packages in its managed local directory. It does not publish, copy to an external destination, or upload. Manual upload is outside Harbor Flow. Reconcile package creation from immutable receipts and package/source digests. |
| Release identity | Global immutable version | Version is globally immutable; channels are movable pointers. |
| Evidence retention | Permanent release pin | A Release permanently pins its task, package, and evidence artifacts. |
| Task purge | CAS, idempotent fenced operation | A real purge requires a soft-deleted task's expected version, a client idempotency key, audit reason, and explicit `--yes`. The durable purge operation freezes dependency facts, owns a task-scoped fencing lease across managed filesystem removal, and is replayed only with the same key. `--dry-run` is read-only. A completed operation is an irreversible task tombstone; later task-content and lifecycle mutations, including restore, are rejected. |
| Actor and audit | Local OS actor | Actor is derived from the local OS user; local users are owners and SQLite stores append-only audit records. |
| SQLite recovery | Automatic verified pure-V2 backup restore | Restore only the latest verified pure-V2 backup on corruption. Create a verified backup every 15 minutes and before critical operations. |
| Integration boundary | Local Docker | Run isolated real Docker integration fixtures; Codex, Harbor API, provider, and publishing remain fake unless separately authorized. |
| CLI compatibility | Hard cutover | Keep the `harbor-factory` binary name but remove old commands and old mutation routes. |
| Continuation plan TTL | 24 hours | A versioned execution profile carries a `continuation_plan_ttl` of exactly 24 hours; each frozen plan stores its absolute expiry from the run's frozen profile. |
| Continuation dry run | Ephemeral preview | `task continue --dry-run` computes and returns a preview without creating a command, frozen plan, audit event, candidate, job, or other durable record. |
| ChangeProvider scope | Local patch plus automated repair | The first provider set includes a structured local manual-patch provider and an isolated-candidate automated Repair Agent provider. Both consume structured findings and never write a sealed revision. |
| Manual patch payload | Unified diff | The local patch provider accepts a standard unified diff and applies it only to an isolated candidate checkout. It then enforces the strict Harbor V2 file policy and records the original diff, validation result, operation key, and receipt. |
| TUI mutation input | Full flow plus plan confirmation | The Task Hub provides a native mutation form and can also confirm an existing frozen plan. Confirmation requires a reason, derives the local OS actor, generates a UUIDv7 idempotency key when opened, and retains it across retries. |
| Attach and reconcile | Local durable runtime first | `run attach` and `run reconcile` cover local durable jobs, leases, control operations, and quota state in this delivery. External provider receipt reconciliation is handled by the ChangeProvider transaction rather than a generic run command. |
| Executor binding | Frozen plugin ID and version | Each compiled stage freezes its typed `plugin_id` and `plugin_version`; a controlled local registry resolves only the exact pair and rejects unknown or mismatched implementations. |
| Quota claim authority | Harbor code-versioned policy | Harbor policy declares resource claims in code; compiled claims are frozen into the execution descriptor and durable job payload, never recomputed from ad hoc caller input. |
| Quota account initialization | Automatic task and actor accounts | Admission automatically initializes the task and local-actor accounts from the frozen policy, with no hidden numeric defaults. |
| Durable job granularity | Coordinator plus StageAttempt jobs | A run/continuation coordinator expands the frozen schedule into idempotent durable StageAttempt jobs; stage execution, quota and control operate at that boundary. |
| Initial DAG scheduling | Dependency-layer concurrency | The coordinator starts every dependency-ready stage in a layer concurrently, subject only to frozen quota/capacity limits; catalog ordering is not an accidental serialization rule. |
| StartRun materialization | Managed canonical profile and spec | Before scheduling, `StartRun` writes the canonical frozen profile and typed `RunExecutionSpec` into the managed Run directory and records their digests in the Run manifest. Caller-owned loose files are not execution authority. |
| Outbox delivery | SQLite claim and lease dispatcher | Outbox consumers claim, heartbeat, acknowledge or retry records under SQLite fences; the outbox is not merely an audit list. |
| Run-control grace | Frozen deployment/profile policy | Grace is declared by the frozen execution/deployment profile, displayed read-only in CLI/TUI, and cannot be overridden by a control form. |
| Continuation run relation | Paused and recoverable failure stay in the Run | Paused and `failed_recoverable` continuations reuse the same Run when their frozen checkpoint is valid; canceled runs and every content revision continuation create parent-linked child runs. |
| Candidate provider budget | Whole-attempt bound | Provider execution receives `AttemptTimeout - StartupGrace - ShutdownGrace`; its candidate lease is bounded by `AttemptTimeout`. |
| Automated repair loop | Automatic through pass or exhaustion | Repair sessions automatically create the next round until checks pass, a frozen bound is exhausted, a non-repairable finding appears, or reconciliation requires a human. |
| TUI mutation CAS | Full captured checkpoint | Every displayed mutation captures the complete relevant task/revision/review/release/run checkpoint and rejects stale confirmation rather than refreshing it silently. |
| Continuation form | Native multi-step form | Continue Processing has a native reason/guidance form with system-recommended scope by default and typed advanced stage/provider/profile/budget/repair options. |
| Task Hub action coverage | Complete displayed action set | Every displayed lifecycle action receives a typed application command, confirmation flow, UUIDv7 idempotency and focused tests in this delivery. |
| Review and release selection | Detail selection | Task Detail lets the operator choose a concrete ReviewRequest or Release before entering the existing confirmation flow. |
| TUI source cutover | Delete legacy workspace TUI | Remove the residual legacy workspace TUI implementation and its obsolete tests; do not retain unreachable source as a compatibility path. |

## CodeEdge Phase-1 production execution decisions

| Area | Decision | Binding Rule |
| --- | --- | --- |
| Workflow descriptor | Independent closed CodeEdge Phase-1 template | CodeEdge never falls back to the Standard template. The frozen profile, execution specification, deployment catalog, registry and Run manifest must name the same template ID and version. |
| Evaluator invocation | Freeze `harbor run --n-attempts 4 --n-concurrent 1 --max-retries 3` | Qwen runs before Opus, and each model runs its four logical attempts serially. Harbor 0.18.0 records `--n-attempts` as the sample count and `--n-concurrent` as concurrency. `--max-retries=3` is Harbor-internal per logical Trial; it never creates a fifth sample or enables a generic stage retry. |
| Evaluation subject | Managed task snapshot | Evaluation binds the immutable task snapshot digest; it does not evaluate a mutable workspace or an early local package. |
| Package timing | Final compliance first | The single managed local package is created only after Qwen, Opus, submission checks and the final compliance decision have completed. It is never uploaded or published by Harbor Flow. |
| Catalog verification | Re-verify each external effect | Catalog receipt, lock identity and runtime attestation are checked immediately before every local command, container, agent or evaluator side effect, including retry/reconcile execution. |
| Metadata mapping | Explicit deployment mapping | CodeEdge `task.toml` fields are selected only by a versioned `MetadataFieldMapping`; no table/key convention is inferred. |
| Evaluation evidence | Trusted `result.json` plus one screenshot per model | A receipt is authoritative only when its result document, four logical trials, immutable identities and exactly one canonical screenshot are verified. |
| Evaluator reconciliation | Local immutable result observation | The evaluator never uploads a job or queries a remote service. Reconciliation reads only the managed local Harbor job directory and accepts a receipt only after `config.json`, `lock.json`, job `result.json`, exactly four Trial results, frozen identities and secret scans pass the strict local parser. Incomplete, ambiguous or malformed local evidence remains `in_doubt`; observation never invokes `resume` or a model. |
| Opus role | Reference evaluator | Opus evidence is retained and must be structurally valid, but it is not an independently invented hard pass/fail threshold. |
| External rate limit | No local three-hour throttle | Harbor Flow does not fabricate a local submission throttle; any externally required limit belongs in an explicitly approved deployment operation. |

The remaining deployment values (real command/image/model identities, secret
references, prompt/schema fingerprints, evaluator ABI, metadata paths, package
tooling and similarity policy) are intentionally not guessed. Until they are
supplied in a versioned catalog and lock, production composition must reject
the corresponding operation before it starts an external side effect.

## Current Deployment Activation Status

This status record does not change any confirmed target behavior above.  It
prevents a source-level contract, parser, or application port from being
mistaken for an enabled production operation.

- The generic Codex App Server runtime is retained as an implementation of the
  application-layer `agent.Runtime` port.  It is not, by itself, a production
  deployment capability: a production `agent.turn` provider must be separately
  catalog-attested and cannot fall back to a PATH-discovered executable, an
  ambient home directory, ambient authentication, or a default model.
- `AgentRepairProvider` and the generic agent port remain application-layer
  contracts, but the current CodeEdge production composition installs no
  `agent.turn` provider.  Automated repair therefore fails closed until its
  own model, prompt/schema, secret, timeout, checkpoint, and reconciliation
  contract is frozen and installed.
- The CodeEdge evaluator has no upload, remote-query or archive-download path.
  It creates and observes only managed local Harbor job directories; incomplete
  local evidence remains `in_doubt` until the strict local result parser can
  prove completion.
- The evaluator child receives a complete child-owned `ExecutionProfile` from
  the immutable deployment lock: its two stage budgets, continuation TTL,
  control grace, and candidate-provider budget are not projected from the
  parent Run. Missing or malformed lock-owned profile data remains fail-closed.

## Canonical Task File Policy V2

Required files:

- `instruction.md`
- `task.toml`
- `tests_analysis.md`
- `solution/solve.sh`
- `tests/test.sh`

Allowed environment files, at least one required:

- `environment/Dockerfile`
- `environment/docker-compose.yaml`

The managed materializer assigns `0755` to the two shell scripts and `0644` to
the remaining files. Raw bytes are authoritative; no line-ending, Unicode,
BOM, whitespace, or TOML normalization occurs.
