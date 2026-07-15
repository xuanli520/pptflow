package workflowadapter

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// CodeEdgeEvaluatorChildWorkflowTemplateID and Version identify the
	// independently executable child Run used only for the approved Qwen then
	// Opus pass@4 evaluations of one immutable task snapshot. It intentionally
	// contains neither authoring/preflight stages nor a package/final-compliance
	// stage: callers import its verified evidence into the owning CodeEdge Run.
	CodeEdgeEvaluatorChildWorkflowTemplateID      = "harbor.codeedge-evaluator"
	CodeEdgeEvaluatorChildWorkflowTemplateVersion = "1.0.0"

	codeEdgeEvaluatorChildCatalogID      = "harbor.codeedge-evaluator-stage-catalog"
	codeEdgeEvaluatorChildCatalogVersion = "1.0.0"

	// CodeEdgeEvaluatorTaskSnapshotArtifact is the sole caller-supplied input.
	// It is bound to both evaluator stages and must identify the same immutable
	// managed snapshot; a mutable workspace or a package is never an input.
	CodeEdgeEvaluatorTaskSnapshotArtifact      = "task_snapshot"
	CodeEdgeEvaluatorTaskSnapshotSchemaVersion = "harbor.artifact.v1"
	CodeEdgeEvaluatorQwenBundleArtifact        = "qwen_trial_result"
	CodeEdgeEvaluatorQwenScreenshotArtifact    = "qwen_pass4_evidence"
	CodeEdgeEvaluatorOpusBundleArtifact        = "opus_trial_result"
	CodeEdgeEvaluatorOpusScreenshotArtifact    = "opus_pass4_evidence"
	CodeEdgeEvaluatorScreenshotSchemaVersion   = "image/png"
)

var codeEdgeEvaluatorChildStageOrder = []workflowkit.StageKey{
	workflowkit.StageKey(HarborRunQwen),
	workflowkit.StageKey(HarborRunOpus),
}

var codeEdgeEvaluatorChildGroups = []StageGroup{StageEvaluation}

// CodeEdgeEvaluatorChildTemplateReference returns the exact identity for the
// closed child evaluator workflow. This is a child Run contract, never an
// alias for the complete CodeEdge Phase-1 template.
func CodeEdgeEvaluatorChildTemplateReference() TemplateReference {
	return TemplateReference{ID: CodeEdgeEvaluatorChildWorkflowTemplateID, Version: CodeEdgeEvaluatorChildWorkflowTemplateVersion}
}

// IsCodeEdgeEvaluatorWorkflowTemplate reports whether a closed template owns
// the CodeEdge four-trial evaluator protocol. The parent template only adopts
// evidence through a durable handoff and must never enter evaluator fencing,
// recovery, or provider execution paths itself.
func IsCodeEdgeEvaluatorWorkflowTemplate(reference TemplateReference) bool {
	return reference.Equal(CodeEdgeEvaluatorChildTemplateReference())
}

// CodeEdgeEvaluatorChildStageOrder returns the immutable serial declaration
// order. The DAG is authoritative: Opus depends on completed Qwen evaluation
// so a worker cannot schedule the two external model calls concurrently.
func CodeEdgeEvaluatorChildStageOrder() []workflowkit.StageKey {
	return append([]workflowkit.StageKey(nil), codeEdgeEvaluatorChildStageOrder...)
}

func codeEdgeEvaluatorChildStageGroups() []StageGroup {
	return append([]StageGroup(nil), codeEdgeEvaluatorChildGroups...)
}

func codeEdgeEvaluatorChildDependencies() map[workflowkit.StageKey][]workflowkit.StageKey {
	return map[workflowkit.StageKey][]workflowkit.StageKey{
		workflowkit.StageKey(HarborRunQwen): nil,
		workflowkit.StageKey(HarborRunOpus): {workflowkit.StageKey(HarborRunQwen)},
	}
}

// CodeEdgeEvaluatorChildWorkflowTemplate returns the narrow production child
// descriptor for an explicitly confirmed evaluation Run. It carries no
// package or compliance authority: its two canonical Harbor bundles and PNG
// screenshots are evidence for the owning CodeEdge package/compliance flow.
func CodeEdgeEvaluatorChildWorkflowTemplate() WorkflowTemplate {
	return WorkflowTemplate{
		ID:          CodeEdgeEvaluatorChildWorkflowTemplateID,
		Version:     CodeEdgeEvaluatorChildWorkflowTemplateVersion,
		Catalog:     CodeEdgeEvaluatorChildStageCatalog(),
		QuotaPolicy: CodeEdgeEvaluatorChildQuotaPolicy(),
	}
}

// CodeEdgeEvaluatorChildStageCatalog declares the two fixed external effects.
// Generic retry and reusable-output behavior are deliberately disabled: each
// stage owns four preallocated logical trials and technical recovery must stay
// within those same TrialExecution identities.
func CodeEdgeEvaluatorChildStageCatalog() StageCatalog {
	return StageCatalog{
		Template: CodeEdgeEvaluatorChildTemplateReference(),
		ID:       codeEdgeEvaluatorChildCatalogID,
		Version:  codeEdgeEvaluatorChildCatalogVersion,
		Stages:   codeEdgeEvaluatorChildStageDefinitions(),
	}
}

func codeEdgeEvaluatorChildStageDefinitions() []StageDefinition {
	return []StageDefinition{
		codeEdgeEvaluatorChildStage(
			HarborRunQwen,
			nil,
			"harborfactory.harbor_run_qwen",
			[]workflowkit.ResourceKey{resourceTaskSnapshot},
			[]workflowkit.ResourceKey{resourceEvidenceEvaluationQwen},
			artifactInputWithSchema(CodeEdgeEvaluatorTaskSnapshotArtifact, CodeEdgeEvaluatorTaskSnapshotSchemaVersion),
			artifactOutputWithSchema(CodeEdgeEvaluatorQwenBundleArtifact, codeedge.HarborRunBundleV018Format),
			artifactOutputWithSchema(CodeEdgeEvaluatorQwenScreenshotArtifact, CodeEdgeEvaluatorScreenshotSchemaVersion),
		),
		codeEdgeEvaluatorChildStage(
			HarborRunOpus,
			[]string{HarborRunQwen},
			"harborfactory.harbor_run_opus",
			[]workflowkit.ResourceKey{resourceTaskSnapshot},
			[]workflowkit.ResourceKey{resourceEvidenceEvaluationOpus},
			artifactInputWithSchema(CodeEdgeEvaluatorTaskSnapshotArtifact, CodeEdgeEvaluatorTaskSnapshotSchemaVersion),
			artifactOutputWithSchema(CodeEdgeEvaluatorOpusBundleArtifact, codeedge.HarborRunBundleV018Format),
			artifactOutputWithSchema(CodeEdgeEvaluatorOpusScreenshotArtifact, CodeEdgeEvaluatorScreenshotSchemaVersion),
		),
	}
}

// codeEdgeEvaluatorChildStage marks an evaluator as an external side effect
// and deliberately disables generic stage retries. One invocation creates the
// confirmed four logical samples, so retrying the whole stage on a transient
// failure could produce a second, incomparable group of four. The durable
// TrialExecution reconciler owns technical retries beneath the same logical
// trial identity instead.
func codeEdgeEvaluatorChildStage(key string, dependencies []string, pluginID string, reads, writes []workflowkit.ResourceKey, artifacts ...stageArtifact) StageDefinition {
	definition := stage(key, StageEvaluation, dependencies, pluginID, reads, writes, workflowkit.EffectExternalSideEffect, 1, evaluationVerdicts(), artifacts...)
	definition.Retry = workflowkit.RetryPolicy{}
	definition.Reuse = workflowkit.ReuseNever
	return definition
}

func (spec RunExecutionSpec) validateCodeEdgeEvaluatorChildBindings() error {
	if !spec.Template.Equal(CodeEdgeEvaluatorChildTemplateReference()) {
		return nil
	}

	qwenSnapshot, err := spec.codeEdgeEvaluatorChildTaskSnapshot(HarborRunQwen)
	if err != nil {
		return err
	}
	opusSnapshot, err := spec.codeEdgeEvaluatorChildTaskSnapshot(HarborRunOpus)
	if err != nil {
		return err
	}
	if qwenSnapshot != opusSnapshot {
		return fmt.Errorf("%w: CodeEdge evaluator child Qwen and Opus must bind the same task_snapshot artifact", errInvalidExecutionSpec)
	}
	return nil
}

func (spec RunExecutionSpec) codeEdgeEvaluatorChildTaskSnapshot(stageKey workflowkit.StageKey) (workflowkit.ArtifactID, error) {
	binding, present := spec.StageBinding(stageKey)
	if !present {
		return "", fmt.Errorf("%w: CodeEdge evaluator child is missing %s binding", errInvalidExecutionSpec, stageKey)
	}
	base, ok := stageBindingBaseOf(binding)
	if !ok {
		return "", fmt.Errorf("%w: CodeEdge evaluator child %s binding is unsupported", errInvalidExecutionSpec, stageKey)
	}
	var snapshot workflowkit.ArtifactID
	for _, input := range base.ArtifactInputs {
		if input.Port != CodeEdgeEvaluatorTaskSnapshotArtifact {
			continue
		}
		if snapshot != "" {
			return "", fmt.Errorf("%w: CodeEdge evaluator child %s binds task_snapshot more than once", errInvalidExecutionSpec, stageKey)
		}
		snapshot = input.ArtifactID
	}
	if snapshot == "" {
		return "", fmt.Errorf("%w: CodeEdge evaluator child %s requires a managed task_snapshot artifact", errInvalidExecutionSpec, stageKey)
	}
	return snapshot, nil
}
