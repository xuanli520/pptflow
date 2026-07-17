# Standard Authoring -> CodeEdge Handoff Contract Refactor Plan

> Status: Proposed. Diagnosis complete; implementation has not started.
>
> Date: 2026-07-17
>
> Scope: Standard Authoring generation, stage-output validation, task
> materialization, the Authoring-to-CodeEdge handoff, CodeEdge preflight,
> continuation/repair guidance, TUI observability, deployment assets, locks,
> production packaging, and tests.
>
> Authority: This plan must not weaken immutable revisions, frozen workflow
> definitions, or repair-first verdict semantics recorded in
> `WORKFLOW_STABILITY_DECISIONS.md`. A future change to those binding decisions
> requires a separate approved decision, not an implementation shortcut here.

## 1. Executive Summary

The incident is a producer/consumer contract failure. Standard Authoring
successfully obtained all three human approvals and sealed a TaskRevision. Its
automatic CodeEdge Phase-1 child then rejected that same immutable task package
at the first deterministic preflight stage. The child Run therefore correctly
entered `waiting_continuation` with `completed/needs_repair`.

The failure is not an approval-state transition, outbox, worker lease, or TUI
action mapping defect. It is also not repairable by retrying the existing child
Run because retry executes the same frozen task snapshot.

The refactor must make a CodeEdge-compatible task package an explicit,
versioned Authoring output contract. The authoritative design is:

1. Build the final task package through a deterministic compiler, rather than
   treating six opaque model artifacts as directly materializable files.
2. Run the same complete CodeEdge admission validation before final human
   approval and before the irreversible `materialize_task` boundary.
3. Represent deterministic content violations as a structured Authoring
   `needs_repair` result with stage-to-source repair guidance.
4. Pin source provenance from the immutable AuthoringSource instead of trusting
   a model to reproduce a repository URL or commit hash.
5. Keep CodeEdge Phase-1 preflight as a defense in depth check. It must not be
   skipped simply because an upstream admission receipt exists.

This changes the Standard Authoring template and deployment contract. New
behavior must be released under a new template/catalog/profile/lock version.
Existing frozen `harbor.standard-authoring@1.2.0` Runs and revisions remain
historical facts and are never reinterpreted in place.

## 2. Incident Evidence And Boundary

### 2.1 Confirmed Runtime Facts

The observed parent and child relationship was:

```text
Standard Authoring parent Run
  task_review       approve/pass
  content_review    approve/pass
  solution_review   approve/pass
  materialize_task  pass
  status            succeeded

  durable handoff
        |
        v
CodeEdge Phase-1 child Run
  repo_prepare      completed/needs_repair
  status            waiting_continuation
```

The parent Run was `019f6d9d-591e-71ff-9954-8ad834aa7ebc`. Its final
`solution_review` decision was persisted as `approve` and the parent completed
successfully. The new task-bound CodeEdge child was
`019f6ddd-ede9-7807-9c52-efddb5b0e3ef`, linked by `parent_run_id` and triggered
by `standard-authoring.materialized`.

The child `repo_prepare` report contains five deterministic findings from three
source files:

| Package file | Finding | Why it is a contract failure |
| --- | --- | --- |
| `environment/Dockerfile` | `COPY . .` | CodeEdge rejects broad task-root and wildcard copy sources because they may copy solution, tests, or unreviewed task content into the build context. |
| `task.toml` | `metadata.commit_id` has 41 hexadecimal characters | The CodeEdge profile accepts a 7-40 character Git commit. The frozen AuthoringSource commit is 40 characters; the generated field contains one extra `b`. |
| `tests_analysis.md` | Three required Markdown sections are absent | Standard Authoring emitted a JSON document, while CodeEdge requires the documented Markdown sections: `instruction 和 environment 已提供的信息`, `模型的理论通过路径`, and `模型具备通过条件的依据`. |

There is also a latent source-provenance failure. Once the invalid 41-character
commit is corrected, the present Dockerfile still lacks the deterministic
source `git clone` and exact `git checkout` evidence required by the CodeEdge
profile for a non-zero-to-one task. The refactor must prevent all four classes,
not merely make this one report shorter.

The user invoked the task continuation once with reason `重试`. The continuation
execution itself succeeded, but it reread the identical immutable
`task_snapshot`; `repo_prepare` produced the same `needs_repair` report. This
is correct immutable-input behavior, not a failed retry transport.

### 2.2 Explicitly Excluded Causes

- The TUI `a` action maps to `approve`, not `request_changes`.
- An Authoring review `approve` transitions its own Run to `running` and
  schedules the successor; it does not write `waiting_continuation`.
- The child status is not a stale projection. The task board intentionally
  presents the newest child Run as the task's current Run.
- The child did not encounter a provider, Docker daemon, network, quota, lease,
  or outbox failure before entering `waiting_continuation`.

### 2.3 Evidence Index

The diagnosis is grounded in the frozen snapshot and the following code
boundaries. These references are also the minimum code-reading set before any
implementation begins:

| Boundary | Evidence | What it proves |
| --- | --- | --- |
| Standard generation DAG | `internal/harbor/workflowadapter/standard_authoring.go` and `deployments/standard-authoring/operation-catalog.v1.json` | The three problematic artifacts are independently model-generated before `solution_review`; no full CodeEdge task-package stage exists. |
| Submission-time validation | `internal/harbor/stageprovider/standard_authoring_codex_output_submission.go` | The submit handler validates the generic output envelope and Docker base-image policy, but not the complete downstream package contract. |
| Authoring materialization | `internal/app/standard_authoring_materializer.go` | The materializer reads the six artifacts, repeats the environment base-image check, and writes the supplied bytes to the sealed layout without full CodeEdge admission. |
| Consumer preflight | `internal/harbor/codeedge/preflight.go` | The downstream owner already has deterministic checks for metadata, analysis headings, broad Docker transfer sources, and repository clone/checkout evidence. |
| Child verdict mapping | `internal/app/codeedge_phase1_parent_executor.go` | A deterministic preflight validation error is intentionally emitted as `completed/needs_repair`. |
| Run-status projection | `internal/app/frozen_execution_runtime.go` | A completed `needs_repair` attempt is intentionally projected to `waiting_continuation`; it is not an approval-state rollback. |

The observed source facts must remain regression fixtures, not be treated as
one-off data repair: the frozen source commit is
`44bed484bf03f70782b1011b6cb527abb83e675c`, while the generated metadata
contained a 41-character value. The snapshot's `environment/Dockerfile` used
`COPY . .`, and `tests_analysis.md` contained JSON rather than the required
Markdown document.

## 3. Root Cause Chain

```text
Prompt and generic output schema accept opaque generated bytes
  |
  +-- dockerfile-generate only enforces the frozen base image
  +-- task-toml-generate asks the model to repeat immutable provenance
  +-- tests-analysis asks for a generic structured artifact
  |
  v
materialize_task directly writes the six artifacts into the sealed task layout
  |
  +-- validates task-policy layout and frozen base image only
  +-- has no full CodeEdge package admission check
  |
  v
human solution review approves a package that has not been validated as a
CodeEdge task package
  |
  v
CodeEdge child repo_prepare is the first whole-package consumer
  |
  v
completed/needs_repair -> waiting_continuation
```

The currently relevant implementation boundaries are:

- `internal/harbor/stageprovider/standard_authoring_codex_output_submission.go`
  validates generic structured submission shape and applies a Dockerfile base
  image check, but has no complete task-package validator.
- `internal/app/standard_authoring_materializer.go` consumes the six artifact
  bytes and writes `tests_analysis` directly to `tests_analysis.md`.
- `internal/harbor/codeedge/preflight.go` owns the deterministic full-package
  checks, including task metadata, tests-analysis headings, environment
  isolation, and provenance evidence.
- `internal/app/codeedge_phase1_parent_executor.go` correctly converts a
  repairable preflight validation error into `completed/needs_repair`.
- `internal/app/frozen_execution_runtime.go` correctly turns that verdict into
  `waiting_continuation`.

The defect is therefore an upstream contract gap. Downstream rejection is
correct and must remain fail-closed.

## 4. Invariants, Constraints, And Non-Goals

### 4.1 Required Invariants

1. A sealed TaskRevision is immutable. No retry, continuation, or repair may
   modify its snapshot in place.
2. A Run executes only its frozen template, profile, deployment catalog, lock,
   prompts, schemas, and task inputs.
3. `completed/needs_repair` means a deterministic content finding with durable
   evidence. It is not an infrastructure failure and must not be flattened into
   `failed_recoverable`.
4. `materialize_task` remains the only operation that creates the first task
   revision from an AuthoringSession, and it may not create a child Run without
   a sealed, validated revision.
5. CodeEdge must keep independently validating the task snapshot it executes.
   An upstream receipt cannot become permission to bypass its consumer-side
   checks.
6. No runtime may read a mutable current CodeEdge deployment profile to judge a
   frozen Standard Authoring Run. Compatibility inputs must be pinned into the
   Standard Authoring deployment contract and Run manifest.
7. Diagnostics must be machine-readable, deterministic, and bound to exact
   input artifact digests. They must not depend on a mutable workspace path.
8. The package admitted before review and the package sealed by materialization
   must be byte-for-byte equivalent under the same canonical compiler.

### 4.2 Non-Goals

- Do not patch the current child Run or its task snapshot in place.
- Do not make the generic `t` retry button mutate task content.
- Do not weaken CodeEdge's broad-copy, source provenance, or tests-analysis
  requirements to accommodate current output.
- Do not merge AuthoringSession and task-bound CodeEdge workflows into one
  Run.
- Do not rely on prompt wording alone as the acceptance mechanism.
- Do not add a generic untyped configuration map to workflow bindings or locks.

## 5. Target Architecture

### 5.1 One Versioned Task Package Admission Contract

Introduce a typed, versioned `CodeEdgeTaskAdmissionContract` owned by the
shared CodeEdge validation boundary. It defines the exact consumer constraints
that Standard Authoring must satisfy before handoff:

```text
contract identity
  + CodeEdge preflight profile fingerprint
  + CodeEdge parent template/catalog receipt and lock identity
  + required task layout version
  + tests-analysis rendering template version
  + environment isolation rule version
  + immutable source-provenance binding rules
```

The Standard Authoring deployment lock records the exact contract identity and
fingerprint. Its Run manifest freezes that identity with the existing template,
profile, prompt, and schema identities. The implementation must not look up
the active CodeEdge production deployment at materialization time.

The CodeEdge package remains the owner of syntax and semantic validation. The
Authoring package may orchestrate its use, but must not duplicate a second
Dockerfile parser, TOML provenance parser, or Markdown-heading validator.

Production root composition must construct and verify the CodeEdge parent
contract first, then inject the resulting typed immutable contract into the
Standard Authoring admission/materialization composition. A global mutable
configuration object, an ambient file read, or a late lookup of the active
CodeEdge deployment is prohibited. A missing or drifted parent identity is a
fail-closed configuration error.

### 5.2 Deterministic Task Package Compiler

Add an application-level compiler, proposed as
`internal/app/standard_authoring_task_package.go`, with a narrow typed API:

```go
type StandardAuthoringTaskPackageInput struct {
    Instruction      []byte
    TaskTOMLDraft    []byte
    Dockerfile       []byte
    SolveScript      []byte
    TestScript       []byte
    TestsAnalysis    []byte
    Source           store.AuthoringSource
    Environment      workflowadapter.StandardAuthoringEnvironmentPolicy
    Admission        codeedge.TaskAdmissionContract
    InputFingerprint workflowkit.Fingerprint
}

type StandardAuthoringTaskPackageResult struct {
    CanonicalFiles []TaskPackageFile
    Report         codeedge.TaskAdmissionReport
    Receipt        StandardAuthoringTaskAdmissionReceipt
}
```

The names are illustrative. The implementation must use repository naming
conventions and avoid exporting mutable workspace paths. The compiler has two
separate responsibilities:

1. Canonicalize the authored artifacts into the managed task layout in a
   private safe staging directory.
2. Run the frozen CodeEdge admission contract against that staged layout and
   return a deterministic pass or a structured repair report.

No Store mutation, TaskRevision allocation, handoff, provider call, or child
Run launch may occur before this compiler reports pass.

### 5.3 Canonicalization Rules

#### Source Provenance

`AuthoringSource.RepositoryURL` and `AuthoringSource.CommitSHA` are the source
of truth. The model must not be allowed to invent or accidentally alter them.

- Launch creates a sealed `source_provenance` input artifact containing the
  normalized repository identity and full frozen commit. The task TOML,
  Dockerfile, content review, admission, and materializer consume this typed
  artifact rather than rediscovering provenance from a checkout or prompt.
- The task TOML generation schema excludes immutable provenance fields from
  model-owned fields where possible.
- The compiler parses the candidate TOML and writes canonical `source`,
  `metadata.github_url`, and `metadata.commit_id` values from the frozen source.
- If a legacy-shaped candidate includes a conflicting source value, the
  compiler emits a deterministic repair finding rather than silently masking
  an inconsistency.
- The canonical commit value is lowercase and exactly the frozen source commit;
  it is never reconstructed from prompt text.

#### Tests Analysis

The model may continue to produce a structured analysis artifact, but the
structure must become a typed Authoring schema rather than arbitrary JSON.
The compiler renders the final `tests_analysis.md` deterministically:

```markdown
## 1. instruction 和 environment 已提供的信息
...

## 2. 模型的理论通过路径
...

## 3. 模型具备通过条件的依据
...
```

Additional coverage gaps, false-positive risks, and acceptance criteria may be
rendered after those three mandatory sections. The renderer, not a model,
owns heading spelling, order, and Markdown formatting. The renderer must be
unit-tested with the exact CodeEdge preflight profile.

#### Dockerfile

The Dockerfile prompt must explain the consumer contract, including an explicit
ban on broad `COPY` or `ADD` task-root/wildcard sources. The host must enforce
the same rule through the CodeEdge-owned validator at submission time and
again during full package admission. A valid generator must enumerate the
minimum build inputs required by the task instead of using `COPY . .`.

The current Standard environment policy continues to enforce the frozen base
image. The new admission contract adds CodeEdge isolation requirements; neither
validator replaces the other.

For a non-zero-to-one task, the same admission validator also verifies the
source provenance program in the Dockerfile: approved GitHub repository, no
credentials, exact frozen commit checkout, and no conflicting clone evidence.
The generated Dockerfile must use the contract-approved deterministic source
preparation pattern and explicit minimal copies where copies are necessary.

### 5.4 Workflow Placement

Add a new sealed Standard Authoring built-in stage,
`codeedge_package_admission`, after `solve_generate`, `test_generate`, and
`tests_analysis`, and before `solution_review`:

```text
content_review
  -> solve_generate
  -> test_generate
  -> tests_analysis
  -> codeedge_package_admission
  -> solution_review
  -> materialize_task
```

`codeedge_package_admission` receives all six content artifacts, the frozen source
identity, the frozen Standard environment policy, and the frozen admission
contract. It emits exactly one
`codeedge_package_admission_report` artifact with a versioned schema such as
`harbor.standard-authoring-task-package-admission.v1`.

| Admission outcome | Stage outcome | Run behavior | Side effects |
| --- | --- | --- | --- |
| Valid package | `completed/pass` | Continue to `solution_review` | Persist report evidence only. |
| Repairable content findings | `completed/needs_repair` | Authoring Run enters `waiting_continuation` | No TaskRevision, no handoff, no CodeEdge child Run. |
| Contract corruption or unverifiable frozen identity | Fail closed | `in_doubt` or the existing frozen-payload failure path | No TaskRevision, no handoff, operator reconciliation required. |

The final human solution review receives the pass receipt as an input and
displays the admission identity and report digest. A reviewer must not have to
infer package admissibility from unrelated generated files.

### 5.5 Materialization As A Second Fence

`materialize_task` remains the atomic commit boundary. It must require a pass
admission receipt bound to:

- the exact six input artifact bindings and their fingerprint;
- the AuthoringSource and AuthoringSession identities and digests;
- the frozen Standard environment policy;
- the frozen CodeEdge task admission contract; and
- the canonical package output digest.

Before calling `Store.MaterializeAuthoringTask`, the materializer rebuilds the
canonical package from the frozen inputs and verifies that its receipt matches
the passed admission receipt. It then writes the canonical files, seals the
revision, and records a handoff receipt that includes the admission receipt
identity/digest.

An absent, mismatched, or unverifiable admission receipt is a frozen contract
breach. Materialization fails closed before any Store mutation. It must not
attempt to fabricate a `needs_repair` handoff after allocating a revision.

The handoff schema needs a new version to carry the task admission receipt.
Readers must retain the old schema only for already-frozen historical Runs.
New Standard Authoring template Runs use the new schema exclusively.

### 5.6 CodeEdge Consumer Behavior

CodeEdge Phase-1 continues to materialize and inspect the actual sealed task
snapshot in `repo_prepare`. It may use the upstream admission receipt for
traceability and diagnostics, but it always performs its own validation.

If a newly admitted snapshot fails the same deterministic rule downstream,
that is a contract-defect alarm, not normal authoring output. The child report
must include both the upstream admission receipt identity and the CodeEdge
preflight profile identity so operators can distinguish a deployment drift
from a compiler defect.

## 6. Repair, Continuation, And User Experience

### 6.1 Repair Is A New Immutable Content Lineage

For a `codeedge_package_admission` repair report, continuation planning must map
diagnostic codes to the producer stages that need new artifacts:

| Diagnostic family | Minimum invalidated producer stages |
| --- | --- |
| `environment_isolation` | `dockerfile_generate` |
| `task_metadata` or source provenance | `task_toml_generate` |
| `tests_analysis` | `tests_analysis` |
| layout, cross-file, solution, or test inconsistency | The smallest complete dependency closure determined by the planner; never a blind rerun of the old immutable output. |

The repair request carries the immutable admission report as an input to the
new generation attempts. It creates new stage artifacts and, after a pass,
creates a new sealed TaskRevision. It never changes a task snapshot already
owned by a CodeEdge child Run.

This must be a new, restricted `AuthoringAdmissionRegeneration` flow. Existing
Authoring recovery does not currently make a `waiting_continuation` Authoring
Run actionable, so merely adding a `needs_repair` stage would recreate the
current dead-end under a different workflow version. The regeneration flow is
available only when all of the following are true:

- the Run is the new Standard Authoring template version;
- the target task is still an unmaterialized draft;
- the report is a verified admission report for that Run and its exact inputs;
- the planner can map every finding to an explicit permitted generation
  dependency closure; and
- no TaskRevision or handoff already exists for the AuthoringSession.

It invalidates and regenerates only the approved source closure, carries the
report as repair context, and reruns admission. It must not become a generic
arbitrary stage replay API.

For legacy task-bound child Runs that already reached
`waiting_continuation`, generic continuation remains a re-execution mechanism
only. The UI must direct the operator to an explicit repair revision flow,
because rerunning the same child snapshot cannot fix deterministic content.

### 6.2 TUI And CLI Requirements

The task board must show phase and lineage, not only the newest Run status:

```text
Authoring 1.3.0: succeeded
  -> CodeEdge Phase-1 child: waiting_continuation
     blocked by task package preflight, 5 repair findings
```

Required user-visible behavior:

- `waiting_continuation` identifies the exact stage, verdict, report digest,
  and repair findings.
- A task-bound immutable child with deterministic package findings offers
  `repair revision`, not a misleading generic `retry` as the primary action.
- An Authoring admission failure offers `repair and continue`; the planner
  displays the invalidated stages before execution.
- CLI/TUI present the same service-generated diagnostics and action policy.
- TUI tests cover background refresh of an open detail view as well as
  parent/child phase labeling.

## 7. File-Level Implementation Map

The exact filenames may change during implementation, but ownership must remain
as follows.

| Area | Primary files | Required change |
| --- | --- | --- |
| Shared consumer contract | `internal/harbor/codeedge/preflight.go` and focused new files in that package | Export typed task admission contract/report APIs and reusable deterministic validation helpers. Do not duplicate parsing rules in Authoring. |
| Authoring package compiler | New focused file(s) under `internal/app/` | Parse/canonicalize authored inputs, inject frozen provenance, render tests-analysis Markdown, stage safely, invoke frozen admission, and return receipt/report. |
| Authoring workflow model | `internal/harbor/workflowadapter/standard_authoring.go`, `execution_spec.go`, `internal/harborfactory/stages.go` | Add the sealed admission stage/binding, `source_provenance` and report artifacts, dependencies, resource sets, versioned template/catalog/profile contracts, and input/output validation. |
| Launch and resolver wiring | `cmd/production_root_composition.go`, `cmd/standard_authoring_production.go`, `internal/app/standard_authoring_launch_service.go`, deployment catalog resolver/composition files | Verify CodeEdge parent identity before constructing Standard components; bind the new built-in only for the new Standard template and carry the frozen admission contract into the Run execution spec. |
| Model submission checks | `internal/harbor/stageprovider/standard_authoring_codex_output_submission.go` | Add stage-specific deterministic candidate validation for task TOML, Dockerfile isolation, and typed tests-analysis input while preserving generic output ownership. |
| Materialization | `internal/app/standard_authoring_materializer.go` | Require and verify a pass receipt, compile canonical files before Store mutation, and emit the new handoff receipt version. |
| Handoff and child admission | `internal/harbor/workflowadapter/standard_authoring_handoff.go`, Store trigger/handoff services, CodeEdge parent executor | Carry receipt lineage, retain defense-in-depth preflight, and surface an upstream/downstream contract mismatch clearly. |
| Repair planning | `internal/app/task_continuation_service.go`, `internal/app/authoring_recovery_service.go`, task-board services | Plan content repair from structured finding codes instead of rerunning unchanged task snapshots. |
| TUI and CLI | `internal/tui/*`, `internal/app/task_board_service.go`, `cmd/*` | Display parent/child phase, durable findings, and the correct repair action. |
| Deployment assets | `deployments/standard-authoring/prompts/*`, schemas, `contract-assets.v1.json`, catalog, execution profile, README | Version prompts/schemas, add the admission asset/operation, and document the new frozen contract. |
| Lock and production tools | `tools/standard-authoring-lock-build`, production build scripts/tests | Validate the new asset and contract identity, regenerate locks from a clean source snapshot, and bind the resulting lock to the binary. |

## 8. Version And Migration Strategy

### 8.1 New Contract Versions

Use an additive Standard Authoring deployment release, proposed as template and
catalog `1.3.0`. The precise version is finalized during implementation, but
it must change because stage topology, inputs, handoff receipt, and execution
semantics change.

The commit-length incompatibility is an explicit compatibility decision. The
recommended outcome is to update the shared CodeEdge admission contract from
7-40 to 7-64 hexadecimal characters, preserving Standard Authoring's documented
support for both SHA-1 and SHA-256 Git object IDs. This requires CodeEdge
preflight/profile/version/lock tests to prove exact checkout behavior for both
lengths. If the product cannot support SHA-256 Git sources, Standard Authoring
must reject a 64-character source at admission time with a clear explanation;
it must not accept it and later create an impossible child workflow.

Version all modified assets independently:

- Dockerfile prompt and its frozen fingerprint;
- task TOML prompt/schema or deterministic provenance compiler contract;
- typed tests-analysis schema and renderer template;
- task package admission report schema;
- Standard Authoring handoff receipt schema; and
- Standard Authoring admission policy/contract identity.

### 8.2 Historical Runs And Revisions

- `1.2.0` Run manifests remain executable only under their existing frozen
  definition and deployment receipt. Do not insert the new stage into them.
- Template dispatch must remain version-aware during the upgrade. Do not
  replace an exact-version predicate with a single "current Standard" check
  that would strand already-running `1.2.0` sessions after the `1.3.0` release.
- Existing invalid TaskRevisions remain immutable. Their child Runs retain the
  recorded `needs_repair` evidence.
- Current stuck child Runs require an explicit new repair revision. They may
  not be silently restarted against a changed template or patched snapshot.
- New `1.3.0` sessions do not materialize a task or launch a child CodeEdge Run
  until task package admission passes.

### 8.3 Rollback Boundary

Rollback is deployment selection, not mixed assets. Retain the previous
complete production package for historical frozen Runs. Do not combine a new
binary with old Standard deployment assets or regenerate only one lock in
place.

## 9. Test Strategy

### 9.1 Unit Tests

Add table-driven tests for the package compiler and shared admission validator:

- current incident fixture: broad `COPY . .`, 41-character commit, and JSON
  `tests_analysis.md` all produce deterministic sorted findings;
- valid explicit-copy Dockerfile and canonical 40-character source commit pass;
- valid deterministic clone/checkout evidence passes, while an incorrect
  repository, wrong checkout, missing checkout, credentials, or 64-character
  source follows the explicitly selected SHA-256 policy;
- a model-supplied provenance mismatch is rejected or explicitly diagnosed;
- renderer output contains the three required headings exactly once, in order,
  with substantive body content;
- malformed or duplicate typed tests-analysis fields are rejected;
- the compiler never writes outside its private staging directory and never
  returns mutable paths;
- CodeEdge and Authoring invoke the same rule implementation for each relevant
  Dockerfile/TOML/Markdown case.

### 9.2 Stage And Workflow Tests

- An invalid package admission returns `completed/needs_repair`, persists the
  report artifact, and creates neither a revision nor a handoff nor a child
  Run.
- A pass admission makes `solution_review` available only with the receipt
  bound to the same input fingerprint.
- A restricted Authoring admission regeneration invalidates the minimal source
  stages, supplies the immutable report to generation, and produces new
  artifact identities.
- An approved valid package materializes exactly one revision and exactly one
  CodeEdge child Run.
- Materializer rejects a missing, stale, tampered, or different-input pass
  receipt before `MaterializeAuthoringTask` is called.
- A CodeEdge child rechecks the sealed snapshot after handoff; an intentional
  validator mismatch is observable as a contract alarm.

### 9.3 Store And Handoff Tests

- New receipt schema validates immutable source/session/revision/digest and
  contract fingerprint bindings.
- Store triggers accept the new handoff only for the new template version and
  continue to read old frozen handoffs without reinterpretation.
- Idempotent materializer replay reuses the same sealed revision but cannot
  bypass or replace the admission receipt.
- Concurrent repair/continuation attempts cannot create two revisions from one
  admission receipt.

### 9.4 UI And CLI Tests

- The board differentiates an Authoring parent from the newest CodeEdge child.
- Details render deterministic repair findings even when `ErrorText` is empty.
- `waiting_continuation` for content defects routes to repair revision guidance,
  not generic retry alone.
- A locally submitted approval clears stale detail state; an externally
  resolved approval refreshes an open detail safely.
- CLI and TUI requests produce the same repair plan and idempotency behavior.

### 9.5 End-To-End And Release Tests

Run a controlled Authoring-to-CodeEdge workflow with a valid fixture and prove:

```text
authoring approvals
  -> codeedge_package_admission pass
  -> one sealed revision
  -> one CodeEdge child
  -> CodeEdge repo_prepare pass
```

Run the incident fixture and prove no revision/handoff/child is created before
repair. Include race tests for the new application/store boundaries, static
analysis, lock generator tests, deployment catalog validation, and complete
production package smoke tests.

## 10. Deployment, Lock, And Production Release

Implementation changes to prompts, schemas, catalog, execution profile,
admission contract, or materialization behavior require all of the following:

1. Update the Standard Authoring deployment README and relevant CodeEdge/TUI
   documentation.
2. Regenerate the Standard Authoring catalog lock with
   `scripts/generate-standard-authoring-lock.sh` from a clean committed source
   snapshot.
3. Regenerate all three ignored production locks before the final production
   build because the source build identity covers the complete Git tree.
4. Build the complete package through
   `scripts/build-codeedge-production.sh`; never replace only the binary.
5. Verify `SHA256SUMS` and run a fresh-root smoke test using the package's own
   binary and sibling `deployments/` tree.
6. Record the new package identity and verify that old frozen Runs remain
   readable under the retained complete prior package.

The planning document itself does not require a production rebuild. The rebuild
becomes mandatory when this plan is implemented and released.

## 11. Acceptance Criteria And Observability

The implementation is complete only when all statements below are true:

1. A new Standard Authoring Run cannot materialize or hand off a package that
   the frozen CodeEdge admission contract would reject for the incident's three
   classes of error.
2. The admission report is durable, input-bound, visible to reviewers, and
   usable as repair context.
3. Provenance in a new task package is derived from the immutable source, not
   from a model's copied commit string.
4. `tests_analysis.md` is canonical Markdown satisfying the CodeEdge profile,
   while any model-facing structured analysis is validated and rendered
   deterministically.
5. Broad Dockerfile copy sources are blocked both before final review and at
   materialization defense-in-depth.
6. The CodeEdge child still validates the sealed snapshot independently.
7. Retrying an immutable failed child cannot be presented as content repair;
   the UI/CLI exposes an explicit new revision repair flow.
8. Old frozen Runs/revisions are unchanged, and new Runs are isolated by the
   new template/catalog/lock identity.
9. Unit, integration, E2E, race, lock-build, and production package tests pass.

Emit durable/auditable telemetry or inspection fields for:

- admission contract identity and fingerprint;
- admission outcome and normalized finding code;
- report artifact ID/digest and input fingerprint;
- source-stage invalidation plan selected for repair; and
- upstream admission versus downstream CodeEdge preflight agreement.

## 12. Implementation Order

1. Add regression fixtures from this incident and characterize the current
   failure without changing behavior.
2. Extract/version the shared CodeEdge admission contract and pure validators.
3. Implement typed tests-analysis input plus deterministic Markdown rendering.
4. Implement frozen-source provenance injection and Dockerfile isolation
   validation in Standard output submission.
5. Establish CodeEdge parent contract construction before Standard composition,
   including the chosen 40/64-character commit compatibility policy.
6. Add the task package compiler and focused unit tests.
7. Add `codeedge_package_admission` to the new Standard template, execution spec,
   resolver, deployment assets, and lock generator.
8. Gate solution review and materialization on a pass receipt; add the
   materializer second fence and new handoff receipt version.
9. Implement restricted Authoring admission regeneration plus TUI/CLI
   phase/finding/repair presentation.
10. Run cross-template integration and E2E regressions, then regenerate locks
   and build a complete production package.
11. Exercise a fresh control-plane root with the packaged binary, verify the
   valid and invalid paths, and document the release identity.

No phase may skip its preceding tests or use a live mutable deployment profile
as a substitute for the frozen contract.
