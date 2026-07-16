# Standard Authoring Deployment Contract

This directory is the source-controlled deployment input for the closed
`harbor.standard-authoring@1.1.0` workflow. It creates a first task from an
immutable `AuthoringSource` / `AuthoringSession`; it does not pretend that the
source session is already a `TaskRevision`.

## Frozen Git source input

This deployment does not pre-approve one repository. Each Standard launch
supplies one source subject that is frozen before an `AuthoringSource` or
`AuthoringSession` exists:

- a credential-free HTTPS Git URI, SSH Git URI, or SSH scp-style address;
- one full lowercase 40- or 64-character Git commit ID; and
- no branch, tag, ref expression, local path, query/fragment selector, or
  inline credential.

The locked Git executable fetches only that commit, verifies that it resolves
to the requested commit object, and writes a bounded read-only archive. The
canonical repository URL, full commit, archive content digest, and source
fingerprint are then durable session facts. A caller-selected source can never
select a catalog, profile, prompt, model, secret, workspace, or executable.

HTTPS acquisition is non-interactive and credential-free. SSH acquisition is
also non-interactive, but is permitted only when its exact host is present in
the package-owned `ssh/known_hosts` allow-list bound into the deployment lock.
The capture adapter rejects an unlisted host before Git invokes SSH, uses a
locked OpenSSH client and locked wrapper shell, disables password and keyboard
authentication, disables system/user SSH configuration, and enforces
`StrictHostKeyChecking=yes`. It never reads `~/.ssh`, the ambient
`SSH_AUTH_SOCK`, an SSH config, or inline credentials.

For a private SSH repository, the process may supply one live agent socket
only through `HARBOR_FACTORY_STANDARD_AUTHORING_SSH_AUTH_SOCK`. The value must
be an absolute non-symlink Unix-domain socket; its name is locked but its value
is not serialized. Without it, no ambient agent or key file is available, so
an SSH server that requires authentication will fail closed. Host-key additions
or rotations require a reviewed `ssh/known_hosts` change, a new Standard lock,
and a new package; `accept-new`, wildcard, hashed, and ambient host-key entries
are not supported.

`materialize_task` is the sole Go-controlled operation allowed to atomically
create the first Task and TaskRevision. It emits
`authoring_task_handoff` (`harbor.authoring-task-handoff.v1`) and terminates
the source-bound Run. A separate task-bound
`harbor.codeedge-phase1@2.2.0` Run owns subsequent verification, evaluator
handoff, compliance, and local packaging.

## Version boundary

The required frozen environment policy is an execution-contract change, so it
belongs to `harbor.standard-authoring@1.1.0`, not to a reinterpretation of a
historical `@1.0.0` Run. The consolidated V2 store intentionally does not
migrate its physical schema in place; install this release with a new managed
control-plane root. Existing `@1.0.0` records remain immutable audit history
and must be handled by the release that owns their frozen deployment contract.

## Frozen task environment policy

Each launch must also provide `--base-image` (or the matching TUI field) as
one fully qualified OCI image reference pinned with a lowercase SHA-256
digest, for example:

```text
docker.io/library/rust:1.65@sha256:<64 lowercase hex characters>
```

The service serializes it as the canonical `environment_policy` session input,
binds its content digest into every consuming stage, and preserves it in the
Run specification. `dockerfile_generate` receives those exact bytes and may
use the value unchanged in every `FROM`; tag-only references, variables,
additional images, substitutions, external `COPY`/`ADD --from` sources,
external BuildKit `RUN --mount=from` sources, and parser directives are
rejected. `COPY`/`ADD --from` may use an earlier local multi-stage alias or
numeric index; `RUN --mount=from` may use only an earlier local alias because
BuildKit does not treat a numeric mount source as a stage index.
`materialize_task` repeats the same validation before a Dockerfile can become
part of the sealed task.
This is a task constraint, not an ambient host-image selection: a new session
may choose another validated digest without changing a historical Run.

## Source-controlled inputs

- `operation-catalog.v1.json` is the exact 14-stage allow-list. It contains
  only generic Git snapshot preparation, locked Codex App Server turns,
  durable review gates, and the `materialize_task` Harbor built-in.
- `contract-assets.v1.json` maps every closed stage to a canonical prompt and
  schema path. It has exact stage coverage and no path-discovery convention.
- `prompts/` and `schemas/` contain immutable handler contracts. Codex prompt
  programs are canonical, self-fingerprinted JSON. The
  `schemas/codex-stage-output.schema.json` asset is an actual Draft-07 JSON
  Schema template, not a field-list description. Its exact locked bytes and
  fingerprint are verified before the runtime derives the stricter per-stage
  `turn/start.outputSchema` from the frozen `StageDescriptor`.
- `schemas/codex-turn-output.json` is retained only to inspect historical
  locks and Runs that froze the former final-text contract. No current catalog
  entry may reference it, and it must never be used to reinterpret an old Run
  under the dynamic-submit semantics.
- `ssh/known_hosts` is the explicit OpenSSH host-key allow-list. Its fixed
  relative path and raw content SHA-256, together with the pinned OpenSSH
  client and wrapper shell, live in `standard_authoring_ssh_transport` in the
  generated lock.

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
  --ssh-executable /usr/bin/ssh \
  --ssh-wrapper-shell /usr/bin/dash \
  --codex-node /absolute/path/to/node \
  --codex-launcher /absolute/path/to/codex.js \
  --codex-home /absolute/path/to/codex-home \
  --codex-model-version <approved-gpt-5.6-terra-version>
```

The generator refuses a dirty worktree, a pre-existing output lock, symlinked
paths, unpinned assets, malformed prompts/schemas, a missing Codex App Server
capability, or a catalog/asset mismatch. It computes the Harbor Flow build
identity from `HEAD` and a source-tree SHA-256 manifest excluding the
self-referential generated lock. It hashes every prompt/schema asset, the
package `ssh/known_hosts` asset, and the actual locked Git, OpenSSH, wrapper
shell, Node, and Codex launcher files before writing the canonical lock
atomically.

The explicit model-version argument is intentional: no script should infer a
mutable model alias. The generator reads no model endpoint, credential, token,
or provider environment value.

Each `agent.turn` in the source-controlled catalog also pins
`gpt-5.6-terra` and `xhigh`. The generator copies that exact pair into the
lock, and the runtime passes it to the App Server instead of inheriting a
local Codex model or reasoning default.

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

## Codex output submission

Each `agent.turn` opens an ephemeral Codex App Server conversation with one
stage-private `harbor_submit_stage_output` dynamic tool. It is registered at
`thread/start`; it is neither a global MCP tool nor a shell command. The host
derives the tool schema, allowed verdicts, artifact names, schema versions,
and paths from the frozen `StageDescriptor`. A model can submit only an
allowed verdict and one base64 content value for each declared artifact in
the declared order.

The derived schema is also passed as `turn/start.outputSchema`, but it is only
the first format barrier. The submit handler performs strict semantic
validation and internally canonicalizes a passing candidate. A successful
submission is the sole output authority for the stage; assistant free text,
including text emitted after the tool call, cannot replace it.

Each submission attempt is independently charged to the `output_submission`
quota dimension. The current Standard-authoring policy reserves three such
units per agent stage, separately from its `agent_turn` units. Candidate size
and the frozen per-turn timeout still apply. Rejected candidates never enter
durable state as raw content: Codex receives only stable diagnostics such as
`invalid_json`, `wrong_verdict`, or `artifact_identity_mismatch`, together
with a digest.

The conversation-level ReAct loop is intentionally ephemeral. Checkpoints do
not contain a resumable Codex session or raw transcript, so a worker crash
does not automatically continue the conversation on another worker. Recovery
currently starts a new fenced stage attempt; historical Runs remain frozen.

See [the non-secret host observation](../../docs/STANDARD_AUTHORING_ENVIRONMENT_ATTESTATION_OBSERVATION.md)
and [the Codex bridge contract](../../docs/STANDARD_AUTHORING_CODEX_AGENT_BOOTSTRAP.md)
for implementation details.
