# Standard Authoring Codex Agent Bootstrap

The production Standard-authoring `agent.turn` handler is created from the
installed catalog-lock verifier and Standard-authoring runtime attestor.  It
does not receive a prompt, output schema, executable path, environment,
credential, model endpoint, checkout revision, or artifact ID from bootstrap
code.

```go
agentTurn, err := stageprovider.NewStandardAuthoringAttestedAgentTurnBridgeFromDeployment(
    stageprovider.StandardAuthoringAttestedAgentTurnBridgeDeploymentConfig{
        Verifier:      verifier,
        Attestor:      attestor,
        WorkspaceRoot: managedWorkspaceRoot,
    },
)
if err != nil {
    return err
}

composition, err := stageprovider.NewStandardAuthoringProviderComposition(
    stageprovider.StandardAuthoringProviderCompositionConfig{
        Template: workflowadapter.StandardAuthoringTemplateReference(),
        Catalog:  catalog,
        Lock:     lock,
        Attestor: attestor,
        Handlers: stageprovider.StandardAuthoringOperationHandlers{
            HostCommand:   hostCommand,
            AgentTurn:     agentTurn,
            HarborBuiltin: builtin,
            DurableReview: review,
        },
    },
)
```

For each real `agent.turn` effect, the bridge:

1. validates the frozen stage operation against the installed catalog and lock;
2. reads the lock-bound prompt and schema through
   `ReadStandardAuthoringContractAssets` using the real frozen resolution;
3. accepts only canonical, self-fingerprinted `StandardAuthoringCodexTurnProgram`
   JSON and the exact locked Draft-07 JSON Schema template, whose fingerprint
   is pinned by the deployment materials;
4. creates a one-effect strict executor; and
5. revalidates the record and attests the Codex App Server immediately before
   `OpenConversation`.

For each effect, the executor creates one stage-private
`harbor_submit_stage_output` dynamic tool and installs it when the conversation
opens. Dynamic tools are fixed for that conversation; the executor derives a
closed `OutputSchema` for each turn from the verified schema template and the
frozen `StageDescriptor`. The model supplies only an allowed verdict and the
base64 content values. Artifact name, schema version, stage identity, and path
are host-owned facts and cannot be selected by the model.

The submit handler strictly validates the candidate and internally
canonicalizes it before producing a `StageExecutionResult`. A passing submit
is the only output authority. Final assistant text is deliberately ignored
and cannot overwrite an accepted artifact.

### Structured planning shape

The `task_design` and `generate_task_files` prompts supply the exact, closed
shape of their `task_proposal` and `generated_task_files` artifacts. They
contain only `format`, `version`,
`contract_claims`, `requirements`, `source_paths`, `packages`, and `commands`.
`contract_claims` is a flat object with `title`, `slug`, `repository_url`,
`commit_sha`, `base_image`, `code_lang`, `task_type`, `application`,
`is_0_to_1`, and `source_root`. Each requirement is `{id, text}`, each package
is `{manifest_path}`, and each command is `{working_directory, argv}`.

`task_proposal` uses `harbor.standard-authoring-task-proposal.v2` and
`generated_task_files` uses `harbor.standard-authoring-generated-task-plan.v2`;
both use version `2`. Nested descriptive structures such as `task`, `source`,
`environment`, scope, or acceptance-criteria objects are not part of either
artifact and are rejected as unknown fields. The host still checks every
claimed contract value and every repository-relative path against the frozen
source; publishing the shape does not relax those checks.

### Task TOML shape

The `task_toml_generate` prompt supplies the minimum Harbor TaskConfig shape
accepted by the submission authority. The raw TOML must include `[task]` with
a valid `harbor/<slug>` name and description, `[metadata]` with the exact
contract `code_lang`, `task_type`, `application`, and `is_0_to_1` values,
`[environment]` with a positive build timeout, `network_mode = "no-network"`,
and `workdir = "/workspace/source"`, plus `[verifier]` with a positive
timeout. It must not use the ignored `[verification]` table or an
`environment.dockerfile` field. The host later canonicalizes frozen source
provenance; publishing this minimum shape does not relax validation.

`agent_turn` and `output_submission` are independently charged quota
dimensions. The current Standard-authoring policy reserves three
`output_submission` attempts for each agent stage, while its frozen turn
timeout and output byte limit continue to bound each turn and submission. An
invalid candidate is not persisted as raw content; the tool returns stable
diagnostic codes and a digest only.

The dynamic submit loop is session-local. Checkpoints preserve no raw Codex
transcript and are not resumable across a worker crash, so the bridge does not
claim automatic cross-process ReAct recovery. A retry begins a new fenced
stage attempt, and an existing Run's frozen definition is not retroactively
changed.

The catalog and matching agent lock must both carry the approved
`gpt-5.6-terra` / `high` pair. The invocation passes that effort to both the
conversation and each turn; a local Codex default cannot alter it. Historical
locks without an explicit effort remain readable for audit and reconciliation,
but the current Standard composition rejects them before an effect can start.

The dynamic read is intentional.  A deployment lock identifies the managed
checkout but does not contain a Run's frozen checkout revision or artifact
identities.  Bootstrap code must not invent those values merely to preload an
asset.  Every runtime failure remains fail-closed and the bridge never retains
an invocation or controlled environment from a prior effect.
