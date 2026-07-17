package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// LocalRuntimeService exposes the local durable-runtime attachment and
// recovery boundary. It deliberately has no provider, process-runtime, or
// filesystem mutation dependency: external effects are reconciled by their
// own domain services, never by `run reconcile`.
type LocalRuntimeService struct{ core *lifecycleServiceCore }

const localRuntimeReconcileBatchSize = 100

type AttachRunRequest struct {
	RunID string
}

// RunAttachment is a read-only projection of a Run's local durable state.
// A lease remains attached only when it is active and unexpired at ObservedAt;
// stale records are reported but never expired by Attach.
type RunAttachment struct {
	Run            store.WorkflowRun               `json:"run"`
	Stages         []store.StageAttempt            `json:"stages"`
	Jobs           []AttachedDurableJob            `json:"jobs"`
	WorkerLeases   []LocalLeaseAttachment          `json:"worker_leases"`
	WorkerHandoffs []store.RunWorkerHandoff        `json:"worker_handoffs"`
	Controls       []store.DurableControlOperation `json:"controls"`
	TaskQuota      LocalQuotaScopeAttachment       `json:"task_quota"`
	ActorQuota     LocalQuotaScopeAttachment       `json:"actor_quota"`
	ObservedAt     time.Time                       `json:"observed_at"`
	AttachableJobs int                             `json:"attachable_jobs"`
}

// AttachedDurableJob retains durable job and lease history while explicitly
// marking the lease facts that are still safe for a local attach operation.
type AttachedDurableJob struct {
	Job                   store.DurableJob       `json:"job"`
	Leases                []LocalLeaseAttachment `json:"leases"`
	Attachable            bool                   `json:"attachable"`
	FailureStage          string                 `json:"failure_stage,omitempty"`
	FailureCode           string                 `json:"failure_code,omitempty"`
	FailureSummary        string                 `json:"failure_summary,omitempty"`
	FailureArtifactID     string                 `json:"failure_artifact_id,omitempty"`
	FailureRecordedAt     *time.Time             `json:"failure_recorded_at,omitempty"`
	FailureRecoveryAction string                 `json:"failure_recovery_action,omitempty"`
}

type LocalLeaseAttachment struct {
	Lease store.Lease `json:"lease"`
	Valid bool        `json:"valid"`
}

type LocalQuotaScopeAttachment struct {
	ScopeKind store.QuotaScopeKind        `json:"scope_kind"`
	ScopeID   string                      `json:"scope_id"`
	Accounts  []store.QuotaAccount        `json:"accounts"`
	Leases    []LocalQuotaLeaseAttachment `json:"leases"`
}

type LocalQuotaLeaseAttachment struct {
	Lease store.DurableQuotaLease `json:"lease"`
	Valid bool                    `json:"valid"`
}

type ReconcileRunRequest struct {
	RunID  string
	Actor  string
	Reason string
}

// RunReconciliationResult records only local recovery projections. Expired
// jobs/leases are fenced and projected in SQLite; unresolved controls and
// quota reservations remain visible for a later explicit, fact-backed action.
type RunReconciliationResult struct {
	Attachment            RunAttachment                     `json:"attachment"`
	RecoveredJobs         []store.ExpiredDurableJobRecovery `json:"recovered_jobs"`
	ExpiredJobLeases      int                               `json:"expired_job_leases"`
	ExpiredWorkerLeases   int                               `json:"expired_worker_leases"`
	ExpiredWorkerHandoffs []store.RunWorkerHandoff          `json:"expired_worker_handoffs"`
	ExpiredTaskQuotas     int                               `json:"expired_task_quotas"`
	ExpiredActorQuotas    int                               `json:"expired_actor_quotas"`
	UnresolvedControls    []store.DurableControlOperation   `json:"unresolved_controls"`
	UnresolvedQuotaLeases []store.DurableQuotaLease         `json:"unresolved_quota_leases"`
}

// AttachRun reads one Run's local state without changing leases, controls,
// quotas, outbox records, managed files, or providers. CLI callers open the
// Store read-only for this operation; the service itself also performs only
// read APIs so in-process callers receive the same guarantee.
func (service *LocalRuntimeService) AttachRun(ctx context.Context, request AttachRunRequest) (RunAttachment, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return RunAttachment{}, fmt.Errorf("local runtime service is not configured")
	}
	runID := strings.TrimSpace(request.RunID)
	if err := store.ValidateUUIDv7(runID); err != nil {
		return RunAttachment{}, err
	}
	run, err := service.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunAttachment{}, err
	}
	if run == nil {
		return RunAttachment{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	subject, err := service.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return RunAttachment{}, err
	}
	taskQuotaID, err := subject.quotaTaskID()
	if err != nil {
		return RunAttachment{}, err
	}
	observedAt := service.core.now().UTC()
	stages, err := service.core.store.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		return RunAttachment{}, fmt.Errorf("list stage attempts for run %s: %w", run.ID, err)
	}
	jobs, err := service.core.store.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		return RunAttachment{}, fmt.Errorf("list durable jobs for run %s: %w", run.ID, err)
	}
	resolvedHandoffFailures := make(map[string]struct{})
	for _, job := range jobs {
		if standardAuthoringHandoffFailureResolved(job, jobs) {
			resolvedHandoffFailures[job.ID] = struct{}{}
		}
	}
	attachedJobs := make([]AttachedDurableJob, 0, len(jobs))
	attachableJobs := 0
	for _, job := range jobs {
		leases, err := service.core.store.ListLeasesForJob(ctx, job.ID)
		if err != nil {
			return RunAttachment{}, fmt.Errorf("list leases for durable job %s: %w", job.ID, err)
		}
		attached := AttachedDurableJob{Job: job, Leases: make([]LocalLeaseAttachment, 0, len(leases))}
		service.populateAttachedDurableJobFailure(&attached, stages)
		if _, resolved := resolvedHandoffFailures[job.ID]; resolved {
			attached.FailureRecoveryAction = ""
		}
		for _, lease := range leases {
			valid := isValidLocalLease(lease, observedAt)
			attached.Leases = append(attached.Leases, LocalLeaseAttachment{Lease: lease, Valid: valid})
			if valid && lease.ResourceType == "job_dispatch" && lease.ResourceID == job.ID && job.State == store.JobRunning {
				attached.Attachable = true
			}
		}
		if attached.Attachable {
			attachableJobs++
		}
		attachedJobs = append(attachedJobs, attached)
	}
	workerLeases, err := service.core.store.ListLeasesForResource(ctx, RunWorkerLeaseResourceType, run.ID)
	if err != nil {
		return RunAttachment{}, fmt.Errorf("list local worker leases for run %s: %w", run.ID, err)
	}
	attachedWorkerLeases := make([]LocalLeaseAttachment, 0, len(workerLeases))
	for _, lease := range workerLeases {
		attachedWorkerLeases = append(attachedWorkerLeases, LocalLeaseAttachment{Lease: lease, Valid: isValidLocalLease(lease, observedAt)})
	}
	handoffs, err := service.core.store.ListRunWorkerHandoffsForRun(ctx, run.ID)
	if err != nil {
		return RunAttachment{}, fmt.Errorf("list controlled worker handoffs for run %s: %w", run.ID, err)
	}
	controls, err := service.core.store.ListExecutionControlOperationsForRun(ctx, run.ID)
	if err != nil {
		return RunAttachment{}, fmt.Errorf("list execution controls for run %s: %w", run.ID, err)
	}
	taskQuota, err := service.readQuotaScope(ctx, store.QuotaScopeTask, taskQuotaID, observedAt)
	if err != nil {
		return RunAttachment{}, err
	}
	actorQuota, err := service.readQuotaScope(ctx, store.QuotaScopeActor, run.CreatedBy, observedAt)
	if err != nil {
		return RunAttachment{}, err
	}
	return RunAttachment{
		Run: *run, Stages: append([]store.StageAttempt(nil), stages...), Jobs: attachedJobs, WorkerLeases: attachedWorkerLeases,
		WorkerHandoffs: append([]store.RunWorkerHandoff(nil), handoffs...),
		Controls:       append([]store.DurableControlOperation(nil), controls...), TaskQuota: taskQuota,
		ActorQuota: actorQuota, ObservedAt: observedAt, AttachableJobs: attachableJobs,
	}, nil
}

func (service *LocalRuntimeService) populateAttachedDurableJobFailure(attached *AttachedDurableJob, stages []store.StageAttempt) {
	if attached == nil || attached.Job.Failure == nil {
		return
	}
	attached.FailureCode = attached.Job.Failure.Code
	attached.FailureSummary = attached.Job.Failure.Message
	for _, stage := range stages {
		if stage.ID == attached.Job.StageAttemptID {
			attached.FailureStage = stage.StageKey
			break
		}
	}
	details := attachedDurableJobFailureDetails(attached.Job.Failure.DetailsJSON)
	if attached.FailureStage == "" {
		attached.FailureStage = details.StageKey
		if attached.FailureStage == "" {
			attached.FailureStage = details.Stage
		}
	}
	if attached.Job.EntityType == "artifact_ref" {
		attached.FailureArtifactID = attached.Job.EntityID
	}
	if attached.FailureArtifactID == "" {
		attached.FailureArtifactID = details.ArtifactID
	}
	if attached.Job.FinishedAt != nil {
		recordedAt := attached.Job.FinishedAt.UTC()
		attached.FailureRecordedAt = &recordedAt
	}
	switch attached.Job.State {
	case store.JobFailed:
		attached.FailureRecoveryAction = "repair_or_new_run"
	case store.JobInDoubt:
		if isStandardAuthoringHandoffCommand(attached.Job.CommandType) && isRecoverableHandoffFailure(attached.Job.Failure) {
			attached.FailureRecoveryAction = "redrive"
		} else {
			attached.FailureRecoveryAction = "reconcile"
		}
	}
}

type attachedDurableJobFailureDetail struct {
	ArtifactID string `json:"artifact_id"`
	Stage      string `json:"stage"`
	StageKey   string `json:"stage_key"`
}

func attachedDurableJobFailureDetails(raw string) attachedDurableJobFailureDetail {
	var details attachedDurableJobFailureDetail
	if err := json.Unmarshal([]byte(raw), &details); err != nil {
		return attachedDurableJobFailureDetail{}
	}
	details.ArtifactID = strings.TrimSpace(details.ArtifactID)
	details.Stage = strings.TrimSpace(details.Stage)
	details.StageKey = strings.TrimSpace(details.StageKey)
	return details
}

// ReconcileRun recovers only scoped local durable state. It never executes a
// ChangeProvider, calls a remote API, or tries to infer an external receipt.
// Unknown/expired quota and control facts remain in the returned result.
func (service *LocalRuntimeService) ReconcileRun(ctx context.Context, request ReconcileRunRequest) (RunReconciliationResult, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return RunReconciliationResult{}, fmt.Errorf("local runtime service is not configured")
	}
	runID := strings.TrimSpace(request.RunID)
	if err := store.ValidateUUIDv7(runID); err != nil {
		return RunReconciliationResult{}, err
	}
	actor, err := requiredLocalRuntimeActor(request.Actor)
	if err != nil {
		return RunReconciliationResult{}, err
	}
	reason, err := requiredLocalRuntimeReason(request.Reason)
	if err != nil {
		return RunReconciliationResult{}, err
	}
	run, err := service.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunReconciliationResult{}, err
	}
	if run == nil {
		return RunReconciliationResult{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	subject, err := service.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return RunReconciliationResult{}, err
	}
	taskQuotaID, err := subject.quotaTaskID()
	if err != nil {
		return RunReconciliationResult{}, err
	}
	var recovered []store.ExpiredDurableJobRecovery
	for {
		batch, err := service.core.store.ScanExpiredDurableJobsForReconcile(ctx, store.ScanExpiredDurableJobsRequest{
			RunID: run.ID, Limit: localRuntimeReconcileBatchSize, Actor: actor, Reason: reason,
		})
		if err != nil {
			return RunReconciliationResult{}, fmt.Errorf("reconcile expired durable jobs for run %s: %w", run.ID, err)
		}
		recovered = append(recovered, batch...)
		if len(batch) < localRuntimeReconcileBatchSize {
			break
		}
	}
	if err := service.projectRecoveredStandardAuthoringHandoffRuns(ctx, recovered, actor, reason); err != nil {
		return RunReconciliationResult{}, err
	}
	expiredJobLeases, err := service.core.store.ExpireLeasesForRun(ctx, run.ID, actor, reason)
	if err != nil {
		return RunReconciliationResult{}, fmt.Errorf("expire local job leases for run %s: %w", run.ID, err)
	}
	expiredWorkerHandoffs, err := service.core.store.ReconcileRunWorkerHandoffs(ctx, store.ReconcileRunWorkerHandoffsRequest{
		RunID: run.ID, Actor: actor, Reason: reason,
	})
	if err != nil {
		return RunReconciliationResult{}, fmt.Errorf("reconcile controlled worker handoffs for run %s: %w", run.ID, err)
	}
	expiredWorkerLeases, err := service.core.store.ExpireLeasesForResource(ctx, RunWorkerLeaseResourceType, run.ID, actor, reason)
	if err != nil {
		return RunReconciliationResult{}, fmt.Errorf("expire local worker leases for run %s: %w", run.ID, err)
	}
	expiredTaskQuotas, err := service.core.store.ExpireQuotaLeasesForScope(ctx, store.QuotaScopeTask, taskQuotaID, actor, reason)
	if err != nil {
		return RunReconciliationResult{}, fmt.Errorf("expire task quota leases for run %s: %w", run.ID, err)
	}
	expiredActorQuotas, err := service.core.store.ExpireQuotaLeasesForScope(ctx, store.QuotaScopeActor, run.CreatedBy, actor, reason)
	if err != nil {
		return RunReconciliationResult{}, fmt.Errorf("expire actor quota leases for run %s: %w", run.ID, err)
	}
	if subject.isTaskRevision() && service.core.repairs != nil {
		if _, err := service.core.repairs.RecoverRunOutcome(ctx, run.ID); err != nil {
			return RunReconciliationResult{}, fmt.Errorf("recover automatic repair for run %s: %w", run.ID, err)
		}
	}
	attachment, err := service.AttachRun(ctx, AttachRunRequest{RunID: run.ID})
	if err != nil {
		return RunReconciliationResult{}, err
	}
	result := RunReconciliationResult{
		Attachment: attachment, RecoveredJobs: append([]store.ExpiredDurableJobRecovery(nil), recovered...),
		ExpiredJobLeases: expiredJobLeases, ExpiredWorkerLeases: expiredWorkerLeases,
		ExpiredWorkerHandoffs: append([]store.RunWorkerHandoff(nil), expiredWorkerHandoffs...),
		ExpiredTaskQuotas:     expiredTaskQuotas, ExpiredActorQuotas: expiredActorQuotas,
	}
	for _, operation := range attachment.Controls {
		if operation.Status == store.ControlOperationReconcileRequired {
			result.UnresolvedControls = append(result.UnresolvedControls, operation)
		}
	}
	for _, scope := range []LocalQuotaScopeAttachment{attachment.TaskQuota, attachment.ActorQuota} {
		for _, lease := range scope.Leases {
			if lease.Lease.State == store.DurableQuotaLeaseExpired || lease.Lease.State == store.DurableQuotaLeaseUncertain {
				result.UnresolvedQuotaLeases = append(result.UnresolvedQuotaLeases, lease.Lease)
			}
		}
	}
	sort.Slice(result.UnresolvedControls, func(left, right int) bool {
		return result.UnresolvedControls[left].ID < result.UnresolvedControls[right].ID
	})
	sort.Slice(result.UnresolvedQuotaLeases, func(left, right int) bool {
		return result.UnresolvedQuotaLeases[left].ID < result.UnresolvedQuotaLeases[right].ID
	})
	return result, nil
}

// projectRecoveredStandardAuthoringHandoffRuns is deliberately narrow: a
// lost handoff delivery can have already created its child Run, so the parent
// must stop ordinary dispatch until an operator explicitly reconciles or
// redrives it. Other recovered command types retain their own fact-backed
// recovery handlers and must not be generically forced into in_doubt here.
func (service *LocalRuntimeService) projectRecoveredStandardAuthoringHandoffRuns(ctx context.Context, recoveries []store.ExpiredDurableJobRecovery, actor, reason string) error {
	for _, recovery := range recoveries {
		job := recovery.Job
		if job.State != store.JobInDoubt || (job.CommandType != standardAuthoringHandoffCommandType && job.CommandType != standardAuthoringHandoffRedriveCommandType && job.CommandType != standardAuthoringHandoffReconcileCommandType) {
			continue
		}
		run, err := service.core.store.GetWorkflowRun(ctx, job.RunID)
		if err != nil {
			return fmt.Errorf("read recovered Standard authoring handoff Run %s: %w", job.RunID, err)
		}
		if run == nil {
			return fmt.Errorf("%w: recovered Standard authoring handoff Run %s", ErrLifecycleNotFound, job.RunID)
		}
		if run.Status == store.WorkflowRunInDoubt || terminalWorkflowRunStatus(run.Status) {
			continue
		}
		if _, err := service.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
			RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunInDoubt,
			Actor: actor, Reason: reason,
		}); err != nil {
			return fmt.Errorf("project recovered Standard authoring handoff Run %s: %w", run.ID, err)
		}
	}
	return nil
}

func (service *LocalRuntimeService) readQuotaScope(ctx context.Context, kind store.QuotaScopeKind, scopeID string, observedAt time.Time) (LocalQuotaScopeAttachment, error) {
	if strings.TrimSpace(scopeID) == "" {
		return LocalQuotaScopeAttachment{}, fmt.Errorf("local runtime quota scope is empty")
	}
	accounts, err := service.core.store.ListQuotaAccountsForScope(ctx, kind, scopeID)
	if err != nil {
		return LocalQuotaScopeAttachment{}, fmt.Errorf("list %s quota accounts: %w", kind, err)
	}
	leases, err := service.core.store.ListDurableQuotaLeasesForScope(ctx, kind, scopeID)
	if err != nil {
		return LocalQuotaScopeAttachment{}, fmt.Errorf("list %s quota leases: %w", kind, err)
	}
	attachment := LocalQuotaScopeAttachment{
		ScopeKind: kind, ScopeID: scopeID, Accounts: append([]store.QuotaAccount(nil), accounts...),
		Leases: make([]LocalQuotaLeaseAttachment, 0, len(leases)),
	}
	for _, lease := range leases {
		attachment.Leases = append(attachment.Leases, LocalQuotaLeaseAttachment{Lease: lease, Valid: isValidLocalQuotaLease(lease, observedAt)})
	}
	return attachment, nil
}

func isValidLocalLease(lease store.Lease, observedAt time.Time) bool {
	return lease.State == store.LeaseActive && lease.ExpiresAt.After(observedAt)
}

func isValidLocalQuotaLease(lease store.DurableQuotaLease, observedAt time.Time) bool {
	return lease.State == store.DurableQuotaLeaseActive && lease.ExpiresAt.After(observedAt)
}

func requiredLocalRuntimeActor(actor string) (string, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", fmt.Errorf("local runtime reconcile actor is required")
	}
	return actor, nil
}

func requiredLocalRuntimeReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", fmt.Errorf("local runtime reconcile reason is required")
	}
	return reason, nil
}
