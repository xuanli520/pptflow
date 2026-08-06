# Harbor CLI 0.18.0 Observed Execution Contract

## Purpose And Scope

This document records the locally observed behavior of the Harbor CLI installed
on 2026-07-14, with its Docker runtime identity re-probed on 2026-07-19. It is a
version-pinned Harbor Flow adapter reference, not a CodeEdge submission rule and
not an upstream compatibility promise.

It exists to prevent two unsafe assumptions:

1. Treating the CodeEdge training document as though it defined Harbor's
   machine-readable result format.
2. Treating a behavior observed from Harbor 0.18.0 as though it applied to a
   newer Harbor release without re-probing it.

> 2026-08-06 状态更新：本文服务的 CodeEdge evaluator 集成（Trial/pass@k、
> `result.json` 解析、evaluator 本地运行）已随 CodeEdge Phase-1 / qwen-opus
> evaluator 链路从 Harbor Factory 删除。本文仅保留为 2026-07 的 Harbor 0.18
> 观察留档，不再作为当前任何适配器的权威依据。

Only the facts in the `Observed Facts` sections are adapter authority. A change
to the Harbor version, launcher fingerprint, installed Python source tree,
Docker client/server version, or Compose/Buildx plugin identity invalidates this
observation and requires a new versioned probe.

## Observed Installation Identity

| Fact | Observed value |
| --- | --- |
| CLI command | `harbor` |
| Resolved launcher | `/root/.local/share/uv/tools/harbor/bin/harbor` |
| CLI version | `0.18.0` |
| Upstream wheel | `harbor-0.18.0-py3-none-any.whl` |
| Upstream wheel SHA-256 | `e436f04fca35bb3705be603b8c123d0472418d10120cfd7e5ba8dc902e56bc32` |
| PyPI release status | latest non-yanked release as checked on 2026-07-14 |
| Launcher SHA-256 | `9b0852df4c749ab9431b7aff6b2f1b1de8b7365ee6a513cdbd7573a1678d4f97` |
| Python interpreter SHA-256 | `4703a3d15898c0b5d81c3f939e93bdd8ca6116342093fb160ab1e01860dd7d8b` |
| Harbor Python package root | `/root/.local/share/uv/tools/harbor/lib/python3.13/site-packages/harbor` |
| Python source-tree SHA-256 | `f9b6817d8f749563ac4a68ea0453d9b9363a730d61b6b4d1bcd81b6d57474cbe` |
| Docker client used by the probe | `29.5.2` (`Docker version 29.5.2, build 79eb04c`) |
| Docker server used by the probe | `29.4.1`, queried independently with `docker version --format '{{.Server.Version}}'` |
| Docker Compose plugin | `/usr/libexec/docker/cli-plugins/docker-compose`; exact output `Docker Compose version v5.1.3`; SHA-256 `a0298760c9772d2c06888fc8703a487c94c3c3b0134adeef830742a2fc7647b4` |
| Docker Buildx plugin | `/usr/libexec/docker/cli-plugins/docker-buildx`; exact output `github.com/docker/buildx v0.33.0 f7897eba028583e0071642db3c011e860444f8cf`; SHA-256 `5f42ff0a165e3834c4fd73a91b8d41c37a3c0a3475d0101cc13cfcf880ce5978` |

The source-tree digest is the SHA-256 of the sorted `sha256sum` output for all
`*.py` files below the package root:

```bash
find "$HARBOR_PACKAGE_ROOT" -type f -name '*.py' -print0 \
  | sort -z \
  | xargs -0 sha256sum \
  | sha256sum
```

The small `harbor` launcher alone is not enough to identify behavior because it
imports the installed Python package.

## Production Installation Lock

The production installation is pinned to the exact non-yanked PyPI wheel named
above. On 2026-07-14 Harbor Flow downloaded that wheel, verified its SHA-256,
then installed it with `uv tool install --reinstall <verified-wheel>`. The
post-install command reported `0.18.0`; its launcher, interpreter, and Python
source-tree identities are the values in the preceding table.

The evaluator's Docker runtime is locked separately from the Harbor wheel. The
client and daemon versions are independent facts. Immediately before an
evaluator effect, Harbor Flow re-proves the launcher, interpreter, Python source
manifest, Docker, Compose, and Buildx identities. Docker discovery runs under
the exact isolated `HOME`, `DOCKER_CONFIG`, and `PATH` later passed to Harbor;
it checks the daemon reports `29.4.1`, resolves both locked plugin paths, and
matches both complete plugin version outputs. All runtime files are re-hashed
again after the subprocess probes. The proof runs before credentials are read
and repeats after the private env-file is written; both passes must derive the
same environment before Harbor starts.

This is intentionally a fail-closed local installation lock rather than a
floating `harbor@latest` request. A future upstream release must be separately
downloaded, hash-verified, installed in a controlled environment, re-probed,
and recorded in a new observed-contract revision before it is admitted for
production evidence capture.

## Observed Facts: Command Surface

`harbor run` is an alias for `harbor job start`.

For one task, one agent configuration, and one model, the production CodeEdge
profile uses the following strictly serial invocation for exactly four logical
trials:

```bash
harbor run \
  --path <TASK_DIRECTORY> \
  --agent <AGENT> \
  --model <MODEL> \
  --n-attempts 4 \
  --n-concurrent 1 \
  --max-retries 3 \
  --job-name <STABLE_JOB_NAME> \
  --jobs-dir <OUTPUT_DIRECTORY> \
  --quiet \
  --yes
```

The isolated Oracle probe used the same four logical attempts with
`--n-concurrent 4` to exercise Harbor's parallel scheduling behavior. That
probe establishes output shape, not the production CodeEdge concurrency rule.
For an Oracle-only local probe, `--model <MODEL>` is omitted:

```bash
harbor run \
  --path <TASK_DIRECTORY> \
  --agent oracle \
  --n-attempts 4 \
  --n-concurrent 4 \
  --job-name <STABLE_JOB_NAME> \
  --jobs-dir <OUTPUT_DIRECTORY> \
  --quiet \
  --yes
```

### Critical Flag Semantics

| Flag | Harbor 0.18.0 observed meaning | Do not interpret it as |
| --- | --- | --- |
| `-k`, `--n-attempts` | Number of logical attempts for each task/agent configuration | Concurrency or a display-only pass@k selector |
| `-n`, `--n-concurrent` | Maximum number of concurrently running trials | Number of logical attempts |
| `--max-retries` | Maximum Harbor-internal retry count for an attempt | A license to create an additional logical sample |
| `-o`, `--jobs-dir` | Parent directory for the local job directory | A remote result destination |
| `--job-name` | Local job-directory name | A guaranteed global/remote job identity |

The Harbor 0.18.0 implementation constructs:

```text
n_total_trials = n_attempts x resolved tasks x configured agent entries
```

Therefore, a four-attempt CodeEdge evaluation must ensure all of the following
before it is accepted as four logical samples:

- `n_attempts == 4`;
- exactly one materialized task;
- exactly one evaluator agent/model configuration; and
- `job result.n_total_trials == 4`.

`--n-concurrent` controls parallelism but is not itself evidence of four
attempts. The locked CodeEdge profile uses `--n-concurrent 1` so its four
logical samples are serial and Qwen finishes before Opus is scheduled.

The approved production profile sets `--max-retries 3`. Harbor retries under
the same logical trial name and replaces its final result; this limit does not
change `n_attempts`, create a fifth sample, or authorize a generic workflow
stage retry.

`stats.n_retries` is a job-wide count of Harbor-internal technical retries.
The controlled adapter records it as an authenticated aggregate on the
immutable evaluation receipt, but does not infer a per-TrialAttempt mapping:
the observed 0.18.0 final layout contains exactly one final `result.json` for
each of the four logical trials and does not establish which logical trial, if
any, consumed an aggregate retry.

## Observed Facts: Local Result Layout

With `--jobs-dir <OUTPUT_DIRECTORY>` and `--job-name <JOB_NAME>`, a completed
job was written under:

```text
<OUTPUT_DIRECTORY>/<JOB_NAME>/
  config.json
  lock.json
  result.json
  job.log
  <task-id>__<generated-suffix>/
    config.json
    lock.json
    result.json
    trial.log
    agent/
      trajectory.json  # Claude Code when Harbor successfully extracts it
    verifier/
    artifacts/manifest.json
```

The generated trial-directory suffix is not stable and must never be used as a
Harbor Flow identity. The trial `id` inside its `result.json` is the external
identity; Harbor Flow records its own UUIDv7 identities separately.

### Job-Level `result.json`

Observed completed job fields include:

```json
{
  "id": "<job UUID>",
  "started_at": "<timestamp>",
  "updated_at": "<timestamp>",
  "finished_at": "<timestamp>",
  "n_total_trials": 4,
  "stats": {
    "n_completed_trials": 4,
    "n_errored_trials": 0,
    "n_running_trials": 0,
    "n_pending_trials": 0,
    "n_cancelled_trials": 0,
    "n_retries": 0,
    "evals": {
      "<agent>__<model>__<source>": {
        "n_trials": 4,
        "n_errors": 0,
        "metrics": [{"mean": 1.0}],
        "pass_at_k": {"2": 1.0, "4": 1.0},
        "reward_stats": {},
        "exception_stats": {}
      }
    }
  }
}
```

For an agent with no model, the observed evaluation key was
`oracle__adhoc`. JSON keys in `pass_at_k` are strings because JSON object keys
are strings, even though Harbor constructs them from integer `k` values.

**Important:** Harbor writes the final job-level `result.json` with
`trial_results` excluded. Do not require or parse a job-level `trial_results`
array in the 0.18.0 adapter. The per-Trial documents below are the detail
authority.

### Trial-Level `result.json`

Each completed Trial has its own `result.json`. The observed evidence-bearing
fields are:

```json
{
  "id": "<trial UUID>",
  "task_name": "<task name>",
  "trial_name": "<trial directory name>",
  "trial_uri": "file://<trial directory>",
  "task_checksum": "<unprefixed SHA-256-like value>",
  "config": {"job_id": "<job UUID>", "agent": {}},
  "agent_info": {
    "name": "<agent>",
    "version": "<agent version>",
    "model_info": {"name": "<model>", "provider": "<optional provider>"}
  },
  "verifier_result": {"rewards": {"<key>": 1.0}},
  "exception_info": null,
  "started_at": "<timestamp>",
  "finished_at": "<timestamp>",
  "agent_result": {},
  "agent_execution": {},
  "verifier": {},
  "step_results": null
}
```

For the Oracle probe, `agent_info.model_info` was `null`. For an actual model
evaluation, the adapter must validate the configured evaluator profile against
the observed model information but must not assume that `provider` is present:
Harbor's own model type makes it optional.

### Claude Code Turn Evidence

For the installed 0.18.0 `claude-code` agent, Harbor does not guarantee that
`agent_result.rollout_details` is populated. After a successful session
extraction it writes `agent/trajectory.json`; its ATIF final metrics contain
`final_metrics.total_steps`, which is the source-backed count of agent turns.

Consequently, a CodeEdge Qwen or Opus bundle that requires the training
document's average-turn threshold must include and validate that trajectory
file for every completed Trial. The adapter must not substitute token IDs,
log-line counts, or an absent `rollout_details` value for `total_steps`.

An exception is represented by a non-null `exception_info`, including an
`exception_type`. It is evidence of a technical/event classification question,
not automatically a failed model attempt.

### Lock Material

The observed job `lock.json` has `schema_version: 2` and records:

- the Harbor release (`harbor.version`);
- `n_concurrent_trials` and retry configuration; and
- one `TrialLock` for each logical trial, including a task digest, agent,
  environment, and verifier configuration.

The observed `TrialLock.task.digest` is prefixed with `sha256:`. It did **not**
equal the Trial result's `task_checksum` in the probe. A Harbor Flow adapter
must not equate those values or fabricate a digest crosswalk. If a future
adapter needs to bind a materialized task to Harbor's `task_checksum`, it must
derive and test that algorithm for this exact CLI release.

## Observed Pass@4 Semantics

Harbor computes pass@k by evaluator group from individual Trial results. In
the installed 0.18.0 source, pass@k is produced only when every included Trial
has exactly one verifier reward and that reward is numeric binary `0` or `1`.
For four completed binary Trials, the job summary may contain `Pass@2` and
`Pass@4`.

The adapter must independently validate the raw evidence instead of trusting
only the terminal rendering:

1. Read the completed job summary and require `n_total_trials == 4`.
2. Discover exactly four completed per-Trial `result.json` files for the
   selected evaluator group.
3. Require every Trial's `config.job_id` to equal the job-level `id`.
4. Require every Trial's evaluator identity to match the frozen profile.
5. Read `agent/trajectory.json.final_metrics.total_steps` for every completed
   Claude Code Trial when the frozen policy requires an average-turn rule.
6. Read the configured reward key from every Trial's `verifier_result.rewards`.
7. Preserve `exception_info`, stderr/stdout, `lock.json`, and raw JSON as
   immutable evidence even when the aggregate cannot be accepted.
8. Calculate the task's pass count and compliance from the four raw Trial
   records; retain Harbor's `stats.evals.*.pass_at_k["4"]` as corroborating
   evidence, not the only source of truth.
9. Strictly parse and retain non-negative `stats.n_retries` as an immutable
   job-wide technical-retry aggregate. Do not manufacture per-TrialAttempt
   evidence from it; the four final logical Trial records remain the detail
   authority.

The four-trial Oracle probe completed successfully with all rewards `1.0` and
reported:

```text
Trials: 4
Exceptions: 0
Mean: 1.000
Pass@2: 1.000
Pass@4: 1.000
Reward 1.0 count: 4
```

This proves the local output shape and pass@4 calculation path. It does not
prove behavior for Qwen, Opus, a particular endpoint, or a future Harbor
version.

### Production Re-Probe After Locked Install

After the exact wheel installation above, Harbor Flow repeated the isolated
Docker/Oracle probe on 2026-07-14 with the same four-trial command shape. The
Docker runtime was re-probed on 2026-07-19 with client `29.5.2`, server `29.4.1`,
Compose `v5.1.3`, and Buildx `v0.33.0` using the exact identities above. The job
wrote four distinct Trial directories, reported `n_total_trials: 4`,
`n_running_trials: 0`, `n_pending_trials: 0`, and `Pass@4: 1.0`; every Trial
`config.job_id` matched the job result ID. The job-level `result.json` again
omitted `trial_results`, while each Trial result carried its own reward and
task checksum. This re-probe is the authority for the launcher fingerprint in
this document.

## Local Observation And Reconciliation Boundary

The CodeEdge evaluator is local-only. Its closed argv never includes upload or
sharing flags, it does not call a hosted service, and the managed
`<jobs-dir>/<job-name>` directory is the sole recovery source. A completed
evaluation receipt is rebuilt only from the local `config.json`, `lock.json`,
job `result.json`, exactly four completed Trial directories and their evidence,
and Harbor Flow's locally written output provenance.

It does not expose a local `job status`, `job list`, or `job cancel` command.
Local completion observation is therefore a filesystem protocol, not a stable
query API.

`harbor job resume` resumes a local job directory. It can remove selected
errored Trial directories before rerunning them, so it is not an immutable
read-only reconciliation operation. Harbor Flow must not invoke it merely to
answer "what happened?" or silently replace durable Trial evidence.

Until an adapter has an explicitly tested atomic snapshot rule, the safe
behavior after an interrupted command is:

1. Preserve the Run and TrialExecution as `in_doubt`.
2. Retain only bounded, redacted local process evidence and the controlled
   local job directory; never submit, upload, or query a remote result.
3. Complete reconciliation only when the strict local parser can prove the
   frozen task, CLI, agent/model, exactly-four-trial, result and secret-scan
   invariants from a complete local result.
4. Do not count missing, malformed, or still-running Trial files as model
   failures; leave them `in_doubt` for an explicit later decision.

## Adapter Requirements Derived From This Probe

The Harbor Flow `harbor-cli@0.18.0` adapter must be version/identity gated and
must fail closed when any of these conditions is false:

- CLI launcher, interpreter, Python source manifest, version, and installation
  fingerprints match this contract immediately before launch;
- Docker client and server versions, Compose/Buildx plugin paths and
  fingerprints, and complete plugin version outputs match this contract;
- the expected job directory is contained within the managed execution root;
- `finished_at` is present, `n_running_trials == 0`,
  `n_pending_trials == 0`, and `n_total_trials` equals the frozen number of
  logical Trials;
- every expected per-Trial result file is present, parseable, unique, and
  bound to the same job ID;
- evaluator/profile/reward expectations match the frozen local execution
  configuration; and
- raw artifacts can be copied into Harbor Flow's content-addressed store
  without leaking credentials.

The adapter must not:

- parse a nonexistent job-level `trial_results` array;
- infer a model provider from an absent `model_info.provider`;
- use a generated trial-directory suffix as a durable identity;
- equate `task_checksum` with the lock's task digest;
- treat a missing result file as a failed model Trial; or
- claim that this locally observed adapter contract is supplied by CodeEdge.

## Re-Probe Checklist

Before enabling the adapter for another Harbor build:

1. Record `harbor --version`, resolved launcher, launcher digest, interpreter
   digest, and source-tree digest.
2. Record the Docker client and server versions, resolved Compose and Buildx
   plugin paths and digests, and both complete plugin version outputs.
3. Run an isolated Oracle task with `--n-attempts 4` and record the raw
   command, output, job result, lock, and all four Trial results.
4. Verify the command flags, output layout, job summary schema, Trial schema,
   pass@4 calculation, and resume behavior again.
5. Update this document and add a new adapter/parser version with fixture-based
   tests before accepting production evidence from the new build.
