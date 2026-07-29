# Repository Guidelines

## Project Structure & Module Organization

This repository is a Go module (`github.com/purplevoid/harbor-factory`). `main.go` wires the CLI, `cmd/` contains Cobra command handlers, `internal/` holds private application, runtime, Codex, storage, TUI, and Harbor implementations, and `pkg/workflowkit/` exposes reusable workflow APIs. Deployment bundles, schemas, prompts, and operation catalogs live in `deployments/`; design notes live in `docs/`. Build and lock-generation helpers are under `scripts/`, with supporting tools in `tools/`.

## Build, Test, and Development Commands

Run commands from the repository root.

- `go run . --help` shows the Harbor Factory CLI and available subcommands.
- `go build ./...` compiles all packages and tools.
- `go test ./...` runs the full Go test suite.
- `go test ./cmd ./internal/app ./pkg/workflowkit` runs focused tests for common workflow changes.
- `gofmt -w <files>` formats edited Go files before review.
- `scripts/build-codeedge-production.sh` and `scripts/generate-*-lock.sh` rebuild production packages or lock artifacts when deployment inputs change.

## Coding Style & Naming Conventions

Use standard Go formatting and idioms: tabs via `gofmt`, short package names, mixedCaps identifiers, and table-driven tests where they reduce duplication. Keep public APIs in `pkg/workflowkit/` narrow and documented. Keep implementation-only code in `internal/`; avoid new cross-boundary imports unless an existing package already establishes that direction. JSON, YAML, schema, and prompt filenames should be lowercase and descriptive, using hyphens for multiword deployment assets.

## Testing Guidelines

Tests are colocated with code as `*_test.go` files and use Go’s standard `testing` package. Name tests by behavior, for example `TestRunWorkerRejectsExpiredLease`. Prefer focused unit tests for package logic and integration tests for workflow lifecycle, deployment catalogs, storage, or CLI behavior. When modifying locks, prompts, or schemas, run the relevant `tools/*-lock-build` tests plus `go test ./...`.

## Commit & Pull Request Guidelines

History uses conventional commit-style messages such as `fix(authoring): validate contract tokens before capture` and `feat(authoring): retain agent turns`. Keep commits imperative, scoped, and behavior-focused: `fix(tui): reset stale start intent`. Pull requests should describe the user-visible change, list verification commands, link related issues or design docs, and include screenshots for TUI changes. Call out deployment, schema, or prompt artifact regeneration explicitly.

## Security & Configuration Tips

Do not commit local runtime state such as `.harbor-factory/`, temporary workspaces, credentials, or generated logs. Treat deployment profiles, known hosts, prompts, and schemas as reviewed contract files; changes should be intentional and covered by tests or lock regeneration.
