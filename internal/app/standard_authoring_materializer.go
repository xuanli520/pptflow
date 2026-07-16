package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const standardAuthoringMaterializeHandlerID = "standard-authoring.materialize-task"

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
	core *lifecycleServiceCore
}

// StandardAuthoringMaterializeExecutorConfig contains only local
// control-plane capabilities.  ManagedRoot is a local Harbor Flow root, not a
// caller-provided task workspace; Store is the durable V2 control plane.
type StandardAuthoringMaterializeExecutorConfig struct {
	ManagedRoot string
	Store       *store.Store
	Now         func() time.Time
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
	return &StandardAuthoringMaterializeExecutor{core: &lifecycleServiceCore{store: config.Store, layout: layout, objects: objects, now: now}}, nil
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
	if payload.HandlerID != standardAuthoringMaterializeHandlerID || invocation.Resolution.StageKey != workflowkit.StageKey(workflowadapter.MaterializeTask) ||
		invocation.Request.Stage.Key != workflowkit.StageKey(workflowadapter.MaterializeTask) {
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
	return executor.materializationResult(ctx, *run, subject, revision)
}

type standardAuthoringMaterializeInputSet struct {
	instruction   []byte
	taskTOML      []byte
	dockerfile    []byte
	solveScript   []byte
	testScript    []byte
	testsAnalysis []byte
	proposalHash  string
}

func standardAuthoringMaterializeInputs(ctx context.Context, request workflowkit.StageExecutionRequest, run store.WorkflowRun, subject workflowRunSubject) (standardAuthoringMaterializeInputSet, error) {
	if !isCurrentStandardAuthoringRun(run) {
		return standardAuthoringMaterializeInputSet{}, fmt.Errorf("Standard authoring materializer Run is not bound to the current template")
	}
	if request.ReadInput == nil {
		return standardAuthoringMaterializeInputSet{}, fmt.Errorf("Standard authoring materializer has no frozen input reader")
	}
	expected, contractErr := standardAuthoringMaterializeInputContract(request.Stage)
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
	if result.solveScript, err = read("solve_script"); err != nil {
		return result, err
	}
	if result.testScript, err = read("test_script"); err != nil {
		return result, err
	}
	if result.testsAnalysis, err = read("tests_analysis"); err != nil {
		return result, err
	}
	decision, err := read("solution_review_decision")
	if err != nil {
		return result, err
	}
	if err := validateApprovedAuthoringSolutionDecision(decision, run, subject); err != nil {
		return result, err
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(request.Inputs)
	if err != nil {
		return result, fmt.Errorf("fingerprint Standard authoring materialization inputs: %w", err)
	}
	result.proposalHash = string(inputFingerprint)
	return result, nil
}

func standardAuthoringMaterializeInputContract(stage workflowkit.StageDescriptor) (map[string]string, error) {
	if stage.Key != workflowkit.StageKey(workflowadapter.MaterializeTask) {
		return nil, fmt.Errorf("Standard authoring materializer stage is not materialize_task")
	}
	current, found := workflowadapter.StandardAuthoringWorkflowTemplate().Catalog.Stage(workflowkit.StageKey(workflowadapter.MaterializeTask))
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

func (executor *StandardAuthoringMaterializeExecutor) materializationResult(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, revision store.TaskRevision) (workflowkit.StageExecutionResult, error) {
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
	handoff := workflowadapter.StandardAuthoringTaskHandoff{
		Format:                workflowadapter.StandardAuthoringTaskHandoffFormat,
		Version:               workflowadapter.StandardAuthoringTaskHandoffVersion,
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
		ChildTemplate: workflowadapter.CodeEdgePhase1TemplateReference(),
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
			{Name: workflowadapter.StandardAuthoringTaskHandoffArtifact, SchemaVersion: workflowadapter.StandardAuthoringTaskHandoffSchemaVersion, Content: handoffBytes},
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
