package cmd

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// Linker values are deliberately separate from both Standard authoring and
// the evaluator child.  Equal source-build provenance does not authorize a
// catalog/lock for one closed template to execute another template.
var (
	codeEdgePhase1ProductionBuildModule                    string
	codeEdgePhase1ProductionBuildVersion                   string
	codeEdgePhase1ProductionBuildCommit                    string
	codeEdgePhase1ProductionBuildContentSHA256             string
	codeEdgePhase1ProductionBuildCatalogReceiptFingerprint string
	codeEdgePhase1ProductionBuildLockID                    string
	codeEdgePhase1ProductionBuildLockVersion               string
	codeEdgePhase1ProductionBuildLockFingerprint           string
)

// codeEdgePhase1ProductionBuildBinding is the complete linker-bound identity
// of the parent deployment materials.  Root composition compares it with the
// independently bound Standard and evaluator identities before it creates a
// Store or executes any provider.
type codeEdgePhase1ProductionBuildBinding struct {
	HarborFlowBuild           stageprovider.HarborFlowBuildIdentity
	CatalogReceiptFingerprint workflowkit.Fingerprint
	LockIdentity              stageprovider.DeploymentOperationCatalogLockIdentity
}

func (binding codeEdgePhase1ProductionBuildBinding) Validate() error {
	if err := binding.HarborFlowBuild.Validate(); err != nil {
		return fmt.Errorf("Harbor Flow build identity: %w", err)
	}
	if err := binding.CatalogReceiptFingerprint.Validate(); err != nil {
		return fmt.Errorf("CodeEdge Phase-1 catalog receipt fingerprint: %w", err)
	}
	if err := binding.LockIdentity.Validate(); err != nil {
		return fmt.Errorf("CodeEdge Phase-1 operation catalog lock identity: %w", err)
	}
	return nil
}

func linkedCodeEdgePhase1ProductionBuildBinding() (codeEdgePhase1ProductionBuildBinding, error) {
	binding := codeEdgePhase1ProductionBuildBinding{
		HarborFlowBuild: stageprovider.HarborFlowBuildIdentity{
			Module: codeEdgePhase1ProductionBuildModule, Version: codeEdgePhase1ProductionBuildVersion,
			Commit: codeEdgePhase1ProductionBuildCommit, ContentSHA256: workflowkitFingerprint(codeEdgePhase1ProductionBuildContentSHA256),
		},
		CatalogReceiptFingerprint: workflowkitFingerprint(codeEdgePhase1ProductionBuildCatalogReceiptFingerprint),
		LockIdentity: stageprovider.DeploymentOperationCatalogLockIdentity{
			LockID: codeEdgePhase1ProductionBuildLockID, LockVersion: codeEdgePhase1ProductionBuildLockVersion,
			Fingerprint: workflowkitFingerprint(codeEdgePhase1ProductionBuildLockFingerprint),
		},
	}
	if err := binding.Validate(); err != nil {
		return codeEdgePhase1ProductionBuildBinding{}, fmt.Errorf("CodeEdge Phase-1 production build binding is unavailable; build with scripts/build-codeedge-production.sh: %w", err)
	}
	return binding, nil
}
