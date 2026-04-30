# p2r TUI QA Iteration Regression Note

Date: 2026-04-30

## Implemented Slices

- Codex CLI capability adapter with Node.js PATH diagnostics and optional-flag degradation.
- SQLite v4 `run_stages.name` migration and fallback stage display names.
- Pipeline stage independence for A/B/C evidence collection.
- Docker runtime cleanup lifecycle with task lock, hash-suffixed compose project names, cleanup summary, and `--keep-runtime`.
- Supplemental docs managed store with `p2r attach`, `p2r docs`, and dropbox import.
- D/E/F prompt context now includes managed supplemental docs as untrusted evidence.
- Stage F now runs Codex static review and keeps `repair_summary.json` / `short_comment.txt` mechanical supplements.
- TUI overview/detail improvements for stage names, docs count, preflight path, cleanup status, and selected-stage details.
- TUI redesign slice: explicit focus model, localized key dispatch, responsive overview columns, execution view model, evidence-first detail rendering, dynamic footer, and rerun impact confirmation.
- Mode switching keeps `m` as the non-input alias because common terminals report `Ctrl+M` as Enter; search focus still treats `m` as text input.
- Detail view model reloads on every selected-task refresh, so same-task tick updates can surface new logs, stages, cleanup, docs, and preflight evidence without doing IO in `View()`.
- TUI render sizing now accounts for panel borders and padding before splitting wide/medium/stacked layouts, preventing width overflow when columns or panels shrink.

## Verification

- `go test ./...` passed.
- `go vet ./...` passed.
- `go build ./...` passed.
- Added focused TUI tests for key/focus dispatch, layout breakpoints, Chinese-width truncation, localization, view model partial-run handling, cleanup parse errors, unavailable-review evidence, and refresh selection stability.
- Added TUI regression coverage for localized search, recheck ref-run gating, same-task detail refresh, running ref-run exclusion, and rendered width bounds at 120/100/80/70 columns.
- `go run ./cmd/p2r --help` shows new `attach` and `docs` commands.
- `go run ./cmd/p2r scan --path ./projects-qa` recognized the three expected tasks.
- `go run ./cmd/p2r status <task-id>` succeeded for `TASK-20260327-6A5EE0`, `TASK-20260327-D3040D`, and `TASK-20260327-E3E478`.
- `go run ./cmd/p2r tui --path ./projects-qa` opened the overview with the three indexed tasks and exited cleanly via `Ctrl+C`.

## Real Static Smoke

Command:

```text
go run ./cmd/p2r run TASK-20260327-6A5EE0 --stage D
```

Result:

- New run: `run-20260430-042123-246353`.
- D/F were not blocked by missing `--ask-for-approval`.
- `preflight.json` recorded `codex-cli 0.125.0` as `degraded`, with `has_ask_for_approval=false` and safe `--sandbox read-only` / `--cd` capability available.
- The actual Codex command used `--skip-git-repo-check --ignore-user-config --sandbox read-only --cd ... --ephemeral -`.
- Codex execution failed because the API stream/network disconnected, not because of `node: not found` or missing approval flag.
- D/F materialized report/log artifacts and High infrastructure findings.

## Residual Manual Checks

- Full B/C Docker runtime cleanup should be rechecked in an environment with a successful compose startup.
- Successful Codex-generated D/E/F content should be rechecked when API connectivity is stable.
- Manual visual TUI inspection should still be repeated in a human terminal for medium, narrow, and minimal widths; automated render tests now cover width bounds but not visual aesthetics.
