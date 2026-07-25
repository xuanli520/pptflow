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

The source checkout is captured before execution and remains immutable. Codex
may inspect only `source/`. It may not use raw sessions, credentials, archives,
memory, unbounded logs, network access, or ambient host configuration. Claims
about source paths, package manifests, command working directories, and
slash-containing command arguments are checked against the verified frozen
source tree.

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
