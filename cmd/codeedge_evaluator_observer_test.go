package cmd

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCodeEdgeEvaluatorCompletedObserverVerifiesAttestsThenObserves(t *testing.T) {
	verifier, record := productionObserverVerifier(t)
	attestor := &recordingCodeEdgeObservationAttestor{}
	executor := &recordingCodeEdgeObservationExecutor{attestor: attestor, result: workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass},
	}}
	observer, err := newCodeEdgeEvaluatorCompletedObserver(verifier, attestor, executor)
	if err != nil {
		t.Fatal(err)
	}
	request := productionObserverRequest(record)
	result, observed, err := observer.ObserveCompletedCodeEdgeEvaluator(context.Background(), request)
	if err != nil || !observed || !reflect.DeepEqual(result, executor.result) {
		t.Fatalf("observe completed CodeEdge evaluator = %+v observed=%t err=%v", result, observed, err)
	}
	if attestor.calls != 1 || executor.calls != 1 {
		t.Fatalf("observer calls = attestor:%d executor:%d, want one each", attestor.calls, executor.calls)
	}
	if !reflect.DeepEqual(attestor.attestation.Record, record) || !reflect.DeepEqual(attestor.attestation.Resolution, request.Resolution) {
		t.Fatalf("observation attestation does not preserve locked record/resolution: %+v", attestor.attestation)
	}
	if attestor.attestation.CatalogReceipt != verifier.CatalogReceipt() || attestor.attestation.LockIdentity != verifier.LockIdentity() || attestor.attestation.HarborFlowBuild != verifier.HarborFlowBuild() {
		t.Fatal("observation attestation does not use the installed lock identity")
	}
	if executor.invocation.Request.Stage.Key != request.Execution.Stage.Key || !reflect.DeepEqual(executor.invocation.Resolution, request.Resolution) || executor.payload.CommandID != stageprovider.HarborEvaluatorQwenCommandID {
		t.Fatalf("read-only executor invocation = %+v payload=%+v", executor.invocation, executor.payload)
	}
}

func TestCodeEdgeEvaluatorCompletedObserverFailsClosedBeforeObservation(t *testing.T) {
	verifier, record := productionObserverVerifier(t)
	request := productionObserverRequest(record)

	t.Run("execution stage mismatch", func(t *testing.T) {
		attestor := &recordingCodeEdgeObservationAttestor{}
		executor := &recordingCodeEdgeObservationExecutor{attestor: attestor}
		observer, err := newCodeEdgeEvaluatorCompletedObserver(verifier, attestor, executor)
		if err != nil {
			t.Fatal(err)
		}
		mismatch := request
		mismatch.Execution.Stage.Key = workflowkit.StageKey(workflowadapter.HarborRunOpus)
		if _, observed, err := observer.ObserveCompletedCodeEdgeEvaluator(context.Background(), mismatch); err == nil || observed || attestor.calls != 0 || executor.calls != 0 {
			t.Fatalf("mismatched observation = observed=%t err=%v calls=%d/%d", observed, err, attestor.calls, executor.calls)
		}
	})

	t.Run("frozen operation drift", func(t *testing.T) {
		attestor := &recordingCodeEdgeObservationAttestor{}
		executor := &recordingCodeEdgeObservationExecutor{attestor: attestor}
		observer, err := newCodeEdgeEvaluatorCompletedObserver(verifier, attestor, executor)
		if err != nil {
			t.Fatal(err)
		}
		drifted := request
		drifted.Resolution = request.Resolution.Clone()
		drifted.Resolution.Operation.OperationID = "unapproved.operation"
		if _, observed, err := observer.ObserveCompletedCodeEdgeEvaluator(context.Background(), drifted); err == nil || observed || attestor.calls != 0 || executor.calls != 0 {
			t.Fatalf("drifted observation = observed=%t err=%v calls=%d/%d", observed, err, attestor.calls, executor.calls)
		}
	})

	t.Run("runtime attestation failure", func(t *testing.T) {
		attestor := &recordingCodeEdgeObservationAttestor{err: errors.New("attestation failed")}
		executor := &recordingCodeEdgeObservationExecutor{attestor: attestor}
		observer, err := newCodeEdgeEvaluatorCompletedObserver(verifier, attestor, executor)
		if err != nil {
			t.Fatal(err)
		}
		if _, observed, err := observer.ObserveCompletedCodeEdgeEvaluator(context.Background(), request); err == nil || observed || attestor.calls != 1 || executor.calls != 0 {
			t.Fatalf("unattested observation = observed=%t err=%v calls=%d/%d", observed, err, attestor.calls, executor.calls)
		}
	})
}

func productionObserverVerifier(t *testing.T) (*stageprovider.DeploymentOperationCatalogLockResolver, stageprovider.DeploymentOperationCatalogLockRecord) {
	t.Helper()
	catalogPath, lockPath := testCodeEdgeProductionDeploymentPaths(t)
	catalog, err := stageprovider.LoadDeploymentOperationCatalogFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	lockRaw, err := readCodeEdgeProductionFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range lock.Operations {
		if record.Stage.Key == workflowkit.StageKey(workflowadapter.HarborRunQwen) {
			return verifier, record
		}
	}
	t.Fatal("production lock omits the Qwen evaluator record")
	return nil, stageprovider.DeploymentOperationCatalogLockRecord{}
}

func productionObserverRequest(record stageprovider.DeploymentOperationCatalogLockRecord) app.CodeEdgeEvaluatorObservationRequest {
	return app.CodeEdgeEvaluatorObservationRequest{
		Execution:  workflowkit.StageExecutionRequest{Stage: workflowkit.StageDescriptor{Key: record.Stage.Key}},
		Resolution: productionAttestationResolution(record),
	}
}

type recordingCodeEdgeObservationAttestor struct {
	calls       int
	attestation stageprovider.DeploymentOperationRuntimeAttestation
	err         error
}

func (attestor *recordingCodeEdgeObservationAttestor) AttestHarborEvaluatorOperation(_ context.Context, attestation stageprovider.DeploymentOperationRuntimeAttestation) (stageprovider.HarborEvaluatorInvocation, error) {
	attestor.calls++
	attestor.attestation = attestation
	return stageprovider.HarborEvaluatorInvocation{}, attestor.err
}

type recordingCodeEdgeObservationExecutor struct {
	attestor   *recordingCodeEdgeObservationAttestor
	calls      int
	invocation stageprovider.StageOperationInvocation
	payload    workflowadapter.LocalCommandOperationPayload
	result     workflowkit.StageExecutionResult
}

func (executor *recordingCodeEdgeObservationExecutor) ObserveCompletedHarborEvaluator(_ context.Context, invocation stageprovider.StageOperationInvocation, payload workflowadapter.LocalCommandOperationPayload) (workflowkit.StageExecutionResult, bool, error) {
	executor.calls++
	if executor.attestor == nil || executor.attestor.calls == 0 {
		return workflowkit.StageExecutionResult{}, false, errors.New("observation ran before runtime attestation")
	}
	executor.invocation = invocation
	executor.payload = payload
	return executor.result, true, nil
}
