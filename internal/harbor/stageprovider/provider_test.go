package stageprovider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	stageProviderTestTaskID     = "018f0a73-3b49-7000-8000-000000000021"
	stageProviderTestRevisionID = "018f0a73-3b49-7000-8000-000000000022"
	stageProviderTestDigest     = "harbor.task.v2:sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestControlledWorkflowkitProviderRegistryRequiresExactFrozenBinding(t *testing.T) {
	specification := testsupport.CompleteRunExecutionSpec(stageProviderTestTaskID, stageProviderTestRevisionID, stageProviderTestDigest)
	resolution, err := specification.ResolveStageOperation(workflowadapter.RepoPrepare)
	if err != nil {
		t.Fatal(err)
	}
	executor := workflowkit.StageExecutorFunc(func(context.Context, workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		return workflowkit.StageExecutionResult{}, nil
	})
	adapter, err := NewStaticWorkflowkitStageOperationProvider([]WorkflowkitStaticStageOperationRegistration{{
		StageKey: resolution.StageKey, Operation: resolution.Operation, Executor: executor,
	}})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewControlledWorkflowkitProviderRegistry([]WorkflowkitProviderRegistration{{Provider: resolution.Provider, Adapter: adapter}})
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := registry.ResolveWorkflowkitStageOperation(resolution); err != nil || resolved == nil {
		t.Fatalf("resolve exact binding = %T, %v", resolved, err)
	}

	unknownProvider := resolution.Clone()
	unknownProvider.Provider.ID = "not-installed"
	unknownProvider.Operation.ProviderID = "not-installed"
	if _, err := registry.ResolveWorkflowkitStageOperation(unknownProvider); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("unknown provider = %v, want ErrProviderUnavailable", err)
	}
	providerVersionDrift := resolution.Clone()
	providerVersionDrift.Provider.Version = "2"
	if _, err := registry.ResolveWorkflowkitStageOperation(providerVersionDrift); !errors.Is(err, ErrProviderVersionMismatch) {
		t.Fatalf("provider version drift = %v, want ErrProviderVersionMismatch", err)
	}
	operationVersionDrift := resolution.Clone()
	operationVersionDrift.Operation.Version = "2"
	if _, err := registry.ResolveWorkflowkitStageOperation(operationVersionDrift); !errors.Is(err, ErrStageOperationUnavailable) {
		t.Fatalf("operation version drift = %v, want ErrStageOperationUnavailable", err)
	}
	payloadDrift := resolution.Clone()
	payloadDrift.Operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{"changed"}}
	if _, err := registry.ResolveWorkflowkitStageOperation(payloadDrift); !errors.Is(err, ErrFrozenOperationPayloadMismatch) {
		t.Fatalf("payload drift = %v, want ErrFrozenOperationPayloadMismatch", err)
	}
}

func TestTypedWorkflowkitStageOperationProviderDispatchesOnlyTypedPayloads(t *testing.T) {
	plugin := workflowkit.PluginBinding{ID: "test.provider", Version: "1"}
	provider := workflowadapter.ProviderReference{ID: "provider", Kind: "controlled", Version: "1"}
	cases := []struct {
		name      string
		operation workflowadapter.StageOperationBinding
		install   func(*bool) TypedWorkflowkitOperationHandlers
	}{
		{
			name:      "local command",
			operation: workflowadapter.StageOperationBinding{ProviderID: provider.ID, OperationID: "local", Version: "1", Payload: workflowadapter.LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{"run"}}},
			install: func(called *bool) TypedWorkflowkitOperationHandlers {
				return TypedWorkflowkitOperationHandlers{LocalCommand: LocalCommandOperationExecutorFunc(func(_ context.Context, invocation StageOperationInvocation, payload workflowadapter.LocalCommandOperationPayload) (workflowkit.StageExecutionResult, error) {
					*called = invocation.Resolution.Provider == provider && payload.CommandID == "harbor-stage"
					return workflowkit.StageExecutionResult{}, nil
				})}
			},
		},
		{
			name:      "container command",
			operation: workflowadapter.StageOperationBinding{ProviderID: provider.ID, OperationID: "container", Version: "1", Payload: workflowadapter.ContainerCommandOperationPayload{ImageDigest: "registry.example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Command: []string{"run"}}},
			install: func(called *bool) TypedWorkflowkitOperationHandlers {
				return TypedWorkflowkitOperationHandlers{ContainerCommand: ContainerCommandOperationExecutorFunc(func(_ context.Context, _ StageOperationInvocation, payload workflowadapter.ContainerCommandOperationPayload) (workflowkit.StageExecutionResult, error) {
					*called = payload.Command[0] == "run"
					return workflowkit.StageExecutionResult{}, nil
				})}
			},
		},
		{
			name:      "agent turn",
			operation: workflowadapter.StageOperationBinding{ProviderID: provider.ID, OperationID: "agent", Version: "1", Payload: workflowadapter.AgentTurnOperationPayload{AgentID: "repair_agent", ModelID: "model_v2", MaxTurns: 2}},
			install: func(called *bool) TypedWorkflowkitOperationHandlers {
				return TypedWorkflowkitOperationHandlers{AgentTurn: AgentTurnOperationExecutorFunc(func(_ context.Context, _ StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload) (workflowkit.StageExecutionResult, error) {
					*called = payload.AgentID == "repair_agent" && payload.MaxTurns == 2
					return workflowkit.StageExecutionResult{}, nil
				})}
			},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			called := false
			stageKey := workflowkit.StageKey("typed-" + test.operation.OperationID)
			adapter, err := NewTypedWorkflowkitStageOperationProvider(TypedWorkflowkitStageOperationProviderConfig{
				Handlers:   test.install(&called),
				Operations: []TypedWorkflowkitStageOperationRegistration{{StageKey: stageKey, Operation: test.operation}},
			})
			if err != nil {
				t.Fatal(err)
			}
			resolution := workflowadapter.StageOperationResolution{StageKey: stageKey, Plugin: plugin, Provider: provider, Operation: test.operation}
			executor, err := adapter.ResolveWorkflowkitStageOperation(resolution)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := executor.ExecuteStage(context.Background(), workflowkit.StageExecutionRequest{Stage: workflowkit.StageDescriptor{Key: stageKey, Plugin: plugin}}); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("typed executor was not invoked")
			}
		})
	}
}

func TestTypedWorkflowkitStageOperationProviderRejectsMissingCapabilityExecutor(t *testing.T) {
	_, err := NewTypedWorkflowkitStageOperationProvider(TypedWorkflowkitStageOperationProviderConfig{Operations: []TypedWorkflowkitStageOperationRegistration{{
		StageKey: "typed-local",
		Operation: workflowadapter.StageOperationBinding{
			ProviderID: "provider", OperationID: "local", Version: "1",
			Payload: workflowadapter.LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{}},
		},
	}}})
	if !errors.Is(err, ErrStageOperationUnavailable) {
		t.Fatalf("missing local.command executor = %v, want ErrStageOperationUnavailable", err)
	}
}

func TestPublicWorkflowkitAdapterReadsFrozenOpaqueExecutionSpec(t *testing.T) {
	specification := testsupport.CompleteRunExecutionSpec(stageProviderTestTaskID, stageProviderTestRevisionID, stageProviderTestDigest)
	registry := completeWorkflowkitStageExecutorRegistry(t, specification)
	definition, found := workflowadapter.StandardStageCatalog().Stage(workflowadapter.RepoPrepare)
	if !found {
		t.Fatal("repo prepare stage is missing")
	}
	executor, err := registry.ResolvePlugin(catalogPluginBinding(definition.Plugin))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.ExecuteStage(context.Background(), workflowkitStageRequest(t, specification, definition)); err != nil {
		t.Fatalf("execute canonical frozen spec: %v", err)
	}

	drifted := specification.Clone()
	for index, binding := range drifted.Stages {
		typed, ok := binding.(workflowadapter.RepoPrepareBinding)
		if !ok {
			continue
		}
		typed.Operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{"drifted"}}
		drifted.Stages[index] = typed
		break
	}
	if _, err := executor.ExecuteStage(context.Background(), workflowkitStageRequest(t, drifted, definition)); !errors.Is(err, ErrFrozenOperationPayloadMismatch) {
		t.Fatalf("adapter did not consume drifted opaque spec: %v", err)
	}
}

func TestPublicWorkflowkitAdapterRequiresExactTemplateAndSupportsCodeEdge(t *testing.T) {
	if _, err := NewWorkflowkitStageExecutorRegistry(); err == nil {
		t.Fatal("template-less public-engine registry unexpectedly succeeded")
	}
	registry, err := NewWorkflowkitStageExecutorRegistry(WorkflowkitRegistryOptions{Template: workflowadapter.CodeEdgePhase1TemplateReference()})
	if err != nil {
		t.Fatalf("construct CodeEdge public-engine registry: %v", err)
	}
	definition, found := workflowadapter.CodeEdgePhase1StageCatalog().Stage(workflowadapter.CodeEdgeLint)
	if !found {
		t.Fatal("CodeEdge lint stage is missing")
	}
	executor, err := registry.ResolvePlugin(catalogPluginBinding(definition.Plugin))
	if err != nil {
		t.Fatal(err)
	}
	codeEdgeSpec := testsupport.CompleteCodeEdgePhase1RunExecutionSpec(stageProviderTestTaskID, stageProviderTestRevisionID, stageProviderTestDigest)
	if _, err := executor.ExecuteStage(context.Background(), workflowkitStageRequest(t, codeEdgeSpec, definition)); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("CodeEdge registry did not consume its exact frozen template: %v", err)
	}
	standardSpec := testsupport.CompleteRunExecutionSpec(stageProviderTestTaskID, stageProviderTestRevisionID, stageProviderTestDigest)
	if _, err := executor.ExecuteStage(context.Background(), workflowkitStageRequest(t, standardSpec, definition)); !errors.Is(err, ErrFrozenExecutionSpec) {
		t.Fatalf("Standard spec reached CodeEdge registry instead of template rejection: %v", err)
	}
}

func TestPublicWorkflowkitAdapterDispatchesAnExplicitMultiTemplateSetByFrozenSpec(t *testing.T) {
	registry, err := NewWorkflowkitStageExecutorRegistry(WorkflowkitRegistryOptions{Templates: []workflowadapter.TemplateReference{
		workflowadapter.StandardTemplateReference(), workflowadapter.CodeEdgePhase1TemplateReference(),
	}})
	if err != nil {
		t.Fatalf("construct explicit multi-template registry: %v", err)
	}
	codeEdgeDefinition, found := workflowadapter.CodeEdgePhase1StageCatalog().Stage(workflowadapter.CodeEdgeLint)
	if !found {
		t.Fatal("CodeEdge lint stage is missing")
	}
	standardDefinition, found := workflowadapter.StandardStageCatalog().Stage(workflowadapter.CodeEdgeLint)
	if !found {
		t.Fatal("Standard lint stage is missing")
	}
	if codeEdgeDefinition.Plugin != standardDefinition.Plugin {
		t.Fatalf("fixture needs one shared plugin binding, CodeEdge=%+v Standard=%+v", codeEdgeDefinition.Plugin, standardDefinition.Plugin)
	}
	executor, err := registry.ResolvePlugin(catalogPluginBinding(codeEdgeDefinition.Plugin))
	if err != nil {
		t.Fatal(err)
	}
	codeEdgeSpec := testsupport.CompleteCodeEdgePhase1RunExecutionSpec(stageProviderTestTaskID, stageProviderTestRevisionID, stageProviderTestDigest)
	if _, err := executor.ExecuteStage(context.Background(), workflowkitStageRequest(t, codeEdgeSpec, codeEdgeDefinition)); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("CodeEdge spec did not reach its explicit template executor: %v", err)
	}
	standardSpec := testsupport.CompleteRunExecutionSpec(stageProviderTestTaskID, stageProviderTestRevisionID, stageProviderTestDigest)
	if _, err := executor.ExecuteStage(context.Background(), workflowkitStageRequest(t, standardSpec, standardDefinition)); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("Standard spec did not reach its explicit template executor: %v", err)
	}
	if _, err := NewWorkflowkitStageExecutorRegistry(WorkflowkitRegistryOptions{
		Template: workflowadapter.StandardTemplateReference(), Templates: []workflowadapter.TemplateReference{workflowadapter.CodeEdgePhase1TemplateReference()},
	}); err == nil {
		t.Fatal("registry accepted ambiguous single and multi template configuration")
	}
}

func completeWorkflowkitStageExecutorRegistry(t *testing.T, specification workflowadapter.RunExecutionSpec) *workflowkit.ControlledPluginRegistry[workflowkit.StageExecutor] {
	t.Helper()
	operationsByProvider := make(map[string][]WorkflowkitStaticStageOperationRegistration)
	providersByID := make(map[string]workflowadapter.ProviderReference)
	for _, provider := range specification.References.Providers {
		providersByID[provider.ID] = provider
	}
	for _, definition := range workflowadapter.StandardStageCatalog().Stages {
		resolution, err := specification.ResolveStageOperation(definition.Key)
		if err != nil {
			t.Fatalf("resolve %q operation: %v", definition.Key, err)
		}
		executor := workflowkit.StageExecutorFunc(func(_ context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
			return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass}, Artifacts: successfulArtifacts(request.Stage)}, nil
		})
		operationsByProvider[resolution.Provider.ID] = append(operationsByProvider[resolution.Provider.ID], WorkflowkitStaticStageOperationRegistration{
			StageKey: definition.Key, Operation: resolution.Operation, Executor: executor,
		})
	}
	registrations := make([]WorkflowkitProviderRegistration, 0, len(operationsByProvider))
	for providerID, operations := range operationsByProvider {
		adapter, err := NewStaticWorkflowkitStageOperationProvider(operations)
		if err != nil {
			t.Fatal(err)
		}
		registrations = append(registrations, WorkflowkitProviderRegistration{Provider: providersByID[providerID], Adapter: adapter})
	}
	providers, err := NewControlledWorkflowkitProviderRegistry(registrations)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewWorkflowkitStageExecutorRegistry(WorkflowkitRegistryOptions{Providers: providers, Template: specification.Template})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func workflowkitStageRequest(t *testing.T, specification workflowadapter.RunExecutionSpec, definition workflowadapter.StageDefinition) workflowkit.StageExecutionRequest {
	t.Helper()
	canonical, err := specification.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := workflowkit.NewOpaqueExecutionBinding(workflowadapter.RunExecutionSpecFormat, workflowadapter.RunExecutionSpecVersion, canonical)
	if err != nil {
		t.Fatal(err)
	}
	stage := workflowkitStageDescriptor(definition)
	execution := workflowkit.FrozenExecution{
		ID:      "stageprovider-public-execution",
		Subject: workflowkit.SubjectBinding{SubjectID: specification.Selection.TaskID, RevisionID: specification.Selection.RevisionID, Digest: specification.Selection.RevisionDigest},
		Binding: binding,
	}
	stageClaim := workflowkit.StageClaim{StageAttempt: workflowkit.AttemptIdentity{ID: "stageprovider-public-attempt", Kind: workflowkit.AttemptStage, ScopeID: string(stage.Key), Ordinal: 1}, Stage: stage}
	claim := workflowkit.JobClaim{
		JobID: "stageprovider-public-job", ClaimID: "stageprovider-public-claim", Kind: workflowkit.JobStage, Owner: "worker", FencingToken: 1,
		LeaseExpiresAt: time.Now().Add(time.Minute), Execution: execution, Stage: &stageClaim,
	}
	return workflowkit.StageExecutionRequest{Execution: execution, Claim: claim, Stage: stage}
}

func workflowkitStageDescriptor(definition workflowadapter.StageDefinition) workflowkit.StageDescriptor {
	return workflowkit.StageDescriptor{
		Key: definition.Key, Version: definition.Version, Plugin: catalogPluginBinding(definition.Plugin), Group: string(definition.Group),
		Dependencies: append([]workflowkit.StageKey(nil), definition.Dependencies...), Inputs: append([]workflowkit.ArtifactSpec(nil), definition.Inputs...),
		Outputs: append([]workflowkit.ArtifactSpec(nil), definition.Outputs...), ReadSet: append([]workflowkit.ResourceKey(nil), definition.ReadSet...),
		WriteSet: append([]workflowkit.ResourceKey(nil), definition.WriteSet...), Effect: definition.Effect, Retry: definition.Retry.Clone(),
		Verdicts: definition.Verdicts.Clone(), Reuse: definition.Reuse, Capabilities: definition.Capabilities.Clone(),
	}
}

func successfulArtifacts(stage workflowkit.StageDescriptor) []workflowkit.StageArtifact {
	artifacts := make([]workflowkit.StageArtifact, 0, len(stage.Outputs))
	for _, output := range stage.Outputs {
		artifacts = append(artifacts, workflowkit.StageArtifact{Name: output.Name, SchemaVersion: output.SchemaVersion, Content: []byte("ok")})
	}
	return artifacts
}
