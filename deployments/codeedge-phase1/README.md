# CodeEdge Phase-1 parent deployment

This directory is the independent deployment bundle for the complete
`harbor.codeedge-phase1@2.2.0` parent workflow.  It is deliberately separate
from `../codeedge-evaluator-child/`: the parent performs the local task
preflight, controlled verification, durable reviews, evidence adoption and
package authorization; the child alone owns the external Qwen and Opus
pass@4 effects.

`operation-catalog.v1.json` is source-controlled policy.  A production build
must install a generated `operation-catalog.lock.json` beside it.  That lock
must contain both `codeedge_phase1_execution_profile` and
`codeedge_phase1_final_compliance_policy`, plus the exact local executable
attestations for Docker-backed stages.  Missing, substituted, or evaluator
child-only material is rejected before a parent Run can start.

The source-controlled parent assets are deliberately small and closed:

- `execution-profile.v1.json` is the complete 15-stage timing and retry
  envelope. Parent stages use one logical attempt; no caller-supplied retry
  default is accepted.
- `preflight-profile.v1.json` contains the task metadata paths and the exact
  names of protected evaluator-related environment variables. It contains no
  environment values.
- `final-compliance-policy.v1.json` freezes the Qwen and Opus evidence rules,
  including exactly four logical trials per evaluator.

Generate the lock only from a clean committed source checkout and explicitly
resolved regular executables:

```sh
HARBOR_FACTORY_GIT_EXECUTABLE=/usr/bin/git \
HARBOR_FACTORY_DOCKER_EXECUTABLE=/usr/bin/docker \
./scripts/generate-codeedge-phase1-lock.sh \
  --build-version v2.0.0 \
  --lock-version 2026.07.19.2
```

The generator refuses a dirty or untracked worktree, substituted asset paths,
symlinked executables, an existing output lock, and ambiguous Docker version
output. Its shared source manifest is the raw `HEAD` `git ls-tree` stream with
all three generated deployment lock paths excluded, so Standard authoring,
parent, and evaluator child locks bind the same source identity without a hash
cycle. It probes only the explicit Docker binary with `--version`; it never
reads provider endpoints, credentials, model environment variables, or PATH
defaults.

The parent has no model endpoint or credential capability.  Model credentials
are restricted to the separately locked evaluator child bundle.
