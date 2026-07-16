package stageprovider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringCodexSubmitToolName                 = "harbor_submit_stage_output"
	standardAuthoringCodexOutputSubmissionQuotaDimension = "output_submission"
	standardAuthoringCodexSubmissionFailureQuota         = "standard_authoring_codex_agent_turn.output_submission_quota"
	standardAuthoringCodexSubmissionFailureAbsent        = "standard_authoring_codex_agent_turn.output_submission_missing"

	// This host-only representation is the stable input to the receipt digest.
	// It records artifact identity only after the frozen StageDescriptor has
	// supplied it, rather than asking a model to repeat identity fields.
	standardAuthoringCodexCanonicalSubmissionFormat  = "harbor.standard-authoring-codex-stage-submission.v1"
	standardAuthoringCodexCanonicalSubmissionVersion = "1"
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

func newStandardAuthoringCodexOutputSubmission(request workflowkit.StageExecutionRequest, maxBytes int, maxAttempts int, now func() time.Time) (*standardAuthoringCodexOutputSubmission, error) {
	if maxBytes <= 0 || maxAttempts <= 0 || request.Charge == nil {
		return nil, errors.New("invalid Standard authoring Codex output submission configuration")
	}
	if err := request.Stage.Validate(); err != nil {
		return nil, fmt.Errorf("validate frozen Standard authoring Codex stage: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	return &standardAuthoringCodexOutputSubmission{
		request:  request,
		stage:    request.Stage.Clone(),
		maxBytes: maxBytes, maxAttempts: maxAttempts, now: now,
	}, nil
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
	return agent.DynamicTool{
		Name:        standardAuthoringCodexSubmitToolName,
		Description: "Validate and submit this stage's frozen output candidate. Submit only the allowed verdict and one base64 content value for each declared output, in declared order.",
		InputSchema: standardAuthoringCodexSubmissionSchema(submission.stage),
		Handler:     submission.handle,
	}
}

// outputSchema is sent with turn/start as a first format barrier. It has the
// same closed, stage-derived candidate shape as the tool input, but it is not
// an authority: only a successful tool call can populate accepted.
func (submission *standardAuthoringCodexOutputSubmission) outputSchema() json.RawMessage {
	if submission == nil {
		return nil
	}
	return standardAuthoringCodexSubmissionSchema(submission.stage)
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
		submission.mu.Lock()
		submission.failureCode = standardAuthoringCodexSubmissionFailureQuota
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_quota_exhausted"}, remaining, digest)
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

	result, canonicalDigest, diagnostic := standardAuthoringCodexValidateSubmissionCandidate(raw, submission.stage, turn)
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

func standardAuthoringCodexValidateSubmissionCandidate(raw []byte, stage workflowkit.StageDescriptor, turnOrdinal int) (workflowkit.StageExecutionResult, workflowkit.Fingerprint, string) {
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
		content, err := base64.StdEncoding.DecodeString(*part.ContentBase64)
		if err != nil || base64.StdEncoding.EncodeToString(content) != *part.ContentBase64 {
			return workflowkit.StageExecutionResult{}, "", "invalid_content_encoding"
		}
		specification := stage.Outputs[index]
		canonical.Artifacts = append(canonical.Artifacts, standardAuthoringCodexCanonicalSubmissionArtifact{
			Name: specification.Name, SchemaVersion: specification.SchemaVersion, ContentBase64: *part.ContentBase64,
		})
		artifacts = append(artifacts, workflowkit.StageArtifact{
			Name: specification.Name, SchemaVersion: specification.SchemaVersion, Content: append([]byte(nil), content...), TurnOrdinal: turnOrdinal,
		})
	}
	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		return workflowkit.StageExecutionResult{}, "", "invalid_json"
	}
	return workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: *candidate.Verdict}, Artifacts: artifacts,
	}, workflowkit.SHA256Fingerprint(canonicalBytes), ""
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
