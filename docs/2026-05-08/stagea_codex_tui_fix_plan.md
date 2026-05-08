# Stage A, Codex, and TUI QA Flow Fix Plan

Status: Frozen for implementation
Date: 2026-05-08

## Background

This iteration fixes three connected QA workflow problems:

1. Stage A may run helper scripts from the outer task directory instead of the canonical p2r package root.
2. Codex QA stages expose too much execution stream and tool-call detail to reviewers, while reviewers primarily need Codex's final response.
3. Long-running Codex reviews can drift for too long without producing a final result, so p2r needs non-interrupting guidance at fixed deadlines.

This document freezes the agreed design. Later implementation should follow this plan unless a new design note explicitly supersedes it.

## Frozen Decisions

- The p2r package root is never inferred from generic repository markers such as `.git`, `package.json`, `pyproject.toml`, or similar files.
- The only canonical p2r package root for indexed projects is:

```text
<scan-root>/<batch>/<task-id>/<task-id>
```

- Stage A receives the canonical package root or its `script_input_snapshot`, not the outer task directory and not `repo/`.
- If the canonical package root is invalid, the run must fail clearly. It must not fall back to the outer task directory.
- Codex report artifacts must prioritize the final Codex response. Raw streams, tool calls, stdout/stderr, and command details are debug evidence.
- Codex deadline handling must use non-interrupting guidance conversation messages. It must not kill, restart, or interrupt Codex work solely to send guidance.

## Canonical Project Root Rules

For a project record:

```text
scanRoot = cfg.ScanPath
batch    = project.Batch
taskID   = project.TaskID
```

The runtime package root is:

```text
packageRoot = projectlayout.ExpectedProjectPath(scanRoot, batch, taskID)
```

That expands to:

```text
<scan-root>/<batch>/<task-id>/<task-id>
```

A valid package root must contain:

```text
metadata.json
docs/
repo/
original_sessions/ or docs/original-session/ or docs/original_sessions/
```

The implementation should keep these checks centralized so scanner, pipeline, Stage A, and tests use one rule set.

Recommended helper:

```go
type PackageRootValidation struct {
	Valid                 bool
	Missing               []string
	OriginalSessionMarker string
}

func ValidatePackageRoot(path string) PackageRootValidation
```

The helper can live in `internal/projectlayout` so it can be reused by `internal/scanner` and `internal/pipeline`.

## Phase 1: Runtime Project Path Canonicalization

Add a run-level canonicalization step immediately after `Runner.Run()` loads the project from the DB.

Current flow:

```go
project, err := r.store.GetProject(ctx, taskID)
```

Target flow:

```go
project, err := r.store.GetProject(ctx, taskID)
project, warnings, err := r.canonicalizeProjectForRun(project)
```

Behavior:

- Recompute the expected package root from `cfg.ScanPath`, `project.Batch`, and `project.TaskID`.
- Validate the expected package root with the shared package-root validator.
- If the expected package root is valid, use it as `project.Path` for the entire run.
- If the DB path differs from the expected package root, continue with the expected path and record a stale-path warning.
- If the expected package root is invalid, fail the run before starting stages.
- Do not fall back to the outer task directory.

Failure message should be explicit:

```text
indexed project path is invalid or stale:
expected package root <scan-root>/<batch>/<task-id>/<task-id>
but it does not contain metadata.json, docs/, repo/, and an original session marker.
Please rerun p2r scan --path <scan-root>; if old artifact rows remain, run p2r scan --path <scan-root> --prune-artifacts.
```

The warning should be written to the run manifest and visible in logs/TUI:

```text
DB project path was stale; runtime used canonical package root.
db_path=<old path>
canonical_path=<expected path>
```

## Phase 2: Stage A Input Root Correction

Stage A should only consume the already-canonicalized `project.Path`.

Definitions:

```text
packageRoot = <scan-root>/<batch>/<task-id>/<task-id>
repoRoot    = packageRoot/repo
```

Stage A helper scripts expect `packageRoot`, because they inspect:

```text
metadata.json
docs/
repo/
original session marker
```

Execution rules:

- `copyPackageSnapshot(project.Path, script_input_snapshot)` uses the canonical package root as source.
- If snapshot creation succeeds, Python helper scripts receive `script_input_snapshot` as input root.
- If snapshot creation fails, helper scripts may receive canonical `project.Path`, but this should produce a finding as it does today.
- The snapshot root must directly contain `metadata.json`, `docs/`, and `repo/`.
- Stage A structural checks must use the shared original-session marker list.

Stage A should not repair or guess paths. If it receives a non-canonical path, that is a pipeline-level bug.

## Phase 3: Codex Final Response First

Reviewer-facing Codex artifacts must be based on Codex's final response.

Result layers:

```text
primary   = final Codex response / final report
secondary = parsed findings and report artifacts
debug     = raw stream, tool calls, stdout/stderr, command line, capability diagnostics
```

Implementation rules:

- Continue using `--output-last-message` when supported.
- Fall back to stdout only when last-message capture is unavailable.
- D/E/F report artifacts should contain only the final Codex response plus a trailing newline.
- `staticReviewFindingsFromReport` must parse findings only from the final response artifact content.
- Raw Codex streams and tool-call traces remain in `logs/*_static.log`.
- TUI and later QA context should default to the final report, not the raw execution stream.

This preserves debuggability while aligning reviewer attention with the actual Codex conclusion.

## Phase 4: Non-Interrupting Codex Deadline Guidance

Codex reviews can run for a long time. p2r should guide them toward completion without interrupting their work.

Deadline schedule:

```text
T+20m: soft acceleration reminder
T+30m: deadline reminder, require completion within 10 minutes
T+40m: final-summary guidance, require immediate final response
```

Guidance messages:

At 20 minutes:

```text
You have been running for 20 minutes without a final result. Please accelerate, focus on the highest-risk review points, and prioritize confirmed findings and the final conclusion.
```

At 30 minutes:

```text
You have been running for 30 minutes without a final result. Please complete the review and return the final response within the next 10 minutes. Avoid expanding the review scope.
```

At 40 minutes:

```text
You have been running for 40 minutes. Stop starting new exploration, summarize the conclusions already confirmed, and return the final review response now. p2r will persist your final response to the required artifact files.
```

Guidance rules:

- Use Codex's guidance conversation ability to append messages to the running review.
- Do not kill, restart, or interrupt Codex merely to send guidance.
- Send each guidance message at most once.
- Trigger based on absence of a final result, not absence of stream output.
- Record each guidance event in the stage log.
- Surface each guidance event in TUI stage status/details.

The current code mostly uses one-shot `codex exec` with stdin. Supporting true guidance requires a runner/session abstraction around Codex execution.

Recommended abstraction:

```go
type CodexReviewSession interface {
	Start(ctx context.Context, request CodexReviewRequest) error
	SendGuidance(ctx context.Context, message string) error
	Wait(ctx context.Context) (CodexReviewResult, error)
}
```

The first implementation can wrap the existing exec path where possible, but the deadline guidance feature should be designed around session semantics rather than scattered timers inside stage functions.

Fallback behavior:

- If the 40-minute guidance is sent and Codex still does not produce a final response after a short grace window, p2r may generate an incomplete-review artifact.
- That artifact must clearly state that Codex did not produce a final response in time.
- It must not pretend to be a completed QA conclusion.

## Phase 5: TUI Reviewer-Focused Display

TUI should show reviewer-relevant output by default.

Display priorities:

- For Codex stages, show the final report artifact path and summary first.
- Keep raw stream/tool-call logs behind debug/log views.
- Show path canonicalization warnings when a stale DB path was corrected.
- Show deadline guidance events in stage details:

```text
20m guidance sent
30m deadline guidance sent
40m final-summary guidance sent
```

TUI should not treat raw tool calls as the primary Codex result.

## Manifest and Artifacts

`run_manifest.json` should include:

```json
{
  "batch": "batch-1",
  "project_path": "<canonical package root>",
  "artifact_root": "<run artifact root>",
  "path_warnings": [
    {
      "type": "stale_project_path",
      "db_path": "<old path>",
      "canonical_path": "<canonical package root>"
    }
  ]
}
```

If there are no warnings, `path_warnings` can be omitted or written as an empty array. Prefer whichever pattern already matches local manifest style.

Codex stage logs should include:

```text
=== codex guidance events ===
20m guidance sent at ...
30m deadline guidance sent at ...
40m final-summary guidance sent at ...
```

## Test Plan

Project root canonicalization:

- DB path is outer `<batch>/<task-id>`, canonical inner path exists, run uses inner path.
- DB path is already canonical, run proceeds without warning.
- Canonical inner path does not exist, run fails before stages.
- Canonical inner path is missing `repo/`, run fails before stages.
- Canonical inner path is missing `metadata.json`, run fails before stages.
- Canonical inner path is missing all accepted original session markers, run fails before stages.
- No fallback occurs to `.git`, `package.json`, `pyproject.toml`, or other generic repo markers.

Stage A:

- Stage A snapshot root contains `metadata.json`, `docs/`, and `repo/` directly.
- `scriptExecution.InputRoot` points to `script_input_snapshot` when snapshot succeeds.
- Stage A Python helper scripts run with the snapshot root as cwd/input root.
- Alternative original session markers are accepted consistently.

Codex final response:

- When `--output-last-message` is available, report artifacts come from that file even if stdout is empty.
- When last-message capture is unavailable, stdout is used.
- Raw logs can contain stream/tool-call details, but report artifact contains final response content.
- Findings are parsed from final response content only.

Codex guidance:

- At 20 minutes without final result, exactly one soft guidance message is sent.
- At 30 minutes without final result, exactly one deadline guidance message is sent.
- At 40 minutes without final result, exactly one final-summary guidance message is sent.
- No guidance is sent after a final result exists.
- Stream output alone does not suppress guidance.
- Guidance events are written to logs and exposed through progress/TUI state.

TUI:

- Codex stage details prioritize final report path/content summary.
- Debug logs remain accessible.
- Path canonicalization warning is visible when DB path was stale.
- Guidance events appear in the selected run/stage details.

## Implementation Order

1. Add shared package-root validation to `internal/projectlayout`.
2. Update scanner to reuse that validation helper without changing the frozen scan semantics.
3. Add run-level project canonicalization in `Runner.Run()`.
4. Persist path warnings into manifest/log/progress events.
5. Verify Stage A consumes the canonical path and snapshot root.
6. Refactor Codex result handling naming and TUI consumption so final response is primary.
7. Introduce Codex review session abstraction for deadline guidance.
8. Add 20/30/40-minute non-interrupting guidance events.
9. Update TUI display around final reports, warnings, and guidance events.
10. Run targeted tests first, then full `go test ./...`.

## Out of Scope

- Changing the indexed project primary key from `task_id` to `(batch, task_id)`.
- Supporting non-`batch-*` top-level directories.
- Migrating historical run artifact physical directories.
- Using generic code repository heuristics to locate p2r package roots.
- Treating Codex tool calls as reviewer-facing final conclusions.
- Forcibly terminating Codex at 20, 30, or 40 minutes solely due to these guidance deadlines.

## Acceptance Criteria

- Stage A no longer runs from the outer task directory.
- A stale DB project path can be corrected at runtime when the canonical inner path is valid.
- Invalid canonical package roots fail clearly and never fall back to outer directories.
- Codex report artifacts reflect the final Codex response.
- Tool calls and raw streams remain available only as debug evidence.
- Long-running Codex reviews receive non-interrupting guidance at 20, 30, and 40 minutes.
- TUI defaults to the information a reviewer needs: final report, findings, warnings, and deadline guidance status.
