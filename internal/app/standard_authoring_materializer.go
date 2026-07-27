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

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringMaterializeHandlerID         = "standard-authoring.materialize-task"
	standardAuthoringPackageAdmissionHandlerID    = "standard-authoring.codeedge-package-admission"
	standardAuthoringHostCandidateVerifyHandlerID = "standard-authoring.host-candidate-verify"
	standardAuthoringFinalAttestationHandlerID    = "standard-authoring.final-attestation"
	standardAuthoringAdmissionReceiptFormat       = "harbor.standard-authoring-task-package-admission.v1"
	standardAuthoringAdmissionReceiptVersion      = "1"
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
// This terminal handler never starts task-bound work under the source/session
// subject or authorizes a child Run.
type StandardAuthoringMaterializeExecutor struct {
	core             *lifecycleServiceCore
	admission        *codeedge.TaskAdmissionContract
	candidateHarness *StandardAuthoringDockerHarness
}

// StandardAuthoringMaterializeExecutorConfig contains only local
// control-plane capabilities.  ManagedRoot is a local Harbor Flow root, not a
// caller-provided task workspace; Store is the durable V2 control plane.
type StandardAuthoringMaterializeExecutorConfig struct {
	ManagedRoot string
	Store       *store.Store
	Now         func() time.Time
	// Admission is mandatory for the sole supported Standard Authoring 3.0
	// template. Historical templates are not executable.
	Admission        *codeedge.TaskAdmissionContract
	CandidateHarness *StandardAuthoringDockerHarness
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
	if config.Admission == nil {
		return nil, fmt.Errorf("Standard authoring CodeEdge admission contract is required")
	}
	if err := config.Admission.Validate(); err != nil {
		return nil, fmt.Errorf("validate Standard authoring CodeEdge admission contract: %w", err)
	}
	admission := *config.Admission
	return &StandardAuthoringMaterializeExecutor{core: &lifecycleServiceCore{store: config.Store, layout: layout, objects: objects, now: now}, admission: &admission, candidateHarness: config.CandidateHarness}, nil
}

// ExecuteHarborBuiltin implements the sealed stageprovider built-in contract.
// It reserves the task_snapshot artifact ID before it emits the immutable
// materialization receipt, so the receipt names the exact output which the
// durable stage projection will later create. All other output IDs remain
// backend-owned.
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
	if payload.HandlerID == standardAuthoringHostCandidateVerifyHandlerID {
		return executor.executeHostCandidateVerify(ctx, invocation)
	}
	if payload.HandlerID == standardAuthoringFinalAttestationHandlerID {
		return executor.executeFinalAttestation(ctx, invocation)
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

	inputs, err := standardAuthoringMaterializeInputs(ctx, invocation.Request, *run, subject)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if err := validateStandardAuthoringMaterializationContract(inputs.contract, subject); err != nil {
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
	if run == nil || !isCurrentStandardAuthoringRun(*run) || run.Status != store.WorkflowRunRunning {
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
	if err := validateStandardAuthoringMaterializationContract(inputs.contract, subject); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	compiled, err := CompileStandardAuthoringTaskPackage(StandardAuthoringTaskPackageInput{
		Instruction: inputs.instruction, TaskTOMLDraft: inputs.taskTOML, Dockerfile: inputs.dockerfile,
		SolveScript: inputs.solveScript, TestScript: inputs.testScript, TestsAnalysis: inputs.testsAnalysis,
		Source: *subject.AuthoringSource, Contract: inputs.contract, Admission: *executor.admission,
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
	candidateSnapshot, validationReceipt, finalAttestation                    []byte
	contract                                                                  workflowadapter.AuthoringContract
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
	if result.dockerfile, err = read("dockerfile"); err != nil {
		return result, err
	}
	if result.solveScript, err = read("solve_script"); err != nil {
		return result, err
	}
	if result.testScript, err = read("test_script"); err != nil {
		return result, err
	}
	if result.testsAnalysis, err = read("tests_analysis"); err != nil {
		return result, err
	}
	if result.candidateSnapshot, err = read("candidate_snapshot"); err != nil {
		return result, err
	}
	if result.validationReceipt, err = read("validation_receipt"); err != nil {
		return result, err
	}
	if result.finalAttestation, err = read("final_attestation"); err != nil {
		return result, err
	}
	if err := validateStandardAuthoringV3MaterializationEvidence(result.candidateSnapshot, result.validationReceipt, result.finalAttestation, standardAuthoringV3CandidateFiles(result.instruction, result.taskTOML, result.dockerfile, result.solveScript, result.testScript, result.testsAnalysis), time.Now); err != nil {
		return result, err
	}
	contractRaw, err := read(workflowadapter.AuthoringContractArtifact)
	if err != nil {
		return result, err
	}
	result.contract, err = workflowadapter.ParseAuthoringContractJSON(contractRaw)
	if err != nil {
		return result, fmt.Errorf("decode frozen Standard authoring root contract: %w", err)
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
	candidateSnapshot []byte
	validationReceipt []byte
	finalAttestation  []byte
	contract          workflowadapter.AuthoringContract
	proposalHash      string
}

func validateStandardAuthoringMaterializationContract(contract workflowadapter.AuthoringContract, subject workflowRunSubject) error {
	if !subject.isAuthoringSession() || subject.AuthoringSource == nil || subject.AuthoringSession == nil {
		return fmt.Errorf("Standard authoring materialization has no source/session subject")
	}
	if err := contract.Validate(); err != nil {
		return fmt.Errorf("validate Standard authoring materialization root contract: %w", err)
	}
	if contract.Task.ID != subject.AuthoringSession.TargetTaskID || contract.Source.RepositoryURL != subject.AuthoringSource.RepositoryURL ||
		contract.Source.CommitSHA != subject.AuthoringSource.CommitSHA || contract.Source.SnapshotDigest != subject.AuthoringSource.SnapshotContentDigest {
		return fmt.Errorf("Standard authoring materialization root contract does not match its frozen subject")
	}
	return nil
}

func standardAuthoringMaterializeInputs(ctx context.Context, request workflowkit.StageExecutionRequest, run store.WorkflowRun, subject workflowRunSubject) (standardAuthoringMaterializeInputSet, error) {
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
	if result.dockerfile, err = read("dockerfile"); err != nil {
		return result, err
	}
	contractRaw, err := read(workflowadapter.AuthoringContractArtifact)
	if err != nil {
		return result, err
	}
	contract, err := workflowadapter.ParseAuthoringContractJSON(contractRaw)
	if err != nil {
		return result, fmt.Errorf("decode frozen Standard authoring root contract: %w", err)
	}
	result.contract = contract
	if result.solveScript, err = read("solve_script"); err != nil {
		return result, err
	}
	if result.testScript, err = read("test_script"); err != nil {
		return result, err
	}
	if result.testsAnalysis, err = read("tests_analysis"); err != nil {
		return result, err
	}
	if result.candidateSnapshot, err = read("candidate_snapshot"); err != nil {
		return result, err
	}
	if result.validationReceipt, err = read("validation_receipt"); err != nil {
		return result, err
	}
	if result.finalAttestation, err = read("final_attestation"); err != nil {
		return result, err
	}
	if err := validateStandardAuthoringV3MaterializationEvidence(result.candidateSnapshot, result.validationReceipt, result.finalAttestation, standardAuthoringV3CandidateFiles(result.instruction, result.taskTOML, result.dockerfile, result.solveScript, result.testScript, result.testsAnalysis), time.Now); err != nil {
		return result, err
	}
	decision, err := read("solution_review_decision")
	if err != nil {
		return result, err
	}
	if err := validateApprovedAuthoringSolutionDecision(decision, run, subject); err != nil {
		return result, err
	}
	if result.admissionReceipt, err = read("codeedge_package_admission_report"); err != nil {
		return result, err
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
	if expected[workflowadapter.AuthoringContractArtifact] != workflowadapter.AuthoringContractSchemaVersion {
		return nil, fmt.Errorf("current Standard authoring root contract schema is invalid")
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
	if executor.admission == nil {
		return store.TaskRevision{}, fmt.Errorf("Standard authoring admission contract is unavailable for the frozen template")
	}
	compiled, err := CompileStandardAuthoringTaskPackage(StandardAuthoringTaskPackageInput{
		Instruction: inputs.instruction, TaskTOMLDraft: inputs.taskTOML, Dockerfile: inputs.dockerfile,
		SolveScript: inputs.solveScript, TestScript: inputs.testScript, TestsAnalysis: inputs.testsAnalysis,
		Source: *subject.AuthoringSource, Contract: inputs.contract, Admission: *executor.admission,
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
	bindings := make([]workflowkit.ArtifactBinding, 0, 10)
	allowed := map[string]struct{}{
		"instruction": {}, "task_toml": {}, "dockerfile": {}, "solve_script": {}, "test_script": {}, "tests_analysis": {},
		"candidate_snapshot": {}, "validation_receipt": {}, "final_attestation": {}, workflowadapter.AuthoringContractArtifact: {},
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
	receipt := workflowadapter.StandardAuthoringMaterializationReceipt{
		Format:                workflowadapter.StandardAuthoringMaterializationReceiptFormat,
		Version:               workflowadapter.StandardAuthoringMaterializationReceiptVersion,
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
		AdmissionReceipt: *admissionReceipt,
	}
	receiptBytes, err := receipt.CanonicalJSON()
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
			{Name: workflowadapter.StandardAuthoringMaterializationReceiptArtifact, SchemaVersion: workflowadapter.StandardAuthoringMaterializationReceiptSchemaVersion, Content: receiptBytes},
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
