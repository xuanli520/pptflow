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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringCodexSubmitToolName                 = "harbor_submit_stage_output"
	standardAuthoringCodexOutputSubmissionQuotaDimension = "output_submission"
	standardAuthoringCodexSubmissionFailureQuota         = "standard_authoring_codex_agent_turn.output_submission_quota"
	standardAuthoringCodexSubmissionFailureLease         = "standard_authoring_codex_agent_turn.output_submission_lease_lost"
	standardAuthoringCodexSubmissionFailureAccounting    = "standard_authoring_codex_agent_turn.output_submission_accounting"
	standardAuthoringCodexSubmissionFailureAbsent        = "standard_authoring_codex_agent_turn.output_submission_missing"

	// This host-only representation is the stable input to the receipt digest.
	// It records artifact identity only after the frozen StageDescriptor has
	// supplied it, rather than asking a model to repeat identity fields.
	standardAuthoringCodexCanonicalSubmissionFormat  = "harbor.standard-authoring-codex-stage-submission.v1"
	standardAuthoringCodexCanonicalSubmissionVersion = "1"

	// This is a separate, deployment-pinned schema for 1.8.0 solve/test
	// fixed-file receipts. It intentionally has no artifacts/content_base64
	// field: the host reads the exact fixed file after verdict=pass.
	standardAuthoringCodexFixedFileOutputSchemaCanonicalJSON = `{"$id":"harbor.standard-authoring-codex-fixed-file-submit.v1","$schema":"http://json-schema.org/draft-07/schema#","additionalProperties":false,"properties":{"verdict":{"enum":["pass"],"type":"string"}},"required":["verdict"],"type":"object"}`
)

// standardAuthoringCodexOutputSubmission owns the one in-memory authority for
// a candidate accepted during an ephemeral App Server conversation. Invalid
// candidates never leave this object: only their digest and stable diagnostic
// are returned to Codex. The caller publishes its accepted StageExecutionResult
// only after the App Server turn is over.
type standardAuthoringCodexOutputSubmission struct {
	mu sync.Mutex

	request     workflowkit.StageExecutionRequest
	stage       workflowkit.StageDescriptor
	maxBytes    int
	maxAttempts int
	now         func() time.Time

	// environmentPolicy is set only for dockerfile_generate after the executor
	// has verified the canonical frozen environment_policy input. Keeping it on
	// the submission authority prevents a model response from selecting its own
	// image after it has seen the policy in the first-turn request.
	environmentPolicy *workflowadapter.StandardAuthoringEnvironmentPolicy

	// fixedFileRelativePath is set only for the 1.8.0 workspace-backed solve
	// and test producers. It is selected from the frozen stage key by the
	// host, never supplied by the model. Those stages retain the ordinary
	// output-submission accounting and result ownership, but their tool carries
	// only verdict=pass and their bytes are read from this fixed workspace file.
	fixedFileRelativePath string
	taskRoot              string
	readFixedFile         func(string, string, int64) ([]byte, error)

	currentTurn int
	attempts    int
	accepted    *standardAuthoringCodexAcceptedOutput
	failureCode string
}

type standardAuthoringCodexAcceptedOutput struct {
	result workflowkit.StageExecutionResult
}

type standardAuthoringCodexSubmissionCandidate struct {
	// Pointers preserve the distinction between an omitted/null required value
	// and an explicit empty value. json.Decoder alone otherwise turns both into
	// zero values, which could accidentally make an absent base64 field look
	// like a valid empty artifact.
	Verdict   *workflowkit.Verdict                             `json:"verdict"`
	Artifacts *[]standardAuthoringCodexSubmissionCandidatePart `json:"artifacts"`
}

// The model deliberately cannot name an artifact, schema, stage, or path.
// Artifact identity comes only from the frozen StageDescriptor held by the
// host, while each array position supplies bytes for that declared output.
type standardAuthoringCodexSubmissionCandidatePart struct {
	ContentBase64 *string `json:"content_base64"`
}

type standardAuthoringCodexCanonicalSubmission struct {
	Format       string                                              `json:"format"`
	Version      string                                              `json:"version"`
	StageKey     workflowkit.StageKey                                `json:"stage_key"`
	StageVersion string                                              `json:"stage_version"`
	Verdict      workflowkit.Verdict                                 `json:"verdict"`
	Artifacts    []standardAuthoringCodexCanonicalSubmissionArtifact `json:"artifacts"`
}

type standardAuthoringCodexCanonicalSubmissionArtifact struct {
	Name          string `json:"name"`
	SchemaVersion string `json:"schema_version"`
	ContentBase64 string `json:"content_base64"`
}

type standardAuthoringCodexSubmissionReceipt struct {
	Accepted  bool                    `json:"accepted"`
	Errors    []string                `json:"errors"`
	Remaining int                     `json:"remaining"`
	Digest    workflowkit.Fingerprint `json:"digest"`
}

func newStandardAuthoringCodexOutputSubmission(request workflowkit.StageExecutionRequest, maxBytes int, maxAttempts int, now func() time.Time, environmentPolicy *workflowadapter.StandardAuthoringEnvironmentPolicy) (*standardAuthoringCodexOutputSubmission, error) {
	if maxBytes <= 0 || maxAttempts <= 0 || request.Charge == nil {
		return nil, errors.New("invalid Standard authoring Codex output submission configuration")
	}
	if err := request.Stage.Validate(); err != nil {
		return nil, fmt.Errorf("validate frozen Standard authoring Codex stage: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	if request.Stage.Key == workflowkit.StageKey(workflowadapter.DockerfileGen) {
		if environmentPolicy == nil || environmentPolicy.Validate() != nil {
			return nil, errors.New("invalid frozen Standard authoring Dockerfile environment policy")
		}
		policyCopy := *environmentPolicy
		environmentPolicy = &policyCopy
	} else {
		environmentPolicy = nil
	}
	return &standardAuthoringCodexOutputSubmission{
		request:  request,
		stage:    request.Stage.Clone(),
		maxBytes: maxBytes, maxAttempts: maxAttempts, now: now, environmentPolicy: environmentPolicy,
	}, nil
}

// newStandardAuthoringCodexFixedFileSubmission creates the submission
// authority for the two pre-harness scripts. The resulting stage artifact is
// read from an attempt-scoped fixed file rather than echoed back through the
// model tool call. This keeps arbitrary model output out of the artifact data
// plane while preserving the normal bounded output_submission quota.
func newStandardAuthoringCodexFixedFileSubmission(request workflowkit.StageExecutionRequest, taskRoot string, maxBytes int, maxAttempts int, now func() time.Time) (*standardAuthoringCodexOutputSubmission, error) {
	submission, err := newStandardAuthoringCodexOutputSubmission(request, maxBytes, maxAttempts, now, nil)
	if err != nil {
		return nil, err
	}
	if request.Execution.Workflow.ID != workflowadapter.StandardAuthoringWorkflowTemplateID || request.Execution.Workflow.Version != workflowadapter.StandardAuthoringFixedFileTemplateVersion {
		return nil, errors.New("Standard authoring Codex fixed-file submission requires template 1.8.0")
	}
	relative, outputName, _, ok := standardAuthoringCodexFixedFileStageContract(submission.stage)
	expected, found := workflowadapter.StandardAuthoringFixedFileStageCatalog().Stage(submission.stage.Key)
	if !ok || !found || submission.stage.Version != expected.Version || submission.stage.Plugin.ID != expected.Plugin.ID || submission.stage.Plugin.Version != expected.Plugin.Version ||
		!reflect.DeepEqual(submission.stage.Outputs, expected.Outputs) || !reflect.DeepEqual(submission.stage.Verdicts, expected.Verdicts) ||
		len(submission.stage.Outputs) != 1 || !submission.stage.Outputs[0].Required || submission.stage.Outputs[0].Name != outputName ||
		!reflect.DeepEqual(submission.stage.Verdicts, workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass}}) {
		return nil, errors.New("invalid Standard authoring Codex fixed-file stage contract")
	}
	absoluteTaskRoot, err := filepath.Abs(strings.TrimSpace(taskRoot))
	if err != nil || strings.TrimSpace(taskRoot) == "" || filepath.Clean(absoluteTaskRoot) != taskRoot {
		return nil, errors.New("invalid Standard authoring Codex fixed-file task workspace")
	}
	submission.fixedFileRelativePath = relative
	// Retain the trusted exact task root only after the constructor has proved
	// it is a clean absolute path. ReadFixedFile re-proves its directory and
	// individual file safety at every submission.
	submission.taskRoot = taskRoot
	submission.readFixedFile = authoringharness.ReadFixedFileWithLimit
	return submission, nil
}

// standardAuthoringCodexPrepareFixedFileWorkspace creates the one parent
// directory selected by the frozen solve/test stage before the model begins.
// The file itself deliberately does not exist yet: a pass without a newly
// authored regular file is rejected by the host reader rather than inheriting
// a deceptively valid placeholder.
func standardAuthoringCodexPrepareFixedFileWorkspace(taskRoot string, stage workflowkit.StageDescriptor) error {
	relative, _, _, ok := standardAuthoringCodexFixedFileStageContract(stage)
	if !ok {
		return errors.New("Standard authoring stage has no fixed-file workspace contract")
	}
	absoluteTaskRoot, err := filepath.Abs(strings.TrimSpace(taskRoot))
	if err != nil || strings.TrimSpace(taskRoot) == "" || filepath.Clean(absoluteTaskRoot) != taskRoot {
		return errors.New("invalid Standard authoring Codex fixed-file task workspace")
	}
	rootInfo, err := os.Lstat(taskRoot)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("Standard authoring Codex fixed-file task root is unsafe")
	}
	directory := filepath.Join(taskRoot, filepath.Dir(filepath.FromSlash(relative)))
	if err := os.Mkdir(directory, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Standard authoring Codex fixed-file directory: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return errors.New("Standard authoring Codex fixed-file directory is unsafe")
	}
	path := filepath.Join(taskRoot, filepath.FromSlash(relative))
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("Standard authoring Codex fixed-file candidate unexpectedly exists")
	}
	return nil
}

func (submission *standardAuthoringCodexOutputSubmission) beginTurn(turn int) error {
	if submission == nil || turn < 1 {
		return errors.New("Standard authoring Codex output submission turn is invalid")
	}
	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.failureCode != "" {
		return errors.New("Standard authoring Codex output submission is unavailable")
	}
	submission.currentTurn = turn
	return nil
}

func (submission *standardAuthoringCodexOutputSubmission) dynamicTool() agent.DynamicTool {
	description := standardAuthoringCodexSubmitToolDescription(submission.stage.Key)
	schema := standardAuthoringCodexSubmissionSchema(submission.stage)
	if submission != nil && submission.fixedFileRelativePath != "" {
		description = "Submit the host-selected fixed workspace file only after writing its final raw bytes under task/" + submission.fixedFileRelativePath + ". The only accepted argument is verdict=pass; artifact bytes, names, paths, schema, and validation are host-owned."
		schema = standardAuthoringCodexFixedFileSubmissionSchema()
	}
	return agent.DynamicTool{
		Name:        standardAuthoringCodexSubmitToolName,
		Description: description,
		InputSchema: schema,
		Handler:     submission.handle,
	}
}

func standardAuthoringCodexSubmitToolDescription(stageKey workflowkit.StageKey) string {
	base := "Validate and submit this stage's frozen output candidate. Submit only the allowed verdict and one base64 content value for each declared output, in declared order."
	switch stageKey {
	case workflowkit.StageKey(workflowadapter.InstructionGen), workflowkit.StageKey(workflowadapter.TaskTOMLGen),
		workflowkit.StageKey(workflowadapter.DockerfileGen), workflowkit.StageKey(workflowadapter.SolveGen), workflowkit.StageKey(workflowadapter.TestGen):
		return base + " The content_base64 value must encode the final raw file bytes themselves, never an extra JSON object, artifact-name, format/version, or content-field wrapper."
	case workflowkit.StageKey(workflowadapter.TestsAnalysis):
		return base + " The content_base64 value must encode exactly one JSON object with the non-empty string fields provided_information, theoretical_path, and passing_evidence, with no wrapper fields."
	default:
		return base
	}
}

// outputSchema is sent with turn/start as a first format barrier. It has the
// same closed, stage-derived candidate shape as the tool input, but it is not
// an authority: only a successful tool call can populate accepted.
func (submission *standardAuthoringCodexOutputSubmission) outputSchema() json.RawMessage {
	if submission == nil {
		return nil
	}
	if submission.fixedFileRelativePath != "" {
		return standardAuthoringCodexFixedFileSubmissionSchema()
	}
	return standardAuthoringCodexSubmissionSchema(submission.stage)
}

func standardAuthoringCodexFixedFileSubmissionSchema() json.RawMessage {
	return json.RawMessage(append([]byte(nil), standardAuthoringCodexFixedFileOutputSchemaTemplate()...))
}

func standardAuthoringCodexFixedFileOutputSchemaTemplate() []byte {
	return []byte(standardAuthoringCodexFixedFileOutputSchemaCanonicalJSON)
}

// StandardAuthoringCodexFixedFileOutputSchemaFingerprint identifies the
// exact JSON Schema asset that protects 1.8.0 fixed-file solve/test turns.
func StandardAuthoringCodexFixedFileOutputSchemaFingerprint() workflowkit.Fingerprint {
	fingerprint, err := workflowkit.FingerprintBytes("harbor.standard-authoring-codex-fixed-file-submit-schema.v1", standardAuthoringCodexFixedFileOutputSchemaTemplate())
	if err != nil {
		panic("fixed Standard authoring Codex file-submission schema fingerprint: " + err.Error())
	}
	return fingerprint
}

// ValidateStandardAuthoringCodexFixedFileOutputSchemaAsset accepts only the
// exact 1.8.0 deployment schema. The optional one terminal LF follows the
// same lock-bound POSIX text-file rule as the legacy Codex output schema.
func ValidateStandardAuthoringCodexFixedFileOutputSchemaAsset(raw []byte) error {
	if len(raw) == 0 || len(raw) > standardAuthoringCodexContractAssetLimit {
		return fmt.Errorf("%w: fixed-file output schema asset has invalid size", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return fmt.Errorf("%w: fixed-file output schema asset has duplicate fields", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if !json.Valid(raw) || !bytes.Equal(standardAuthoringCodexCanonicalAssetBody(raw), standardAuthoringCodexFixedFileOutputSchemaTemplate()) {
		return fmt.Errorf("%w: fixed-file output schema asset is not the locked JSON Schema template", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return nil
}

// ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage selects the
// one schema that may be used for a frozen template/stage pair. 1.7.0
// solve/test remains pinned to the legacy base64 schema; only 1.8.0 maps
// those two keys to the fixed-file schema.
func ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(template workflowadapter.TemplateReference, stageKey workflowkit.StageKey, raw []byte) error {
	if standardAuthoringCodexUsesFixedFileOutputSchema(template, stageKey) {
		return ValidateStandardAuthoringCodexFixedFileOutputSchemaAsset(raw)
	}
	return ValidateStandardAuthoringCodexOutputSchemaAsset(raw)
}

func StandardAuthoringCodexOutputSchemaFingerprintForTemplateStage(template workflowadapter.TemplateReference, stageKey workflowkit.StageKey) workflowkit.Fingerprint {
	if standardAuthoringCodexUsesFixedFileOutputSchema(template, stageKey) {
		return StandardAuthoringCodexFixedFileOutputSchemaFingerprint()
	}
	return StandardAuthoringCodexOutputSchemaFingerprint()
}

func standardAuthoringCodexUsesFixedFileOutputSchema(template workflowadapter.TemplateReference, stageKey workflowkit.StageKey) bool {
	return template.Equal(workflowadapter.StandardAuthoringFixedFileTemplateReference()) && standardAuthoringCodexFixedFileStageKey(stageKey)
}

func standardAuthoringCodexSubmissionSchema(stage workflowkit.StageDescriptor) json.RawMessage {
	verdicts := append([]workflowkit.Verdict(nil), stage.Verdicts.Allowed...)
	sort.Slice(verdicts, func(left, right int) bool { return verdicts[left] < verdicts[right] })
	values := make([]string, 0, len(verdicts))
	for _, verdict := range verdicts {
		values = append(values, string(verdict))
	}
	// Start with the same JSON Schema template whose exact bytes are verified
	// from the deployment lock. The only mutations are constraints that cannot
	// live in a shared static asset because they belong to this frozen stage.
	var schema map[string]any
	if err := json.Unmarshal(standardAuthoringCodexOutputSchemaTemplate(), &schema); err != nil {
		panic("decode fixed Standard authoring Codex output schema: " + err.Error())
	}
	properties, propertiesOK := schema["properties"].(map[string]any)
	verdict, verdictOK := properties["verdict"].(map[string]any)
	artifacts, artifactsOK := properties["artifacts"].(map[string]any)
	if !propertiesOK || !verdictOK || !artifactsOK {
		panic("fixed Standard authoring Codex output schema has an invalid shape")
	}
	verdict["enum"] = values
	artifacts["minItems"] = len(stage.Outputs)
	artifacts["maxItems"] = len(stage.Outputs)
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic("marshal fixed Standard authoring Codex submission schema: " + err.Error())
	}
	return json.RawMessage(encoded)
}

func (submission *standardAuthoringCodexOutputSubmission) handle(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	digest := workflowkit.SHA256Fingerprint(raw)
	if submission == nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_unavailable"}, 0, digest)
	}

	submission.mu.Lock()
	if submission.accepted != nil {
		remaining := submission.remainingLocked()
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"already_accepted"}, remaining, digest)
	}
	if submission.failureCode != "" {
		remaining := submission.remainingLocked()
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_unavailable"}, remaining, digest)
	}
	if submission.attempts >= submission.maxAttempts {
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"submit_attempts_exhausted"}, 0, digest)
	}
	submission.attempts++
	attempt := submission.attempts
	turn := submission.currentTurn
	remaining := submission.remainingLocked()
	submission.mu.Unlock()

	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, remaining, digest)
	}
	if turn < 1 {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_unavailable"}, remaining, digest)
	}
	if err := submission.request.Charge(ctx, workflowkit.StageUsage{
		OperationKey: standardAuthoringCodexSubmissionUsageKey(submission.request, turn, attempt),
		Dimension:    standardAuthoringCodexOutputSubmissionQuotaDimension,
		Units:        1,
		OccurredAt:   submission.now().UTC(),
	}); err != nil {
		if contextError(ctx) != nil {
			return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, remaining, digest)
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
		return standardAuthoringCodexSubmissionResponse(false, []string{diagnostic}, remaining, digest)
	}
	// invokeDynamicTool returns as soon as the App Server turn context expires,
	// while a handler that was already scheduled may still be unwinding. Do not
	// let a completed quota charge turn into an accepted artifact after that
	// timeout; an expired call is never a submission authority.
	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, remaining, digest)
	}
	if len(raw) == 0 || len(raw) > submission.maxBytes {
		return standardAuthoringCodexSubmissionResponse(false, []string{"byte_limit_exceeded"}, remaining, digest)
	}
	if submission.fixedFileRelativePath != "" {
		return submission.handleFixedFileCandidate(ctx, raw, turn, remaining, digest)
	}

	result, canonicalDigest, diagnostic := standardAuthoringCodexValidateSubmissionCandidate(raw, submission.stage, turn, submission.environmentPolicy)
	if diagnostic != "" {
		return standardAuthoringCodexSubmissionResponse(false, []string{diagnostic}, remaining, digest)
	}

	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.accepted != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"already_accepted"}, submission.remainingLocked(), digest)
	}
	// Keep the final cancellation check under the same mutex that protects the
	// sole accepted result. This closes the delayed-handler path where a later
	// turn has started after the original App Server call timed out.
	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, submission.remainingLocked(), digest)
	}
	submission.accepted = &standardAuthoringCodexAcceptedOutput{result: cloneStandardAuthoringCodexStageResult(result)}
	return standardAuthoringCodexSubmissionResponse(true, nil, submission.remainingLocked(), canonicalDigest)
}

// handleFixedFileCandidate admits only a pass receipt for the host-selected
// fixed file. The second safe read closes the gap between structural checking
// and publishing the immutable StageArtifact: an edit after validation must
// be submitted and checked again, never silently become the accepted bytes.
func (submission *standardAuthoringCodexOutputSubmission) handleFixedFileCandidate(ctx context.Context, raw json.RawMessage, turn, remaining int, rawDigest workflowkit.Fingerprint) (json.RawMessage, error) {
	if !standardAuthoringCodexFixedFilePassCandidate(raw) {
		return standardAuthoringCodexSubmissionResponse(false, []string{"wrong_verdict"}, remaining, rawDigest)
	}
	_, outputName, _, ok := standardAuthoringCodexFixedFileStageContract(submission.stage)
	if !ok || submission.fixedFileRelativePath == "" || submission.taskRoot == "" {
		submission.mu.Lock()
		submission.failureCode = standardAuthoringCodexFailureConfiguration
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_unavailable"}, remaining, rawDigest)
	}
	if submission.readFixedFile == nil {
		submission.mu.Lock()
		submission.failureCode = standardAuthoringCodexFailureConfiguration
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_unavailable"}, remaining, rawDigest)
	}
	candidate, err := submission.readFixedFile(submission.taskRoot, submission.fixedFileRelativePath, int64(submission.maxBytes))
	if err != nil {
		if errors.Is(err, authoringharness.ErrFixedFileExceedsLimit) {
			return standardAuthoringCodexSubmissionResponse(false, []string{"byte_limit_exceeded"}, remaining, rawDigest)
		}
		return standardAuthoringCodexSubmissionResponse(false, []string{"candidate_unavailable"}, remaining, rawDigest)
	}
	if len(candidate) > submission.maxBytes {
		return standardAuthoringCodexSubmissionResponse(false, []string{"byte_limit_exceeded"}, remaining, workflowkit.SHA256Fingerprint(candidate))
	}
	artifact := workflowkit.StageArtifact{
		Name: outputName, SchemaVersion: submission.stage.Outputs[0].SchemaVersion, Content: append([]byte(nil), candidate...), TurnOrdinal: turn,
	}
	if contentDiagnostic := standardAuthoringCodexArtifactContentDiagnostic(submission.stage.Key, []workflowkit.StageArtifact{artifact}); contentDiagnostic != "" {
		return standardAuthoringCodexSubmissionResponse(false, []string{contentDiagnostic}, remaining, workflowkit.SHA256Fingerprint(candidate))
	}
	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, remaining, workflowkit.SHA256Fingerprint(candidate))
	}
	acceptedContent, err := submission.readFixedFile(submission.taskRoot, submission.fixedFileRelativePath, int64(submission.maxBytes))
	if errors.Is(err, authoringharness.ErrFixedFileExceedsLimit) {
		return standardAuthoringCodexSubmissionResponse(false, []string{"byte_limit_exceeded"}, remaining, workflowkit.SHA256Fingerprint(candidate))
	}
	if err != nil || !bytes.Equal(candidate, acceptedContent) {
		return standardAuthoringCodexSubmissionResponse(false, []string{"candidate_changed_after_validation"}, remaining, workflowkit.SHA256Fingerprint(candidate))
	}
	if len(acceptedContent) > submission.maxBytes {
		return standardAuthoringCodexSubmissionResponse(false, []string{"byte_limit_exceeded"}, remaining, workflowkit.SHA256Fingerprint(acceptedContent))
	}
	result := workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass},
		Artifacts: []workflowkit.StageArtifact{{
			Name: outputName, SchemaVersion: submission.stage.Outputs[0].SchemaVersion, Content: append([]byte(nil), acceptedContent...), TurnOrdinal: turn,
		}},
	}
	candidateDigest := workflowkit.SHA256Fingerprint(acceptedContent)

	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.accepted != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"already_accepted"}, submission.remainingLocked(), candidateDigest)
	}
	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, submission.remainingLocked(), candidateDigest)
	}
	submission.accepted = &standardAuthoringCodexAcceptedOutput{result: cloneStandardAuthoringCodexStageResult(result)}
	return standardAuthoringCodexSubmissionResponse(true, nil, submission.remainingLocked(), candidateDigest)
}

type standardAuthoringCodexFixedFileSubmissionCandidate struct {
	Verdict *workflowkit.Verdict `json:"verdict"`
}

func standardAuthoringCodexFixedFilePassCandidate(raw []byte) bool {
	if len(raw) == 0 || rejectDuplicateDeploymentCatalogJSONKeys(raw) != nil {
		return false
	}
	var candidate standardAuthoringCodexFixedFileSubmissionCandidate
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

// standardAuthoringCodexFixedFileStageContract is deliberately closed to the
// two pre-harness script producers. It is the only path authority for the
// fixed-file submission mechanism.
func standardAuthoringCodexFixedFileStageContract(stage workflowkit.StageDescriptor) (relative, outputName, diagnostic string, ok bool) {
	switch stage.Key {
	case workflowkit.StageKey(workflowadapter.SolveGen):
		return authoringharness.SolveScriptRelativePath, "solve_script", "solve_script_invalid", true
	case workflowkit.StageKey(workflowadapter.TestGen):
		return authoringharness.TestScriptRelativePath, "test_script", "test_script_invalid", true
	default:
		return "", "", "", false
	}
}

func standardAuthoringCodexFixedFileStageKey(stageKey workflowkit.StageKey) bool {
	switch stageKey {
	case workflowkit.StageKey(workflowadapter.SolveGen), workflowkit.StageKey(workflowadapter.TestGen):
		return true
	default:
		return false
	}
}

func (submission *standardAuthoringCodexOutputSubmission) acceptedResult() (workflowkit.StageExecutionResult, bool) {
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

func (submission *standardAuthoringCodexOutputSubmission) failure() string {
	if submission == nil {
		return ""
	}
	submission.mu.Lock()
	defer submission.mu.Unlock()
	return submission.failureCode
}

func (submission *standardAuthoringCodexOutputSubmission) remainingLocked() int {
	remaining := submission.maxAttempts - submission.attempts
	if remaining < 0 {
		return 0
	}
	return remaining
}

func standardAuthoringCodexSubmissionResponse(accepted bool, diagnostics []string, remaining int, digest workflowkit.Fingerprint) (json.RawMessage, error) {
	if diagnostics == nil {
		diagnostics = []string{}
	}
	encoded, err := json.Marshal(standardAuthoringCodexSubmissionReceipt{
		Accepted: accepted, Errors: diagnostics, Remaining: remaining, Digest: digest,
	})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func standardAuthoringCodexValidateSubmissionCandidate(raw []byte, stage workflowkit.StageDescriptor, turnOrdinal int, environmentPolicy *workflowadapter.StandardAuthoringEnvironmentPolicy) (workflowkit.StageExecutionResult, workflowkit.Fingerprint, string) {
	if turnOrdinal < 1 {
		return workflowkit.StageExecutionResult{}, "", "submission_unavailable"
	}
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return workflowkit.StageExecutionResult{}, "", "invalid_json"
	}
	var candidate standardAuthoringCodexSubmissionCandidate
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		return workflowkit.StageExecutionResult{}, "", "invalid_json"
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return workflowkit.StageExecutionResult{}, "", "invalid_json"
	}
	if candidate.Verdict == nil || !stage.Verdicts.Allows(*candidate.Verdict) {
		return workflowkit.StageExecutionResult{}, "", "wrong_verdict"
	}
	if candidate.Artifacts == nil || len(*candidate.Artifacts) != len(stage.Outputs) {
		return workflowkit.StageExecutionResult{}, "", "artifact_identity_mismatch"
	}

	canonical := standardAuthoringCodexCanonicalSubmission{
		Format:       standardAuthoringCodexCanonicalSubmissionFormat,
		Version:      standardAuthoringCodexCanonicalSubmissionVersion,
		StageKey:     stage.Key,
		StageVersion: stage.Version,
		Verdict:      *candidate.Verdict,
		Artifacts:    make([]standardAuthoringCodexCanonicalSubmissionArtifact, 0, len(*candidate.Artifacts)),
	}
	artifacts := make([]workflowkit.StageArtifact, 0, len(*candidate.Artifacts))
	for index, part := range *candidate.Artifacts {
		if part.ContentBase64 == nil {
			return workflowkit.StageExecutionResult{}, "", "invalid_content_encoding"
		}
		canonicalContentBase64, content, err := canonicalStandardAuthoringCodexBase64(*part.ContentBase64)
		if err != nil {
			return workflowkit.StageExecutionResult{}, "", "invalid_content_encoding"
		}
		specification := stage.Outputs[index]
		canonical.Artifacts = append(canonical.Artifacts, standardAuthoringCodexCanonicalSubmissionArtifact{
			Name: specification.Name, SchemaVersion: specification.SchemaVersion, ContentBase64: canonicalContentBase64,
		})
		artifacts = append(artifacts, workflowkit.StageArtifact{
			Name: specification.Name, SchemaVersion: specification.SchemaVersion, Content: append([]byte(nil), content...), TurnOrdinal: turnOrdinal,
		})
	}
	if *candidate.Verdict == workflowkit.VerdictPass {
		if stage.Key == workflowkit.StageKey(workflowadapter.DockerfileGen) {
			if environmentPolicy == nil || len(artifacts) != 1 || artifacts[0].Name != "dockerfile" {
				return workflowkit.StageExecutionResult{}, "", "dockerfile_environment_policy_mismatch"
			}
			if err := workflowadapter.ValidateDockerfileBaseImage(artifacts[0].Content, *environmentPolicy); err != nil {
				return workflowkit.StageExecutionResult{}, "", "dockerfile_environment_policy_mismatch"
			}
		}
		if diagnostic := standardAuthoringCodexArtifactContentDiagnostic(stage.Key, artifacts); diagnostic != "" {
			return workflowkit.StageExecutionResult{}, "", diagnostic
		}
	}
	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		return workflowkit.StageExecutionResult{}, "", "invalid_json"
	}
	return workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: *candidate.Verdict}, Artifacts: artifacts,
	}, workflowkit.SHA256Fingerprint(canonicalBytes), ""
}

func standardAuthoringCodexArtifactContentDiagnostic(stageKey workflowkit.StageKey, artifacts []workflowkit.StageArtifact) string {
	if len(artifacts) != 1 {
		return ""
	}
	content := artifacts[0].Content
	switch stageKey {
	case workflowkit.StageKey(workflowadapter.InstructionGen):
		if artifacts[0].Name != "instruction" || !standardAuthoringCodexRawInstruction(content) {
			return "instruction_invalid"
		}
	case workflowkit.StageKey(workflowadapter.TaskTOMLGen):
		if artifacts[0].Name != "task_toml" || !standardAuthoringCodexTaskTOML(content) {
			return "task_toml_invalid"
		}
	case workflowkit.StageKey(workflowadapter.SolveGen):
		if artifacts[0].Name != "solve_script" || !standardAuthoringCodexShellScript(content) {
			return "solve_script_invalid"
		}
	case workflowkit.StageKey(workflowadapter.TestGen):
		if artifacts[0].Name != "test_script" || !standardAuthoringCodexShellScript(content) {
			return "test_script_invalid"
		}
	case workflowkit.StageKey(workflowadapter.TestsAnalysis):
		if artifacts[0].Name != "tests_analysis" || !standardAuthoringCodexTestsAnalysis(content) {
			return "tests_analysis_invalid"
		}
	}
	return ""
}

func standardAuthoringCodexRawInstruction(content []byte) bool {
	if !standardAuthoringCodexText(content) {
		return false
	}
	trimmed := bytes.TrimSpace(content)
	if trimmed[0] == '{' || bytes.HasPrefix(trimmed, []byte("```")) {
		return false
	}
	return trimmed[0] != '[' || !json.Valid(trimmed)
}

func standardAuthoringCodexTaskTOML(content []byte) bool {
	return taskpolicy.ValidateStandardAuthoringTaskTOML(content) == nil
}

func standardAuthoringCodexShellScript(content []byte) bool {
	if !standardAuthoringCodexText(content) || !bytes.HasPrefix(content, []byte("#!")) {
		return false
	}
	lineEnd := bytes.IndexByte(content, '\n')
	if lineEnd < 3 || bytes.IndexByte(content[:lineEnd], '\r') >= 0 {
		return false
	}
	return strings.TrimSpace(string(content[lineEnd+1:])) != ""
}

type standardAuthoringCodexTestsAnalysisCandidate struct {
	ProvidedInformation *string `json:"provided_information"`
	TheoreticalPath     *string `json:"theoretical_path"`
	PassingEvidence     *string `json:"passing_evidence"`
}

func standardAuthoringCodexTestsAnalysis(content []byte) bool {
	if len(content) == 0 || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 || rejectDuplicateDeploymentCatalogJSONKeys(content) != nil {
		return false
	}
	var candidate standardAuthoringCodexTestsAnalysisCandidate
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		return false
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false
	}
	return candidate.ProvidedInformation != nil && standardAuthoringCodexNonEmptyText(*candidate.ProvidedInformation) &&
		candidate.TheoreticalPath != nil && standardAuthoringCodexNonEmptyText(*candidate.TheoreticalPath) &&
		candidate.PassingEvidence != nil && standardAuthoringCodexNonEmptyText(*candidate.PassingEvidence)
}

func standardAuthoringCodexText(content []byte) bool {
	return len(bytes.TrimSpace(content)) != 0 && utf8.Valid(content) && bytes.IndexByte(content, 0) < 0
}

func standardAuthoringCodexNonEmptyText(content string) bool {
	return strings.TrimSpace(content) != "" && utf8.ValidString(content) && !strings.ContainsRune(content, '\x00')
}

// canonicalStandardAuthoringCodexBase64 accepts the line-oriented output that
// common shell tooling emits while keeping the stored identity strict. ASCII
// whitespace is transport framing, so it is removed before decoding; the
// decoded bytes must still round-trip to the standard, padded base64 spelling.
func canonicalStandardAuthoringCodexBase64(input string) (string, []byte, error) {
	normalized := make([]byte, 0, len(input))
	for index := 0; index < len(input); index++ {
		switch input[index] {
		case ' ', '\t', '\r', '\n', '\v', '\f':
			continue
		default:
			normalized = append(normalized, input[index])
		}
	}

	content, err := base64.StdEncoding.DecodeString(string(normalized))
	if err != nil {
		return "", nil, err
	}
	canonical := base64.StdEncoding.EncodeToString(content)
	if canonical != string(normalized) {
		return "", nil, errors.New("base64 content is not canonical")
	}
	return canonical, content, nil
}

func cloneStandardAuthoringCodexStageResult(result workflowkit.StageExecutionResult) workflowkit.StageExecutionResult {
	copyResult := result
	copyResult.Artifacts = make([]workflowkit.StageArtifact, len(result.Artifacts))
	for index, artifact := range result.Artifacts {
		copyResult.Artifacts[index] = artifact
		copyResult.Artifacts[index].Content = append([]byte(nil), artifact.Content...)
	}
	return copyResult
}

func standardAuthoringCodexSubmissionUsageKey(request workflowkit.StageExecutionRequest, turn, attempt int) string {
	return "standard-authoring-codex-output-submission:" + standardAuthoringCodexExecutionKey(request, turn, "submission-"+strconv.Itoa(attempt))
}
