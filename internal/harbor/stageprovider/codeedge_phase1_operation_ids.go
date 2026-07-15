package stageprovider

// CodeEdge Phase-1 parent operation IDs are deployment capability names. The
// catalog/lock binds them to one exact handler or local executable; they are
// deliberately not paths, shell fragments, or user-configurable stage names.
const (
	CodeEdgePhase1TaskLayoutPreflightHandlerID           = "codeedge-phase1.task-layout-preflight"
	CodeEdgePhase1RepoProvenancePreflightHandlerID       = "codeedge-phase1.repo-provenance-preflight"
	CodeEdgePhase1EnvironmentIsolationPreflightHandlerID = "codeedge-phase1.environment-isolation-preflight"
	CodeEdgePhase1TestsAnalysisValidateHandlerID         = "codeedge-phase1.tests-analysis-validate"
	CodeEdgePhase1QualityCheckHandlerID                  = "codeedge-phase1.quality-check"
	CodeEdgePhase1SimilarityCheckHandlerID               = "codeedge-phase1.similarity-check"
	CodeEdgePhase1SubmissionLintHandlerID                = "codeedge-phase1.submission-lint"
	CodeEdgePhase1LocalPackageHandlerID                  = "codeedge-phase1.local-package"

	CodeEdgePhase1DockerBuildCommandID   = "codeedge-phase1.docker-build"
	CodeEdgePhase1InitialVerifyCommandID = "codeedge-phase1.initial-verify"
	CodeEdgePhase1OracleVerifyCommandID  = "codeedge-phase1.oracle-verify"
)

// IsCodeEdgePhase1BuiltinHandlerID reports whether handlerID belongs to the
// closed parent implementation. Package remains operator-only and is not
// admitted by the worker executor; local package materialization belongs to
// the explicit lifecycle release boundary.
func IsCodeEdgePhase1BuiltinHandlerID(handlerID string) bool {
	switch handlerID {
	case CodeEdgePhase1TaskLayoutPreflightHandlerID,
		CodeEdgePhase1RepoProvenancePreflightHandlerID,
		CodeEdgePhase1EnvironmentIsolationPreflightHandlerID,
		CodeEdgePhase1TestsAnalysisValidateHandlerID,
		CodeEdgePhase1QualityCheckHandlerID,
		CodeEdgePhase1SimilarityCheckHandlerID,
		CodeEdgePhase1SubmissionLintHandlerID,
		CodeEdgePhase1LocalPackageHandlerID:
		return true
	default:
		return false
	}
}

// IsCodeEdgePhase1LocalCommandID reports whether commandID belongs to the
// three fixed Docker-related parent operations.
func IsCodeEdgePhase1LocalCommandID(commandID string) bool {
	switch commandID {
	case CodeEdgePhase1DockerBuildCommandID,
		CodeEdgePhase1InitialVerifyCommandID,
		CodeEdgePhase1OracleVerifyCommandID:
		return true
	default:
		return false
	}
}
