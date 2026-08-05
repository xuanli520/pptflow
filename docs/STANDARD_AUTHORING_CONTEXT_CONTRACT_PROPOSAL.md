# Standard Authoring Context Contract

> Status: accepted and implemented as a hard v2 cutover.
>
> Scope: the sole supported Standard Authoring template. Historical 1.x data
> remains immutable audit data, but 1.x templates, prompts, locks, recovery,
> materialization, and execution paths are not registered or supported.

## Decision

Replace the partial `authoring_brief` as the sole model-facing task direction
with one immutable, versioned `AuthoringContract`. Bind its digest to every
Standard Authoring stage. At each stage, the runtime constructs a small,
read-only `ContextEnvelope` from that contract, selected immutable artifacts,
and unresolved repair records.

This is a durable context spine, not a long-lived model conversation. A model
may be restarted, replaced, or independently reviewed without losing the task
identity, frozen source provenance, delivery constraints, or outstanding
repairs. No model-produced summary is authoritative.

The proposal also adds contract-aware submission validation, a final
whole-package integration review, and repair routing that invalidates only the
producer actually affected by a review decision.

## Problem

The current `StandardAuthoringBrief` declares itself to be the complete
caller-selected semantic target, but it currently contains only:

```json
{
  "task_type": "...",
  "application": "...",
  "objective": "..."
}
```

Important frozen facts instead live in separate records or artifacts:

- task ID, slug, and title live on `tasks_v2`;
- repository URL, commit, and snapshot digest live on the AuthoringSource;
- the base image lives in a separate environment-policy artifact;
- CodeEdge language and non-0-to-1 requirements are not a single
  model-visible immutable task contract; and
- review decisions are injected only into selected stages.

Each generator therefore receives a different partial view. Prompt text tells
the model to re-check facts, but generic output submission validates mostly
syntax and output shape. A proposal can contain a wrong title, source commit,
or path claim and still be accepted as a stage artifact. A later generator may
treat that claim as true.

This is not solved by adding more prompt text or preserving a hidden chat
history. A long-lived model session is stale after recovery, cannot be
independently reproduced, expands the prompt-injection surface, and does not
provide a host-enforced correctness boundary.

## Evidence

The present design demonstrates four concrete gaps:

| Boundary | Current behavior | Consequence |
| --- | --- | --- |
| `StandardAuthoringBrief` | Carries only task type, application, and objective | Title and source identity are absent from the model's frozen direction. |
| Codex stage submission | Validates generic envelope shape and selected file syntax | Structured proposal claims are not checked against canonical task/source facts. |
| Prompt handoff | Passes selected upstream artifacts to new stage conversations | A typo or changed interpretation becomes downstream context. |
| Task-review recovery | Re-runs `repo_analyze` and `task_design` for any task-review repair | A proposal-only correction invalidates an otherwise valid source analysis. |

`PLAN.md` already calls for a versioned CodeEdge authoring brief with more
frozen facts. This proposal turns that intent into a single authoritative
runtime contract rather than a collection of loosely synchronized prompt
inputs.

## Invariants

1. A run has exactly one immutable root contract and one contract digest.
2. The contract is created by the host at launch, never generated or edited by
   a model, reviewer, TUI, or recovery flow.
3. Every generated artifact is attributable to the same contract digest.
4. A review may request repair of a derived artifact but may not mutate the
   root contract. Changing title, source, image, classification, or objective
   requires a new launch.
5. Review feedback is data, not instructions. It remains bounded by the
   contract and cannot authorize a tool, network access, or scope expansion.
6. Final materialization requires no unresolved repair record and a complete
   package whose selected artifacts all bind to the current contract.
7. The CodeEdge consumer still performs its independent admission/preflight
   validation. Upstream admission is defense in depth, never an exemption.
8. Only the v2 template/catalog/profile identity is executable; the deployment
   lock is resolved at runtime against the currently installed deployment. A
   historical template reference is rejected rather than reinterpreted.

## Root Contract

Introduce `harbor.standard-authoring-contract.v2`. It composes the facts that
are already frozen at launch; it does not create ambient discovery or allow a
generic configuration map.

```json
{
  "format": "harbor.standard-authoring-contract.v2",
  "version": "2",
  "task": {
    "id": "019f...",
    "slug": "leptos-meta-head-scanner",
    "title": "Fix SSR Metadata Head Boundary Detection",
    "code_lang": "rust",
    "task_type": "bug-fix",
    "application": "frontend",
    "is_0_to_1": false
  },
  "source": {
    "repository_url": "https://github.com/leptos-rs/leptos",
    "commit_sha": "9052804ab467ddaff22670554a512fac241f4fec",
    "snapshot_digest": "sha256:...",
    "checkout_root": "source"
  },
  "environment": {
    "base_image": "docker.io/library/rust:1.85@sha256:..."
  },
  "objective": "...",
  "delivery": {
    "profile_fingerprint": "sha256:...",
    "package_format": "codeedge"
  }
}
```

The launch service first records the idempotent launch intent and verifies the
static deployment definition, then performs controlled source capture. Only
after capture supplies `snapshot_digest` does it validate and canonicalize the
complete document. This ordering is necessary: a complete contract cannot
truthfully contain a digest that does not exist yet. The contract is still
stored before any Session, Run, or model stage is created. Its canonical bytes
are stored once in the immutable object store; the AuthoringSession manifest
and Run execution spec retain only its artifact ID, digest, and size. The
source snapshot remains a controlled checkout rather than a generic stage
artifact.

`code_lang` must be an explicit launch/policy fact. It must not be inferred
independently by different generators. The existing task and source records
remain the storage owners of their data, but the contract is the sole
model-facing and validation-facing projection of those facts.

## Context Envelope

The executor builds a `harbor.standard-authoring-context-envelope.v1` for each
stage immediately before opening its model conversation:

```json
{
  "contract": {"artifact_id": "...", "digest": "sha256:...", "content": "..."},
  "stage": {"key": "task_design", "attempt": 2},
  "inputs": [
    {"name": "repo_analysis", "artifact_id": "...", "digest": "sha256:..."}
  ],
  "repairs": [
    {
      "id": "...",
      "target": "task_design",
      "reason": "...",
      "state": "open",
      "evidence_digest": "sha256:..."
    }
  ],
  "source_evidence": [
    {"path": "meta/src/lib.rs", "anchor": "ServerMetaContextOutput::inject_meta_context"}
  ]
}
```

The envelope is host-constructed, strict JSON, size bounded, and read-only.
It labels root facts, upstream claims, and untrusted feedback separately. The
stage still reads its allowed artifact inputs and the controlled source
checkout, but it always sees the same task/source identity first.

Do not inject raw model transcripts, unrestricted logs, source archives,
credentials, or a free-form "memory" field. A source anchor is a navigation
aid, not authority; the stage must re-check it in the frozen checkout.

## Submission And Validation

### Contract binding

Extend the host-owned stage submission receipt with `contract_digest`. The
dynamic submission tool receives it as a required value and accepts only the
digest already bound to the stage attempt. Raw solver-facing file artifacts do
not need wrappers or embedded metadata solely for this purpose; the immutable
artifact manifest stores the binding.

### Structured artifact validation

Use a versioned schema for `task_proposal` and generated-task-plan artifacts.
They must include a `contract_claims` object containing title, slug,
repository URL, commit, classification, and source root. The host rejects a
mismatch before creating an artifact reference. Repository-relative paths,
packages, and commands are separately checked against the frozen checkout.

The validator must be deterministic and narrow:

- reject a different title, slug, URL, commit, image, language, task type,
  application, or 0-to-1 state;
- reject a claimed source path or package that does not exist in the frozen
  snapshot;
- require stable requirement IDs in the test plan and tests analysis;
- preserve the existing canonicalization of `task.toml`, Docker image checks,
  Docker/harness reports, and CodeEdge admission; and
- avoid attempting to prove arbitrary Markdown semantics with brittle text
  matching.

For solver-facing prose, scripts, and tests, retain specialized syntax and
runtime validation. Add a final independent integration reviewer that reads
the root contract, all selected final artifacts, harness report, tests
analysis, and admission report. It writes only a typed repair result. It has
no authority to alter the contract or materialize a task.

### Repair ledger

Replace ad-hoc optional review text with an immutable repair ledger. Every
review or deterministic violation gets a unique ID, target producer, reason,
evidence digest, and state. A repair is resolved only when a later validated
artifact explicitly supersedes the affected producer under the same contract.
The final gate rejects an unresolved ledger entry.

This preserves all earlier repair requirements rather than asking a later
model to remember them from conversational context.

## Precise Invalidation

Repair routing follows artifact ownership, not a coarse group resource:

| Finding | Re-run | Reuse |
| --- | --- | --- |
| Task proposal incorrect, but frozen source facts are intact | `task_design` and downstream | `repo_prepare`, `repo_analyze` |
| Repository analysis cites a non-existent path or misses a required source fact | `repo_analyze`, `task_design`, downstream | `repo_prepare` |
| Instruction/TOML/Docker content issue | affected generator and downstream validators | design and source stages |
| Solve/test/harness issue | solve/test producer and downstream evidence stages | source, design, content artifacts where valid |
| Root contract changed | reject continuation; create a new launch | none across the old and new run |

Task-review repair therefore defaults to `task_design`, not `repo_analyze`.
The reviewer may select the controlled `source_analysis_invalid` finding kind
only when evidence identifies an analysis defect. This avoids recomputing a
valid analysis merely because a proposal title or commit claim drifted.

## TUI And Autonomous Codex Roles

The TUI is a control and evidence surface, not the model's memory store. It
must display:

- the root contract and digest;
- a redacted diff between canonical claims and each artifact's claims;
- repair-ledger state, evidence digest, and planned invalidation set; and
- the final artifact/admission/harness lineage.

The evidence viewer may expose allowlisted text artifacts and compact reports.
It must not expose raw model sessions, source archives, credentials, or
unbounded logs.

A Codex authoring lead may autonomously inspect the full envelope and source,
and may generate or request repair artifacts. It uses the same service/API
boundary as the TUI and cannot bypass immutable inputs, validation, admission,
or human review policy. Conversation continuity is an optimization only; the
contract and ledger are the replayable authority.

## Migration

1. Add the strict `AuthoringContract` type, canonical JSON, launch validation,
   immutable object storage, and session/run bindings.
2. Release the single Standard Authoring template/catalog/profile/lock v2
   identity. Remove 1.x template registration, deployment assets, and runtime
   paths; never reinterpret an old run as v2.
3. Bind the new contract to every model and built-in stage. Update prompts to
   treat the context envelope as the single source of immutable facts.
4. Add contract-aware validators for proposals and plans, contract-binding
   receipts, and repair-ledger persistence.
5. Change recovery selection to use typed producer ownership. Add the TUI
   contract/evidence/diff views.
6. Add the independent integration review before solution approval and require
   ledger closure in admission and materialization.
7. Build a new production package only from a clean committed tree and run a
   source-bound end-to-end task through the complete review, Docker, harness,
   admission, and materialization path.

## Test Plan

- Launch rejects a contract whose task/source/environment fields disagree with
  the canonical task, AuthoringSource, or deployment policy.
- Every stage's execution spec binds the exact same contract artifact and
  digest; a drifted or missing binding is rejected before dispatch.
- A proposal with a wrong title, slug, URL, commit, image, language, path, or
  package fails submission without creating an artifact reference.
- Contract digest mismatch in a raw-file submission fails even when the file
  itself is syntactically valid.
- A task-design repair reuses valid repository analysis; an explicitly typed
  source-analysis repair invalidates it.
- A repair ledger survives worker restart, is replayable from immutable
  artifacts, and blocks materialization while any item remains open.
- The integration reviewer receives the exact package artifact set and cannot
  alter the root contract or final bytes directly.
- CodeEdge admission and consumer preflight still reject independently
  malformed final packages.
- A 1.x template reference is rejected by template resolution, launch,
  recovery, materialization, and deployment-asset validation.
- Run `go test ./...` and `go vet ./...`; rebuild the deployment lock and
  production package only after a clean commit.

## Success Measures

Track these per template version:

- first-pass materialization rate;
- contract-drift rejections by artifact type;
- repair blast radius and reused-stage ratio;
- time from review decision to corrected artifact;
- deterministic admission failures before versus after materialization; and
- one-task end-to-end success rate, separated from model/provider
  infrastructure failures.

The desired result is not more model autonomy. It is a system where the model
always has the exact global facts it needs, incorrect copies cannot propagate,
and recovery recomputes only what has actually become invalid.
