# p2r TUI QA Workflow Improvement Plan

Date: 2026-04-30
Status: Draft — reviewed and updated 2026-04-30
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

- Codex CLI flag compatibility blocks all D/E static review stages.
- Codex cannot start at all due to `node: not found` (Node.js not on PATH).
- Stage A failure blocks B and C from running, preventing complete evidence collection.
- Stage F (`3_标注员AI报告问题的修复报告.md`) is mechanically generated instead of being a real Codex static analysis.
- TUI execution view loses important stage names and truncates key evidence.
- Supplemental QA files are assumed to live inside the original delivery package.
- Docker/runtime cleanup is insufficient, causing port conflicts and polluted subsequent runs.
- The current TUI is functional but not yet clear enough for fast human triage.

This plan merges the engineering review findings with the product requirements discussed after the end-to-end test.

## 2. Product Principles

1. **The original p2r package is evidence, not workspace.**
   p2r should not require QA operators to modify or add files into the submitted delivery package.

2. **The tool must tolerate messy real submissions.**
   File names, supplemental evidence names, and directory habits may vary. p2r should ingest and normalize, not force ideal names.

3. **Runtime evidence must be reproducible and isolated.**
   Every run should start from a clean p2r-managed Docker state and should not leave ports, containers, networks, volumes, or local images that interfere with the next run.

4. **Static review must degrade gracefully.**
   D/E should fail only when the static reviewer is truly unavailable or unsafe, not because one optional Codex CLI flag changed names.

5. **TUI must answer the QA operator's first three questions quickly.**
   What failed? Why did it fail? Where is the evidence?

## 3. Workstream A: Codex CLI Compatibility & Runtime Environment

### Current Problem

There are two blocking issues preventing Codex from running D/E stages:

**Issue 1 — Node.js not on PATH**: The Codex CLI is a Node.js script that `exec`s `node`. On the current machine, Node.js is installed (v25.7.0) at the same directory as the `codex` script, but `node` is not on the WSL PATH. This causes `exec: node: not found` — Codex cannot start at all.

**Issue 2 — Flag compatibility**: Current preflight requires `codex exec --help` to contain all of:

- `--ask-for-approval`
- `--sandbox`
- `--cd`
- `--ephemeral`

On the current machine, `codex-cli 0.125.0` supports `--sandbox`, `-C/--cd`, and `--ephemeral`, but does not expose `--ask-for-approval`. This causes D/E to be marked blocked even when Codex would otherwise be usable.

Both issues must be fixed — the environment issue is the hard blocker, and the flag compatibility issue prevents graceful degradation.

### Target Behavior

p2r should build the Codex command from detected capabilities:

- Require `codex exec`.
- Require a read-only sandbox mode or a safe equivalent.
- Prefer `--sandbox read-only` when available.
- Prefer `--cd <project>` or `-C <project>` when available.
- Use `--ephemeral` when available.
- Use `--ask-for-approval never` only when available.
- Do not block D/E solely because `--ask-for-approval` is absent.
- Always use a per-stage temporary `CODEX_HOME` under the run artifact root.
- Always pass the review prompt through stdin.
- Always treat self-test reports, ref-run reports, and attached documents as untrusted context.

### Proposed Implementation

**Node.js PATH fix** (`internal/codex/sandbox.go` or `internal/pipeline/pipeline.go` stageCodex):

- When resolving the `codex` binary path via `exec.LookPath`, detect if `node` exists in the same directory.
- If found, prepend that directory to `PATH` in the sandbox environment.
- This ensures Codex's `exec: node` call resolves without requiring system-wide PATH changes.

**Codex CLI adapter** (`internal/codex/cli.go`):

- `DetectCLI(ctx, exec, path) Capability`
- `BuildExecArgs(capability, projectPath, extraArgs) ([]string, error)`
- `Capability` fields:
  - `Version`
  - `HasSandbox`
  - `HasAskForApproval`
  - `HasCDLong`
  - `HasCDShort`
  - `HasEphemeral`
  - `HasSkipGitRepoCheck`
  - `HasIgnoreUserConfig`

Change preflight:

- `codex` check should be `ok` if a safe static-review command can be built.
- Missing optional flags should be recorded as `degraded`, not `missing`.
- Missing read-only sandbox support should remain blocking.

Change Stage D/E runner:

- Use adapter output rather than hard-coded args.
- Keep existing unsafe `codex.extra_args` validation.
- Keep `--sandbox read-only`.
- Keep per-stage `.codex-home-D` and `.codex-home-E` cleanup.

### Acceptance Criteria

- Codex can start and execute without `exec: node: not found` errors.
- D/E are not blocked on current `codex-cli 0.125.0` solely because `--ask-for-approval` is missing.
- Existing older Codex CLI help output remains supported in unit tests.
- If sandbox support is absent, D/E are blocked with a clear unsafe-environment message.
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
- Persist `StageRecord.Name` in `PutStage`.
- Read `name` in `Stages`.
- Fallback to stage display name when old rows have empty names.

Code organization:

- Export or centralize stage display names, for example:
  - `pipeline.StageDisplayName(stage string) string`

TUI:

- Replace blank stage name with fallback display name.
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

p2r supports task-level supplemental documents stored outside the original package and ingested into D/E as untrusted evidence.

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

- `task_id`
- `doc_id`
- `original_name`
- `stored_name`
- `source_path`
- `sha256`
- `size_bytes`
- `mime_or_extension`
- `attached_at`
- `attached_by`
- `notes`
- `included_in_stages`

### Proposed CLI/TUI

CLI MVP:

```text
p2r attach <task-id> --file <path>
p2r attach <task-id> --file <path> --note "operator note"
p2r docs <task-id>
```

Potential later commands:

```text
p2r docs remove <task-id> <doc-id>
p2r docs import-dropbox <task-id>
```

TUI MVP:

- `a`: attach a supplemental file by path.
- Execution panel shows attached document count.
- Detail panel lists attached docs.
- D/E context includes all managed task docs plus recognized dropbox docs.

### D/E Context Rules

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

### Acceptance Criteria

- QA operators can attach a file with arbitrary file name without modifying the original delivery package.
- D/E can read attached text documents.
- E3-like packages with only `docs/self-test-report.md` are handled by discovery or attachment, not hard failure.
- `run_manifest.json` records which supplemental docs were used.

## 6. Workstream D: Docker and Runtime Cleanup

### Current Problem

Real runtime tests showed port conflicts and environment pollution. A p2r run should not fail because a prior p2r-managed Docker environment left fixed host ports, containers, volumes, networks, or local images behind.

Important nuance: B starts the runtime environment and C consumes that same environment. Cleanup must happen after C, or when the B/C chain is skipped/failed, not immediately after a successful B if C still needs the services.

### Target Behavior

Each run should have an explicit Docker lifecycle:

1. Pre-run cleanup for the task's stale p2r-managed resources.
2. Stage B creates a uniquely named compose project.
3. Stage C consumes the Stage B runtime environment.
4. After C completes, fails, or is skipped, p2r cleans the compose project.
5. After F, p2r performs a final cleanup pass and writes cleanup evidence.

### Proposed Implementation

Add Docker cleanup manager methods:

- `CleanupTaskStaleResources(taskID)`
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

### Acceptance Criteria

- Consecutive runs of the same task do not fail because of previous p2r-managed ports.
- `cleanup_summary.json` records what was removed and what failed to remove.
- Failed B/C still triggers cleanup.
- TUI shows cleanup status.
- A final manual `docker ps` should not show stale p2r-managed containers after a normal run.

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

Execution panel:

- Add selected-stage details below the stage list.
- Wrap long error summaries.
- Display artifact paths in a scrollable detail section.
- Show preflight issues before rerun.
- Show attached docs list.
- Show cleanup summary after run.

Keybindings:

- `r`: rerun selected dependency chain.
- `a`: attach supplemental file.
- `d`: show docs.
- `p`: show preflight.
- `c`: show cleanup summary.
- `enter`: switch to execution detail.
- `tab`: switch panel.

Rerun confirmation should include:

- selected task
- mode
- affected stages
- whether Docker cleanup will run
- whether supplemental docs will be included
- ref run if recheck mode

### Acceptance Criteria

- A QA operator can understand a failed run without opening raw JSON first.
- TUI can attach and display supplemental docs.
- TUI shows D/E blocked reasons clearly.
- TUI shows cleanup outcome clearly.

## 8. Workstream F: Pipeline Stage Independence

### Current Problem

`executeStage()` in `internal/pipeline/pipeline.go` contains blocking logic:

- Line 186-188: Stage B is blocked if Stage A failed.
- Line 191-193: Stage C is blocked if Stage B did not complete successfully.

This means if Stage A discovers structural issues, the QA operator receives no Stage B (Docker runtime evidence) or Stage C (test runtime evidence) artifacts. Per Product Principle 2 ("tolerate messy real submissions"), the tool should collect all available evidence and let the human operator decide, not preemptively skip stages.

### Target Behavior

All stages A-F run independently. Each stage may fail on its own merits (e.g., B fails if Docker is unavailable, C fails if `run_tests.sh` is missing), but no stage is artificially blocked because a prior stage failed.

### Proposed Implementation

In `internal/pipeline/pipeline.go`, `executeStage()`:

- Remove the Stage A → B blocking check (lines 186-188).
- Remove the Stage B → C blocking check (lines 191-193).
- Stages B and C retain their own failure handling (missing Docker, missing `run_tests.sh`, missing port mappings, etc.).

### Acceptance Criteria

- A Blocker finding in Stage A does not prevent Stage B or C from attempting to run.
- Stage B fails with its own clear reason when Docker prerequisites are missing.
- Stage C fails with its own clear reason when `run_tests.sh` or port mappings are missing.
- All six stage artifacts are produced in every complete run.

---

## 9. Workstream G: Stage F — Codex-Driven Annotator Fix Report

### Current Problem

`stageF()` (lines 1228-1259) calls `repairMarkdown()` to mechanically generate `3_标注员AI报告问题的修复报告.md`. This is a simple findings summary, not a real static analysis report. The QA operator needs Codex to perform a structured review based on the actual repository code, cross-referencing the worker's self-test report (as untrusted evidence) against the codebase.

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
- Stage D and Stage E findings from the current run.

Mechanical supplements (`repair_summary.json`, `short_comment.txt`) are still generated as structured summaries for the TUI.

### Proposed Implementation

**`assets/prompt_profiles/annotator_fix.md`**: Rewrite the current 3-line profile into a detailed template specifying:
- Hard boundaries (no services, no Docker, no tests, no file modification).
- Required three-section output structure.
- Severity definitions.
- Rules for handling untrusted documents.

**`internal/pipeline/pipeline.go`**, `stageF()`: Rewrite to:
1. Build Codex context from prior stage findings, self-test report, and metadata.
2. Run Codex via the same sandbox mechanism as stages D/E.
3. Write the Codex output to `3_标注员AI报告问题的修复报告.md`.
4. Generate `repair_summary.json` and `short_comment.txt` as mechanical supplements.
5. Extract findings from the Codex report for the findings database.

### Recheck Mode (再次质检)

When a QA operator rejects a delivery and the worker resubmits:
- Stages D, E, F each produce a **confirmation fix report** (确认修复报告) — checking whether the issues raised in the previous round have been fully addressed.
- Three confirmation reports are produced:
  - **自测报告确认修复报告** (D) — did the worker fix issues identified in the self-test report review?
  - **Codex 静态审计确认修复报告** (E) — did the worker fix the static acceptance audit findings?
  - **API 端点覆盖率确认修复报告** (F) — did the worker fix the endpoint coverage gaps?
- Each confirmation report follows the same three-section structure (Repository Mapping / Prompt Fit / Issues).
- The third and subsequent rechecks are idempotent — same flow as the second recheck.

### Acceptance Criteria

- `3_标注员AI报告问题的修复报告.md` contains Codex-generated analysis, not a mechanical findings summary.
- All three sections (Repository Mapping, Prompt Fit, Issues) are present.
- Worker's self-test report is passed as untrusted context, not as instructions.
- `repair_summary.json` and `short_comment.txt` are still generated for TUI consumption.
- Recheck mode produces D/E/F confirmation fix reports.

---

## 10. Ralph Iteration Plan

### Iteration 0: Planning Gate

Artifacts:

- This plan document.
- Optional follow-up PRD/test-spec if we want a stricter Ralph loop.

Gate:

- User reviews and confirms storage locations, cleanup defaults, and CLI names.

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

### Iteration 2: Stage Names and TUI Readability

Implementation:

- DB migration v4 for `run_stages.name`.
- Store/read stage name.
- Fallback stage names for old rows.
- Improve execution panel selected-stage details.

Gate:

- Existing runs display stage names.
- TUI smoke test with three indexed projects.

### Iteration 3: Pipeline Stage Independence

Implementation:
- Remove Stage A → B blocking check in `executeStage()`.
- Remove Stage B → C blocking check in `executeStage()`.

Gate:
- `go test ./...`
- A Blocker finding does not prevent B/C from running.
- B/C produce their own clear error reasons when prerequisites are missing.

### Iteration 4: Stage F — Codex-Driven Annotator Report

Implementation:
- Rewrite `assets/prompt_profiles/annotator_fix.md` with three-section template.
- Rewrite `stageF()` to run Codex via the same sandbox mechanism as D/E.
- Build Codex context from prior stage findings, self-test report, and metadata.
- Keep `repair_summary.json` and `short_comment.txt` as mechanical supplements.
- Wire up recheck mode logic for D/E/F confirmation fix reports.

Gate:
- `3_标注员AI报告问题的修复报告.md` is Codex-generated with all three sections.
- Worker's self-test report is passed as untrusted context.
- `repair_summary.json` and `short_comment.txt` are still generated.

### Iteration 5: Supplemental Docs

Implementation:

- Add task docs store under `projects-qa/task-docs/<task-id>/`.
- Add attach/list CLI.
- Add dropbox discovery.
- Add run manifest integration.
- Include text docs in D/E/F context as untrusted evidence.

Gate:

- Attach arbitrary file name.
- D/E/F context includes attached docs.
- Original package tree remains unchanged.

### Iteration 6: Docker Cleanup

Implementation:

- Add cleanup policy config.
- Pre-run stale cleanup.
- Post-C cleanup.
- Final cleanup summary.
- Optional build-cache prune config.

Gate:

- Repeated B/C runs of the same task do not collide on p2r-managed ports.
- Cleanup artifacts are written.

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

- A-F runs produce complete artifacts for all six stages regardless of individual failures.
- D/E/F run when Codex is available and safe, with `node` properly resolved.
- Stage F produces Codex-generated three-section analysis in `3_标注员AI报告问题的修复报告.md`.
- Supplemental docs appear in run manifest and D/E/F context.
- Docker cleanup prevents repeat-run port conflicts.
- TUI shows enough context to support human PASS/REWORK/FAIL decisions.
- Recheck mode produces per-stage D/E/F confirmation fix reports.

## 11. Open Decisions

1. ~~Should the primary human dropbox be `projects-qa/task-docs/<task-id>/` or `projects-qa/<batch>/<task-id>/task-docs/`?~~
   → **Resolved**: `projects-qa/task-docs/<task-id>/` (centralized, easier for QA operators).

2. Should p2r copy attached files into `.qa-control`, or only reference them by path?
   - Recommended: copy into `.qa-control` for reproducibility.

3. Should Docker build cache cleanup be default-on?
   - Recommended: default-off for global builder cache, default-on for p2r compose resources and local compose images.

4. Should `--keep-runtime` exist for debugging?
   - Recommended: yes, opt-in only.

5. Which binary document formats should be parsed in MVP?
   - Recommended MVP: list binary files in manifest only.
   - Later: PDF/DOCX extraction.

6. Should D/E require network-disabled Codex execution at the OS level, or is read-only sandbox plus no-runtime prompt acceptable for MVP?
   - Recommended: keep current read-only/no-runtime policy, document limitations, and improve later if Codex exposes stricter network controls.

7. Should stage failures block downstream stages?
   → **Resolved**: No. Remove A→B and B→C blocking. All stages run independently.

8. Should Stage F be Codex-driven (like D/E) or mechanically generated?
   → **Resolved**: Codex-driven, using a rewritten `annotator_fix.md` profile with three-section output structure.

9. Should recheck mode produce per-stage confirmation fix reports?
   → **Resolved**: Yes. D, E, F each produce a confirmation fix report checking whether previous-round issues were addressed. Third and subsequent rechecks are idempotent.

## 12. Definition of Done

This improvement round is done when:

- All unit tests pass.
- `go vet ./...` passes.
- The three real QA projects can be scanned and run without p2r infrastructure failures.
- Codex can start and execute without `node: not found` errors.
- Codex CLI version drift does not block D/E/F unnecessarily.
- Stage failures do not block downstream stages; all A-F artifacts are produced in every run.
- `3_标注员AI报告问题的修复报告.md` is a Codex-generated static analysis with three sections (Repository Mapping / Prompt Fit / Issues).
- The annotator_fix.md profile is rewritten with the three-section template.
- Supplemental docs are supported outside the original package under `projects-qa/task-docs/<task-id>/`.
- Docker resources are cleaned after runs.
- TUI can show stage names, failed reasons, attached docs, and cleanup results clearly.
- Recheck mode produces per-stage D/E/F confirmation fix reports.
- p2r still never auto-sets PASS/REWORK/FAIL; it only prepares evidence and short comments for human decision.
