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
   JSON and the exact versioned output-schema fingerprint;
4. creates a one-effect strict executor; and
5. revalidates the record and attests the Codex App Server immediately before
   `OpenConversation`.

The dynamic read is intentional.  A deployment lock identifies the managed
checkout but does not contain a Run's frozen checkout revision or artifact
identities.  Bootstrap code must not invent those values merely to preload an
asset.  Every runtime failure remains fail-closed and the bridge never retains
an invocation or controlled environment from a prior effect.
