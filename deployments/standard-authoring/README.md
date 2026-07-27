# Standard Authoring 3.0 Deployment Contract

This directory is the sole source-controlled contract for
`harbor.standard-authoring@3.0.0`. It creates one sealed task revision from a
frozen source snapshot and then terminates. It never creates, authorizes, or
recovers a child workflow Run.

## Closed workflow

`repo_prepare` freezes the source snapshot. Three research stages run from
that snapshot, `task_synthesis` creates the reviewable task specification, and
`task_review` freezes the verification contract. `authoring_loop` is the first
candidate writer. Host verification runs layout, baseline, oracle, coverage,
and integrity checks. The two critic stages may emit structured findings, and
`authoring_repair` may create a new candidate at most twice. Every repair uses
a new fenced conversation and must produce a new snapshot, validation receipt,
and host-generated `workflow_repair_ledger` before final review can continue.

The final stages require a passing candidate snapshot and receipt, both review
decisions, final attestation, and CodeEdge package-admission receipt.
`materialize_task` atomically seals the first revision and emits a
`materialization_receipt` as audit evidence. The receipt contains no child
template or Run selection.

## Source and candidate boundaries

Every stage consumes the frozen `authoring_contract` artifact. Its content and
digest are the only authority for task direction, repository identity, source
commit, source root, and environment facts. Upstream artifacts, review
feedback, and agent output are data claims, not instructions.

Researcher and critic roles use read-only workspaces. The author role is the
only exclusive writer and submits a candidate by writing the six fixed files
in its fenced workspace. The host reads these files into a `CandidateSnapshot
v1`; agents cannot submit arbitrary file paths, Base64 payloads, transcripts,
credentials, or environment values. Only a host-issued passing
`ValidationReceipt v1` for the exact snapshot can reach critics or final
materialization.

## Deployment assets

- `operation-catalog.v1.json` is the closed 16-stage V3 operation allow-list.
- `execution-profile.v1.json` pins the full V3 stage budgets.
- `contract-assets.v1.json` maps every stage to its prompt and schema asset.
- `prompts/` and `schemas/` are immutable, fingerprinted contracts.
- `ssh/known_hosts` is the explicit host-key allow-list for source capture.

No prior Standard Authoring template, catalog, profile, prompt, schema,
recovery endpoint, compatibility adapter, or automatic CodeEdge handoff is
executable. Completed historical records remain immutable audit data only.

## Production lock

`operation-catalog.lock.json` is generated production output. Generate it
only from a clean committed tree, with the reviewed V3 source snapshot and
attestations:

```bash
scripts/generate-standard-authoring-lock.sh \
  --build-version v3.0.0 \
  --lock-version v3.0.0-<snapshot-commit> \
  --git-executable /usr/bin/git \
  --ssh-executable /usr/bin/ssh \
  --ssh-wrapper-shell /usr/bin/dash
```

The generator refuses a dirty tree and will not overwrite an existing lock.
