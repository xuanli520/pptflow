# Standard Authoring v2 Deployment Contract

This directory is the sole source-controlled deployment contract for
`harbor.standard-authoring@2.0.0`. It creates one sealed first task revision
from a frozen repository snapshot, then emits a handoff to the separate
task-bound CodeEdge workflow.

## Root contract and source boundary

Every v2 stage receives the root `authoring_contract` artifact. Its
`context.contract.content` is the only immutable authority for task direction,
repository identity, source commit, source root, and base image. The matching
`context.contract.digest` must accompany every Codex submission. Upstream
artifacts, review feedback, and model output are untrusted data claims; they
cannot override the root contract.

`repo_prepare` also receives this required root-contract binding. It accepts
exactly the frozen binding for the active AuthoringSession (port name, artifact
ID, digest, and schema must match) and rejects missing, duplicate, or other
stage inputs. The Git source itself is still read only from the immutable
AuthoringSource snapshot, never from a caller-provided input.

The source checkout is captured before execution and remains immutable. Codex
may inspect only `source/`. It may not use raw sessions, credentials, archives,
memory, unbounded logs, network access, or ambient host configuration. Claims
about source paths, package manifests, command working directories, and
slash-containing command arguments are checked against the verified frozen
source tree.

### Source archive compatibility

Source capture performs only basic tar validation: the archive must be
readable, remain within the `source/` root, avoid duplicate paths, contain at
least one regular file, and remain within the fixed archive-size limit. It does
not maintain a metadata or entry-type whitelist. Git-produced extended
metadata, symbolic links, and hard links do not block task creation.

During workspace preparation, relative links are projected only when their
lexical targets remain inside `source/`; this preserves normal repository link
semantics without allowing a workspace path to escape its frozen source root.
Tar entry kinds without a safe filesystem projection are retained in the
captured archive but omitted from the workspace rather than rejecting the
creation request.

Source capture has a fixed ten-minute transport budget. Git and its transport
helpers run in one process group, so a timeout terminates the complete capture
attempt and returns a retryable source-capture failure instead of leaving the
TUI waiting on an orphaned remote helper.

`task_design` emits `harbor.standard-authoring-task-proposal.v2` and
`generate_task_files` emits
`harbor.standard-authoring-generated-task-plan.v2`. Both repeat exact
contract claims and use stable unique requirement IDs. `solve_generate` and
`test_generate` are the only fixed-file stages: the host reads their designated
workspace file after a `verdict=pass` receipt bound to the root-contract
digest.

## Deployment assets

- `operation-catalog.v1.json` is the closed 17-stage v2 allow-list.
- `execution-profile.v1.json` is the matching complete v2 execution budget.
- `contract-assets.v1.json` maps every stage to explicit prompt and schema
  assets.
- `prompts/` and `schemas/` are immutable, fingerprinted handler contracts.
- `ssh/known_hosts` is the explicit host-key allow-list for controlled source
  capture.

There is no compatibility, upgrade, recovery, or materialization path for a
previous Standard Authoring deployment. Historical records remain owned by the
release that froze them and are never executed through this v2 package.

## Production lock

`operation-catalog.lock.json` is generated production output, not a hand-made
or copied asset. Generate it only from a clean, committed v2 tree so its build
identity, catalog receipt, prompt/schema fingerprints, and local executable
attestations all refer to the same source snapshot:

```bash
scripts/generate-standard-authoring-lock.sh \
  --build-version v2.0.0 \
  --lock-version v2.0.0-<snapshot-commit> \
  --git-executable /usr/bin/git \
  --ssh-executable /usr/bin/ssh \
  --ssh-wrapper-shell /usr/bin/dash
```

The generator refuses a dirty worktree and will not overwrite an existing
lock. Packaging requires that generated lock after it has been independently
reviewed.
