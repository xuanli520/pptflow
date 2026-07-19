package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

const (
	repairSessionAdvanceCommandType    = "repair_session.advance"
	repairSessionAdvancePayloadFormat  = "harbor.repair-session-advance.v1"
	automaticRepairCommandKeyNamespace = "automatic-repair-command"
	automaticRepairOperationKeyPrefix  = "automatic-repair-operation"
)

// RepairLoopService is a durable coordinator for the existing continuation
// protocol. It does not mutate a task itself: every new round is planned and
// committed through TaskContinuationService and ChangeProviderService.
type RepairLoopService struct {
	core          *lifecycleServiceCore
	continuations *TaskContinuationService
}

func newRepairLoopService(core *lifecycleServiceCore, continuations *TaskContinuationService) *RepairLoopService {
	return &RepairLoopService{core: core, continuations: continuations}
}

type repairSessionAdvancePayload struct {
	Format          string `json:"format"`
	RepairSessionID string `json:"repair_session_id"`
	RunID           string `json:"run_id"`
	CandidateID     string `json:"candidate_id"`
}

// RepairLoopAdvanceResult describes the durable result of one coordinator
// delivery. Empty IDs mean the selected run is not part of an automated repair
// session and was deliberately ignored.
type RepairLoopAdvanceResult struct {
	Session   store.RepairSession
	PlanID    string
	Candidate store.RevisionCandidate
	Execution store.ContinuationExecution
	Action    string
}

func automaticRepairCommandKey(sessionID string, round int) string {
	return fmt.Sprintf("%s:%s:round:%d", automaticRepairCommandKeyNamespace, sessionID, round)
}

func automaticRepairOperationKey(sessionID string, round int) string {
	return fmt.Sprintf("%s:%s:round:%d", automaticRepairOperationKeyPrefix, sessionID, round)
}

func repairSessionAdvanceJobKey(sessionID, runID string) string {
	return "repair-session-advance:" + sessionID + ":" + runID
}

// EnqueueRunOutcome records the durable coordinator handoff after a repair
// child Run reaches a relevant terminal projection. It is intentionally safe
// to call after every Run outcome; unrelated Runs are no-ops and retries return
// the same durable job.
func (service *RepairLoopService) EnqueueRunOutcome(ctx context.Context, runID, actor, reason string) (store.DurableJob, bool, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.DurableJob{}, false, fmt.Errorf("repair loop service is not configured")
	}
	run, candidate, session, actionable, err := service.repairRunBinding(ctx, runID)
	if err != nil {
		return store.DurableJob{}, false, err
	}
	if !actionable || !repairOutcomeNeedsAdvance(run.Status) || session.Status != store.RepairSessionOpen {
		return store.DurableJob{}, false, nil
	}
	payload, err := json.Marshal(repairSessionAdvancePayload{
		Format: repairSessionAdvancePayloadFormat, RepairSessionID: session.ID, RunID: run.ID, CandidateID: candidate.ID,
	})
	if err != nil {
		return store.DurableJob{}, false, fmt.Errorf("encode repair-session advance payload: %w", err)
	}
	job, err := service.core.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: repairSessionAdvanceCommandType, EntityType: "repair_session", EntityID: session.ID, RunID: run.ID,
		PayloadJSON: string(payload), IdempotencyKey: repairSessionAdvanceJobKey(session.ID, run.ID), Actor: actor, Reason: reason,
	})
	if err != nil {
		return store.DurableJob{}, false, err
	}
	return job, true, nil
}

func repairOutcomeNeedsAdvance(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunWaitingContinuation, store.WorkflowRunSucceeded, store.WorkflowRunFailedTerminal, store.WorkflowRunInDoubt:
		return true
	default:
		return false
	}
}

// HandleDurableJob advances one queued repair-session coordinator. It owns no
// external side effect itself, so a re-delivery is safe: command, operation,
// candidate, plan, and execution all use deterministic idempotency keys.
func (service *RepairLoopService) HandleDurableJob(ctx context.Context, job store.DurableJob) (RepairLoopAdvanceResult, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return RepairLoopAdvanceResult{}, fmt.Errorf("repair loop service is not configured")
	}
	if job.CommandType != repairSessionAdvanceCommandType || job.EntityType != "repair_session" || strings.TrimSpace(job.EntityID) == "" || strings.TrimSpace(job.RunID) == "" {
		return RepairLoopAdvanceResult{}, fmt.Errorf("invalid repair-session advance durable job")
	}
	var payload repairSessionAdvancePayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		return RepairLoopAdvanceResult{}, fmt.Errorf("decode repair-session advance payload: %w", err)
	}
	if payload.Format != repairSessionAdvancePayloadFormat || payload.RepairSessionID != job.EntityID || payload.RunID != job.RunID || strings.TrimSpace(payload.CandidateID) == "" {
		return RepairLoopAdvanceResult{}, fmt.Errorf("repair-session advance payload does not bind durable job")
	}
	result, err := service.AdvanceRunOutcome(ctx, payload.RunID, payload.RepairSessionID, payload.CandidateID)
	if err == nil {
		return result, nil
	}

	// ChangeProvider converts any provider-side ambiguity into a reconciliation
	// error. For an unexpected application/store error we make the same safe
	// terminal projection instead of leaving an open session with no executable
	// retry job. The original error remains in the audit reason.
	if _, transitionErr := service.transitionSession(ctx, payload.RepairSessionID, store.RepairSessionNeedsHuman, job.CreatedBy, "automatic repair progression requires human review: "+err.Error()); transitionErr != nil {
		return RepairLoopAdvanceResult{}, fmt.Errorf("advance automatic repair: %v; mark repair session needs_human: %w", err, transitionErr)
	}
	return RepairLoopAdvanceResult{Action: "needs_human"}, nil
}

// AdvanceRunOutcome is exposed for recovery and focused integration tests. The
// normal runtime invokes it only through a claimed repair_session.advance job.
func (service *RepairLoopService) AdvanceRunOutcome(ctx context.Context, runID, sessionID, candidateID string) (RepairLoopAdvanceResult, error) {
	run, candidate, session, actionable, err := service.repairRunBinding(ctx, runID)
	if err != nil {
		return RepairLoopAdvanceResult{}, err
	}
	if !actionable {
		return RepairLoopAdvanceResult{}, nil
	}
	if session.ID != sessionID || candidate.ID != candidateID {
		return RepairLoopAdvanceResult{}, fmt.Errorf("repair-session advance payload refers to another candidate or session")
	}
	result := RepairLoopAdvanceResult{Session: session, Candidate: candidate}
	if session.Status != store.RepairSessionOpen {
		result.Action = string(session.Status)
		return result, nil
	}

	switch run.Status {
	case store.WorkflowRunSucceeded:
		updated, err := service.transitionSession(ctx, session.ID, store.RepairSessionCompleted, run.CreatedBy, "repair child run passed all checks")
		if err != nil {
			return RepairLoopAdvanceResult{}, err
		}
		result.Session, result.Action = updated, "completed"
		return result, nil
	case store.WorkflowRunFailedTerminal:
		updated, err := service.transitionSession(ctx, session.ID, store.RepairSessionNeedsHuman, run.CreatedBy, "repair child run reached a non-repairable verdict")
		if err != nil {
			return RepairLoopAdvanceResult{}, err
		}
		result.Session, result.Action = updated, "needs_human"
		return result, nil
	case store.WorkflowRunInDoubt:
		updated, err := service.transitionSession(ctx, session.ID, store.RepairSessionNeedsHuman, run.CreatedBy, "repair child run requires reconciliation")
		if err != nil {
			return RepairLoopAdvanceResult{}, err
		}
		result.Session, result.Action = updated, "needs_human"
		return result, nil
	case store.WorkflowRunWaitingContinuation:
		if err := service.requireRepairableFinding(ctx, run); err != nil {
			updated, transitionErr := service.transitionSession(ctx, session.ID, store.RepairSessionNeedsHuman, run.CreatedBy, "repair child run has no actionable structured repair finding: "+err.Error())
			if transitionErr != nil {
				return RepairLoopAdvanceResult{}, transitionErr
			}
			result.Session, result.Action = updated, "needs_human"
			return result, nil
		}
		if candidate.RoundOrdinal >= session.MaxRounds {
			updated, err := service.transitionSession(ctx, session.ID, store.RepairSessionNeedsHuman, run.CreatedBy, "automatic repair round limit exhausted")
			if err != nil {
				return RepairLoopAdvanceResult{}, err
			}
			result.Session, result.Action = updated, "needs_human"
			return result, nil
		}
		return service.planAndCommitNextRound(ctx, run, candidate, session)
	default:
		return RepairLoopAdvanceResult{}, fmt.Errorf("repair child run %s is not at an actionable terminal outcome", run.ID)
	}
}

// RecoverRunOutcome is the explicit local-runtime reconciliation hook. Unlike
// normal progression it does not invent a command: it reuses the same
// deterministic session/round identities that a lost advance job would use.
func (service *RepairLoopService) RecoverRunOutcome(ctx context.Context, runID string) (RepairLoopAdvanceResult, error) {
	run, candidate, session, actionable, err := service.repairRunBinding(ctx, runID)
	if err != nil || !actionable || !repairOutcomeNeedsAdvance(run.Status) {
		return RepairLoopAdvanceResult{}, err
	}
	return service.AdvanceRunOutcome(ctx, run.ID, session.ID, candidate.ID)
}

func (service *RepairLoopService) repairRunBinding(ctx context.Context, runID string) (store.WorkflowRun, store.RevisionCandidate, store.RepairSession, bool, error) {
	run, err := service.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return store.WorkflowRun{}, store.RevisionCandidate{}, store.RepairSession{}, false, err
	}
	if run == nil {
		return store.WorkflowRun{}, store.RevisionCandidate{}, store.RepairSession{}, false, fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, runID)
	}
	if run.SubjectKind == store.WorkflowRunSubjectAuthoringSession {
		return *run, store.RevisionCandidate{}, store.RepairSession{}, false, nil
	}
	candidate, err := service.core.store.GetRevisionCandidateByTargetRevision(ctx, run.RevisionID)
	if err != nil {
		return store.WorkflowRun{}, store.RevisionCandidate{}, store.RepairSession{}, false, err
	}
	if candidate == nil || candidate.RepairSessionID == "" {
		return *run, store.RevisionCandidate{}, store.RepairSession{}, false, nil
	}
	if candidate.TargetRunID != run.ID || candidate.TaskID != run.TaskID {
		return store.WorkflowRun{}, store.RevisionCandidate{}, store.RepairSession{}, false, fmt.Errorf("repair candidate does not bind the selected child run")
	}
	session, err := service.core.store.GetRepairSession(ctx, candidate.RepairSessionID)
	if err != nil {
		return store.WorkflowRun{}, store.RevisionCandidate{}, store.RepairSession{}, false, err
	}
	if session == nil || session.SubjectID != run.TaskID {
		return store.WorkflowRun{}, store.RevisionCandidate{}, store.RepairSession{}, false, fmt.Errorf("repair session does not bind the selected child run task")
	}
	return *run, *candidate, *session, true, nil
}

func (service *RepairLoopService) requireRepairableFinding(ctx context.Context, run store.WorkflowRun) error {
	attempts, err := service.core.store.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		return err
	}
	for index := len(attempts) - 1; index >= 0; index-- {
		attempt := attempts[index]
		if attempt.ExecutionStatus == store.StageExecutionCompleted && attempt.Verdict == store.VerdictNeedsRepair {
			return nil
		}
	}
	return fmt.Errorf("no completed needs_repair stage attempt")
}

func (service *RepairLoopService) planAndCommitNextRound(ctx context.Context, run store.WorkflowRun, previous store.RevisionCandidate, session store.RepairSession) (RepairLoopAdvanceResult, error) {
	if service == nil || service.continuations == nil {
		return RepairLoopAdvanceResult{}, fmt.Errorf("repair loop continuation service is not configured")
	}
	root, err := service.core.store.GetContinuationCommand(ctx, session.CommandID)
	if err != nil {
		return RepairLoopAdvanceResult{}, err
	}
	if root == nil {
		return RepairLoopAdvanceResult{}, fmt.Errorf("repair session root command is missing")
	}
	var original normalizedChangeCommand
	if err := decodeStrictJSON(root.PayloadJSON, &original); err != nil {
		return RepairLoopAdvanceResult{}, fmt.Errorf("decode repair session root command: %w", err)
	}
	if original.Format != "harbor.content-continuation-command.v1" || original.Change.ProviderID != AgentRepairProviderID || original.Change.MaxRepairRounds != session.MaxRounds {
		return RepairLoopAdvanceResult{}, fmt.Errorf("repair session root command is not a matching automated repair intent")
	}
	nextRound := previous.RoundOrdinal + 1
	commandKey := automaticRepairCommandKey(session.ID, nextRound)
	command, err := service.automaticRoundCommand(ctx, run, session, *root, original, nextRound, commandKey)
	if err != nil {
		return RepairLoopAdvanceResult{}, err
	}
	plan, err := service.continuations.PlanTaskContinuation(ctx, command)
	if err != nil {
		return RepairLoopAdvanceResult{}, err
	}
	execution, err := service.continuations.ExecuteTaskContinuation(ctx, plan.ID())
	if err != nil {
		return RepairLoopAdvanceResult{}, err
	}
	nextCandidate, err := service.core.store.GetRevisionCandidateByFrozenPlan(ctx, plan.ID())
	if err != nil {
		return RepairLoopAdvanceResult{}, err
	}
	if nextCandidate == nil || nextCandidate.RepairSessionID != session.ID || nextCandidate.RoundOrdinal != nextRound {
		return RepairLoopAdvanceResult{}, fmt.Errorf("automatic repair continuation did not bind the expected candidate round")
	}
	return RepairLoopAdvanceResult{Session: session, Candidate: *nextCandidate, PlanID: plan.ID(), Execution: execution, Action: "next_round_queued"}, nil
}

func (service *RepairLoopService) automaticRoundCommand(ctx context.Context, run store.WorkflowRun, session store.RepairSession, root store.ContinuationCommand, original normalizedChangeCommand, round int, commandKey string) (ContinueTaskCommand, error) {
	if existing, err := service.core.store.GetContinuationCommandByKey(ctx, commandKey); err != nil {
		return ContinueTaskCommand{}, err
	} else if existing != nil {
		var frozen normalizedChangeCommand
		if err := decodeStrictJSON(existing.PayloadJSON, &frozen); err != nil {
			return ContinueTaskCommand{}, fmt.Errorf("decode persisted automatic repair command: %w", err)
		}
		if frozen.Format != "harbor.content-continuation-command.v1" || frozen.Command.CommandKey != commandKey || frozen.Command.TaskID != run.TaskID || frozen.Command.RunID != run.ID ||
			frozen.Change.ProviderID != AgentRepairProviderID || frozen.Change.RepairSessionID != session.ID || frozen.Change.RepairRoundOrdinal != round || frozen.Change.MaxRepairRounds != session.MaxRounds ||
			frozen.Change.OperationKey != automaticRepairOperationKey(session.ID, round) {
			return ContinueTaskCommand{}, fmt.Errorf("persisted automatic repair command does not match session round")
		}
		return ContinueTaskCommand{
			CommandKey: existing.CommandKey, TaskID: existing.SubjectID, RunID: existing.RunID, Expected: frozen.Command.Expected, Actor: existing.Actor, Reason: existing.Reason,
			Change: &TaskChangeRequest{ProviderID: frozen.Change.ProviderID, OperationKey: frozen.Change.OperationKey, Payload: append(json.RawMessage(nil), frozen.Change.Payload...), Findings: frozen.Change.Findings,
				MaxRepairRounds: frozen.Change.MaxRepairRounds, repairSessionID: frozen.Change.RepairSessionID, repairRoundOrdinal: frozen.Change.RepairRoundOrdinal},
		}, nil
	}
	revision, err := service.core.store.GetTaskRevision(ctx, run.RevisionID)
	if err != nil {
		return ContinueTaskCommand{}, err
	}
	if revision == nil || revision.TaskID != run.TaskID {
		return ContinueTaskCommand{}, fmt.Errorf("repair child run has no matching revision")
	}
	findings, err := service.retargetSessionFindings(ctx, session, root, run, *revision)
	if err != nil {
		return ContinueTaskCommand{}, err
	}
	checkpoint, err := currentContinuationCheckpoint(ctx, service.core, run.ID)
	if err != nil {
		return ContinueTaskCommand{}, err
	}
	return ContinueTaskCommand{
		CommandKey: commandKey, TaskID: run.TaskID, RunID: run.ID, Expected: checkpoint, Actor: root.Actor, Reason: root.Reason,
		Change: &TaskChangeRequest{
			ProviderID: AgentRepairProviderID, OperationKey: automaticRepairOperationKey(session.ID, round), Payload: append(json.RawMessage(nil), original.Change.Payload...),
			Findings: findings, MaxRepairRounds: session.MaxRounds, repairSessionID: session.ID, repairRoundOrdinal: round,
		},
	}, nil
}

// retargetSessionFindings carries the immutable, validated root finding bundle
// into a later revision without fabricating a new checker result from free-form
// stage output. The original report refs remain immutable evidence; the two
// top-level revision fields bind the new candidate checkout.
func (service *RepairLoopService) retargetSessionFindings(ctx context.Context, session store.RepairSession, root store.ContinuationCommand, run store.WorkflowRun, revision store.TaskRevision) (FindingBundle, error) {
	var findings FindingBundle
	if err := decodeStrictJSON(session.FindingsJSON, &findings); err != nil {
		return FindingBundle{}, fmt.Errorf("decode frozen repair findings: %w", err)
	}
	rootRun, _, rootRevision, err := loadContinuationRunBinding(ctx, service.core, root.RunID)
	if err != nil {
		return FindingBundle{}, err
	}
	if root.SubjectID != session.SubjectID || rootRun.ID != root.RunID || rootRevision.ID != session.BaseRevisionID {
		return FindingBundle{}, fmt.Errorf("repair session root binding is inconsistent")
	}
	if err := findings.Validate(rootRevision.ID, rootRevision.TaskDigest); err != nil {
		return FindingBundle{}, fmt.Errorf("validate frozen repair findings: %w", err)
	}
	if err := service.core.changes.validateFindingEvidence(ctx, rootRun.ID, rootRevision.ID, rootRevision.TaskDigest, findings); err != nil {
		return FindingBundle{}, fmt.Errorf("validate frozen repair finding evidence: %w", err)
	}
	findings.RevisionID = revision.ID
	findings.RevisionDigest = revision.TaskDigest
	if run.TaskID != session.SubjectID {
		return FindingBundle{}, fmt.Errorf("repair child run task differs from session")
	}
	return findings, nil
}

func (service *RepairLoopService) transitionSession(ctx context.Context, sessionID string, target store.RepairSessionState, actor, reason string) (store.RepairSession, error) {
	for attempt := 0; attempt < 3; attempt++ {
		session, err := service.core.store.GetRepairSession(ctx, sessionID)
		if err != nil {
			return store.RepairSession{}, err
		}
		if session == nil {
			return store.RepairSession{}, fmt.Errorf("%w: repair session %s", ErrLifecycleNotFound, sessionID)
		}
		if session.Status == target || session.Status != store.RepairSessionOpen {
			return *session, nil
		}
		updated, err := service.core.store.TransitionRepairSession(ctx, store.TransitionRepairSessionRequest{
			RepairSessionID: session.ID, ExpectedVersion: session.Version, Status: target, Actor: actor, Reason: reason,
		})
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, store.ErrOptimisticLock) {
			return store.RepairSession{}, err
		}
	}
	return store.RepairSession{}, fmt.Errorf("repair session %s changed concurrently", sessionID)
}

// validateAutomaticRepairRound is deliberately on ChangeProviderService so the
// same validation runs for an in-process retry and a recovered durable job.
func (service *ChangeProviderService) validateAutomaticRepairRound(ctx context.Context, command ContinueTaskCommand, change TaskChangeRequest, providerID string) error {
	if providerID != AgentRepairProviderID || change.repairRoundOrdinal <= 1 || strings.TrimSpace(change.repairSessionID) == "" {
		return fmt.Errorf("automatic repair continuation is incomplete")
	}
	session, err := service.core.store.GetRepairSession(ctx, change.repairSessionID)
	if err != nil {
		return err
	}
	if session == nil || session.Status != store.RepairSessionOpen || session.SubjectID != command.TaskID || change.MaxRepairRounds != session.MaxRounds || change.repairRoundOrdinal > session.MaxRounds {
		return fmt.Errorf("automatic repair continuation does not match its frozen session")
	}
	if command.CommandKey != automaticRepairCommandKey(session.ID, change.repairRoundOrdinal) || change.OperationKey != automaticRepairOperationKey(session.ID, change.repairRoundOrdinal) {
		return fmt.Errorf("automatic repair continuation keys do not match the frozen session round")
	}
	root, err := service.core.store.GetContinuationCommand(ctx, session.CommandID)
	if err != nil {
		return err
	}
	if root == nil {
		return fmt.Errorf("repair session root command is missing")
	}
	var original normalizedChangeCommand
	if err := decodeStrictJSON(root.PayloadJSON, &original); err != nil {
		return fmt.Errorf("decode repair session root command: %w", err)
	}
	if original.Format != "harbor.content-continuation-command.v1" || original.Change.ProviderID != providerID || original.Change.MaxRepairRounds != session.MaxRounds || !bytes.Equal(original.Change.Payload, change.Payload) {
		return fmt.Errorf("automatic repair continuation changed frozen provider binding")
	}
	findings, err := (&RepairLoopService{core: service.core}).retargetSessionFindings(ctx, *session, *root, store.WorkflowRun{TaskID: command.TaskID}, store.TaskRevision{ID: command.Expected.SubjectRevisionID, TaskID: command.TaskID, TaskDigest: string(command.Expected.SubjectDigest)})
	if err != nil {
		return err
	}
	if !sameRetargetedFindingBundle(findings, change.Findings) {
		return fmt.Errorf("automatic repair continuation changed frozen structured findings")
	}
	return nil
}

func sameRetargetedFindingBundle(left, right FindingBundle) bool {
	encodedLeft, leftErr := json.Marshal(left)
	encodedRight, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(encodedLeft, encodedRight)
}
