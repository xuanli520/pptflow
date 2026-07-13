package app

import (
	"context"
	"errors"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestRequiredStageArtifactsRejectsIncompleteOrDriftedOutput(t *testing.T) {
	stage := workflowkit.StageDescriptor{
		Key: "verify",
		Outputs: []workflowkit.ArtifactSpec{
			{Name: "report", SchemaVersion: "v1", Required: true},
			{Name: "trace", SchemaVersion: "v1", Required: false},
		},
	}
	if err := RequiredStageArtifacts(stage, nil); !errors.Is(err, ErrInvalidStageExecution) {
		t.Fatalf("missing required artifact error = %v, want ErrInvalidStageExecution", err)
	}
	if err := RequiredStageArtifacts(stage, []StageArtifact{{Key: "report", SchemaVersion: "v2"}}); !errors.Is(err, ErrInvalidStageExecution) {
		t.Fatalf("schema drift error = %v, want ErrInvalidStageExecution", err)
	}
	if err := RequiredStageArtifacts(stage, []StageArtifact{
		{Key: "report", SchemaVersion: "v1"},
		{Key: "report", SchemaVersion: "v1"},
	}); !errors.Is(err, ErrInvalidStageExecution) {
		t.Fatalf("duplicate artifact error = %v, want ErrInvalidStageExecution", err)
	}
	if err := RequiredStageArtifacts(stage, []StageArtifact{{Key: "report", SchemaVersion: "v1"}}); err != nil {
		t.Fatalf("complete required output rejected: %v", err)
	}
}

func TestStageExecutorRegistryResolvesOnlyExactFrozenPluginBinding(t *testing.T) {
	executor := StageExecutorFunc(func(_ context.Context, _ StageExecutionRequest) (StageExecutionResult, error) {
		return StageExecutionResult{}, nil
	})
	registry, err := workflowkit.NewControlledPluginRegistry([]workflowkit.PluginRegistration[StageExecutor]{
		{Binding: workflowkit.PluginBinding{ID: "harborfactory.verify", Version: "1.0.0"}, Implementation: executor},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := registry.ResolveStagePlugin(workflowkit.StageDescriptor{Plugin: workflowkit.PluginBinding{ID: "harborfactory.verify", Version: "1.0.0"}}); err != nil || resolved == nil {
		t.Fatalf("registered executor = %T, err=%v", resolved, err)
	}
	if _, err := registry.ResolvePlugin(workflowkit.PluginBinding{ID: "harborfactory.verify", Version: "2.0.0"}); !errors.Is(err, workflowkit.ErrPluginVersionMismatch) {
		t.Fatalf("version drift error = %v, want ErrPluginVersionMismatch", err)
	}
	if _, err := workflowkit.NewControlledPluginRegistry([]workflowkit.PluginRegistration[StageExecutor]{
		{Binding: workflowkit.PluginBinding{ID: "harborfactory.broken", Version: "1.0.0"}},
	}); !errors.Is(err, workflowkit.ErrPluginUnavailable) {
		t.Fatalf("nil executor error = %v, want ErrPluginUnavailable", err)
	}
}

func TestNormalizeQuotaClaimsRequiresUniqueExplicitDimensions(t *testing.T) {
	claims, err := NormalizeQuotaClaims([]store.TaskActorQuotaClaim{
		{Dimension: "token", Units: 3, ReclaimPolicy: store.QuotaReclaimNever},
		{Dimension: "trial", Units: 1, ReclaimPolicy: store.QuotaReclaimUnused},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 || claims[0].Dimension != "token" || claims[1].Dimension != "trial" {
		t.Fatalf("normalized claims = %#v", claims)
	}
	if _, err := NormalizeQuotaClaims([]store.TaskActorQuotaClaim{
		{Dimension: "token", Units: 1, ReclaimPolicy: store.QuotaReclaimUnused},
		{Dimension: "token", Units: 2, ReclaimPolicy: store.QuotaReclaimUnused},
	}); !errors.Is(err, ErrInvalidStageExecution) {
		t.Fatalf("duplicate quota dimensions error = %v, want ErrInvalidStageExecution", err)
	}
}

func TestStageQuotaPlannerFunctionNormalizesBeforeAdmission(t *testing.T) {
	planner := StageQuotaPlannerFunc(func(context.Context, StageExecutionRequest) ([]store.TaskActorQuotaClaim, error) {
		return []store.TaskActorQuotaClaim{
			{Dimension: "trial", Units: 1, ReclaimPolicy: store.QuotaReclaimUnused},
			{Dimension: "token", Units: 10, ReclaimPolicy: store.QuotaReclaimNever},
		}, nil
	})
	claims, err := planner.PlanStageQuota(context.Background(), StageExecutionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 || claims[0].Dimension != "token" || claims[1].Dimension != "trial" {
		t.Fatalf("planned claims = %#v", claims)
	}
}

func TestStageExecutorFunctionCannotMutateFrozenStageDescriptor(t *testing.T) {
	request := StageExecutionRequest{Stage: workflowkit.StageDescriptor{
		Key:          "verify",
		Dependencies: []workflowkit.StageKey{"prepare"},
		Inputs:       []workflowkit.ArtifactSpec{{Name: "input", SchemaVersion: "v1", Required: true}},
		Outputs:      []workflowkit.ArtifactSpec{{Name: "report", SchemaVersion: "v1", Required: true}},
	}}
	executor := StageExecutorFunc(func(_ context.Context, received StageExecutionRequest) (StageExecutionResult, error) {
		received.Stage.Dependencies[0] = "mutated"
		received.Stage.Inputs[0].Name = "mutated"
		received.Stage.Outputs[0].Name = "mutated"
		return StageExecutionResult{}, nil
	})
	if _, err := executor.ExecuteStage(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if request.Stage.Dependencies[0] != "prepare" || request.Stage.Inputs[0].Name != "input" || request.Stage.Outputs[0].Name != "report" {
		t.Fatalf("executor mutated frozen stage descriptor: %#v", request.Stage)
	}
}
