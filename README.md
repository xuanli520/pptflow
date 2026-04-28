# p2r

`p2r` is a local prompt2repo QA orchestration CLI/TUI. It indexes delivery packages, runs the A-F evidence pipeline, stores status in SQLite, and writes review artifacts without replacing the human PASS/REWORK/FAIL decision.

## Commands

```text
p2r scan --path ./projects-qa
p2r run TASK-20260408-A1B2C3
p2r run TASK-20260408-A1B2C3 --static-only
p2r run TASK-20260408-A1B2C3 --stage D
p2r run TASK-20260408-A1B2C3 --from C
p2r status TASK-20260408-A1B2C3
p2r tui --path ./projects-qa
```

## Checks

```text
go test ./...
go vet ./...
go build ./...
```

The SQLite store uses a pure-Go driver so the default build does not require CGO.
