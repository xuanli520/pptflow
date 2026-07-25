package workflowadapter

import (
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func quotaClaimsForStage(policy QuotaPolicy, key workflowkit.StageKey) ([]workflowkit.QuotaClaim, bool) {
	for _, stage := range policy.Stages {
		if stage.StageKey == key {
			return stage.Claims, true
		}
	}
	return nil, false
}

func mutableCatalogStage(t *testing.T, catalog *StageCatalog, key workflowkit.StageKey) *StageDefinition {
	t.Helper()
	for index := range catalog.Stages {
		if catalog.Stages[index].Key == key {
			return &catalog.Stages[index]
		}
	}
	t.Fatalf("catalog omits stage %q", key)
	return nil
}

func mutableStageInput(t *testing.T, stage *StageDefinition, name string) *workflowkit.ArtifactSpec {
	t.Helper()
	for index := range stage.Inputs {
		if stage.Inputs[index].Name == name {
			return &stage.Inputs[index]
		}
	}
	t.Fatalf("stage %q omits input %q", stage.Key, name)
	return nil
}

func removeStageResource(resources []workflowkit.ResourceKey, target workflowkit.ResourceKey) []workflowkit.ResourceKey {
	result := make([]workflowkit.ResourceKey, 0, len(resources))
	for _, resource := range resources {
		if resource != target {
			result = append(result, resource)
		}
	}
	return result
}

func stageHasArtifact(artifacts []workflowkit.ArtifactSpec, name, schemaVersion string) bool {
	for _, artifact := range artifacts {
		if artifact.Name == name && artifact.SchemaVersion == schemaVersion && artifact.Required {
			return true
		}
	}
	return false
}

func stageCatalogResourceCount(resources []workflowkit.ResourceKey, target workflowkit.ResourceKey) int {
	count := 0
	for _, resource := range resources {
		if resource == target {
			count++
		}
	}
	return count
}
