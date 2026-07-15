# CodeEdge Phase-1 Production Deployment

`operation-catalog.v1.json` and `operation-catalog.lock.json` are the only
production authority for the closed `harbor.codeedge-evaluator@1.0.0` child
workflow. The candidate inventory remains an observation record only; it is
not loaded as a fallback.

The catalog permits exactly two serial `local.command` operations:

- `codeedge-qwen-pass4`: `claude-code@2.1.207`, `qwen3.7-max`, four attempts,
  concurrency one, and at most three Harbor-internal technical retries per
  logical Trial.
- `codeedge-opus-pass4`: `claude-code@2.1.207`, `claude-opus-4-6`, four
  attempts, concurrency one, and at most three Harbor-internal technical
  retries per logical Trial.

The lock freezes the Harbor `0.18.0` launcher, Python interpreter and source
tree, Docker CLI, CodeEdge renderer/schema fingerprints, model identities,
canonical endpoint fingerprints, and a complete child-owned execution profile:
both 110-minute single-turn evaluator budgets, a 120-minute single attempt,
24-hour continuation TTL, one-minute control grace, and bounded generic
candidate-provider timing. It deliberately contains no endpoint, credential,
or other secret value.

At execution time Harbor Factory reads only these approved host environment
names:

- `QWEN_HARBOR_BASE_URL` -> private child `ANTHROPIC_BASE_URL` for Qwen.
- `OPUS_HARBOR_BASE_URL` -> private child `ANTHROPIC_BASE_URL` for Opus.
- `ANTHROPIC_AUTH_TOKEN` -> private child `ANTHROPIC_AUTH_TOKEN` for both.

The controlled evaluator verifies an endpoint's canonical fingerprint before
launch, writes a per-stage `0600` `--env-file` containing only the frozen model
endpoint/token mappings, and removes it after the direct local Harbor
invocation. Hub credentials are neither read nor written. Values never enter
argv, catalog/lock JSON, run manifests, artifacts, screenshots, or command
logs.

The evaluator is strictly local-only: it writes its four-trial job under the
managed `--jobs-dir`, never passes Harbor upload or sharing flags, and rebuilds
evidence only from the completed local job's `result.json`, Trial results,
`config.json`, `lock.json` and Harbor Flow provenance.

## Local Production Package

Build only from a clean, committed source tree:

```bash
scripts/build-codeedge-production.sh
```

The script produces `dist/codeedge-production/harbor-factory`, a colocated
`deployments/codeedge-phase1/` directory, a local tarball, and `SHA256SUMS`.
`SHA256SUMS` covers every colocated payload plus the tarball (and intentionally
does not checksum itself); validate it with `sha256sum -c SHA256SUMS` from the
output directory. The archive is reproducible: it uses the source commit time,
sorted entries, normalized ownership, and a timestamp-free gzip header. The
output directory must not already exist or be a symlink; it is assembled in a
private sibling directory and atomically published only after all checksums are
written. It computes a SHA-256 source manifest over the canonical Git tree
listing excluding this self-referential lock and verifies it against
`harbor_flow_build` in the lock.
`harbor_flow_build.commit` is the reviewed source-baseline provenance recorded
by that lock; it does not need to equal the final commit that carries the lock,
because doing so would create a hash cycle. The content manifest is the
binding source proof. The build injects the locked module, version, reviewed
baseline commit, manifest fingerprint, canonical catalog-receipt fingerprint,
and full lock identity/fingerprint with Go linker flags. A production binary
loads deployment files only from its colocated managed directory and fails
closed if any bound value does not equal those files.

Any change to the source revision, lock, executable, model, endpoint, agent,
or result contract requires a reviewed lock revision and a newly built local
package. Do not edit candidate observations to enable execution.
