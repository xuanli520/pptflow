package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// LifecycleInspectionService provides a deliberately read-only, joined view
// of V2 lifecycle records. Adapters such as the TUI use it instead of opening
// SQLite or managed directories themselves.
type LifecycleInspectionService struct{ core *lifecycleServiceCore }

// TaskInspectionQuery identifies the immutable Task and, optionally, the Run
// that was selected when opening a detail surface. When RunID is given it must
// belong to TaskID; if TaskID is omitted it is derived from the durable Run.
type TaskInspectionQuery struct {
	TaskID string
	RunID  string
}

// TaskInspectionSnapshot contains only durable facts. JSON payloads remain in
// the store records for auditability, but presentation adapters should project
// only fields that are safe and meaningful for their surface.
type TaskInspectionSnapshot struct {
	Task          store.TaskV2
	SelectedRunID string
	Revisions     []store.TaskRevision
	Runs          []RunInspection
	Releases      []store.LocalPackageRelease
	Artifacts     []ArtifactInspection
	Reviews       []ReviewInspection
	Repairs       []RepairInspection
	ObservedAt    time.Time
}

// RunInspection associates a run with all of its durable stage attempts.
type RunInspection struct {
	Run    store.WorkflowRun
	Stages []store.StageAttempt
	Jobs   []DurableJobInspection
}

// DurableJobInspection joins one durable job with its append-only lease
// history. It is read-only attachment data; callers cannot derive a process
// cancellation handle from this projection.
type DurableJobInspection struct {
	Job    store.DurableJob
	Leases []store.Lease
}

// ArtifactInspection contains an immutable manifest and its typed lineage
// references. Artifact content itself is intentionally not read here.
type ArtifactInspection struct {
	Manifest store.ArtifactManifest
	Refs     []store.ArtifactRef
}

// ReviewInspection represents a review envelope and its immutable decisions.
type ReviewInspection struct {
	Request   store.ReviewRequest
	Decisions []store.ReviewDecision
}

// RepairInspection joins a bounded repair session to prepared provider facts.
type RepairInspection struct {
	Session store.RepairSession
	Changes []RepairChangeInspection
}

// RepairChangeInspection joins a prepared change to its immutable receipts.
type RepairChangeInspection struct {
	Change   store.PreparedChange
	Receipts []store.MutationReceipt
}

// ReadTaskDetail resolves a Task-centric detail snapshot. It performs no
// writes, package creation, filesystem traversal, provider invocation, or
// lease/control transitions.
func (service *LifecycleInspectionService) ReadTaskDetail(ctx context.Context, query TaskInspectionQuery) (TaskInspectionSnapshot, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return TaskInspectionSnapshot{}, fmt.Errorf("lifecycle inspection service is not configured")
	}
	query.TaskID = strings.TrimSpace(query.TaskID)
	query.RunID = strings.TrimSpace(query.RunID)

	var selectedRun *store.WorkflowRun
	if query.RunID != "" {
		run, err := service.core.store.GetWorkflowRun(ctx, query.RunID)
		if err != nil {
			return TaskInspectionSnapshot{}, fmt.Errorf("read selected run %s: %w", query.RunID, err)
		}
		if run == nil {
			return TaskInspectionSnapshot{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, query.RunID)
		}
		selectedRun = run
		if query.TaskID == "" {
			query.TaskID = run.TaskID
		} else if query.TaskID != run.TaskID {
			return TaskInspectionSnapshot{}, fmt.Errorf("run %s does not belong to task %s", run.ID, query.TaskID)
		}
	}
	if query.TaskID == "" {
		return TaskInspectionSnapshot{}, fmt.Errorf("task ID or run ID is required for lifecycle detail")
	}
	task, err := service.core.store.GetTaskV2(ctx, query.TaskID)
	if err != nil {
		return TaskInspectionSnapshot{}, fmt.Errorf("read task %s: %w", query.TaskID, err)
	}
	if task == nil {
		return TaskInspectionSnapshot{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, query.TaskID)
	}

	revisions, err := service.core.store.ListTaskRevisions(ctx, task.ID)
	if err != nil {
		return TaskInspectionSnapshot{}, fmt.Errorf("list revisions for task %s: %w", task.ID, err)
	}
	runs, err := service.core.store.ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil {
		return TaskInspectionSnapshot{}, fmt.Errorf("list runs for task %s: %w", task.ID, err)
	}
	releases, err := service.core.store.ListLocalPackageReleasesForTask(ctx, task.ID)
	if err != nil {
		return TaskInspectionSnapshot{}, fmt.Errorf("list local packages for task %s: %w", task.ID, err)
	}

	snapshot := TaskInspectionSnapshot{
		Task:       *task,
		Revisions:  append([]store.TaskRevision(nil), revisions...),
		Releases:   append([]store.LocalPackageRelease(nil), releases...),
		ObservedAt: service.core.now().UTC(),
	}
	if selectedRun != nil {
		snapshot.SelectedRunID = selectedRun.ID
	}
	for _, run := range runs {
		stages, err := service.core.store.ListStageAttemptsForRun(ctx, run.ID)
		if err != nil {
			return TaskInspectionSnapshot{}, fmt.Errorf("list stages for run %s: %w", run.ID, err)
		}
		jobs, err := service.inspectDurableJobs(ctx, run.ID)
		if err != nil {
			return TaskInspectionSnapshot{}, err
		}
		snapshot.Runs = append(snapshot.Runs, RunInspection{Run: run, Stages: append([]store.StageAttempt(nil), stages...), Jobs: jobs})
	}
	for _, revision := range revisions {
		if err := service.appendRevisionInspection(ctx, &snapshot, revision); err != nil {
			return TaskInspectionSnapshot{}, err
		}
	}
	if err := service.appendRepairInspection(ctx, &snapshot); err != nil {
		return TaskInspectionSnapshot{}, err
	}
	sort.SliceStable(snapshot.Runs, func(left, right int) bool {
		if !snapshot.Runs[left].Run.CreatedAt.Equal(snapshot.Runs[right].Run.CreatedAt) {
			return snapshot.Runs[left].Run.CreatedAt.After(snapshot.Runs[right].Run.CreatedAt)
		}
		return snapshot.Runs[left].Run.ID < snapshot.Runs[right].Run.ID
	})
	return snapshot, nil
}

func (service *LifecycleInspectionService) inspectDurableJobs(ctx context.Context, runID string) ([]DurableJobInspection, error) {
	jobs, err := service.core.store.ListDurableJobsForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list durable jobs for run %s: %w", runID, err)
	}
	result := make([]DurableJobInspection, 0, len(jobs))
	for _, job := range jobs {
		leases, err := service.core.store.ListLeasesForJob(ctx, job.ID)
		if err != nil {
			return nil, fmt.Errorf("list leases for durable job %s: %w", job.ID, err)
		}
		result = append(result, DurableJobInspection{Job: job, Leases: append([]store.Lease(nil), leases...)})
	}
	return result, nil
}

func (service *LifecycleInspectionService) appendRevisionInspection(ctx context.Context, snapshot *TaskInspectionSnapshot, revision store.TaskRevision) error {
	manifests, err := service.core.store.ListArtifactManifestsForRevision(ctx, revision.ID)
	if err != nil {
		return fmt.Errorf("list artifact manifests for revision %s: %w", revision.ID, err)
	}
	for _, manifest := range manifests {
		references, err := service.core.store.ListArtifactRefs(ctx, manifest.ID)
		if err != nil {
			return fmt.Errorf("list artifact refs for manifest %s: %w", manifest.ID, err)
		}
		snapshot.Artifacts = append(snapshot.Artifacts, ArtifactInspection{Manifest: manifest, Refs: append([]store.ArtifactRef(nil), references...)})
	}
	reviews, err := service.core.store.ListReviewRequestsForRevision(ctx, revision.ID)
	if err != nil {
		return fmt.Errorf("list review requests for revision %s: %w", revision.ID, err)
	}
	for _, review := range reviews {
		decisions, err := service.core.store.ListReviewDecisionsForRequest(ctx, review.ID)
		if err != nil {
			return fmt.Errorf("list review decisions for request %s: %w", review.ID, err)
		}
		snapshot.Reviews = append(snapshot.Reviews, ReviewInspection{Request: review, Decisions: append([]store.ReviewDecision(nil), decisions...)})
	}
	return nil
}

func (service *LifecycleInspectionService) appendRepairInspection(ctx context.Context, snapshot *TaskInspectionSnapshot) error {
	subjects := []string{snapshot.Task.ID}
	for _, revision := range snapshot.Revisions {
		subjects = append(subjects, revision.ID)
	}
	for _, run := range snapshot.Runs {
		subjects = append(subjects, run.Run.ID)
	}
	seenSubjects := make(map[string]struct{}, len(subjects))
	seenSessions := make(map[string]struct{})
	for _, subjectID := range subjects {
		if _, seen := seenSubjects[subjectID]; seen {
			continue
		}
		seenSubjects[subjectID] = struct{}{}
		sessions, err := service.core.store.ListRepairSessionsForSubject(ctx, subjectID)
		if err != nil {
			return fmt.Errorf("list repair sessions for subject %s: %w", subjectID, err)
		}
		for _, session := range sessions {
			if _, seen := seenSessions[session.ID]; seen {
				continue
			}
			seenSessions[session.ID] = struct{}{}
			changes, err := service.core.store.ListPreparedChangesForRepairSession(ctx, session.ID)
			if err != nil {
				return fmt.Errorf("list prepared changes for repair session %s: %w", session.ID, err)
			}
			inspection := RepairInspection{Session: session}
			for _, change := range changes {
				receipts, err := service.core.store.ListMutationReceiptsForPreparedChange(ctx, change.ID)
				if err != nil {
					return fmt.Errorf("list mutation receipts for prepared change %s: %w", change.ID, err)
				}
				inspection.Changes = append(inspection.Changes, RepairChangeInspection{Change: change, Receipts: append([]store.MutationReceipt(nil), receipts...)})
			}
			snapshot.Repairs = append(snapshot.Repairs, inspection)
		}
	}
	sort.SliceStable(snapshot.Repairs, func(left, right int) bool {
		if !snapshot.Repairs[left].Session.CreatedAt.Equal(snapshot.Repairs[right].Session.CreatedAt) {
			return snapshot.Repairs[left].Session.CreatedAt.After(snapshot.Repairs[right].Session.CreatedAt)
		}
		return snapshot.Repairs[left].Session.ID < snapshot.Repairs[right].Session.ID
	})
	return nil
}
