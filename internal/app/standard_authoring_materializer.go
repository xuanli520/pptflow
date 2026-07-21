package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringMaterializeHandlerID      = "standard-authoring.materialize-task"
	standardAuthoringPackageAdmissionHandlerID = "standard-authoring.codeedge-package-admission"
	standardAuthoringAdmissionReceiptFormat    = "harbor.standard-authoring-task-package-admission.v1"
	standardAuthoringAdmissionReceiptVersion   = "1"
)

// StandardAuthoringMaterializeExecutor is the one Harbor-built-in operation
// permitted to turn an AuthoringSource/AuthoringSession result into a sealed
// first TaskRevision.  It deliberately owns no external command, model,
// endpoint, credential, or mutable workspace capability: its only inputs are
// the exact six frozen stage artifacts plus the approved durable review
// decision.
//
// It is constructed from the same managed root and Store as the application
// service, but independently of a RunService so a deployment composition can
// install the handler before it admits the first Standard authoring Run.
// Starting the separate CodeEdge Phase-1 Run is intentionally a later
// durable handoff after this stage output has been persisted; the handler
// never executes task-bound work under the source/session subject.
type StandardAuthoringMaterializeExecutor struct {
	core      *lifecycleServiceCore
	admission *codeedge.TaskAdmissionContract
}

// StandardAuthoringMaterializeExecutorConfig contains only local
// control-plane capabilities.  ManagedRoot is a local Harbor Flow root, not a
// caller-provided task workspace; Store is the durable V2 control plane.
type StandardAuthoringMaterializeExecutorConfig struct {
	ManagedRoot string
	Store       *store.Store
	Now         func() time.Time
	// Admission is required by the 1.3 Authoring template. It is optional
	// here solely so frozen 1.2 Runs can remain executable under their old
	// materialization contract.
	Admission *codeedge.TaskAdmissionContract
}

// NewStandardAuthoringMaterializeExecutor constructs the built-in materialize
// handler without installing an execution provider or an operation resolver.
// Catalog/lock/build checks remain owned by stageprovider's outer resolver.
func NewStandardAuthoringMaterializeExecutor(config StandardAuthoringMaterializeExecutorConfig) (*StandardAuthoringMaterializeExecutor, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("Standard authoring materializer Store is required")
	}
	layout, err := newManagedLayout(config.ManagedRoot)
	if err != nil {
		return nil, err
	}
	objects, err := workflowruntime.NewArtifactObjectStore(filepath.Join(layout.root, "objects"))
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring materializer object store: %w", err)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	var admission *codeedge.TaskAdmissionContract
	if config.Admission != nil {
		if err := config.Admission.Validate(); err != nil {
			return nil, fmt.Errorf("validate Standard authoring CodeEdge admission contract: %w", err)
		}
		copy := *config.Admission
		admission = &copy
	}
	return &StandardAuthoringMaterializeExecutor{core: &lifecycleServiceCore{store: config.Store, layout: layout, objects: objects, now: now}, admission: admission}, nil
}

// ExecuteHarborBuiltin implements the sealed stageprovider built-in contract.
// It reserves the task_snapshot artifact ID before it emits the handoff, so
// the handoff can name the exact output which the durable stage projection
// will later create.  All other output IDs remain backend-owned.
func (executor *StandardAuthoringMaterializeExecutor) ExecuteHarborBuiltin(ctx context.Context, invocation stageprovider.StageOperationInvocation, payload workflowadapter.HarborBuiltinOperationPayload) (workflowkit.StageExecutionResult, error) {
	if executor == nil || executor.core == nil || executor.core.store == nil || executor.core.objects == nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring materializer is not configured")
	}
	if ctx == nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring materializer context is required")
	}
	if err := ctx.Err(); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if payload.HandlerID == standardAuthoringPackageAdmissionHandlerID {
		return executor.executePackageAdmission(ctx, invocation)
	}
	if payload.HandlerID != standardAuthoringMaterializeHandlerID || invocation.Resolution.StageKey != workflowkit.StageKey(workflowadapter.MaterializeTask) || invocation.Request.Stage.Key != workflowkit.StageKey(workflowadapter.MaterializeTask) {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring materializer received an unbound built-in operation")
	}
	if invocation.Request.Execution.ID == "" || invocation.Request.Claim.Stage == nil || invocation.Request.Claim.Stage.StageAttempt.ID == "" {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring materializer requires a frozen Run and stage attempt")
	}

	run, err := executor.core.store.GetWorkflowRun(ctx, invocation.Request.Execution.ID)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if run == nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: authoring Run %s", ErrLifecycleNotFound, invocation.Request.Execution.ID)
	}
	subject, err := executor.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if !isCurrentStandardAuthoringRun(*run) || !subject.isAuthoringSession() || subject.Binding != invocation.Request.Execution.Subject {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring materializer Run is not a frozen source/session subject")
	}
	if invocation.Request.Claim.Stage.StageAttempt.ID == "" {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring materializer stage attempt is required")
	}
	attempt, err := executor.core.store.GetStageAttempt(ctx, string(invocation.Request.Claim.Stage.StageAttempt.ID))
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if attempt == nil || attempt.RunID != run.ID || attempt.StageKey != workflowadapter.MaterializeTask || attempt.ExecutionStatus != store.StageExecutionRunning {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring materializer stage attempt does not match the active frozen Run")
	}

	inputs, err := standardAuthoringMaterializeInputs(ctx, invocation.Request, *run, subject, executor.admission)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}

	// A prior atomic materialization is authoritative after a crash.  Rebuild
	// the deterministic stage outputs from that immutable revision instead of
	// creating a second revision or trusting leftover temporary files.
	materialization, err := executor.core.store.GetAuthoringTaskMaterializationForRun(ctx, run.ID)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	var revision store.TaskRevision
	if materialization != nil {
		if materialization.SessionID != subject.AuthoringSession.ID || materialization.SourceID != subject.AuthoringSource.ID || materialization.TaskID != subject.TargetTask.ID {
			return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: committed Standard authoring materialization has inconsistent subject lineage", store.ErrImmutable)
		}
		loaded, loadErr := executor.core.store.GetTaskRevision(ctx, materialization.RevisionID)
		if loadErr != nil {
			return workflowkit.StageExecutionResult{}, loadErr
		}
		if loaded == nil || loaded.TaskID != materialization.TaskID || loaded.TaskDigest != materialization.TaskDigest || loaded.State != store.RevisionStateSealed {
			return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: committed Standard authoring materialization revision", store.ErrImmutable)
		}
		revision = *loaded
	} else {
		revision, err = executor.materializeNewAuthoringTask(ctx, invocation.Request, *run, subject, inputs)
		if err != nil {
			return workflowkit.StageExecutionResult{}, err
		}
	}
	admissionReceipt, err := standardAuthoringAdmissionReceiptReference(invocation.Request, *run)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	return executor.materializationResult(ctx, *run, subject, revision, admissionReceipt)
}

type standardAuthoringAdmissionReceipt struct {
	Format             string                   `json:"format"`
	Version            string                   `json:"version"`
	RunID              string                   `json:"run_id"`
	AuthoringSourceID  string                   `json:"authoring_source_id"`
	AuthoringSessionID string                   `json:"authoring_session_id"`
	InputFingerprint   workflowkit.Fingerprint  `json:"input_fingerprint"`
	Report             codeedge.AdmissionReport `json:"report"`
}

// executePackageAdmission turns deterministic package violations into the
// normal content-repair verdict before a human can approve materialization.
func (executor *StandardAuthoringMaterializeExecutor) executePackageAdmission(ctx context.Context, invocation stageprovider.StageOperationInvocation) (workflowkit.StageExecutionResult, error) {
	if executor.admission == nil || invocation.Resolution.StageKey != workflowkit.StageKey(workflowadapter.CodeEdgePackageAdmission) || invocation.Request.Stage.Key != workflowkit.StageKey(workflowadapter.CodeEdgePackageAdmission) {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring package admission received an unbound built-in operation")
	}
	resolved, ok := invocation.Resolution.Operation.Payload.(workflowadapter.HarborBuiltinOperationPayload)
	if !ok || resolved.HandlerID != standardAuthoringPackageAdmissionHandlerID {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring package admission payload is not frozen")
	}
	if invocation.Request.Execution.ID == "" || invocation.Request.Claim.Stage == nil || invocation.Request.Claim.Stage.StageAttempt.ID == "" {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring package admission requires a frozen Run and stage attempt")
	}
	run, err := executor.core.store.GetWorkflowRun(ctx, invocation.Request.Execution.ID)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if run == nil || !isAdmissionAwareStandardAuthoringRun(*run) || run.Status != store.WorkflowRunRunning {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring package admission Run is not active under the admission template")
	}
	subject, err := executor.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if !subject.isAuthoringSession() || subject.Binding != invocation.Request.Execution.Subject || subject.AuthoringSource == nil || subject.AuthoringSession == nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring package admission Run has an invalid source/session subject")
	}
	inputs, err := standardAuthoringPackageAdmissionInputs(ctx, invocation.Request, *run)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	compiled, err := CompileStandardAuthoringTaskPackage(StandardAuthoringTaskPackageInput{
		Instruction: inputs.instruction, TaskTOMLDraft: inputs.taskTOML, Dockerfile: inputs.dockerfile,
		SolveScript: inputs.solveScript, TestScript: inputs.testScript, TestsAnalysis: inputs.testsAnalysis,
		Source: *subject.AuthoringSource, Environment: inputs.environment, Brief: inputs.brief, Admission: *executor.admission,
	})
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("compile Standard authoring task package admission: %w", err)
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(invocation.Request.Inputs)
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("fingerprint Standard authoring admission inputs: %w", err)
	}
	receipt := standardAuthoringAdmissionReceipt{Format: standardAuthoringAdmissionReceiptFormat, Version: standardAuthoringAdmissionReceiptVersion,
		RunID: run.ID, AuthoringSourceID: subject.AuthoringSource.ID, AuthoringSessionID: subject.AuthoringSession.ID,
		InputFingerprint: inputFingerprint, Report: compiled.Report}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("encode Standard authoring admission receipt: %w", err)
	}
	verdict := workflowkit.VerdictPass
	if !compiled.Report.Passed {
		verdict = workflowkit.VerdictNeedsRepair
	}
	return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: verdict}, Artifacts: []workflowkit.StageArtifact{{Name: "codeedge_package_admission_report", SchemaVersion: standardAuthoringAdmissionReceiptFormat, Content: encoded}}}, nil
}

type standardAuthoringPackageAdmissionInputSet struct {
	instruction, taskTOML, dockerfile, solveScript, testScript, testsAnalysis []byte
	dockerBuildReport, harnessReport                                          []byte
	environment                                                               workflowadapter.StandardAuthoringEnvironmentPolicy
	brief                                                                     *workflowadapter.StandardAuthoringBrief
}

func standardAuthoringPackageAdmissionInputs(ctx context.Context, request workflowkit.StageExecutionRequest, run store.WorkflowRun) (standardAuthoringPackageAdmissionInputSet, error) {
	if request.ReadInput == nil {
		return standardAuthoringPackageAdmissionInputSet{}, fmt.Errorf("Standard authoring package admission has no frozen input reader")
	}
	template, err := workflowadapter.ResolveWorkflowTemplate(workflowadapter.TemplateReference{ID: run.WorkflowTemplateID, Version: run.WorkflowTemplateVersion})
	if err != nil {
		return standardAuthoringPackageAdmissionInputSet{}, err
	}
	stage, found := template.Catalog.Stage(workflowkit.StageKey(workflowadapter.CodeEdgePackageAdmission))
	if !found || !reflect.DeepEqual(stage.Inputs, request.Stage.Inputs) {
		return standardAuthoringPackageAdmissionInputSet{}, fmt.Errorf("Standard authoring package admission stage does not match the frozen input contract")
	}
	bindings := make(map[string]workflowkit.ArtifactBinding, len(request.Inputs))
	for _, binding := range request.Inputs {
		if _, duplicate := bindings[binding.Name]; duplicate {
			return standardAuthoringPackageAdmissionInputSet{}, fmt.Errorf("Standard authoring package admission received duplicate input %q", binding.Name)
		}
		bindings[binding.Name] = binding
	}
	read := func(name string) ([]byte, error) {
		binding, found := bindings[name]
		if !found {
			return nil, fmt.Errorf("Standard authoring package admission lacks input %q", name)
		}
		return request.ReadInput(ctx, binding)
	}
	result := standardAuthoringPackageAdmissionInputSet{}
	if result.instruction, err = read("instruction"); err != nil {
		return result, err
	}
	if result.taskTOML, err = read("task_toml"); err != nil {
		return result, err
	}
	dockerfileName, solveName, testName := standardAuthoringPackageArtifactNames(run)
	if result.dockerfile, err = read(dockerfileName); err != nil {
		return result, err
	}
	if result.solveScript, err = read(solveName); err != nil {
		return result, err
	}
	if result.testScript, err = read(testName); err != nil {
		return result, err
	}
	if result.testsAnalysis, err = read("tests_analysis"); err != nil {
		return result, err
	}
	policyRaw, err := read(workflowadapter.StandardAuthoringEnvironmentPolicyArtifact)
	if err != nil {
		return result, err
	}
	result.environment, err = workflowadapter.ParseStandardAuthoringEnvironmentPolicyJSON(policyRaw)
	if err != nil {
		return result, fmt.Errorf("decode frozen Standard authoring environment policy: %w", err)
	}
	if isBriefAwareStandardAuthoringRun(run) {
		binding, found := bindings[workflowadapter.StandardAuthoringBriefArtifact]
		if !found || binding.SchemaVersion != workflowadapter.StandardAuthoringBriefSchemaVersion {
			return result, fmt.Errorf("Standard authoring package admission brief does not match the frozen contract")
		}
		briefRaw, readErr := read(workflowadapter.StandardAuthoringBriefArtifact)
		if readErr != nil {
			return result, readErr
		}
		brief, parseErr := workflowadapter.ParseStandardAuthoringBriefJSON(briefRaw)
		if parseErr != nil {
			return result, fmt.Errorf("decode frozen Standard authoring brief: %w", parseErr)
		}
		result.brief = &brief
	}
	if isHarnessAwareStandardAuthoringRun(run) {
		if result.dockerBuildReport, err = read(workflowadapter.StandardAuthoringDockerfileBuildReportArtifact); err != nil {
			return result, err
		}
		if result.harnessReport, err = read(workflowadapter.StandardAuthoringHarnessReportArtifact); err != nil {
			return result, err
		}
		if err := validateStandardAuthoringHarnessEvidence(run.ID, result.dockerfile, result.solveScript, result.testScript, result.dockerBuildReport, result.harnessReport); err != nil {
			return result, err
		}
	}
	return result, nil
}

type standardAuthoringMaterializeInputSet struct {
	instruction       []byte
	taskTOML          []byte
	dockerfile        []byte
	solveScript       []byte
	testScript        []byte
	testsAnalysis     []byte
	admissionReceipt  []byte
	dockerBuildReport []byte
	harnessReport     []byte
	environment       workflowadapter.StandardAuthoringEnvironmentPolicy
	brief             *workflowadapter.StandardAuthoringBrief
	proposalHash      string
}

func standardAuthoringMaterializeInputs(ctx context.Context, request workflowkit.StageExecutionRequest, run store.WorkflowRun, subject workflowRunSubject, admission *codeedge.TaskAdmissionContract) (standardAuthoringMaterializeInputSet, error) {
	if !isCurrentStandardAuthoringRun(run) {
		return standardAuthoringMaterializeInputSet{}, fmt.Errorf("Standard authoring materializer Run is not bound to the current template")
	}
	if request.ReadInput == nil {
		return standardAuthoringMaterializeInputSet{}, fmt.Errorf("Standard authoring materializer has no frozen input reader")
	}
	expected, contractErr := standardAuthoringMaterializeInputContract(request.Stage, run)
	if contractErr != nil {
		return standardAuthoringMaterializeInputSet{}, contractErr
	}
	bindings := make(map[string]workflowkit.ArtifactBinding, len(request.Inputs))
	for _, input := range request.Inputs {
		if _, duplicate := bindings[input.Name]; duplicate {
			return standardAuthoringMaterializeInputSet{}, fmt.Errorf("Standard authoring materializer received duplicate input %q", input.Name)
		}
		if _, expectedInput := expected[input.Name]; !expectedInput {
			return standardAuthoringMaterializeInputSet{}, fmt.Errorf("Standard authoring materializer received undeclared input %q", input.Name)
		}
		bindings[input.Name] = input
	}
	read := func(name string) ([]byte, error) {
		binding, found := bindings[name]
		if !found || binding.SchemaVersion != expected[name] {
			return nil, fmt.Errorf("Standard authoring materializer input %q does not match the frozen contract", name)
		}
		bytes, readErr := request.ReadInput(ctx, binding)
		if readErr != nil {
			return nil, fmt.Errorf("read frozen Standard authoring input %q: %w", name, readErr)
		}
		return append([]byte(nil), bytes...), nil
	}
	result := standardAuthoringMaterializeInputSet{}
	var err error
	if result.instruction, err = read("instruction"); err != nil {
		return result, err
	}
	if result.taskTOML, err = read("task_toml"); err != nil {
		return result, err
	}
	dockerfileName, solveName, testName := standardAuthoringPackageArtifactNames(run)
	if result.dockerfile, err = read(dockerfileName); err != nil {
		return result, err
	}
	policyRaw, err := read(workflowadapter.StandardAuthoringEnvironmentPolicyArtifact)
	if err != nil {
		return result, err
	}
	environmentPolicy, err := workflowadapter.ParseStandardAuthoringEnvironmentPolicyJSON(policyRaw)
	if err != nil {
		return result, fmt.Errorf("decode frozen Standard authoring environment policy: %w", err)
	}
	if err := environmentPolicy.ValidateDockerfile(result.dockerfile); err != nil {
		return result, fmt.Errorf("validate frozen Standard authoring Dockerfile base image: %w", err)
	}
	result.environment = environmentPolicy
	if isBriefAwareStandardAuthoringRun(run) {
		if admission == nil {
			return result, fmt.Errorf("Standard authoring admission contract is unavailable for the frozen brief template")
		}
		briefRaw, readErr := read(workflowadapter.StandardAuthoringBriefArtifact)
		if readErr != nil {
			return result, readErr
		}
		brief, parseErr := workflowadapter.ParseStandardAuthoringBriefJSON(briefRaw)
		if parseErr != nil {
			return result, fmt.Errorf("decode frozen Standard authoring brief: %w", parseErr)
		}
		violations, validateErr := validateStandardAuthoringTaskTOMLBrief(result.taskTOML, admission.Profile.Metadata, brief)
		if validateErr != nil {
			return result, fmt.Errorf("validate generated task.toml against frozen Standard authoring brief: %w", validateErr)
		}
		if len(violations) != 0 {
			return result, fmt.Errorf("CodeEdge task admission rejected materialization: %s", admissionViolationSummary(violations))
		}
		result.brief = &brief
	}
	if result.solveScript, err = read(solveName); err != nil {
		return result, err
	}
	if result.testScript, err = read(testName); err != nil {
		return result, err
	}
	if result.testsAnalysis, err = read("tests_analysis"); err != nil {
		return result, err
	}
	if isHarnessAwareStandardAuthoringRun(run) {
		if result.dockerBuildReport, err = read(workflowadapter.StandardAuthoringDockerfileBuildReportArtifact); err != nil {
			return result, err
		}
		if result.harnessReport, err = read(workflowadapter.StandardAuthoringHarnessReportArtifact); err != nil {
			return result, err
		}
		if err := validateStandardAuthoringHarnessEvidence(run.ID, result.dockerfile, result.solveScript, result.testScript, result.dockerBuildReport, result.harnessReport); err != nil {
			return result, err
		}
	}
	decision, err := read("solution_review_decision")
	if err != nil {
		return result, err
	}
	if err := validateApprovedAuthoringSolutionDecision(decision, run, subject); err != nil {
		return result, err
	}
	if isAdmissionAwareStandardAuthoringRun(run) {
		if result.admissionReceipt, err = read("codeedge_package_admission_report"); err != nil {
			return result, err
		}
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(request.Inputs)
	if err != nil {
		return result, fmt.Errorf("fingerprint Standard authoring materialization inputs: %w", err)
	}
	result.proposalHash = string(inputFingerprint)
	return result, nil
}

func standardAuthoringMaterializeInputContract(stage workflowkit.StageDescriptor, run store.WorkflowRun) (map[string]string, error) {
	if stage.Key != workflowkit.StageKey(workflowadapter.MaterializeTask) {
		return nil, fmt.Errorf("Standard authoring materializer stage is not materialize_task")
	}
	templateReference := workflowadapter.TemplateReference{ID: run.WorkflowTemplateID, Version: run.WorkflowTemplateVersion}
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(templateReference) {
		return nil, fmt.Errorf("Standard authoring materializer Run is not bound to a registered template")
	}
	template, err := workflowadapter.ResolveWorkflowTemplate(templateReference)
	if err != nil {
		return nil, fmt.Errorf("resolve Standard authoring materializer template: %w", err)
	}
	current, found := template.Catalog.Stage(workflowkit.StageKey(workflowadapter.MaterializeTask))
	if !found {
		return nil, fmt.Errorf("current Standard authoring catalog omits materialize_task")
	}
	expected := make(map[string]string, len(current.Inputs))
	for _, input := range current.Inputs {
		if !input.Required || input.SchemaVersion == "" {
			return nil, fmt.Errorf("current Standard authoring materialize_task input contract is invalid")
		}
		if _, duplicate := expected[input.Name]; duplicate {
			return nil, fmt.Errorf("current Standard authoring materialize_task repeats input %q", input.Name)
		}
		expected[input.Name] = input.SchemaVersion
	}
	if len(stage.Inputs) != len(expected) {
		return nil, fmt.Errorf("Standard authoring materializer stage does not match the current input contract")
	}
	seen := make(map[string]struct{}, len(stage.Inputs))
	for _, input := range stage.Inputs {
		expectedSchema, requiredInput := expected[input.Name]
		if _, duplicate := seen[input.Name]; duplicate || !requiredInput || !input.Required || input.SchemaVersion != expectedSchema {
			return nil, fmt.Errorf("Standard authoring materializer stage does not match the current input contract")
		}
		seen[input.Name] = struct{}{}
	}
	if expected[workflowadapter.StandardAuthoringEnvironmentPolicyArtifact] != workflowadapter.StandardAuthoringEnvironmentPolicySchemaVersion {
		return nil, fmt.Errorf("current Standard authoring environment policy schema is invalid")
	}
	if isBriefAwareStandardAuthoringRun(run) && expected[workflowadapter.StandardAuthoringBriefArtifact] != workflowadapter.StandardAuthoringBriefSchemaVersion {
		return nil, fmt.Errorf("current Standard authoring brief schema is invalid")
	}
	return expected, nil
}

func validateApprovedAuthoringSolutionDecision(raw []byte, run store.WorkflowRun, subject workflowRunSubject) error {
	var decision authoringReviewGateDecisionArtifact
	if err := decodeStrictJSON(string(raw), &decision); err != nil {
		return fmt.Errorf("decode Standard authoring solution review decision: %w", err)
	}
	if decision.Format != authoringReviewGateDecisionArtifactFormat || decision.Action != store.ReviewDecisionApprove ||
		decision.AuthoringSourceID != subject.AuthoringSource.ID || decision.AuthoringSessionID != subject.AuthoringSession.ID ||
		decision.SourceSnapshotDigest != subject.subjectDigest() || decision.ReviewKind != string(workflowadapter.ReviewSolutionVerifier) ||
		decision.ReviewRequestID == "" || decision.ReviewDecisionID == "" || run.ID == "" {
		return fmt.Errorf("Standard authoring solution review decision is not an approved frozen source/session decision")
	}
	return nil
}

func (executor *StandardAuthoringMaterializeExecutor) materializeNewAuthoringTask(ctx context.Context, request workflowkit.StageExecutionRequest, run store.WorkflowRun, subject workflowRunSubject, inputs standardAuthoringMaterializeInputSet) (store.TaskRevision, error) {
	task := subject.TargetTask
	if task == nil || task.LifecycleState != store.TaskLifecycleDraft || task.CurrentRevisionID != "" {
		return store.TaskRevision{}, fmt.Errorf("Standard authoring materializer target Task is no longer a revision-free draft")
	}
	if run.Status != store.WorkflowRunRunning {
		return store.TaskRevision{}, fmt.Errorf("Standard authoring materializer Run is not running")
	}
	if isAdmissionAwareStandardAuthoringRun(run) {
		if executor.admission == nil {
			return store.TaskRevision{}, fmt.Errorf("Standard authoring admission contract is unavailable for the frozen template")
		}
		compiled, err := CompileStandardAuthoringTaskPackage(StandardAuthoringTaskPackageInput{
			Instruction: inputs.instruction, TaskTOMLDraft: inputs.taskTOML, Dockerfile: inputs.dockerfile,
			SolveScript: inputs.solveScript, TestScript: inputs.testScript, TestsAnalysis: inputs.testsAnalysis,
			Source: *subject.AuthoringSource, Environment: inputs.environment, Brief: inputs.brief, Admission: *executor.admission,
		})
		if err != nil {
			return store.TaskRevision{}, fmt.Errorf("compile Standard authoring task package: %w", err)
		}
		if !compiled.Report.Passed {
			return store.TaskRevision{}, fmt.Errorf("CodeEdge task admission rejected materialization: %s", admissionViolationSummary(compiled.Report.Violations))
		}
		if err := verifyStandardAuthoringAdmissionReceipt(request, run, subject, inputs.admissionReceipt, compiled.Report); err != nil {
			return store.TaskRevision{}, err
		}
		inputs = materializeInputsFromCanonicalPackage(inputs, compiled.CanonicalFiles)
	}
	revisionID, err := store.NewUUIDv7()
	if err != nil {
		return store.TaskRevision{}, fmt.Errorf("allocate Standard authoring TaskRevision ID: %w", err)
	}
	staging, cleanupStaging, err := executor.newMaterializationStagingDirectory(run.ID)
	if err != nil {
		return store.TaskRevision{}, err
	}
	defer cleanupStaging()
	if err := writeStandardAuthoringTaskSnapshot(staging, inputs); err != nil {
		return store.TaskRevision{}, err
	}
	prepared, cleanupRevision, err := (&RevisionService{core: executor.core}).prepareSnapshot(ctx, task.ID, revisionID, staging)
	if err != nil {
		return store.TaskRevision{}, fmt.Errorf("prepare Standard authoring sealed snapshot: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			cleanupRevision()
		}
	}()
	result, err := executor.core.store.MaterializeAuthoringTask(ctx, store.MaterializeAuthoringTaskRequest{
		IdempotencyKey:      "standard-authoring-materialize:" + run.ID + ":" + string(request.Claim.Stage.StageAttempt.ID),
		AuthoringSessionID:  subject.AuthoringSession.ID,
		AuthoringRunID:      run.ID,
		ExpectedTaskVersion: task.Version,
		ExpectedRunVersion:  run.Version,
		RevisionID:          revisionID,
		TaskDigest:          prepared.TaskDigest,
		ProposalDigest:      inputs.proposalHash,
		ManifestID:          prepared.ManifestObjectID,
		ChangeSummary:       "materialized approved Standard authoring session " + subject.AuthoringSession.ID,
		MetadataJSON:        task.MetadataJSON,
		Actor:               request.Execution.Actor,
		Reason:              "materialize approved Standard authoring task",
	})
	if err != nil {
		return store.TaskRevision{}, err
	}
	if result.Revision.ID != revisionID || result.Revision.TaskDigest != prepared.TaskDigest || result.Task.ID != task.ID {
		return store.TaskRevision{}, fmt.Errorf("%w: Standard authoring materialization receipt differs from prepared snapshot", store.ErrImmutable)
	}
	committed = true
	return result.Revision, nil
}

func verifyStandardAuthoringAdmissionReceipt(request workflowkit.StageExecutionRequest, run store.WorkflowRun, subject workflowRunSubject, raw []byte, report codeedge.AdmissionReport) error {
	var receipt standardAuthoringAdmissionReceipt
	if err := decodeStrictJSON(string(raw), &receipt); err != nil {
		return fmt.Errorf("decode Standard authoring admission receipt: %w", err)
	}
	if receipt.Format != standardAuthoringAdmissionReceiptFormat || receipt.Version != standardAuthoringAdmissionReceiptVersion ||
		receipt.RunID != run.ID || subject.AuthoringSource == nil || subject.AuthoringSession == nil ||
		receipt.AuthoringSourceID != subject.AuthoringSource.ID || receipt.AuthoringSessionID != subject.AuthoringSession.ID ||
		!receipt.Report.Passed || !reflect.DeepEqual(receipt.Report, report) {
		return fmt.Errorf("Standard authoring admission receipt does not bind the frozen package")
	}
	bindings := make([]workflowkit.ArtifactBinding, 0, 8)
	allowed := map[string]struct{}{
		"instruction": {}, "task_toml": {}, workflowadapter.StandardAuthoringEnvironmentPolicyArtifact: {}, "tests_analysis": {},
	}
	dockerfileName, solveName, testName := standardAuthoringPackageArtifactNames(run)
	allowed[dockerfileName] = struct{}{}
	allowed[solveName] = struct{}{}
	allowed[testName] = struct{}{}
	if isHarnessAwareStandardAuthoringRun(run) {
		allowed[workflowadapter.StandardAuthoringDockerfileBuildReportArtifact] = struct{}{}
		allowed[workflowadapter.StandardAuthoringHarnessReportArtifact] = struct{}{}
	}
	if isBriefAwareStandardAuthoringRun(run) {
		allowed[workflowadapter.StandardAuthoringBriefArtifact] = struct{}{}
	}
	for _, binding := range request.Inputs {
		if _, keep := allowed[binding.Name]; keep {
			bindings = append(bindings, binding)
		}
	}
	if len(bindings) != len(allowed) {
		return fmt.Errorf("Standard authoring materialization lacks the admission receipt input set")
	}
	fingerprint, err := workflowkit.FingerprintArtifactBindings(bindings)
	if err != nil || fingerprint != receipt.InputFingerprint {
		return fmt.Errorf("Standard authoring admission receipt input fingerprint does not match materialization")
	}
	return nil
}

// isAdmissionAwareStandardAuthoringRun keeps package interpretation
// version-scoped for fixtures and historical inspection. This release executes
// only the current fixed-file template; older Runs remain owned by their original
// deployment and control-plane root.
func isAdmissionAwareStandardAuthoringRun(run store.WorkflowRun) bool {
	return run.WorkflowTemplateID == workflowadapter.StandardAuthoringWorkflowTemplateID &&
		(run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringTaskAdmissionTemplateVersion ||
			run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringBriefTemplateVersion ||
			run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringRepairFeedbackTemplateVersion ||
			run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringTestsAnalysisInputTemplateVersion ||
			run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringHarnessTemplateVersion ||
			run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringFixedFileTemplateVersion)
}

func isBriefAwareStandardAuthoringRun(run store.WorkflowRun) bool {
	return run.WorkflowTemplateID == workflowadapter.StandardAuthoringWorkflowTemplateID &&
		(run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringBriefTemplateVersion ||
			run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringRepairFeedbackTemplateVersion ||
			run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringTestsAnalysisInputTemplateVersion ||
			run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringHarnessTemplateVersion ||
			run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringFixedFileTemplateVersion)
}

func isHarnessAwareStandardAuthoringRun(run store.WorkflowRun) bool {
	return run.WorkflowTemplateID == workflowadapter.StandardAuthoringWorkflowTemplateID &&
		(run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringHarnessTemplateVersion ||
			run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringFixedFileTemplateVersion)
}

func standardAuthoringPackageArtifactNames(run store.WorkflowRun) (dockerfile, solve, test string) {
	if isHarnessAwareStandardAuthoringRun(run) {
		return workflowadapter.StandardAuthoringValidatedDockerfileArtifact, workflowadapter.StandardAuthoringValidatedSolveScriptArtifact, workflowadapter.StandardAuthoringValidatedTestScriptArtifact
	}
	return "dockerfile", "solve_script", "test_script"
}

func validateStandardAuthoringHarnessEvidence(runID string, dockerfile, solveScript, testScript, dockerBuildReport, harnessReport []byte) error {
	buildCandidate, err := authoringharness.CandidateFromBytes(authoringharness.ModeDockerfileBuild, dockerfile, nil, nil)
	if err != nil {
		return fmt.Errorf("reconstruct Standard authoring Docker candidate: %w", err)
	}
	fullCandidate, err := authoringharness.CandidateFromBytes(authoringharness.ModeInitialOracle, dockerfile, solveScript, testScript)
	if err != nil {
		return fmt.Errorf("reconstruct Standard authoring full candidate: %w", err)
	}
	checks := []struct {
		raw       []byte
		mode      authoringharness.Mode
		stage     workflowkit.StageKey
		candidate authoringharness.Candidate
	}{
		{raw: dockerBuildReport, mode: authoringharness.ModeDockerfileBuild, stage: workflowkit.StageKey(workflowadapter.DockerfileBuildValidate), candidate: buildCandidate},
		{raw: harnessReport, mode: authoringharness.ModeInitialOracle, stage: workflowkit.StageKey(workflowadapter.AuthoringHarness), candidate: fullCandidate},
	}
	for _, check := range checks {
		var report authoringharness.Result
		if err := decodeStrictJSON(string(check.raw), &report); err != nil {
			return fmt.Errorf("decode Standard authoring harness evidence: %w", err)
		}
		report.ReportJSON = append([]byte(nil), check.raw...)
		if err := report.ValidateReportJSON(); err != nil || !report.Passed || report.Mode != check.mode || report.StageKey != check.stage || report.RunID != runID || report.CandidateDigest != check.candidate.CandidateDigest || report.EnvironmentDigest != check.candidate.EnvironmentDigest {
			return fmt.Errorf("Standard authoring harness evidence does not bind the frozen package")
		}
	}
	return nil
}

func materializeInputsFromCanonicalPackage(inputs standardAuthoringMaterializeInputSet, files []codeedge.TaskPackageFile) standardAuthoringMaterializeInputSet {
	for _, file := range files {
		switch file.Path {
		case "instruction.md":
			inputs.instruction = append([]byte(nil), file.Data...)
		case "task.toml":
			inputs.taskTOML = append([]byte(nil), file.Data...)
		case "environment/Dockerfile":
			inputs.dockerfile = append([]byte(nil), file.Data...)
		case "solution/solve.sh":
			inputs.solveScript = append([]byte(nil), file.Data...)
		case "tests/test.sh":
			inputs.testScript = append([]byte(nil), file.Data...)
		case "tests_analysis.md":
			inputs.testsAnalysis = append([]byte(nil), file.Data...)
		}
	}
	return inputs
}

func admissionViolationSummary(violations []codeedge.Violation) string {
	if len(violations) == 0 {
		return "unknown deterministic finding"
	}
	parts := make([]string, 0, len(violations))
	for _, violation := range violations {
		parts = append(parts, violation.String())
	}
	return strings.Join(parts, "; ")
}

func standardAuthoringAdmissionReceiptReference(request workflowkit.StageExecutionRequest, run store.WorkflowRun) (*workflowadapter.ArtifactReference, error) {
	if !isAdmissionAwareStandardAuthoringRun(run) {
		return nil, nil
	}
	for _, binding := range request.Inputs {
		if binding.Name == "codeedge_package_admission_report" {
			if binding.SchemaVersion != standardAuthoringAdmissionReceiptFormat {
				return nil, fmt.Errorf("Standard authoring materialization admission receipt has the wrong schema")
			}
			return &workflowadapter.ArtifactReference{ID: binding.ArtifactID, ContentDigest: binding.ContentDigest, SchemaVersion: binding.SchemaVersion}, nil
		}
	}
	return nil, fmt.Errorf("Standard authoring materialization lacks the admission receipt artifact")
}

func (executor *StandardAuthoringMaterializeExecutor) materializationResult(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, revision store.TaskRevision, admissionReceipt *workflowadapter.ArtifactReference) (workflowkit.StageExecutionResult, error) {
	if revision.TaskID != subject.TargetTask.ID || revision.Origin != store.RevisionOriginGenerated || revision.State != store.RevisionStateSealed {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: Standard authoring result is not a generated sealed first revision", store.ErrImmutable)
	}
	snapshot, err := materializeManagedTaskSnapshotObject(ctx, executor.core, revision)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	snapshotArtifactID, err := store.NewUUIDv7()
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("allocate Standard authoring task snapshot artifact ID: %w", err)
	}
	templateReference := workflowadapter.TemplateReference{ID: run.WorkflowTemplateID, Version: run.WorkflowTemplateVersion}
	handoffSchema, err := workflowadapter.StandardAuthoringTaskHandoffSchemaForTemplate(templateReference)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	handoffVersion := workflowadapter.StandardAuthoringTaskHandoffVersion
	if admissionReceipt != nil {
		handoffVersion = workflowadapter.StandardAuthoringTaskAdmissionHandoffVersion
	}
	handoff := workflowadapter.StandardAuthoringTaskHandoff{
		Format:                workflowadapter.StandardAuthoringTaskHandoffFormat,
		Version:               handoffVersion,
		AuthoringSourceID:     subject.AuthoringSource.ID,
		AuthoringSessionID:    subject.AuthoringSession.ID,
		AuthoringRunID:        run.ID,
		AuthoringSourceDigest: workflowkit.SubjectDigest(subject.subjectDigest()),
		TaskID:                revision.TaskID,
		RevisionID:            revision.ID,
		RevisionDigest:        workflowkit.SubjectDigest(revision.TaskDigest),
		TaskSnapshot: workflowadapter.ArtifactReference{
			ID: workflowkit.ArtifactID(snapshotArtifactID), ContentDigest: snapshot.Digest, SchemaVersion: "harbor.artifact.v1",
		},
		ChildTemplate:    workflowadapter.CodeEdgePhase1TemplateReference(),
		AdmissionReceipt: admissionReceipt,
	}
	handoffBytes, err := handoff.CanonicalJSON()
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	snapshotBytes, err := executor.core.objects.ReadAll(ctx, snapshot)
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("read materialized Standard authoring task snapshot: %w", err)
	}
	return workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass},
		Artifacts: []workflowkit.StageArtifact{
			{ID: workflowkit.ArtifactID(snapshotArtifactID), Name: "task_snapshot", SchemaVersion: "harbor.artifact.v1", Content: snapshotBytes},
			{Name: "task_digest", SchemaVersion: "harbor.artifact.v1", Content: []byte(revision.TaskDigest)},
			{Name: workflowadapter.StandardAuthoringTaskHandoffArtifact, SchemaVersion: handoffSchema, Content: handoffBytes},
		},
	}, nil
}

func (executor *StandardAuthoringMaterializeExecutor) newMaterializationStagingDirectory(runID string) (string, func(), error) {
	if err := executor.core.layout.ensureRoot(); err != nil {
		return "", nil, err
	}
	runDirectory := executor.core.layout.runDirectory(runID)
	info, err := os.Lstat(runDirectory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", nil, fmt.Errorf("Standard authoring Run directory is unavailable for materialization")
	}
	parent := filepath.Join(runDirectory, "materialization")
	if err := os.Mkdir(parent, 0o750); err != nil && !os.IsExist(err) {
		return "", nil, fmt.Errorf("create Standard authoring materialization directory: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return "", nil, fmt.Errorf("Standard authoring materialization directory is unsafe")
	}
	staging, err := os.MkdirTemp(parent, "snapshot-*")
	if err != nil {
		return "", nil, fmt.Errorf("create Standard authoring materialization staging directory: %w", err)
	}
	return staging, func() { _ = os.RemoveAll(staging) }, nil
}

func writeStandardAuthoringTaskSnapshot(directory string, inputs standardAuthoringMaterializeInputSet) error {
	files := []struct {
		path string
		mode os.FileMode
		data []byte
	}{
		{"instruction.md", 0o644, inputs.instruction},
		{"task.toml", 0o644, inputs.taskTOML},
		{"tests_analysis.md", 0o644, inputs.testsAnalysis},
		{"environment/Dockerfile", 0o644, inputs.dockerfile},
		{"solution/solve.sh", 0o755, inputs.solveScript},
		{"tests/test.sh", 0o755, inputs.testScript},
	}
	for _, file := range files {
		if len(file.data) == 0 {
			return fmt.Errorf("Standard authoring materialization required output %s is empty", file.path)
		}
		path := filepath.Join(directory, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("create Standard authoring snapshot parent for %s: %w", file.path, err)
		}
		output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, file.mode)
		if err != nil {
			return fmt.Errorf("create Standard authoring snapshot file %s: %w", file.path, err)
		}
		if _, err := output.Write(file.data); err != nil {
			_ = output.Close()
			return fmt.Errorf("write Standard authoring snapshot file %s: %w", file.path, err)
		}
		if err := output.Chmod(file.mode); err != nil {
			_ = output.Close()
			return fmt.Errorf("set Standard authoring snapshot file mode %s: %w", file.path, err)
		}
		if err := output.Sync(); err != nil {
			_ = output.Close()
			return fmt.Errorf("sync Standard authoring snapshot file %s: %w", file.path, err)
		}
		if err := output.Close(); err != nil {
			return fmt.Errorf("close Standard authoring snapshot file %s: %w", file.path, err)
		}
	}
	return nil
}

var _ stageprovider.HarborBuiltinOperationExecutor = (*StandardAuthoringMaterializeExecutor)(nil)
