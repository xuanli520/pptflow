package app

import (
	"fmt"

	deterministicplugins "github.com/purplevoid/harbor-factory/internal/plugins/deterministic"
	genplugins "github.com/purplevoid/harbor-factory/internal/plugins/gen"
	infraplugins "github.com/purplevoid/harbor-factory/internal/plugins/infra"
	verifyplugins "github.com/purplevoid/harbor-factory/internal/plugins/verify"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func buildProductionRegistry(broker infraplugins.GateBroker) (*workflow.Registry, error) {
	registry := workflow.NewRegistry()
	plugins := []workflow.Plugin{
		infraplugins.RepoPreparePlugin{},
		infraplugins.HumanGatePlugin{Broker: broker},
		infraplugins.TaskRepairPlugin{},
		genplugins.RepoAnalyzePlugin{},
		genplugins.TaskDesignPlugin{},
		genplugins.GenerateTaskFilesPlugin{},
		genplugins.InstructionPlugin{},
		genplugins.TaskTOMLPlugin{},
		genplugins.DockerfilePlugin{},
		genplugins.SolvePlugin{},
		genplugins.TestPlugin{},
		genplugins.TestsAnalysisPlugin{},
		genplugins.MaterializePlugin{},
		genplugins.PublishTaskPlugin{},
		genplugins.RuntimeSelfCheckPlugin{},
		verifyplugins.DockerBuildPlugin{},
		verifyplugins.InitialVerifyPlugin{},
		verifyplugins.OracleVerifyPlugin{},
		verifyplugins.VerifyReportImportPlugin{},
		verifyplugins.QualityCheckPlugin{},
		deterministicplugins.CodeEdgeLintPlugin{},
		deterministicplugins.SimilarityPlugin{},
		deterministicplugins.SimilarityReportImportPlugin{},
		verifyplugins.HarborRunQwenPlugin{},
		verifyplugins.HarborRunOpusPlugin{},
		deterministicplugins.PackagePlugin{},
	}
	for _, plugin := range plugins {
		if err := registry.Register(plugin); err != nil {
			return nil, fmt.Errorf("register production workflow plugin %s: %w", plugin.Manifest().ID, err)
		}
	}
	return registry, nil
}
