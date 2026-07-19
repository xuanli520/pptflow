# CodeEdge evaluator-child production deployment

`operation-catalog.v1.json` and `operation-catalog.lock.json` are the only
production authority for the closed `harbor.codeedge-evaluator@1.0.0` child
workflow. They do not authorize the CodeEdge Phase-1 parent. The candidate
inventory remains an observation record only; it is not loaded as a fallback.

The catalog permits exactly two serial `local.command` operations:

- `codeedge-qwen-pass4`: `claude-code@2.1.207`, `qwen3.7-max`, four attempts,
  concurrency one, and at most three Harbor-internal technical retries per
  logical Trial.
- `codeedge-opus-pass4`: `claude-code@2.1.207`, `claude-opus-4-6`, four
  attempts, concurrency one, and at most three Harbor-internal technical
  retries per logical Trial.

The lock freezes the Harbor `0.18.0` launcher, Python interpreter and source
tree, Docker CLI `29.5.2`, Docker server `29.4.1`, the exact `docker-compose`
and `docker-buildx` plugins and their complete version outputs, CodeEdge
renderer/schema fingerprints, model identities, canonical endpoint
fingerprints, and a complete child-owned execution profile:
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

Before each evaluator effect, and before reading model credentials, it re-proves
the Harbor launcher, its shebang-bound Python interpreter, and the complete
locked Python source manifest. It then uses the exact `HOME`, `DOCKER_CONFIG`,
and `PATH` that will be passed to Harbor to prove bare `docker` resolves to the
locked CLI, verify the independently locked server version, resolve the exact
Compose and Buildx plugin files, and match both plugin version outputs
byte-for-byte. After every subprocess probe it re-hashes every executable and
plugin and re-computes the Python source manifest. A missing daemon, substituted
runtime file, or version/output drift
fails before Harbor can allocate a Trial. The complete proof runs once before
credential lookup and again after the temporary env-file is materialized; both
passes must derive the same process environment before the runner is called.

The evaluator is strictly local-only: it writes its four-trial job under the
managed `--jobs-dir`, never passes Harbor upload or sharing flags, and rebuilds
evidence only from the completed local job's `result.json`, Trial results,
`config.json`, `lock.json` and Harbor Flow provenance.

## Generate The Lock

Generate the evaluator lock only from a clean committed source tree. The
generator accepts no caller arguments and uses these explicitly supplied
environment names for its controlled inputs:

```bash
export HARBOR_FACTORY_GIT_EXECUTABLE=/absolute/path/to/git
export HARBOR_FACTORY_HARBOR_LAUNCHER=/absolute/path/to/harbor
export HARBOR_FACTORY_PYTHON_INTERPRETER=/absolute/path/to/python
export HARBOR_FACTORY_HARBOR_PYTHON_SOURCE_TREE=/absolute/path/to/site-packages/harbor
export HARBOR_FACTORY_DOCKER_EXECUTABLE=/absolute/path/to/docker
export HARBOR_FACTORY_DOCKER_COMPOSE_PLUGIN=/absolute/path/to/docker-compose
export HARBOR_FACTORY_DOCKER_BUILDX_PLUGIN=/absolute/path/to/docker-buildx
export HARBOR_FACTORY_BUILD_VERSION=v2.0.0
export HARBOR_FACTORY_CODEEDGE_EVALUATOR_LOCK_VERSION=2026.07.19.2
scripts/generate-codeedge-evaluator-lock.sh
```

`QWEN_HARBOR_BASE_URL`, `OPUS_HARBOR_BASE_URL`, and
`ANTHROPIC_AUTH_TOKEN` must already be present in the invoking environment.
They are compared against the catalog or checked for presence only; the script,
generator, lock, and normal output do not print or persist their values.

## Local Production Package

Build only from a clean, committed source tree:

```bash
scripts/build-codeedge-production.sh
```

The unified production build produces `dist/harbor-flow-production/harbor-factory`
with colocated `deployments/standard-authoring/`,
`deployments/codeedge-phase1/`, and
`deployments/codeedge-evaluator-child/` directories, a local tarball, and
`SHA256SUMS`. The evaluator's source-only `candidates/` discovery record is
never copied into the package.
`SHA256SUMS` covers every colocated payload plus the tarball (and intentionally
does not checksum itself); validate it with `sha256sum -c SHA256SUMS` from the
output directory. The archive is reproducible: it uses the source commit time,
sorted entries, normalized ownership, and a timestamp-free gzip header. The
output directory must not already exist or be a symlink; it is assembled in a
private sibling directory and atomically published only after all checksums are
written. It computes a SHA-256 source manifest over the canonical Git tree
listing excluding all three self-referential generated locks and verifies the
same manifest against `harbor_flow_build` in every lock.
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
