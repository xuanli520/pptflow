package stageprovider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringCodexValidationQuotaDimension    = "authoring_validation"
	standardAuthoringCodexSubmissionFailureValidation = "standard_authoring_codex_agent_turn.validation"
)

// standardAuthoringCodexWorkspaceSubmission is the output authority for the
// two ReAct authoring stages. The model edits fixed files in its attempt-scoped
// workspace and this handler invokes the host-owned Docker harness. Model
// arguments carry no artifact bytes, paths, commands, or validation claims.
type standardAuthoringCodexWorkspaceSubmission struct {
	mu sync.Mutex

	request     workflowkit.StageExecutionRequest
	stage       workflowkit.StageDescriptor
	taskRoot    string
	mode        authoringharness.Mode
	validator   authoringharness.Validator
	maxAttempts int
	now         func() time.Time
	environment *workflowadapter.StandardAuthoringEnvironmentPolicy
	frozenEnv   workflowkit.Fingerprint

	currentTurn int
	attempts    int
	accepted    *standardAuthoringCodexAcceptedOutput
	failureCode string
}

type standardAuthoringCodexWorkspaceSubmissionCandidate struct {
	Verdict *workflowkit.Verdict `json:"verdict"`
}

type standardAuthoringCodexWorkspaceSubmissionReceipt struct {
	Accepted    bool                    `json:"accepted"`
	Errors      []string                `json:"errors"`
	Remaining   int                     `json:"remaining"`
	Digest      workflowkit.Fingerprint `json:"digest,omitempty"`
	Step        string                  `json:"step,omitempty"`
	ExitCode    int                     `json:"exit_code,omitempty"`
	StdoutTail  string                  `json:"stdout_tail,omitempty"`
	StderrTail  string                  `json:"stderr_tail,omitempty"`
	ImageReused bool                    `json:"image_reused,omitempty"`
}

func newStandardAuthoringCodexWorkspaceSubmission(request workflowkit.StageExecutionRequest, taskRoot string, maxAttempts int, now func() time.Time, validator authoringharness.Validator, environment *workflowadapter.StandardAuthoringEnvironmentPolicy) (*standardAuthoringCodexWorkspaceSubmission, error) {
	if maxAttempts <= 0 || request.Charge == nil || request.Claim.Stage == nil || strings.TrimSpace(string(request.Claim.Stage.StageAttempt.ID)) == "" || isNilInterface(validator) {
		return nil, errors.New("invalid Standard authoring Codex workspace submission configuration")
	}
	if err := request.Stage.Validate(); err != nil {
		return nil, fmt.Errorf("validate frozen Standard authoring Codex workspace stage: %w", err)
	}
	mode, err := standardAuthoringCodexWorkspaceHarnessMode(request.Stage)
	if err != nil {
		return nil, err
	}
	absoluteTaskRoot, err := filepath.Abs(strings.TrimSpace(taskRoot))
	if err != nil || strings.TrimSpace(taskRoot) == "" || filepath.Clean(absoluteTaskRoot) != taskRoot {
		return nil, errors.New("invalid Standard authoring Codex task workspace")
	}
	if now == nil {
		now = time.Now
	}
	if environment == nil || environment.Validate() != nil {
		return nil, errors.New("invalid frozen Standard authoring environment policy")
	}
	environmentCopy := *environment
	submission := &standardAuthoringCodexWorkspaceSubmission{
		request: request, stage: request.Stage.Clone(), taskRoot: taskRoot, mode: mode,
		validator: validator, maxAttempts: maxAttempts, now: now, environment: &environmentCopy,
	}
	if mode == authoringharness.ModeInitialOracle {
		candidate, err := authoringharness.ReadCandidate(taskRoot, mode)
		if err != nil {
			return nil, fmt.Errorf("read frozen Standard authoring harness inputs: %w", err)
		}
		submission.frozenEnv = candidate.EnvironmentDigest
	}
	return submission, nil
}

func standardAuthoringCodexWorkspaceHarnessMode(stage workflowkit.StageDescriptor) (authoringharness.Mode, error) {
	var mode authoringharness.Mode
	var names []string
	switch stage.Key {
	case workflowkit.StageKey(workflowadapter.DockerfileBuildValidate):
		mode = authoringharness.ModeDockerfileBuild
		names = []string{"validated_dockerfile", "dockerfile_build_report"}
	case workflowkit.StageKey(workflowadapter.AuthoringHarness):
		mode = authoringharness.ModeInitialOracle
		names = []string{"validated_solve_script", "validated_test_script", "authoring_harness_report"}
	default:
		return "", errors.New("Standard authoring stage does not use the writable harness")
	}
	if len(stage.Outputs) != len(names) || !stage.Verdicts.Allows(workflowkit.VerdictPass) {
		return "", errors.New("Standard authoring writable harness stage contract is invalid")
	}
	for index, name := range names {
		if stage.Outputs[index].Name != name || !stage.Outputs[index].Required {
			return "", errors.New("Standard authoring writable harness output contract is invalid")
		}
	}
	return mode, nil
}

func standardAuthoringCodexPrepareWorkspaceCandidate(stage workflowkit.StageDescriptor, taskRoot string, inputs []standardAuthoringCodexInput) error {
	mode, err := standardAuthoringCodexWorkspaceHarnessMode(stage)
	if err != nil {
		return err
	}
	inputNames := map[string]string{}
	switch mode {
	case authoringharness.ModeDockerfileBuild:
		inputNames[authoringharness.DockerfileRelativePath] = "dockerfile"
	case authoringharness.ModeInitialOracle:
		inputNames[authoringharness.DockerfileRelativePath] = workflowadapter.StandardAuthoringValidatedDockerfileArtifact
		inputNames[authoringharness.SolveScriptRelativePath] = "solve_script"
		inputNames[authoringharness.TestScriptRelativePath] = "test_script"
	}
	byName := make(map[string]standardAuthoringCodexInput, len(inputs))
	for _, input := range inputs {
		if _, duplicate := byName[input.Name]; duplicate {
			return errors.New("Standard authoring workspace received a duplicate frozen input")
		}
		byName[input.Name] = input
	}
	for relative, inputName := range inputNames {
		input, found := byName[inputName]
		if !found {
			return fmt.Errorf("Standard authoring workspace is missing frozen input %q", inputName)
		}
		content, err := base64.StdEncoding.DecodeString(input.ContentBase64)
		if err != nil || workflowkit.SHA256Fingerprint(content) != input.ContentDigest {
			return fmt.Errorf("Standard authoring workspace input %q is invalid", inputName)
		}
		mode := os.FileMode(0o640)
		if relative == authoringharness.SolveScriptRelativePath || relative == authoringharness.TestScriptRelativePath {
			mode = 0o750
		}
		if err := standardAuthoringCodexWriteCandidateFile(taskRoot, relative, content, mode); err != nil {
			return err
		}
	}
	_, err = authoringharness.ReadCandidate(taskRoot, mode)
	return err
}

func standardAuthoringCodexWriteCandidateFile(taskRoot, relative string, content []byte, mode os.FileMode) error {
	directory := filepath.Join(taskRoot, filepath.Dir(filepath.FromSlash(relative)))
	if err := os.Mkdir(directory, 0o750); err != nil {
		return fmt.Errorf("create Standard authoring candidate directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Standard authoring candidate directory is unsafe")
	}
	path := filepath.Join(taskRoot, filepath.FromSlash(relative))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create Standard authoring candidate file: %w", err)
	}
	writeErr := func() error {
		if _, err := file.Write(content); err != nil {
			return err
		}
		return file.Sync()
	}()
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		if writeErr != nil {
			return fmt.Errorf("write Standard authoring candidate file: %w", writeErr)
		}
		return fmt.Errorf("close Standard authoring candidate file: %w", closeErr)
	}
	return nil
}

func (submission *standardAuthoringCodexWorkspaceSubmission) beginTurn(turn int) error {
	if submission == nil || turn < 1 {
		return errors.New("Standard authoring Codex workspace submission turn is invalid")
	}
	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.failureCode != "" {
		return errors.New("Standard authoring Codex workspace submission is unavailable")
	}
	submission.currentTurn = turn
	return nil
}

func (submission *standardAuthoringCodexWorkspaceSubmission) dynamicTool() agent.DynamicTool {
	return agent.DynamicTool{
		Name:        standardAuthoringCodexSubmitToolName,
		Description: "Validate the fixed files under task/ with the host-owned Docker harness and submit them only if every required check passes. On rejection, inspect the bounded findings, edit the workspace, and call this tool again. The only argument is the pass verdict; paths, commands, artifact bytes, and evidence are host-owned.",
		InputSchema: submission.outputSchema(),
		Handler:     submission.handle,
	}
}

func (submission *standardAuthoringCodexWorkspaceSubmission) outputSchema() json.RawMessage {
	if submission == nil {
		return nil
	}
	encoded, err := json.Marshal(map[string]any{
		"$id":                  "harbor.standard-authoring-codex-workspace-submit.v1",
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"additionalProperties": false,
		"properties": map[string]any{
			"verdict": map[string]any{"enum": []string{string(workflowkit.VerdictPass)}, "type": "string"},
		},
		"required": []string{"verdict"},
		"type":     "object",
	})
	if err != nil {
		panic("marshal fixed Standard authoring Codex workspace submission schema: " + err.Error())
	}
	return json.RawMessage(encoded)
}

func (submission *standardAuthoringCodexWorkspaceSubmission) handle(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	rawDigest := workflowkit.SHA256Fingerprint(raw)
	if submission == nil {
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"submission_unavailable"}, 0, rawDigest, authoringharness.Result{})
	}

	submission.mu.Lock()
	if submission.accepted != nil {
		remaining := submission.remainingLocked()
		submission.mu.Unlock()
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"already_accepted"}, remaining, rawDigest, authoringharness.Result{})
	}
	if submission.failureCode != "" {
		remaining := submission.remainingLocked()
		submission.mu.Unlock()
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"submission_unavailable"}, remaining, rawDigest, authoringharness.Result{})
	}
	if submission.attempts >= submission.maxAttempts {
		submission.mu.Unlock()
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"submit_attempts_exhausted"}, 0, rawDigest, authoringharness.Result{})
	}
	submission.attempts++
	attempt := submission.attempts
	turn := submission.currentTurn
	remaining := submission.remainingLocked()
	submission.mu.Unlock()

	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"submission_timeout"}, remaining, rawDigest, authoringharness.Result{})
	}
	if turn < 1 || !standardAuthoringCodexWorkspacePassCandidate(raw) {
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"wrong_verdict"}, remaining, rawDigest, authoringharness.Result{})
	}
	for _, dimension := range []string{standardAuthoringCodexOutputSubmissionQuotaDimension, standardAuthoringCodexValidationQuotaDimension} {
		if err := submission.request.Charge(ctx, workflowkit.StageUsage{
			OperationKey: standardAuthoringCodexWorkspaceUsageKey(submission.request, turn, attempt, dimension),
			Dimension:    dimension, Units: 1, OccurredAt: submission.now().UTC(),
		}); err != nil {
			if contextError(ctx) != nil {
				return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"submission_timeout"}, remaining, rawDigest, authoringharness.Result{})
			}
			failureCode := standardAuthoringCodexSubmissionFailureAccounting
			diagnostic := "submission_accounting_unavailable"
			switch {
			case errors.Is(err, store.ErrQuotaExhausted):
				failureCode = standardAuthoringCodexSubmissionFailureQuota
				diagnostic = "submission_quota_exhausted"
			case errors.Is(err, store.ErrQuotaLeaseExpired), errors.Is(err, store.ErrFencingToken), errors.Is(err, store.ErrLeaseHeld), errors.Is(err, store.ErrImmutable):
				failureCode = standardAuthoringCodexSubmissionFailureLease
				diagnostic = "submission_lease_lost"
			}
			submission.mu.Lock()
			submission.failureCode = failureCode
			submission.mu.Unlock()
			return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{diagnostic}, remaining, rawDigest, authoringharness.Result{})
		}
	}

	candidate, err := authoringharness.ReadCandidate(submission.taskRoot, submission.mode)
	if err != nil {
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"candidate_unavailable"}, remaining, rawDigest, authoringharness.Result{})
	}
	if err := submission.environment.ValidateDockerfile(candidate.Dockerfile); err != nil {
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"dockerfile_environment_policy_mismatch"}, remaining, candidate.CandidateDigest, authoringharness.Result{})
	}
	if submission.frozenEnv != "" && candidate.EnvironmentDigest != submission.frozenEnv {
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"validated_dockerfile_changed"}, remaining, candidate.CandidateDigest, authoringharness.Result{})
	}

	result, err := submission.validator.Validate(ctx, authoringharness.Request{
		Mode: submission.mode, RunID: submission.request.Execution.ID, StageKey: submission.stage.Key,
		StageAttemptID: string(submission.request.Claim.Stage.StageAttempt.ID),
	})
	if err != nil {
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"validation_unavailable"}, remaining, candidate.CandidateDigest, authoringharness.Result{})
	}
	if err := result.ValidateReportJSON(); err != nil || result.Mode != submission.mode || result.RunID != submission.request.Execution.ID || result.StageKey != submission.stage.Key || result.StageAttemptID != string(submission.request.Claim.Stage.StageAttempt.ID) {
		submission.mu.Lock()
		submission.failureCode = standardAuthoringCodexSubmissionFailureValidation
		submission.mu.Unlock()
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"validation_contract_mismatch"}, remaining, candidate.CandidateDigest, authoringharness.Result{})
	}
	if !result.Passed {
		diagnostics := append([]string(nil), result.Findings...)
		if len(diagnostics) == 0 {
			diagnostics = []string{"validation_failed"}
		}
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, diagnostics, remaining, result.CandidateDigest, result)
	}

	// Re-read after the harness returns. A successful report is authority only
	// for the exact candidate it checked; any concurrent or post-validation edit
	// forces another complete validation attempt.
	acceptedCandidate, err := authoringharness.ReadCandidate(submission.taskRoot, submission.mode)
	if err != nil || acceptedCandidate.CandidateDigest != result.CandidateDigest || acceptedCandidate.EnvironmentDigest != result.EnvironmentDigest || !standardAuthoringCodexCandidatesEqual(candidate, acceptedCandidate) {
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"candidate_changed_after_validation"}, remaining, result.CandidateDigest, result)
	}
	artifacts, err := standardAuthoringCodexWorkspaceArtifacts(submission.stage, acceptedCandidate, result.ReportJSON, turn)
	if err != nil {
		submission.mu.Lock()
		submission.failureCode = standardAuthoringCodexSubmissionFailureValidation
		submission.mu.Unlock()
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"validation_contract_mismatch"}, remaining, result.CandidateDigest, authoringharness.Result{})
	}
	accepted := workflowkit.StageExecutionResult{
		Outcome:   workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass},
		Artifacts: artifacts,
	}

	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.accepted != nil {
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"already_accepted"}, submission.remainingLocked(), result.CandidateDigest, authoringharness.Result{})
	}
	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexWorkspaceSubmissionResponse(false, []string{"submission_timeout"}, submission.remainingLocked(), result.CandidateDigest, authoringharness.Result{})
	}
	submission.accepted = &standardAuthoringCodexAcceptedOutput{result: cloneStandardAuthoringCodexStageResult(accepted)}
	return standardAuthoringCodexWorkspaceSubmissionResponse(true, nil, submission.remainingLocked(), result.CandidateDigest, result)
}

func standardAuthoringCodexWorkspacePassCandidate(raw []byte) bool {
	if len(raw) == 0 || rejectDuplicateDeploymentCatalogJSONKeys(raw) != nil {
		return false
	}
	var candidate standardAuthoringCodexWorkspaceSubmissionCandidate
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		return false
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false
	}
	return candidate.Verdict != nil && *candidate.Verdict == workflowkit.VerdictPass
}

func standardAuthoringCodexCandidatesEqual(left, right authoringharness.Candidate) bool {
	return left.Mode == right.Mode && left.CandidateDigest == right.CandidateDigest && left.EnvironmentDigest == right.EnvironmentDigest &&
		bytes.Equal(left.Dockerfile, right.Dockerfile) && bytes.Equal(left.SolveScript, right.SolveScript) && bytes.Equal(left.TestScript, right.TestScript)
}

func standardAuthoringCodexWorkspaceArtifacts(stage workflowkit.StageDescriptor, candidate authoringharness.Candidate, report []byte, turn int) ([]workflowkit.StageArtifact, error) {
	contents := map[string][]byte{
		"validated_dockerfile":     candidate.Dockerfile,
		"dockerfile_build_report":  report,
		"validated_solve_script":   candidate.SolveScript,
		"validated_test_script":    candidate.TestScript,
		"authoring_harness_report": report,
	}
	artifacts := make([]workflowkit.StageArtifact, 0, len(stage.Outputs))
	for _, output := range stage.Outputs {
		content, found := contents[output.Name]
		if !found || len(content) == 0 {
			return nil, fmt.Errorf("unsupported Standard authoring workspace output %q", output.Name)
		}
		artifacts = append(artifacts, workflowkit.StageArtifact{
			Name: output.Name, SchemaVersion: output.SchemaVersion, Content: append([]byte(nil), content...), TurnOrdinal: turn,
		})
	}
	return artifacts, nil
}

func (submission *standardAuthoringCodexWorkspaceSubmission) acceptedResult() (workflowkit.StageExecutionResult, bool) {
	if submission == nil {
		return workflowkit.StageExecutionResult{}, false
	}
	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.accepted == nil {
		return workflowkit.StageExecutionResult{}, false
	}
	return cloneStandardAuthoringCodexStageResult(submission.accepted.result), true
}

func (submission *standardAuthoringCodexWorkspaceSubmission) failure() string {
	if submission == nil {
		return ""
	}
	submission.mu.Lock()
	defer submission.mu.Unlock()
	return submission.failureCode
}

func (submission *standardAuthoringCodexWorkspaceSubmission) remainingLocked() int {
	remaining := submission.maxAttempts - submission.attempts
	if remaining < 0 {
		return 0
	}
	return remaining
}

func standardAuthoringCodexWorkspaceSubmissionResponse(accepted bool, diagnostics []string, remaining int, digest workflowkit.Fingerprint, result authoringharness.Result) (json.RawMessage, error) {
	if diagnostics == nil {
		diagnostics = []string{}
	}
	encoded, err := json.Marshal(standardAuthoringCodexWorkspaceSubmissionReceipt{
		Accepted: accepted, Errors: diagnostics, Remaining: remaining, Digest: digest,
		Step: result.Step, ExitCode: result.ExitCode, StdoutTail: result.StdoutTail, StderrTail: result.StderrTail, ImageReused: result.ImageReused,
	})
	return json.RawMessage(encoded), err
}

func standardAuthoringCodexWorkspaceUsageKey(request workflowkit.StageExecutionRequest, turn, attempt int, dimension string) string {
	return "standard-authoring-codex-workspace-" + dimension + ":" + standardAuthoringCodexExecutionKey(request, turn, "workspace-"+strconv.Itoa(attempt))
}
