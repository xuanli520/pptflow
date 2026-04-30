# p2r TUI QA Workflow Improvement Plan

Date: 2026-04-30
Status: Ralph-reviewed implementation baseline — updated 2026-04-30
Scope: Improve p2r TUI/CLI as an end-to-end graphical QA orchestration tool for real prompt2repo delivery packages.

## 1. Background

We used the three real QA packages under `projects-qa` to exercise the current p2r TUI/CLI:

- `TASK-20260327-6A5EE0`
- `TASK-20260327-D3040D`
- `TASK-20260327-E3E478`

The current implementation can already:

- Scan and index the three projects.
- Run Stage A structural/static scripts.
- Store run/stage/finding records in SQLite.
- Show project status in the TUI.
- Generate `repair_summary.json` and `short_comment.txt`.
- Collect real Docker and runtime artifacts in some historical runs.

But the product is not yet reliable enough as a daily QA workbench. The main gaps are:

- Codex CLI flag compatibility blocks all current D/E static review stages and would also block F once F becomes Codex-driven.
- Codex cannot start at all due to `node: not found` (Node.js not on PATH).
- Stage A failure blocks B and C from running, preventing complete evidence collection.
- Stage F (`3_标注员AI报告问题的修复报告.md`) is mechanically generated instead of being a real Codex static analysis.
- TUI execution view loses important stage names and truncates key evidence.
- Supplemental QA files are assumed to live inside the original delivery package.
- Docker/runtime cleanup is insufficient, causing port conflicts and polluted subsequent runs.
- The current TUI is functional but not yet clear enough for fast human triage.

This plan merges the engineering review findings with the product requirements discussed after the end-to-end test.

### Ralph Review Corrections Applied

The Ralph review pass found several design holes in the earlier draft and fixes them in this version:

- Static-review scope was inconsistent: Workstream A described D/E only, while later sections made F Codex-driven. The plan now treats D/E/F as one static-review capability family.
- Stage independence was too broad: B and C should not be skipped because A failed, but C still needs B runtime evidence. The plan now distinguishes prior-stage blocking from data-dependency failure.
- Recheck report mapping was wrong: F was described as an API endpoint coverage report even though D owns that concern. The plan now maps D/E/F confirmation reports to their canonical stage responsibilities.
- Docker cleanup lacked concurrency, identity, and collision safeguards. The plan now requires task/run locking, hash-suffixed compose project names, p2r labels, and explicit `--keep-runtime` semantics.
- Supplemental docs lacked reproducibility and safety details. The plan now requires managed copies, manifest versioning, size limits, binary skip reasons, and untrusted-context wrapping for D/E/F.
- Several "open decisions" were already product defaults. The plan now resolves them so implementation does not stall at the planning gate.

## 2. Product Principles

1. **The original p2r package is evidence, not workspace.**
   p2r should not require QA operators to modify or add files into the submitted delivery package.

2. **The tool must tolerate messy real submissions.**
   File names, supplemental evidence names, and directory habits may vary. p2r should ingest and normalize, not force ideal names.

3. **Runtime evidence must be reproducible and isolated.**
   Every run should start from a clean p2r-managed Docker state and should not leave ports, containers, networks, volumes, or local images that interfere with the next run.

4. **Static review must degrade gracefully.**
   D/E/F should fail only when the static reviewer is truly unavailable or unsafe, not because one optional Codex CLI flag changed names.

5. **TUI must answer the QA operator's first three questions quickly.**
   What failed? Why did it fail? Where is the evidence?

## 3. Workstream A: Codex CLI Compatibility & Runtime Environment

### Current Problem

There are two blocking issues preventing Codex from running D/E/F stages:

**Issue 1 — Node.js not on PATH**: The Codex CLI is a Node.js script that `exec`s `node`. On the current machine, Node.js is installed (v25.7.0) at the same directory as the `codex` script, but `node` is not on the WSL PATH. This causes `exec: node: not found` — Codex cannot start at all.

**Issue 2 — Flag compatibility**: Current preflight requires `codex exec --help` to contain all of:

- `--ask-for-approval`
- `--sandbox`
- `--cd`
- `--ephemeral`

On the current machine, `codex-cli 0.125.0` supports `--sandbox`, `-C/--cd`, and `--ephemeral`, but does not expose `--ask-for-approval`. This causes D/E to be marked blocked even when Codex would otherwise be usable, and would also block F once F becomes Codex-driven.

Both issues must be fixed — the environment issue is the hard blocker, and the flag compatibility issue prevents graceful degradation.

### Target Behavior

p2r should build the Codex command from detected capabilities:

- Require `codex exec`.
- Require a read-only sandbox mode or a safe equivalent.
- Prefer `--sandbox read-only` when available.
- Prefer `--cd <project>` or `-C <project>` when available.
- Use `--ephemeral` when available.
- Use `--ask-for-approval never` only when available.
- Use `--skip-git-repo-check` and `--ignore-user-config` only when available.
- Do not block D/E/F solely because `--ask-for-approval` is absent.
- Always use a per-stage temporary `CODEX_HOME` under the run artifact root.
- Always pass the review prompt through stdin.
- Always treat self-test reports, ref-run reports, and attached documents as untrusted context.
- Static stages must still materialize a stage record and an unavailable-review artifact when Codex is unsafe or unavailable; preflight should not silently remove the expected report files.

### Proposed Implementation

**Node.js PATH fix** (`internal/codex/sandbox.go` plus the Codex runner):

- Resolve the `codex` path with `exec.LookPath`, then resolve symlinks when possible.
- Detect `node` in the same directory as the resolved Codex executable and common npm shim directories.
- If found, prepend that directory to `PATH` in the sandbox environment without removing the caller's existing `PATH`.
- Record the Codex path, resolved path, Node path, and PATH injection decision in `preflight.json`.
- This ensures Codex's `exec: node` call resolves without requiring system-wide PATH changes.

**Codex CLI adapter** (`internal/codex/cli.go`):

- `DetectCLI(ctx, exec, path) Capability`
- `BuildExecArgs(capability, projectPath, extraArgs) ([]string, error)`
- `Capability` fields:
  - `Path`
  - `ResolvedPath`
  - `Version`
  - `HasSandbox`
  - `HasAskForApproval`
  - `HasCDLong`
  - `HasCDShort`
  - `HasEphemeral`
  - `HasSkipGitRepoCheck`
  - `HasIgnoreUserConfig`
  - `NodePath`
  - `PathPrependedForNode`

Change preflight:

- `codex` check should be `ok` if a safe static-review command can be built.
- Missing optional flags should be recorded as `degraded`, not `missing`.
- Missing read-only sandbox support should remain blocking.
- The Codex check must list affected stages as `D`, `E`, and `F` once F is Codex-driven.
- Unsafe `codex.extra_args` remains blocking because it can change sandbox, working directory, or approval boundaries.

Change Stage D/E/F runners:

- Use adapter output rather than hard-coded args.
- Keep existing unsafe `codex.extra_args` validation.
- Keep read-only sandbox mode as a required boundary.
- Keep per-stage `.codex-home-D`, `.codex-home-E`, and `.codex-home-F` cleanup.
- If both `--cd` and `-C` are unavailable, run Codex with executor working directory set to the project path only if the detected CLI behavior is covered by a unit test; otherwise mark the command unsafe.

### Acceptance Criteria

- Codex can start and execute without `exec: node: not found` errors.
- D/E/F are not blocked on current `codex-cli 0.125.0` solely because `--ask-for-approval` is missing.
- Existing older Codex CLI help output remains supported in unit tests.
- If sandbox support is absent, D/E/F produce clear unsafe-environment messages and materialized unavailable-review artifacts.
- `preflight.json` records the exact Codex capability decision and environment diagnostics.

## 4. Workstream B: Stage Names and TUI Readability

### Current Problem

`stage_status.json` contains stage names, but the SQLite `run_stages` table does not persist `name`. The TUI reads stages from SQLite, so execution view shows blank stage-name columns for existing and new runs.

The TUI also truncates key values such as status, artifact paths, and error summaries too aggressively.

### Target Behavior

The TUI execution view must always show:

- Stage letter.
- Stage display name.
- Status.
- Duration.
- Error summary or blocked reason.
- Selected-stage log preview.
- Top findings and their evidence path.

Old runs must remain readable even if the DB row has no stored stage name.

### Proposed Implementation

Data model:

- Add schema version `v4`.
- Add `run_stages.name TEXT`.
- Update the fresh schema and add a `migrateV3ToV4` migration; do not rely only on `CREATE TABLE IF NOT EXISTS`, because existing databases will keep the old table shape.
- Update legacy inference so a database with `run_stages` but no `name` column is treated as pre-v4.
- Persist `StageRecord.Name` in `PutStage`.
- Read `name` in `Stages`.
- Fallback to stage display name when old rows have empty names.

Code organization:

- Export or centralize stage display names, for example:
  - `pipeline.StageDisplayName(stage string) string`

TUI:

- Replace blank stage name with fallback display name.
- Keep stage order fixed as A, B, C, D, E, F even when SQLite returns a partial set.
- Use responsive widths based on terminal size.
- Add a detail block for the selected stage:
  - full status
  - full error summary
  - log path
  - artifact paths
  - selected finding titles
- Avoid putting critical information only in a narrow table cell.

### Acceptance Criteria

- Existing historical runs display meaningful stage names.
- New runs persist stage names in SQLite.
- Terminal widths around 100 columns remain usable.
- No key blocked reason is visible only as a truncated fragment.

## 5. Workstream C: Supplemental QA Files Outside Original Package

### Current Problem

The current D-stage self-test report logic defaults to `repo/self_test_report.md`. In real packages, QA/self-test documents may appear as:

- `docs/self-test-report.md`
- arbitrary file names
- extra files provided by human QA operators after the original package was submitted

These supplemental files should not be placed into the original p2r package.

### Target Behavior

p2r supports task-level supplemental documents stored outside the original package and ingested into D/E/F as untrusted evidence.

File names must not be restricted. Operators may attach files named in Chinese, with spaces, timestamps, screenshots, PDFs, or ad hoc names.

### Proposed Storage Model

Human dropbox (approved):

```text
projects-qa/task-docs/<task-id>/
```

This centralized location keeps all QA supplemental files together, making it easier for QA operators to find documents across tasks. The inner original delivery directory remains untouched.

Managed p2r store:

```text
projects-qa/.qa-control/task-docs/<task-id>/
  manifest.json
  files/
    <original-name-or-safe-collision-name>
```

Human dropbox candidates:

```text
projects-qa/task-docs/<task-id>/
projects-qa/<batch>/<task-id>/task-docs/
```

Notes:

- The inner original delivery directory remains untouched.
- The outer task folder is acceptable as a human-facing dropbox because it is outside the submitted package root.
- Managed copies should preserve the original file name when possible.
- On name collision, append a short hash while retaining the original extension.

Manifest fields:

- `manifest_version`
- `task_id`
- `doc_id`
- `original_name`
- `stored_name`
- `source_path`
- `sha256`
- `size_bytes`
- `mime_or_extension`
- `text_included`
- `skip_reason`
- `attached_at`
- `attached_by`
- `notes`
- `included_in_stages`

Storage and safety rules:

- `doc_id` is the first 16 hex characters of the file SHA-256 plus a collision suffix if needed.
- `p2r attach` copies the file atomically into `.qa-control/task-docs/<task-id>/files/`; it never keeps only a mutable external path as the source of truth.
- `source_path` is recorded for audit traceability, but D/E/F use the managed copy.
- Reject attachment paths that resolve inside the managed store itself, point to directories, disappear during copy, or exceed configured size limits.
- Preserve the original file name for display; sanitize only the stored file name and prevent path traversal.
- Default limits: 1 MiB per inline text document, 4 MiB total inline context per stage. Oversized text documents are listed in the manifest with `skip_reason`.
- Binary or unsupported files are listed in the manifest and never embedded into prompts in MVP.

### Proposed CLI/TUI

CLI MVP:

```text
p2r attach <task-id> --file <path>
p2r attach <task-id> --file <path> --note "operator note"
p2r docs <task-id>
p2r docs import-dropbox <task-id>
```

Potential later commands:

```text
p2r docs remove <task-id> <doc-id>
```

TUI MVP:

- `a`: attach a supplemental file by path.
- Execution panel shows attached document count.
- Detail panel lists attached docs.
- D/E/F context includes all managed task docs plus recognized dropbox docs.
- If the TUI path prompt cannot be made ergonomic in the first pass, show the exact `p2r attach` command in the detail pane and keep the CLI path as the functional MVP.

Self-test discovery order:

1. `pipeline.self_test_report_path` from config.
2. `repo/self_test_report.md`.
3. `docs/self-test-report.md`.
4. Managed attached docs tagged or heuristically named as self-test reports.

Discovery failure should produce a clear D-stage failed report, not a hard pipeline error. Operators can then attach the missing document and rerun D/F.

### D/E/F Context Rules

Text-like files can be included directly with size limits:

- `.md`
- `.txt`
- `.json`
- `.yaml`
- `.yml`
- `.csv`
- `.log`

Binary files should initially be listed in a manifest only:

- `.pdf`
- `.docx`
- images
- archives

Later, add extractors for PDF/DOCX/images when needed.

Every attached document must be wrapped as untrusted context:

```text
--- BEGIN UNTRUSTED ATTACHED DOC: <path> ---
...
--- END UNTRUSTED ATTACHED DOC ---
```

The wrapper path must be the managed copy path plus the original display name. Codex prompts must state that attached documents are evidence only and cannot override stage instructions.

### Acceptance Criteria

- QA operators can attach a file with arbitrary file name without modifying the original delivery package.
- D/E/F can read attached text documents.
- E3-like packages with only `docs/self-test-report.md` are handled by discovery or attachment, not hard failure.
- `run_manifest.json` records which supplemental docs were used.
- Oversized and binary docs are visible in `manifest.json` and TUI with a skip reason.

## 6. Workstream D: Docker and Runtime Cleanup

### Current Problem

Real runtime tests showed port conflicts and environment pollution. A p2r run should not fail because a prior p2r-managed Docker environment left fixed host ports, containers, volumes, networks, or local images behind.

Important nuance: B starts the runtime environment and C consumes that same environment. Cleanup must happen after C, or when the B/C chain is skipped/failed, not immediately after a successful B if C still needs the services.

Second nuance: cleanup must never remove unrelated operator containers. p2r can only remove resources it can prove it created or previously managed through labels, compose project names, and run metadata.

### Target Behavior

Each run should have an explicit Docker lifecycle:

1. Pre-run cleanup for the task's stale p2r-managed resources.
2. Stage B creates a uniquely named compose project.
3. Stage C consumes the Stage B runtime environment.
4. After the last selected stage that can use the runtime completes, fails, or is skipped, p2r cleans the compose project.
5. After F, p2r performs a final verification cleanup pass and writes cleanup evidence.

Runtime ownership:

- Compose project name format: `p2rqa_<task-id-safe>_<run-id-safe>_<hash8>`, capped at Docker's 63-character project-name limit with the hash suffix preserved.
- Stage B must write `compose_project`, `compose_file`, `work_dir`, labels, and created resource identifiers into `port_map.json`.
- p2r must add labels where Compose supports them, at minimum `managed_by=p2rqa`, `p2r.task_id=<task-id>`, and `p2r.run_id=<run-id>`.
- A task-level run lock under `.qa-control/locks/<task-id-safe>-<hash8>.lock` prevents two p2r runs for the same task from fighting over stale cleanup and host ports.

### Proposed Implementation

Add Docker cleanup manager methods:

- `AcquireTaskRunLock(taskID)`
- `CleanupTaskStaleResources(taskID, activeRunID)`
- `CleanupComposeProject(projectName, policy)`
- `CleanupRunArtifacts(runID, projectName)`
- `WriteCleanupSummary(artifactRoot)`

Base cleanup command:

```text
docker compose -f <compose-file> -p <project-name> down -v --remove-orphans --rmi local
```

Fallback cleanup:

- Remove containers with compose project label.
- Remove networks with compose project label.
- Remove volumes with compose project label.
- Remove local images built for the compose project when identifiable.
- Refuse to remove any resource without either the expected compose project label or the p2r labels above.

Build cache:

- Do not run global `docker builder prune` silently by default.
- Add config:

```yaml
docker:
  cleanup_policy: "always"
  cleanup_images: true
  cleanup_volumes: true
  cleanup_build_cache: false
  build_cache_prune_until: "24h"
  keep_runtime: false
```

If `cleanup_build_cache: true`, run:

```text
docker builder prune --force --filter until=<duration>
```

This should be explicit because builder cache may be shared across unrelated projects.

Debug override:

```text
p2r run <task-id> --keep-runtime
```

Default should be clean. Keeping runtime should be opt-in and clearly shown in `run_manifest.json`.

`--keep-runtime` behavior:

- Still run pre-run cleanup for stale resources from older runs of the same task.
- Do not tear down the current run's compose project after B/C.
- Mark `cleanup_summary.json` as `kept_by_operator_request`.
- TUI must show that the runtime was intentionally retained and include the exact cleanup command.

Cleanup errors:

- Cleanup failures write `cleanup_summary.json` with `status=failed`, resource IDs, command output, and next manual command.
- Cleanup failure should not erase stage findings. It should add an infrastructure finding or run warning so repeat-run risk is visible.
- A final cleanup verification pass runs `docker ps`/`docker compose ps` for the p2r labels and records the result.

### Acceptance Criteria

- Consecutive runs of the same task do not fail because of previous p2r-managed ports.
- `cleanup_summary.json` records what was removed and what failed to remove.
- Failed B/C still triggers cleanup.
- TUI shows cleanup status.
- A final manual `docker ps` should not show stale p2r-managed containers after a normal run.
- Concurrent runs for different tasks do not remove each other's resources.
- Long task IDs do not collide after Docker project-name truncation.

## 7. Workstream E: TUI as QA Workbench

### Current Problem

The TUI can navigate and rerun, but it does not yet feel like a QA workbench. It hides too much behind truncated columns and does not expose preflight/docs/cleanup information.

### Target Behavior

The TUI should help the QA operator quickly inspect:

- Project list and latest status.
- Highest risk finding.
- Stage health.
- Attached supplemental docs.
- Preflight state.
- Cleanup state.
- Artifact locations.
- Rerun impact.

### Proposed TUI Changes

Overview panel:

- Rename compressed columns:
  - `bad` -> `failed`
  - `blk` -> `blocker`
  - `hi` -> `high`
- Show latest run mode: `initial`, `recheck`, `static-only`, or runtime.
- Show docs count.
- Show cleanup result if available.
- Keep columns responsive: hide lower-priority columns before truncating task ID, status, failed stage, or high-risk counts.
- Sort and filter must keep the selected row stable when background refresh happens.

Execution panel:

- Add selected-stage details below the stage list.
- Wrap long error summaries.
- Display artifact paths in a scrollable detail section.
- Show preflight issues before rerun.
- Show attached docs list.
- Show cleanup summary after run.
- Show "report unavailable" artifacts from static stages as first-class evidence, not as empty logs.
- Bound selected-stage index by the actual stage list length, not by a hard-coded `0..5`, so partial runs and old DB rows do not panic or point at the wrong stage.

Keybindings:

- `r`: rerun selected dependency chain.
- `a`: attach supplemental file.
- `d`: show docs.
- `p`: show preflight.
- `c`: show cleanup summary.
- `enter`: switch to execution detail.
- `tab`: switch panel forward.
- `shift+tab`: switch panel backward.

Rerun confirmation should include:

- selected task
- mode
- affected stages
- whether Docker cleanup will run
- whether supplemental docs will be included
- ref run if recheck mode
- whether `--keep-runtime` is enabled
- exact report files that will be regenerated

### Acceptance Criteria

- A QA operator can understand a failed run without opening raw JSON first.
- TUI can attach and display supplemental docs.
- TUI shows D/E/F blocked or unavailable reasons clearly.
- TUI shows cleanup outcome clearly.
- Shift+Tab moves backward, and selected-stage navigation works for partial historical runs.

## 8. Workstream F: Pipeline Stage Independence

### Current Problem

`executeStage()` in `internal/pipeline/pipeline.go` contains blocking logic:

- Stage B is blocked if Stage A failed.
- Stage C is blocked if Stage B did not complete successfully.

This means if Stage A discovers structural issues, the QA operator receives no Stage B (Docker runtime evidence) or Stage C (test runtime evidence) artifacts. Per Product Principle 2 ("tolerate messy real submissions"), the tool should collect all available evidence and let the human operator decide, not preemptively skip stages.

### Target Behavior

Selected stages A-F run independently with clear status semantics:

- A structural finding never prevents B/C/D/E/F from attempting their own evidence collection.
- B attempts Docker/runtime startup when selected and Docker preflight is safe.
- C attempts its own checks when selected. If B did not produce usable `port_map.json`, C fails with a C-owned "runtime evidence missing" finding and writes `test_runtime_summary.json`; it is not marked skipped solely because B failed.
- Static-only and explicit stage selection can still intentionally skip stages.
- Infrastructure preflight can still mark a stage unavailable, but the stage should materialize a standard unavailable artifact where the external QA contract expects one.

### Proposed Implementation

In `internal/pipeline/pipeline.go`, `executeStage()`:

- Remove the Stage A → B blocking check.
- Remove the Stage B → C blocking check.
- Stages B and C retain their own failure handling (missing Docker, missing `run_tests.sh`, missing port mappings, etc.).
- Keep Docker lifecycle cleanup tied to B/C selection so independent execution does not leave a runtime behind.
- Update TUI affected-stage rerun rules: selecting A should no longer imply B/C rerun unless the operator explicitly chooses the dependency chain. A conservative shortcut can offer `A,F` by default and a separate "runtime chain" option for `B,C,F`.

### Acceptance Criteria

- A Blocker finding in Stage A does not prevent Stage B or C from attempting to run.
- Stage B fails with its own clear reason when Docker prerequisites are missing.
- Stage C fails with its own clear reason when `run_tests.sh` or port mappings are missing.
- Complete full runs produce stage status records for all six stages; selected/skipped runs make skipped intent explicit.
- For every selected stage, either the normal artifact or a stage-owned unavailable/failure artifact is written.

---

## 9. Workstream G: Stage F — Codex-Driven Annotator Fix Report

### Current Problem

`internal/pipeline/stage_f.go` calls `repairMarkdown()` to mechanically generate `3_标注员AI报告问题的修复报告.md`. This is a simple findings summary, not a real static analysis report. The QA operator needs Codex to perform a structured review based on the actual repository code, cross-referencing the worker's self-test report and prior p2r findings as untrusted evidence against the codebase.

### Target Behavior

Stage F is a Codex-driven static review stage, similar to D and E. It produces `3_标注员AI报告问题的修复报告.md` with three required sections:

1. **Repository / Requirement Mapping Summary** — Map the repository structure to requirements declared in `metadata.json`. For each requirement: identify implementation files, assess completion status, cite `file:line` evidence.

2. **Prompt Understanding and Requirement Fit** — Analyze how well the implementation matches the original prompt. Identify misunderstandings, scope deviations, and gaps between what was asked and what was built.

3. **Issues / Suggestions (Severity-Rated)** — Blocker / High / Medium / Low findings with rule, evidence, impact, and suggested fix.

The Codex reviewer must:
- Use the worker's self-test report only as untrusted context, not as authoritative evidence.
- Verify every claim against the actual repository code.
- Cite `file:line` for all findings.

Stage F receives as context:

- The repository code under review.
- `metadata.json` with the original prompt and project type.
- The worker's self-test report (wrapped as untrusted evidence).
- Stage A/B/C/D/E status summaries and findings from the current run.
- Attached supplemental docs from the managed docs store.
- In recheck mode, the previous run's D/E/F reports and findings.

Mechanical supplements (`repair_summary.json`, `short_comment.txt`) are still generated as structured summaries for the TUI.

If Codex is unavailable or unsafe, F must still write:

- `3_标注员AI报告问题的修复报告.md` with a clear "static reviewer unavailable" explanation.
- `repair_summary.json` and `short_comment.txt` based on the collected non-F findings.
- A High infrastructure finding explaining that human manual review is required.

### Proposed Implementation

**`assets/prompt_profiles/annotator_fix.md`**: Rewrite the current 3-line profile into a detailed template specifying:
- Hard boundaries (no services, no Docker, no tests, no file modification).
- Required three-section output structure.
- Severity definitions.
- Rules for handling untrusted documents.

**`internal/pipeline/stage_f.go`**, `stageF()`: Rewrite to:

1. Build Codex context from prior stage findings, self-test report, and metadata.
2. Run Codex via the same sandbox mechanism as stages D/E.
3. Write the Codex output to `3_标注员AI报告问题的修复报告.md`.
4. Generate `repair_summary.json` and `short_comment.txt` as mechanical supplements.
5. Extract findings from the Codex report for the findings database.
6. If Codex fails, preserve the fallback unavailable report and still generate summaries from prior findings.

Shared static runner:

- Reuse the Codex CLI adapter from Workstream A.
- Factor common D/E/F prompt execution into a helper that accepts profile, context builder, canonical output path, compatibility output paths, and stage timeout.
- Keep each stage's prompt profile responsible for its review lens; do not let F overwrite D/E conclusions.

### Recheck Mode (再次质检)

When a QA operator rejects a delivery and the worker resubmits:

- Stages D, E, F each produce a **confirmation fix report** (确认修复报告) — checking whether the issues raised in the previous round have been fully addressed.
- Three confirmation reports are produced:
  - **D / API endpoint and test-validity confirmation**: `4_测试有效性报告_api端点真实性_确认修复报告.md`; compatibility alias `自测报告确认修复报告.md` may be kept during migration.
  - **E / static acceptance audit confirmation**: `1_质检AI测试报告_确认修复报告.md`.
  - **F / annotator issue repair confirmation**: `3_标注员AI报告问题_确认修复报告.md`.
- Each confirmation report follows the same three-section structure (Repository Mapping / Prompt Fit / Issues).
- Each confirmation report receives the matching previous-run stage report plus the previous-run `repair_summary.json`; F also receives previous D/E/F summaries so it can reason about the full returned-issue set.
- `打回问题修复确认报告.md`, if retained, is an aggregate compatibility alias, not the canonical output for D/E/F.
- The third and subsequent rechecks are idempotent — same flow as the second recheck.

### Acceptance Criteria

- `3_标注员AI报告问题的修复报告.md` contains Codex-generated analysis, not a mechanical findings summary.
- All three sections (Repository Mapping, Prompt Fit, Issues) are present.
- Worker's self-test report is passed as untrusted context, not as instructions.
- `repair_summary.json` and `short_comment.txt` are still generated for TUI consumption.
- Recheck mode produces D/E/F confirmation fix reports with stage-correct names and no F/API endpoint naming mix-up.
- Codex-unavailable F still writes a clear unavailable report plus mechanical summaries.

---

## 10. Ralph Iteration Plan

Ralph loop for this improvement round:

1. Review the current contract and implementation surface.
2. Patch one bounded product/engineering slice.
3. Verify with unit tests, smoke artifacts, and a short regression note.
4. Fold the learning into the next slice before broadening scope.

Do not stop for another planning approval unless a step would delete user data, widen sandbox permissions, or change the resolved storage/cleanup defaults below.

### Iteration 0: Planning Gate

Artifacts:

- This plan document.
- The Ralph review correction list near the top of this document.
- Optional follow-up PRD/test-spec only if the implementation phase needs separate acceptance files; it is not a blocker for this document update.

Gate:

- Storage locations, cleanup defaults, and CLI names are resolved in Section 11.
- No unresolved design decision should block Iteration 1.

### Iteration 1: Codex Compatibility & Environment

Implementation:
- Add Node.js PATH detection in Codex sandbox (fix `exec: node: not found`).
- Add Codex capability detection (`internal/codex/cli.go`).
- Replace hard-coded D/E/F args with capability-based arg building.
- Relax preflight around optional approval flag.
- Add fake-help tests for new and old Codex CLI.

Gate:
- `go test ./...`
- Codex can start without `node: not found` errors.
- D/E/F are not blocked solely on missing `--ask-for-approval`.
- D/E/F write unavailable-review artifacts when Codex is unsafe or unavailable.

### Iteration 2: Stage Names and TUI Readability

Implementation:

- DB migration v4 for `run_stages.name`.
- Store/read stage name.
- Fallback stage names for old rows.
- Improve execution panel selected-stage details.

Gate:

- Existing runs display stage names.
- TUI smoke test with three indexed projects.

### Iteration 3: Docker Cleanup and Runtime Lifecycle

Implementation:

- Add cleanup policy config.
- Add hash-suffixed compose project names and p2r labels.
- Add task-level run lock.
- Add pre-run stale cleanup.
- Add post-runtime cleanup after C, or after B when C is not selected.
- Add final cleanup verification summary.
- Add optional build-cache prune config.
- Add `--keep-runtime` and manifest/TUI visibility.

Gate:

- `go test ./...`
- Repeated B/C runs of the same task do not collide on p2r-managed ports.
- Cleanup artifacts are written.
- `--keep-runtime` keeps only the current run's resources and explains manual cleanup.

### Iteration 4: Pipeline Stage Independence

Implementation:

- Remove Stage A → B blocking check in `executeStage()`.
- Remove Stage B → C blocking check in `executeStage()`.
- Make C fail with a C-owned missing-runtime finding when B did not produce usable `port_map.json`.
- Update rerun affected-stage defaults so A does not force B/C unless runtime chain is requested.

Gate:

- `go test ./...`
- A Blocker finding does not prevent B/C from attempting their own evidence collection.
- B/C produce their own clear error reasons when prerequisites are missing.
- Cleanup still runs after independent B/C attempts.

### Iteration 5: Supplemental Docs

Implementation:

- Add human dropbox under `projects-qa/task-docs/<task-id>/`.
- Add managed docs store under `.qa-control/task-docs/<task-id>/`.
- Add attach/list CLI.
- Add dropbox discovery.
- Add manifest versioning, hash doc IDs, size limits, and binary skip reasons.
- Add run manifest integration.
- Include text docs in D/E/F context as untrusted evidence.

Gate:

- Attach arbitrary file name.
- D/E/F context includes attached docs.
- Original package tree remains unchanged.
- Oversized/binary docs are listed but not embedded.

### Iteration 6: Stage F — Codex-Driven Annotator Report

Implementation:

- Rewrite `assets/prompt_profiles/annotator_fix.md` with three-section template.
- Rewrite `stageF()` to run Codex via the same sandbox mechanism as D/E.
- Build Codex context from prior stage findings, self-test report, metadata, attached docs, and previous D/E/F reports in recheck mode.
- Keep `repair_summary.json` and `short_comment.txt` as mechanical supplements.
- Wire up stage-correct recheck confirmation reports for D/E/F.
- Add Codex-unavailable fallback report behavior.

Gate:

- `3_标注员AI报告问题的修复报告.md` is Codex-generated with all three sections when Codex is safe.
- Worker's self-test report is passed as untrusted context.
- `repair_summary.json` and `short_comment.txt` are still generated.
- F/API endpoint naming mismatch is gone.

### Iteration 7: Final TUI Workbench Pass

Implementation:

- Docs/preflight/cleanup panes or detail sections.
- Better column labels and responsive layout.
- Rerun confirmation includes cleanup/docs impact.

Gate:

- QA operator can inspect and rerun from TUI without consulting raw files for first-level triage.

### Final End-to-End Gate

Run against all three real projects:

```text
p2r scan --path ./projects-qa
p2r run TASK-20260327-6A5EE0
p2r run TASK-20260327-D3040D
p2r run TASK-20260327-E3E478
p2r status TASK-20260327-6A5EE0
p2r status TASK-20260327-D3040D
p2r status TASK-20260327-E3E478
p2r tui --path ./projects-qa
```

Verification:

- Full runs produce status records for all six stages; every selected stage writes either normal artifacts or a stage-owned unavailable/failure artifact.
- D/E/F run when Codex is available and safe, with `node` properly resolved.
- Stage F produces Codex-generated three-section analysis in `3_标注员AI报告问题的修复报告.md`.
- Supplemental docs appear in run manifest and D/E/F context.
- Docker cleanup prevents repeat-run port conflicts.
- TUI shows enough context to support human PASS/REWORK/FAIL decisions.
- Recheck mode produces per-stage D/E/F confirmation fix reports.

Recheck smoke:

```text
p2r run TASK-20260327-6A5EE0 --mode recheck --ref-run <previous-run-id>
```

## 11. Resolved Decisions

1. Primary human dropbox:
   → **Resolved**: `projects-qa/task-docs/<task-id>/` (centralized, easier for QA operators).

2. Attached-file storage:
   → **Resolved**: copy into `.qa-control/task-docs/<task-id>/files/` for reproducibility; keep source path only as audit metadata.

3. Docker build cache cleanup:
   → **Resolved**: default-off for global builder cache; default-on for p2r compose resources, volumes, networks, and local compose images.

4. `--keep-runtime`:
   → **Resolved**: yes, opt-in only, visible in `run_manifest.json`, `cleanup_summary.json`, and TUI.

5. Binary document parsing:
   → **Resolved MVP**: list binary files in manifest only; add PDF/DOCX/image extraction later.

6. Codex network boundary:
   → **Resolved MVP**: keep read-only static review plus no-runtime/no-network policy in prompt and config, record the limitation in preflight, and improve later if Codex exposes stricter network controls. Do not claim OS-level network isolation unless implemented.

7. Stage failure blocking:
   → **Resolved**: No. Remove A→B and B→C blocking. All stages run independently.

8. Stage F implementation:
   → **Resolved**: Codex-driven, using a rewritten `annotator_fix.md` profile with three-section output structure.

9. Recheck confirmation reports:
   → **Resolved**: Yes. D, E, F each produce a stage-correct confirmation fix report checking whether previous-round issues were addressed. Third and subsequent rechecks are idempotent.

## 12. Definition of Done

This improvement round is done when:

- All unit tests pass.
- `go vet ./...` passes.
- The three real QA projects can be scanned and run without p2r infrastructure failures.
- Codex can start and execute without `node: not found` errors.
- Codex CLI version drift does not block D/E/F unnecessarily.
- Stage failures do not block downstream stages; full runs produce all A-F stage records, and selected stages produce normal or unavailable/failure artifacts.
- `3_标注员AI报告问题的修复报告.md` is a Codex-generated static analysis with three sections (Repository Mapping / Prompt Fit / Issues).
- Codex-unavailable static stages write explicit unavailable-review artifacts rather than disappearing from the evidence set.
- The annotator_fix.md profile is rewritten with the three-section template.
- Supplemental docs are supported outside the original package under `projects-qa/task-docs/<task-id>/`.
- Supplemental docs are copied into `.qa-control` with manifest versioning, size limits, and binary skip reasons.
- Docker resources are cleaned after runs, with task/run locking and hash-stable compose project names.
- TUI can show stage names, failed reasons, attached docs, and cleanup results clearly.
- Recheck mode produces per-stage D/E/F confirmation fix reports with correct stage responsibilities.
- p2r still never auto-sets PASS/REWORK/FAIL; it only prepares evidence and short comments for human decision.
