# Standard Authoring Deployment Contract

This directory is the source-controlled deployment input for the closed
`harbor.standard-authoring@1.0.0` workflow. It creates a first task from an
immutable `AuthoringSource` / `AuthoringSession`; it does not pretend that the
source session is already a `TaskRevision`.

The source subject currently approved for authoring is:

- repository: `https://github.com/tower-rs/tower-http.git`
- commit: `f066e10ebc07ea9050a2ce4576315abfa568edf4`

`materialize_task` is the sole Go-controlled operation allowed to atomically
create the first Task and TaskRevision. It emits
`authoring_task_handoff` (`harbor.authoring-task-handoff.v1`) and terminates
the source-bound Run. A separate task-bound
`harbor.codeedge-phase1@2.2.0` Run owns subsequent verification, evaluator
handoff, compliance, and local packaging.

## Source-controlled inputs

- `operation-catalog.v1.json` is the exact 14-stage allow-list. It contains
  only Git snapshot preparation, locked Codex App Server turns, durable review
  gates, and the `materialize_task` Harbor built-in.
- `contract-assets.v1.json` maps every closed stage to a canonical prompt and
  schema path. It has exact stage coverage and no path-discovery convention.
- `prompts/` and `schemas/` contain immutable handler contracts. Codex prompt
  programs are canonical, self-fingerprinted JSON; their output schema is the
  exact `harbor.standard-authoring-codex-turn-output.v1` envelope.

There is deliberately no checked-in `operation-catalog.lock.json`. A lock
must bind the exact final source snapshot and local executable bytes; placing a
placeholder or an old build identity in source control would be an unsafe
claim of production authority.

## Generate the final lock

After a final local snapshot commit, run the generator from a clean worktree:

```bash
scripts/generate-standard-authoring-lock.sh \
  --build-version v2.0.0 \
  --lock-version v2.0.0-<snapshot-commit> \
  --git-executable /usr/bin/git \
  --codex-node /absolute/path/to/node \
  --codex-launcher /absolute/path/to/codex.js \
  --codex-home /absolute/path/to/codex-home \
  --codex-model-version <approved-gpt-5.5-version>
```

The generator refuses a dirty worktree, a pre-existing output lock, symlinked
paths, unpinned assets, malformed prompts/schemas, a missing Codex App Server
capability, or a catalog/asset mismatch. It computes the Harbor Flow build
identity from `HEAD` and a source-tree SHA-256 manifest excluding the
self-referential generated lock. It hashes every prompt/schema asset and the
actual locked Git, Node, and Codex launcher files before writing the canonical
lock atomically.

The explicit model-version argument is intentional: no script should infer a
mutable model alias. The generator reads no model endpoint, credential, token,
or provider environment value.

## Runtime boundary

`NewStandardAuthoringProviderComposition` accepts one exact catalog/lock and
one `StandardAuthoringRuntimeAttestor`. For an `agent.turn` catalog entry it
automatically constructs the attested Codex bridge when no explicit test
handler is supplied; production composition must provide a managed,
non-symlink `CodexWorkspaceRoot`. Every effect then revalidates the frozen
operation, hashes the lock-bound prompt/schema files, parses the canonical
program/schema, and reattests the Codex runtime immediately before opening an
App Server conversation.

The only intended host executable in this authoring catalog is the pinned Git
snapshot executable. Docker build/evaluation and Qwen/Opus pass@4 are not
silently reused here: they belong to the later task-bound CodeEdge workflow
and its separate evaluator deployment contract.

See [the non-secret host observation](../../docs/STANDARD_AUTHORING_ENVIRONMENT_ATTESTATION_OBSERVATION.md)
and [the Codex bridge contract](../../docs/STANDARD_AUTHORING_CODEX_AGENT_BOOTSTRAP.md)
for implementation details.
