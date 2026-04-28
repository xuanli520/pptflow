# p2r

`p2r` is a local prompt2repo QA orchestration CLI/TUI. It indexes delivery packages, runs the A-F evidence pipeline, stores status in SQLite, and writes review artifacts without replacing the human PASS/REWORK/FAIL decision.

## Install

For local development, install the CLI command from the dedicated command package:

```text
go install ./cmd/p2r
```

Make sure Go's binary directory is on `PATH`. On Windows this is usually
`%USERPROFILE%\go\bin`; on Linux and macOS it is usually `$HOME/go/bin`.

To build a standalone binary instead:

```text
go build -o p2r ./cmd/p2r
```

On Windows, use `go build -o p2r.exe ./cmd/p2r`, then put `p2r.exe` in a
directory on `Path`, such as `%USERPROFILE%\bin`.

## Configuration

Config is loaded from one discovered file, then environment variables and CLI
flags are applied on top:

```text
defaults < user config or current directory .p2r.yaml or P2R_CONFIG < P2R_SCAN_PATH/P2R_DB_PATH < CLI flags
```

`P2R_CONFIG` selects an explicit config file and skips automatic config file
discovery. Automatic discovery prefers the current directory `.p2r.yaml` before
the user config file.

Supported user config locations are:

```text
Windows: %AppData%\p2r\config.yaml
Linux: $XDG_CONFIG_HOME/p2r/config.yaml or ~/.config/p2r/config.yaml
macOS: ~/Library/Application Support/p2r/config.yaml
Fallback: ~/.p2r.yaml
```

Useful environment variables:

```text
P2R_CONFIG=F:\projects\p2r_tui\.p2r.yaml
P2R_SCAN_PATH=F:\projects\p2r_tui\projects-qa
P2R_DB_PATH=F:\projects\p2r_tui\projects-qa\.qa-control\index.db
```

Relative paths inside config files are resolved from the config file directory.
Relative paths from environment variables and CLI flags are resolved from the
current working directory.

## Commands

```text
p2r scan --path ./projects-qa
p2r run TASK-20260408-A1B2C3
p2r run TASK-20260408-A1B2C3 --static-only
p2r run TASK-20260408-A1B2C3 --stage D
p2r run TASK-20260408-A1B2C3 --from C
p2r status TASK-20260408-A1B2C3
p2r tui --path ./projects-qa
p2r version
```

## Ubuntu/Linux release

The recommended Linux distribution is a static `linux/amd64` binary archive. It
does not require CGO or system SQLite libraries; runtime checks still require the
tools used by the pipeline, especially Docker Compose and Python or uv.

Build a local Ubuntu-compatible release:

```text
scripts/build-linux-release.sh
```

From Windows PowerShell, cross-compile the same release:

```text
.\scripts\build-linux-release.ps1
```

The archive is written to `dist/p2r_<version>_linux_amd64.tar.gz` with a matching
SHA256 file. See `docs/linux-release.md` for cloud VM installation steps.

## Checks

```text
go test ./...
go vet ./...
go build ./...
```

The SQLite store uses a pure-Go driver so the default build does not require CGO.
