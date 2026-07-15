package stageprovider

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

// CodeEdgePhase1RuntimeAttestorConfig contains only the linked Harbor Flow
// build identity. All executable paths, content hashes, review policies and
// built-in handler identities are read from the immutable parent deployment
// lock at effect time.
type CodeEdgePhase1RuntimeAttestorConfig struct {
	HarborFlowBuild HarborFlowBuildIdentity
}

// CodeEdgePhase1RuntimeAttestor is the narrowly scoped runtime proof for the
// CodeEdge Phase-1 parent. The parent permits locked local commands for its
// controlled Harbor/Docker work and linked harbor.builtin handlers for local
// deterministic checks. It deliberately does not accept evaluator, arbitrary
// container, or agent-turn payloads: Qwen/Opus belong only to the separately
// locked evaluator child and Standard authoring owns Codex turns.
type CodeEdgePhase1RuntimeAttestor struct {
	harborFlowBuild HarborFlowBuildIdentity
	local           *LocalFilesystemRuntimeAttestor
}

// NewCodeEdgePhase1RuntimeAttestor constructs a closed parent attestor.
func NewCodeEdgePhase1RuntimeAttestor(config CodeEdgePhase1RuntimeAttestorConfig) (*CodeEdgePhase1RuntimeAttestor, error) {
	if err := config.HarborFlowBuild.Validate(); err != nil {
		return nil, fmt.Errorf("%w: configured Harbor Flow build identity is invalid: %w", ErrDeploymentOperationRuntimeAttestationFailed, err)
	}
	local, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{HarborFlowBuild: config.HarborFlowBuild})
	if err != nil {
		return nil, err
	}
	return &CodeEdgePhase1RuntimeAttestor{harborFlowBuild: config.HarborFlowBuild, local: local}, nil
}

// HarborFlowBuild returns the immutable linked build identity used for every
// parent operation proof.
func (attestor *CodeEdgePhase1RuntimeAttestor) HarborFlowBuild() HarborFlowBuildIdentity {
	if attestor == nil {
		return HarborFlowBuildIdentity{}
	}
	return attestor.harborFlowBuild
}

// AttestDeploymentOperation rechecks the record, frozen resolution and
// linked-build identity immediately before the delegated operation executes.
// It never discovers a command through PATH or accepts a child evaluator
// operation under the parent template.
func (attestor *CodeEdgePhase1RuntimeAttestor) AttestDeploymentOperation(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) error {
	if attestor == nil || attestor.local == nil {
		return ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return err
	}
	if err := validateLocalFilesystemRuntimeAttestation(attestation); err != nil {
		return err
	}
	if !attestation.CatalogReceipt.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return fmt.Errorf("%w: CodeEdge Phase-1 attestor received another template", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if attestation.HarborFlowBuild != attestor.harborFlowBuild {
		return fmt.Errorf("%w: Harbor Flow build identity does not match the installed parent deployment", ErrDeploymentOperationRuntimeAttestationFailed)
	}

	switch payload := attestation.Record.Operation.Payload.(type) {
	case workflowadapter.LocalCommandOperationPayload:
		return attestor.local.AttestDeploymentOperation(ctx, attestation)
	case workflowadapter.HarborBuiltinOperationPayload:
		if attestation.Record.HarborFlowBuiltin == nil || attestation.Record.HarborFlowBuiltin.HandlerID != payload.HandlerID {
			return fmt.Errorf("%w: CodeEdge Phase-1 built-in handler lock is inconsistent", ErrDeploymentOperationRuntimeAttestationFailed)
		}
		return nil
	case workflowadapter.DurableReviewOperationPayload:
		if attestation.Record.DurableReviewPolicy == nil || attestation.Record.DurableReviewPolicy.PolicyID != payload.PolicyID {
			return fmt.Errorf("%w: CodeEdge Phase-1 durable review policy lock is inconsistent", ErrDeploymentOperationRuntimeAttestationFailed)
		}
		return nil
	case workflowadapter.AgentTurnOperationPayload,
		workflowadapter.ContainerCommandOperationPayload:
		return fmt.Errorf("%w: CodeEdge Phase-1 parent does not authorize %s", ErrDeploymentOperationRuntimeAttestationUnavailable, attestation.Record.ExecutionKind)
	default:
		return fmt.Errorf("%w: unsupported CodeEdge Phase-1 operation payload", ErrDeploymentOperationRuntimeAttestationUnavailable)
	}
}

var _ DeploymentOperationRuntimeAttestor = (*CodeEdgePhase1RuntimeAttestor)(nil)
