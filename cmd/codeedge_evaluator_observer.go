package cmd

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// codeEdgeEvaluatorCompletedObserver shares the production lock verifier,
// attestor, and local executor with the normal evaluator execution path. It
// intentionally has no launch capability: it may only turn complete local
// Harbor evidence into the same immutable result that the original attempt
// would have returned.
type codeEdgeEvaluatorCompletedObserver struct {
	verifier stageprovider.DeploymentOperationCatalogLockVerifier
	attestor codeEdgeEvaluatorObservationAttestor
	executor codeEdgeEvaluatorObservationExecutor
}

var _ app.CodeEdgeEvaluatorCompletedObserver = (*codeEdgeEvaluatorCompletedObserver)(nil)

type codeEdgeEvaluatorObservationAttestor interface {
	AttestHarborEvaluatorOperation(context.Context, stageprovider.DeploymentOperationRuntimeAttestation) (stageprovider.HarborEvaluatorInvocation, error)
}

type codeEdgeEvaluatorObservationExecutor interface {
	ObserveCompletedHarborEvaluator(context.Context, stageprovider.StageOperationInvocation, workflowadapter.LocalCommandOperationPayload) (workflowkit.StageExecutionResult, bool, error)
}

func newCodeEdgeEvaluatorCompletedObserver(
	verifier stageprovider.DeploymentOperationCatalogLockVerifier,
	attestor codeEdgeEvaluatorObservationAttestor,
	executor codeEdgeEvaluatorObservationExecutor,
) (*codeEdgeEvaluatorCompletedObserver, error) {
	if verifier == nil {
		return nil, fmt.Errorf("CodeEdge evaluator completed observer requires a catalog-lock verifier")
	}
	if attestor == nil {
		return nil, fmt.Errorf("CodeEdge evaluator completed observer requires a runtime attestor")
	}
	if executor == nil {
		return nil, fmt.Errorf("CodeEdge evaluator completed observer requires a local executor")
	}
	if err := verifier.VerifyLockIdentity(verifier.LockIdentity()); err != nil {
		return nil, fmt.Errorf("verify CodeEdge evaluator completed observer lock: %w", err)
	}
	return &codeEdgeEvaluatorCompletedObserver{verifier: verifier, attestor: attestor, executor: executor}, nil
}

// ObserveCompletedCodeEdgeEvaluator refuses every input that is not still
// approved by the installed catalog lock, re-attests the local evaluator
// installation and endpoint identity, then delegates only to the executor's
// read-only observation method. No path here can invoke Harbor or a model.
func (observer *codeEdgeEvaluatorCompletedObserver) ObserveCompletedCodeEdgeEvaluator(ctx context.Context, request app.CodeEdgeEvaluatorObservationRequest) (workflowkit.StageExecutionResult, bool, error) {
	if observer == nil || observer.verifier == nil || observer.attestor == nil || observer.executor == nil {
		return workflowkit.StageExecutionResult{}, false, fmt.Errorf("CodeEdge evaluator completed observer is unavailable")
	}
	if request.Execution.Stage.Key != request.Resolution.StageKey {
		return workflowkit.StageExecutionResult{}, false, fmt.Errorf("CodeEdge evaluator observation stage %q does not match frozen operation stage %q", request.Execution.Stage.Key, request.Resolution.StageKey)
	}
	record, err := observer.verifier.VerifyStageOperation(request.Resolution)
	if err != nil {
		return workflowkit.StageExecutionResult{}, false, fmt.Errorf("verify CodeEdge evaluator observation operation: %w", err)
	}
	payload, ok := record.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
	if !ok {
		return workflowkit.StageExecutionResult{}, false, fmt.Errorf("CodeEdge evaluator observation operation is not a local command")
	}
	if _, err := observer.attestor.AttestHarborEvaluatorOperation(ctx, stageprovider.DeploymentOperationRuntimeAttestation{
		CatalogReceipt:  observer.verifier.CatalogReceipt(),
		LockIdentity:    observer.verifier.LockIdentity(),
		HarborFlowBuild: observer.verifier.HarborFlowBuild(),
		Record:          record.Clone(),
		Resolution:      request.Resolution.Clone(),
	}); err != nil {
		return workflowkit.StageExecutionResult{}, false, fmt.Errorf("attest CodeEdge evaluator observation runtime: %w", err)
	}
	return observer.executor.ObserveCompletedHarborEvaluator(ctx, stageprovider.StageOperationInvocation{
		Request: request.Execution, Resolution: request.Resolution.Clone(),
	}, payload)
}
