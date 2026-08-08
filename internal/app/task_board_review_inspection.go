package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// A board projection carries only enough of a review gate to route a decision:
// its kind and request ID. That is not enough to decide anything, which is why
// the terminal could show that a gate was open but not what it was about.
//
// This file adds the read-only inspection an operator needs: which stage opened
// the gate, exactly which immutable artifacts it froze as its inputs, what was
// already decided, and the agent findings that exist for it.
const (
	// TaskBoardReviewFindingBodyLimit bounds one agent finding body. A finding is
	// small structured data today, but the limit is enforced at the read so a
	// larger artifact can never be pushed whole into a terminal.
	TaskBoardReviewFindingBodyLimit = 32 * 1024
	// taskBoardReviewFindingTruncationNote is appended to a clipped body so a reader
	// can never mistake a partial finding for the complete one.
	taskBoardReviewFindingTruncationNote = "\n…（正文超出 32KiB 上限，已截断）"
	// taskBoardReviewDiagnosticTailLimit bounds one command tail. A container
	// build log can be megabytes; the tail an operator needs to act on is the
	// last few lines, so the rest is never carried into a terminal.
	taskBoardReviewDiagnosticTailLimit = 1024
	// taskBoardReviewDiagnosticLimit bounds how many failing checks one finding
	// contributes, so a receipt reporting every command as failed cannot flood
	// the gate screen.
	taskBoardReviewDiagnosticLimit = 8
)

// TaskBoardInspectReviewRequest selects one open review gate previously seen in
// a board projection. The service re-reads its durable state; it never trusts
// the caller's copy as the authority.
type TaskBoardInspectReviewRequest struct {
	TaskID string
	Review TaskBoardReview
}

// TaskBoardReviewInspection is the complete read-only readout for one review
// gate. It contains no decision authority: DecideReview still captures and
// validates its own checkpoint before writing anything.
type TaskBoardReviewInspection struct {
	Kind      TaskBoardReviewKind `json:"kind"`
	RequestID string              `json:"request_id"`
	// RevisionID is present only for a TaskRevision review.
	RevisionID string `json:"revision_id,omitempty"`
	// StageKey is the workflow stage whose attempt opened this gate.
	StageKey string `json:"stage_key,omitempty"`
	// ReviewKind is the frozen catalog review role, e.g. task_direction.
	ReviewKind string     `json:"review_kind,omitempty"`
	RunID      string     `json:"run_id,omitempty"`
	State      string     `json:"state,omitempty"`
	OpenedAt   *time.Time `json:"opened_at,omitempty"`
	OpenedBy   string     `json:"opened_by,omitempty"`
	// Artifacts is the frozen input binding set the gate must be decided
	// against, decoded from the durable binding record.
	Artifacts []TaskBoardReviewArtifact `json:"artifacts,omitempty"`
	// PriorDecisions is the immutable decision history for this request.
	PriorDecisions []TaskBoardReviewDecisionRecord `json:"prior_decisions,omitempty"`
	// AgentFindings are the critic outputs bound to this Run. They are read from
	// the Run's own stage attempts, not from the gate's input bindings: critic
	// findings are an optional repair input and are never a gate input.
	AgentFindings []TaskBoardReviewFinding `json:"agent_findings,omitempty"`
	// AgentFindingsMessage explains an empty or partial AgentFindings list so a
	// gate that legitimately has no agent opinion does not look broken.
	AgentFindingsMessage string `json:"agent_findings_message,omitempty"`
	// Checkpoint identity an operator can compare against a later decision.
	InputFingerprint       string `json:"input_fingerprint,omitempty"`
	EvidenceManifestDigest string `json:"evidence_manifest_digest,omitempty"`
	BindingFingerprint     string `json:"binding_fingerprint,omitempty"`
	DefinitionHash         string `json:"definition_hash,omitempty"`
	// ArtifactsMessage reports why the artifact list is incomplete without
	// failing the whole read.
	ArtifactsMessage string `json:"artifacts_message,omitempty"`
}

// TaskBoardReviewArtifact is one immutable artifact the gate froze as input.
type TaskBoardReviewArtifact struct {
	Name          string `json:"name"`
	ArtifactID    string `json:"artifact_id,omitempty"`
	ContentDigest string `json:"content_digest,omitempty"`
	SchemaVersion string `json:"schema_version,omitempty"`
}

// TaskBoardReviewDecisionRecord is one immutable prior decision.
type TaskBoardReviewDecisionRecord struct {
	Action    string    `json:"action"`
	Actor     string    `json:"actor,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskBoardReviewFinding is one critic finding produced for this Run.
type TaskBoardReviewFinding struct {
	ArtifactKey     string `json:"artifact_key"`
	StageKey        string `json:"stage_key,omitempty"`
	Code            string `json:"code,omitempty"`
	ProducingStage  string `json:"producing_stage,omitempty"`
	TargetWriter    string `json:"target_writer,omitempty"`
	EvidenceDigest  string `json:"evidence_digest,omitempty"`
	CandidateDigest string `json:"candidate_digest,omitempty"`
	ArtifactID      string `json:"artifact_id,omitempty"`
	// Body is the verbatim finding text, bounded by TaskBoardReviewFindingBodyLimit.
	Body string `json:"body,omitempty"`
	// BodyTruncated marks a body that was clipped at the limit.
	BodyTruncated bool `json:"body_truncated,omitempty"`
	// RecordedAt is when the producing stage attempt finished.
	RecordedAt *time.Time `json:"recorded_at,omitempty"`
	// Message reports why this finding's body is unavailable.
	Message string `json:"message,omitempty"`
	// Diagnostics are the failing checks from the validation receipt this
	// finding cites through its DiagnosticDigest. A WorkflowFinding carries no
	// prose by design, so this is the only human-readable account of why a
	// critic objected. Without it the gate screen could show a finding code and
	// a digest but never the reason behind either.
	Diagnostics []TaskBoardReviewDiagnostic `json:"diagnostics,omitempty"`
	// DiagnosticSummary counts the checks behind Diagnostics, so a reader can
	// tell "1 of 6 failed" from "6 of 6 failed" without expanding anything.
	DiagnosticSummary string `json:"diagnostic_summary,omitempty"`
	// DiagnosticMessage explains why Diagnostics is empty when a digest was
	// cited, rather than leaving the section silently blank.
	DiagnosticMessage string `json:"diagnostic_message,omitempty"`
}

// TaskBoardReviewDiagnostic is one failing check from the validation receipt a
// finding cites. It is the actionable content an operator decides against.
type TaskBoardReviewDiagnostic struct {
	CommandID   string `json:"command_id"`
	ExitCode    int    `json:"exit_code"`
	TestStarted bool   `json:"test_started"`
	// StdoutTail and StderrTail are bounded by taskBoardReviewDiagnosticTailLimit.
	StdoutTail string `json:"stdout_tail,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
}

// InspectReview returns the full read-only readout for one open review gate.
func (service *TaskBoardService) InspectReview(ctx context.Context, request TaskBoardInspectReviewRequest) (TaskBoardReviewInspection, error) {
	if service == nil || service.core == nil || service.core.store == nil || service.inspection == nil {
		return TaskBoardReviewInspection{}, fmt.Errorf("task board service is not configured")
	}
	request.TaskID = strings.TrimSpace(request.TaskID)
	requestID := strings.TrimSpace(request.Review.RequestID)
	if requestID == "" {
		return TaskBoardReviewInspection{}, fmt.Errorf("task board review request ID is required")
	}
	detail, err := service.taskBoardRun(ctx, request.TaskID, "")
	if err != nil {
		return TaskBoardReviewInspection{}, err
	}
	switch request.Review.Kind {
	case TaskBoardAuthoringReview:
		return service.inspectAuthoringReview(ctx, detail, requestID)
	case TaskBoardRevisionReview:
		return inspectRevisionReview(detail, requestID)
	default:
		return TaskBoardReviewInspection{}, fmt.Errorf("unsupported task board review kind %q", request.Review.Kind)
	}
}

func (service *TaskBoardService) inspectAuthoringReview(ctx context.Context, detail TaskInspectionSnapshot, requestID string) (TaskBoardReviewInspection, error) {
	for _, review := range detail.AuthoringReviews {
		if review.Request.ID != requestID {
			continue
		}
		openedAt := review.Request.CreatedAt.UTC()
		inspection := TaskBoardReviewInspection{
			Kind:                   TaskBoardAuthoringReview,
			RequestID:              review.Request.ID,
			StageKey:               review.Binding.StageKey,
			ReviewKind:             review.Binding.ReviewKind,
			RunID:                  review.Request.RunID,
			State:                  string(review.State),
			OpenedAt:               &openedAt,
			OpenedBy:               review.Request.CreatedBy,
			InputFingerprint:       review.Binding.InputFingerprint,
			EvidenceManifestDigest: review.Binding.EvidenceManifestDigest,
			BindingFingerprint:     review.Binding.BindingFingerprint,
			DefinitionHash:         review.Binding.DefinitionHash,
		}
		artifacts, artifactErr := decodeTaskBoardReviewArtifacts(review.Binding.InputBindingsJSON)
		if artifactErr != nil {
			// A binding that cannot be decoded is a real diagnostic. Report it
			// rather than showing an empty artifact list that looks intentional.
			inspection.ArtifactsMessage = "待审产物清单不可读: " + artifactErr.Error()
		}
		inspection.Artifacts = artifacts
		for _, decision := range review.Decisions {
			inspection.PriorDecisions = append(inspection.PriorDecisions, TaskBoardReviewDecisionRecord{
				Action: string(decision.Action), Actor: decision.Actor, Reason: decision.Reason,
				CreatedAt: decision.CreatedAt.UTC(),
			})
		}
		inspection.AgentFindings, inspection.AgentFindingsMessage = service.readTaskBoardReviewFindings(ctx, detail, review.Binding)
		return inspection, nil
	}
	return TaskBoardReviewInspection{}, fmt.Errorf("%w: authoring review request %s", ErrLifecycleNotFound, requestID)
}

func inspectRevisionReview(detail TaskInspectionSnapshot, requestID string) (TaskBoardReviewInspection, error) {
	for _, review := range detail.Reviews {
		if review.Request.ID != requestID {
			continue
		}
		openedAt := review.Request.CreatedAt.UTC()
		inspection := TaskBoardReviewInspection{
			Kind:                   TaskBoardRevisionReview,
			RequestID:              review.Request.ID,
			RevisionID:             review.Request.RevisionID,
			State:                  review.Request.State,
			OpenedAt:               &openedAt,
			OpenedBy:               review.Request.CreatedBy,
			EvidenceManifestDigest: review.Request.EvidenceManifestDigest,
			// A TaskRevision review is decided against a revision digest rather
			// than a source-session artifact binding set.
			AgentFindingsMessage: "任务修订审核不产生 agent 评审意见",
		}
		for _, decision := range review.Decisions {
			inspection.PriorDecisions = append(inspection.PriorDecisions, TaskBoardReviewDecisionRecord{
				Action: string(decision.Action), Actor: decision.Actor, Reason: decision.Reason,
				CreatedAt: decision.CreatedAt.UTC(),
			})
		}
		return inspection, nil
	}
	return TaskBoardReviewInspection{}, fmt.Errorf("%w: review request %s", ErrLifecycleNotFound, requestID)
}

// decodeTaskBoardReviewArtifacts decodes the frozen input binding set a gate
// was opened with. The durable record holds the same encoded
// []workflowkit.ArtifactBinding the runtime wrote when it opened the gate.
func decodeTaskBoardReviewArtifacts(encoded string) ([]TaskBoardReviewArtifact, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, nil
	}
	var bindings []workflowkit.ArtifactBinding
	if err := json.Unmarshal([]byte(encoded), &bindings); err != nil {
		return nil, fmt.Errorf("decode review gate input bindings: %w", err)
	}
	artifacts := make([]TaskBoardReviewArtifact, 0, len(bindings))
	for _, binding := range bindings {
		artifacts = append(artifacts, TaskBoardReviewArtifact{
			Name:          binding.Name,
			ArtifactID:    string(binding.ArtifactID),
			ContentDigest: string(binding.ContentDigest),
			SchemaVersion: binding.SchemaVersion,
		})
	}
	sort.SliceStable(artifacts, func(left, right int) bool { return artifacts[left].Name < artifacts[right].Name })
	return artifacts, nil
}

// taskBoardReviewFindingArtifacts are the critic outputs the Standard authoring
// graph can produce. They are optional repair inputs, so a gate may legitimately
// have none.
var taskBoardReviewFindingArtifacts = []string{"test_quality_finding", "solution_integrity_finding"}

// readTaskBoardReviewFindings reads the critic findings for the Run that owns a
// gate. Findings come from the Run's own completed critic stage attempts rather
// than from the gate's input bindings, because the authoring graph binds them
// only as optional inputs to authoring_repair.
//
// The returned note explains an empty result. task_review runs before every
// critic, so having no agent opinion there is correct rather than a fault.
func (service *TaskBoardService) readTaskBoardReviewFindings(ctx context.Context, detail TaskInspectionSnapshot, binding store.AuthoringReviewGateBinding) ([]TaskBoardReviewFinding, string) {
	if binding.StageKey == workflowadapter.TaskReview {
		return nil, "此门禁无 agent 评审意见（critic 阶段在其后执行）"
	}
	if service.core.objects == nil {
		return nil, "评审意见存储未配置"
	}
	var stages []store.StageAttempt
	var run *store.WorkflowRun
	for index := range detail.Runs {
		if detail.Runs[index].Run.ID == binding.RunID {
			stages = detail.Runs[index].Stages
			run = &detail.Runs[index].Run
			break
		}
	}
	if run == nil {
		return nil, "未找到该门禁所属 Run 的阶段记录"
	}
	subject, err := service.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return nil, "无法解析 Run 主体以校验意见血缘: " + err.Error()
	}

	// Select the latest completed attempt per finding artifact key, using the
	// same candidate comparison the rest of the board projection uses.
	latest := make(map[string]stageArtifactCandidate)
	for _, attempt := range stages {
		if attempt.ExecutionStatus != store.StageExecutionCompleted || strings.TrimSpace(attempt.ArtifactManifestID) == "" {
			continue
		}
		if attempt.StageKey != workflowadapter.TestQualityCritic && attempt.StageKey != workflowadapter.SolutionIntegrityCritic {
			continue
		}
		references, refsErr := service.core.store.ListArtifactRefsForAttempt(ctx, attempt.ID)
		if refsErr != nil {
			return nil, "读取意见产物引用失败: " + refsErr.Error()
		}
		for _, reference := range references {
			if !taskBoardIsReviewFindingArtifact(reference.ArtifactKey) {
				continue
			}
			if reference.RunID != run.ID || reference.StageKey != attempt.StageKey || reference.AttemptID != attempt.ID ||
				reference.SubjectRevisionID != subject.subjectRevisionID() || reference.SubjectDigest != subject.subjectDigest() ||
				reference.WorkflowFingerprint != run.DefinitionHash {
				return nil, "意见产物与 Run 血缘不一致，已拒绝展示"
			}
			current, found := latest[reference.ArtifactKey]
			if !found || laterArtifactCandidate(attempt, reference, current) {
				latest[reference.ArtifactKey] = stageArtifactCandidate{attempt: attempt, ref: reference}
			}
		}
	}
	if len(latest) == 0 {
		return nil, "该 Run 尚未产生 agent 评审意见"
	}
	findings := make([]TaskBoardReviewFinding, 0, len(latest))
	for _, key := range taskBoardReviewFindingArtifacts {
		candidate, found := latest[key]
		if !found {
			continue
		}
		findings = append(findings, service.readTaskBoardReviewFinding(ctx, *run, subject, stages, key, candidate))
	}
	return findings, ""
}

func taskBoardIsReviewFindingArtifact(key string) bool {
	for _, candidate := range taskBoardReviewFindingArtifacts {
		if candidate == key {
			return true
		}
	}
	return false
}

// readTaskBoardReviewFinding reads one finding body through the same manifest,
// lineage, and object path the board's validation readout already uses. A read
// or decode failure degrades to a message on that finding instead of failing
// the whole inspection.
func (service *TaskBoardService) readTaskBoardReviewFinding(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, stages []store.StageAttempt, key string, candidate stageArtifactCandidate) TaskBoardReviewFinding {
	finding := TaskBoardReviewFinding{
		ArtifactKey: key,
		StageKey:    candidate.attempt.StageKey,
		ArtifactID:  candidate.ref.ID,
	}
	recordedAt := candidate.ref.CreatedAt.UTC()
	if candidate.attempt.FinishedAt != nil {
		recordedAt = candidate.attempt.FinishedAt.UTC()
	}
	finding.RecordedAt = &recordedAt

	index, err := loadStageArtifactManifestIndex(ctx, service.core.store, candidate.ref.ManifestID)
	if err != nil {
		finding.Message = "读取意见清单失败: " + err.Error()
		return finding
	}
	// The caller already resolved this Run's subject to screen the artifact refs
	// it selected, so the same value is threaded in rather than re-resolved here.
	if index.manifest.SubjectRevisionID != subject.subjectRevisionID() || index.manifest.SubjectDigest != subject.subjectDigest() ||
		index.manifest.WorkflowFingerprint != run.DefinitionHash || index.payload.RunID != run.ID ||
		index.payload.StageAttemptID != candidate.ref.AttemptID || string(index.payload.StageKey) != candidate.ref.StageKey {
		finding.Message = "意见清单与 Run 血缘不一致"
		return finding
	}
	object, err := index.objectFor(candidate.ref)
	if err != nil {
		finding.Message = "定位意见对象失败: " + err.Error()
		return finding
	}
	raw, err := service.core.objects.ReadAll(ctx, object)
	if err != nil {
		finding.Message = "读取意见正文失败: " + err.Error()
		return finding
	}
	body := strings.ToValidUTF8(string(raw), "?")
	if len(body) > TaskBoardReviewFindingBodyLimit {
		body = body[:TaskBoardReviewFindingBodyLimit] + taskBoardReviewFindingTruncationNote
		finding.BodyTruncated = true
	}
	finding.Body = body

	// A finding is durable structured data. Decoding it strictly lets the gate
	// screen present its bound code and repair target instead of raw JSON, while
	// the verbatim body above remains available either way.
	var decoded workflowkit.WorkflowFinding
	if err := decodeStrictJSON(string(raw), &decoded); err != nil {
		finding.Message = "意见结构不可解析，仅展示原文: " + err.Error()
		return finding
	}
	if err := decoded.Validate(); err != nil {
		finding.Message = "意见未通过校验，仅展示原文: " + err.Error()
		return finding
	}
	finding.Code = decoded.Code
	finding.ProducingStage = string(decoded.ProducingStage)
	finding.TargetWriter = string(decoded.TargetWriter)
	finding.EvidenceDigest = string(decoded.EvidenceDigest)
	finding.CandidateDigest = string(decoded.CandidateDigest)

	// A WorkflowFinding is metadata only: it binds a closed code to digests and
	// carries no prose at all. The reason a critic objected lives in the
	// validation receipt named by DiagnosticDigest, so resolve it here. Showing
	// the digest alone tells an operator that evidence exists without letting
	// them read it, which is the same as showing nothing.
	if decoded.DiagnosticDigest != "" {
		finding.Diagnostics, finding.DiagnosticSummary, finding.DiagnosticMessage =
			service.readTaskBoardReviewDiagnostics(ctx, run, subject, stages, decoded.DiagnosticDigest)
	}
	return finding
}

// readTaskBoardReviewDiagnostics resolves the validation receipt a finding cites
// through its DiagnosticDigest and returns that receipt's failing checks.
//
// DiagnosticDigest is the receipt's own canonical digest, not the digest of the
// object that holds it, so the receipt cannot be fetched by content address. It
// is located by decoding this Run's validation_receipt artifacts and matching
// that field. An artifact that fails Run lineage is skipped rather than
// rendered: this is a display path feeding a decision, and showing foreign
// evidence beside a gate is worse than showing none.
func (service *TaskBoardService) readTaskBoardReviewDiagnostics(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, stages []store.StageAttempt, diagnosticDigest workflowkit.Fingerprint) ([]TaskBoardReviewDiagnostic, string, string) {
	candidates := make([]stageArtifactCandidate, 0, len(stages))
	for _, attempt := range stages {
		if attempt.ExecutionStatus != store.StageExecutionCompleted || strings.TrimSpace(attempt.ArtifactManifestID) == "" {
			continue
		}
		references, err := service.core.store.ListArtifactRefsForAttempt(ctx, attempt.ID)
		if err != nil {
			return nil, "", "读取校验回执引用失败: " + err.Error()
		}
		for _, reference := range references {
			if reference.ArtifactKey != taskBoardValidationReceiptArtifactKey {
				continue
			}
			if reference.RunID != run.ID || reference.StageKey != attempt.StageKey || reference.AttemptID != attempt.ID ||
				reference.SubjectRevisionID != subject.subjectRevisionID() || reference.SubjectDigest != subject.subjectDigest() ||
				reference.WorkflowFingerprint != run.DefinitionHash {
				continue
			}
			candidates = append(candidates, stageArtifactCandidate{attempt: attempt, ref: reference})
		}
	}
	// Newest first: a finding almost always cites the current wave's receipt, so
	// the match lands on the first read and no further objects are touched.
	sort.SliceStable(candidates, func(left, right int) bool {
		return laterArtifactCandidate(candidates[left].attempt, candidates[left].ref, candidates[right])
	})
	for _, candidate := range candidates {
		receipt, readable := service.readTaskBoardValidationReceipt(ctx, run, subject, candidate)
		if !readable || receipt.Digest != diagnosticDigest {
			continue
		}
		return taskBoardReviewDiagnosticsFromReceipt(receipt)
	}
	return nil, "", "未能在本 Run 中定位该意见引用的校验回执"
}

// taskBoardValidationReceiptArtifactKey is the artifact a finding's
// DiagnosticDigest can name. It is declared here rather than imported because
// this read is a projection concern, not a workflow contract.
const taskBoardValidationReceiptArtifactKey = "validation_receipt"

// readTaskBoardValidationReceipt reads one validation receipt through the same
// manifest, lineage, and object path the rest of this file uses. Every failure
// degrades to "not readable" so a single unreadable receipt cannot fail the
// whole gate inspection.
func (service *TaskBoardService) readTaskBoardValidationReceipt(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, candidate stageArtifactCandidate) (workflowkit.ValidationReceipt, bool) {
	index, err := loadStageArtifactManifestIndex(ctx, service.core.store, candidate.ref.ManifestID)
	if err != nil {
		return workflowkit.ValidationReceipt{}, false
	}
	if index.manifest.SubjectRevisionID != subject.subjectRevisionID() || index.manifest.SubjectDigest != subject.subjectDigest() ||
		index.manifest.WorkflowFingerprint != run.DefinitionHash || index.payload.RunID != run.ID ||
		index.payload.StageAttemptID != candidate.ref.AttemptID || string(index.payload.StageKey) != candidate.ref.StageKey {
		return workflowkit.ValidationReceipt{}, false
	}
	object, err := index.objectFor(candidate.ref)
	if err != nil {
		return workflowkit.ValidationReceipt{}, false
	}
	raw, err := service.core.objects.ReadAll(ctx, object)
	if err != nil {
		return workflowkit.ValidationReceipt{}, false
	}
	var receipt workflowkit.ValidationReceipt
	if err := decodeStrictJSON(string(raw), &receipt); err != nil {
		return workflowkit.ValidationReceipt{}, false
	}
	return receipt, true
}

// taskBoardReviewDiagnosticsFromReceipt selects the checks an operator can act
// on. Passing checks are counted but not listed: a receipt carries every command
// it ran, and reproducing a successful container build log is what made this
// readout unreadable in the first place.
func taskBoardReviewDiagnosticsFromReceipt(receipt workflowkit.ValidationReceipt) ([]TaskBoardReviewDiagnostic, string, string) {
	failed := 0
	for _, report := range receipt.Diagnostics {
		if report.ExitCode != 0 {
			failed++
		}
	}
	// The verdict is stated alongside the counts because a passing receipt with
	// a failing baseline check is normal here, and that combination is exactly
	// what an operator misreads when only one of the two is shown.
	summary := fmt.Sprintf("裁决 %s · %d 项检查 · %d 项失败", receipt.Verdict, len(receipt.Diagnostics), failed)
	diagnostics := make([]TaskBoardReviewDiagnostic, 0, failed)
	for _, report := range receipt.Diagnostics {
		if report.ExitCode == 0 {
			continue
		}
		if len(diagnostics) == taskBoardReviewDiagnosticLimit {
			break
		}
		diagnostics = append(diagnostics, TaskBoardReviewDiagnostic{
			CommandID:   report.CommandID,
			ExitCode:    report.ExitCode,
			TestStarted: report.TestStarted,
			StdoutTail:  boundTaskBoardDiagnosticTail(report.StdoutTail),
			StderrTail:  boundTaskBoardDiagnosticTail(report.StderrTail),
		})
	}
	if len(diagnostics) == 0 {
		// A pass-code finding cites a passing receipt. Stating that is the
		// readout; an empty list with no sentence would look like a failed read.
		return nil, summary, "该回执所有检查均通过"
	}
	if failed > len(diagnostics) {
		return diagnostics, summary, fmt.Sprintf("仅显示前 %d 项失败检查", len(diagnostics))
	}
	return diagnostics, summary, ""
}

// boundTaskBoardDiagnosticTail keeps the end of a command tail. A failure reason
// is at the end of its output, and a build log can be megabytes.
func boundTaskBoardDiagnosticTail(tail string) string {
	tail = strings.TrimSpace(tail)
	if len(tail) > taskBoardReviewDiagnosticTailLimit {
		tail = "…" + tail[len(tail)-taskBoardReviewDiagnosticTailLimit:]
	}
	// Applied after the cut so a rune split by the slice cannot reach a terminal.
	return strings.ToValidUTF8(tail, "?")
}
