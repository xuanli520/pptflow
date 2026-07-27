package app

import "github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"

const (
	StandardAuthoringVerificationContractFormat  = workflowadapter.StandardAuthoringVerificationContractFormat
	StandardAuthoringVerificationContractVersion = workflowadapter.StandardAuthoringVerificationContractVersion
)

type StandardAuthoringCoverageMode = workflowadapter.StandardAuthoringCoverageMode

const (
	StandardAuthoringCoverageNative      = workflowadapter.StandardAuthoringCoverageNative
	StandardAuthoringCoverageIntegration = workflowadapter.StandardAuthoringCoverageIntegration
	StandardAuthoringCoverageBrowserWASM = workflowadapter.StandardAuthoringCoverageBrowserWASM
)

type StandardAuthoringVerificationContract = workflowadapter.StandardAuthoringVerificationContract

func ParseStandardAuthoringVerificationContractJSON(raw []byte) (StandardAuthoringVerificationContract, error) {
	return workflowadapter.ParseStandardAuthoringVerificationContractJSON(raw)
}
