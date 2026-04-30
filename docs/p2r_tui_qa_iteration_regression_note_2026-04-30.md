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

## Verification

- `go test ./...` passed.
- `go vet ./...` passed.
- `go build ./...` passed.
- `go run ./cmd/p2r --help` shows new `attach` and `docs` commands.
- `go run ./cmd/p2r scan --path ./projects-qa` recognized the three expected tasks.

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
