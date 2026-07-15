package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// workflowRunSubject is the application-owned resolution of workflowkit's
// opaque SubjectBinding. The kernel does not know whether a durable Run is
// bound to a sealed task revision or to the frozen source/session that exists
// before materialize_task creates the first revision.
//
// TaskRevision is intentionally nil for an authoring-session Run. Callers
// must branch explicitly instead of manufacturing an empty revision just to
// satisfy a task-only helper.
type workflowRunSubject struct {
	Binding workflowkit.SubjectBinding
	Kind    store.WorkflowRunSubjectKind

	Task     *store.TaskV2
	Revision *store.TaskRevision

	AuthoringSource  *store.AuthoringSource
	AuthoringSession *store.AuthoringSession
	TargetTask       *store.TaskV2
}

func (subject workflowRunSubject) isTaskRevision() bool {
	return subject.Kind == store.WorkflowRunSubjectTaskRevision && subject.Revision != nil && subject.Task != nil
}

func (subject workflowRunSubject) isAuthoringSession() bool {
	return subject.Kind == store.WorkflowRunSubjectAuthoringSession && subject.AuthoringSource != nil && subject.AuthoringSession != nil
}

func (core *lifecycleServiceCore) resolveWorkflowRunSubject(ctx context.Context, run store.WorkflowRun) (workflowRunSubject, error) {
	if core == nil || core.store == nil {
		return workflowRunSubject{}, fmt.Errorf("workflow Run subject resolver is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	switch run.SubjectKind {
	case store.WorkflowRunSubjectTaskRevision:
		if strings.TrimSpace(run.TaskID) == "" || strings.TrimSpace(run.RevisionID) == "" || strings.TrimSpace(run.AuthoringSessionID) != "" {
			return workflowRunSubject{}, fmt.Errorf("workflow Run %s has an invalid task-revision subject", run.ID)
		}
		task, err := core.store.GetTaskV2(ctx, run.TaskID)
		if err != nil {
			return workflowRunSubject{}, err
		}
		if task == nil {
			return workflowRunSubject{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, run.TaskID)
		}
		revision, err := core.store.GetTaskRevision(ctx, run.RevisionID)
		if err != nil {
			return workflowRunSubject{}, err
		}
		if revision == nil || revision.TaskID != task.ID {
			return workflowRunSubject{}, fmt.Errorf("%w: TaskRevision %s", ErrLifecycleNotFound, run.RevisionID)
		}
		binding := workflowkit.SubjectBinding{SubjectID: task.ID, RevisionID: revision.ID, Digest: workflowkit.SubjectDigest(revision.TaskDigest)}
		if err := binding.Validate(); err != nil {
			return workflowRunSubject{}, fmt.Errorf("task-revision workflow subject: %w", err)
		}
		return workflowRunSubject{Binding: binding, Kind: run.SubjectKind, Task: task, Revision: revision}, nil

	case store.WorkflowRunSubjectAuthoringSession:
		if strings.TrimSpace(run.TaskID) != "" || strings.TrimSpace(run.RevisionID) != "" || strings.TrimSpace(run.AuthoringSessionID) == "" {
			return workflowRunSubject{}, fmt.Errorf("workflow Run %s has an invalid authoring-session subject", run.ID)
		}
		session, err := core.store.GetAuthoringSession(ctx, run.AuthoringSessionID)
		if err != nil {
			return workflowRunSubject{}, err
		}
		if session == nil {
			return workflowRunSubject{}, fmt.Errorf("%w: authoring session %s", ErrLifecycleNotFound, run.AuthoringSessionID)
		}
		if session.WorkflowTemplateID != run.WorkflowTemplateID || session.WorkflowTemplateVersion != run.WorkflowTemplateVersion {
			return workflowRunSubject{}, fmt.Errorf("workflow Run %s template differs from its immutable authoring session", run.ID)
		}
		source, err := core.store.GetAuthoringSource(ctx, session.SourceID)
		if err != nil {
			return workflowRunSubject{}, err
		}
		if source == nil {
			return workflowRunSubject{}, fmt.Errorf("%w: authoring source %s", ErrLifecycleNotFound, session.SourceID)
		}
		binding := workflowkit.SubjectBinding{
			SubjectID: source.ID, RevisionID: session.ID, Digest: workflowkit.SubjectDigest(source.SnapshotContentDigest),
		}
		if err := binding.Validate(); err != nil {
			return workflowRunSubject{}, fmt.Errorf("authoring-session workflow subject: %w", err)
		}
		if strings.TrimSpace(session.TargetTaskID) == "" {
			return workflowRunSubject{}, fmt.Errorf("authoring session %s has no draft Task ownership", session.ID)
		}
		targetTask, err := core.store.GetTaskV2(ctx, session.TargetTaskID)
		if err != nil {
			return workflowRunSubject{}, err
		}
		if targetTask == nil || targetTask.LifecycleState != store.TaskLifecycleDraft || targetTask.CurrentRevisionID != "" || targetTask.SourceRepo != source.RepositoryURL || targetTask.SourceCommit != source.CommitSHA {
			return workflowRunSubject{}, fmt.Errorf("authoring session %s draft Task ownership is no longer valid", session.ID)
		}
		return workflowRunSubject{
			Binding: binding, Kind: run.SubjectKind, AuthoringSource: source, AuthoringSession: session, TargetTask: targetTask,
		}, nil

	default:
		return workflowRunSubject{}, fmt.Errorf("workflow Run %s has unsupported subject kind %q", run.ID, run.SubjectKind)
	}
}
