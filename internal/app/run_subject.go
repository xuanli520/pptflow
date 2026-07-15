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
	// Artifact-lineage helpers may intentionally carry only the immutable
	// TaskRevision, whereas admission/ownership resolution additionally needs
	// Task.  Requiring Task here would make a harmless lineage wrapper look
	// like an unsupported subject and would tempt callers to fabricate one.
	return subject.Kind == store.WorkflowRunSubjectTaskRevision && subject.Revision != nil
}

func (subject workflowRunSubject) isAuthoringSession() bool {
	return subject.Kind == store.WorkflowRunSubjectAuthoringSession && subject.AuthoringSource != nil && subject.AuthoringSession != nil
}

// subjectRevisionID and subjectDigest are the sole lineage coordinates used by
// the generic workflow runtime.  For an ordinary Run they are the real task
// revision and task digest; for a pre-materialization Run they are the
// AuthoringSession and immutable source-snapshot digest.  Keeping this
// projection here prevents downstream runtime code from accidentally treating
// a draft Task as a synthetic TaskRevision.
func (subject workflowRunSubject) subjectRevisionID() string {
	return subject.Binding.RevisionID
}

func (subject workflowRunSubject) subjectDigest() string {
	return string(subject.Binding.Digest)
}

// quotaTaskID returns the durable Task account that owns quota.  An
// AuthoringSession has a real draft Task for ownership and accounting, but
// that Task never becomes the workflow subject until materialize_task creates
// its first revision.
func (subject workflowRunSubject) quotaTaskID() (string, error) {
	switch {
	case subject.isTaskRevision() && subject.Task != nil:
		return subject.Task.ID, nil
	case subject.isAuthoringSession() && subject.TargetTask != nil:
		return subject.TargetTask.ID, nil
	default:
		return "", fmt.Errorf("workflow Run subject has no quota-owning Task")
	}
}

func (subject workflowRunSubject) matchesRun(run store.WorkflowRun) bool {
	if subject.Kind != run.SubjectKind || subject.Binding.SubjectID != run.SubjectID ||
		subject.Binding.RevisionID != run.SubjectRevisionID || string(subject.Binding.Digest) != run.SubjectDigest {
		return false
	}
	if subject.isTaskRevision() {
		return subject.Revision != nil && run.TaskID == subject.Binding.SubjectID && run.RevisionID == subject.Revision.ID && run.AuthoringSessionID == ""
	}
	if subject.isAuthoringSession() {
		return subject.AuthoringSession != nil && run.TaskID == "" && run.RevisionID == "" && run.AuthoringSessionID == subject.AuthoringSession.ID
	}
	return false
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
		subject := workflowRunSubject{Binding: binding, Kind: run.SubjectKind, Task: task, Revision: revision}
		if !subject.matchesRun(run) {
			return workflowRunSubject{}, fmt.Errorf("workflow Run %s generic task-revision subject fields differ from its TaskRevision", run.ID)
		}
		return subject, nil

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
		if targetTask == nil || targetTask.SourceRepo != source.RepositoryURL || targetTask.SourceCommit != source.CommitSHA {
			return workflowRunSubject{}, fmt.Errorf("authoring session %s target Task ownership is no longer valid", session.ID)
		}
		// Before materialize_task, the target must still be the reserved draft.
		// Afterwards this same source/session Run remains an immutable historical
		// parent of the generated task-bound child Run, so resolving it must not
		// pretend the newly-created revision makes its original subject invalid.
		// Require the one durable materialization receipt in that case; a revision
		// attached by any other path is not a valid Standard-authoring handoff.
		if targetTask.CurrentRevisionID != "" || targetTask.LifecycleState != store.TaskLifecycleDraft {
			materialization, materializationErr := core.store.GetAuthoringTaskMaterializationForRun(ctx, run.ID)
			if materializationErr != nil {
				return workflowRunSubject{}, materializationErr
			}
			if materialization == nil || materialization.SessionID != session.ID || materialization.SourceID != source.ID ||
				materialization.TaskID != targetTask.ID || materialization.RevisionID != targetTask.CurrentRevisionID {
				return workflowRunSubject{}, fmt.Errorf("authoring session %s target Task is no longer the durable materialization of Run %s", session.ID, run.ID)
			}
		}
		subject := workflowRunSubject{
			Binding: binding, Kind: run.SubjectKind, AuthoringSource: source, AuthoringSession: session, TargetTask: targetTask,
		}
		if !subject.matchesRun(run) {
			return workflowRunSubject{}, fmt.Errorf("workflow Run %s generic authoring subject fields differ from its AuthoringSession", run.ID)
		}
		return subject, nil

	default:
		return workflowRunSubject{}, fmt.Errorf("workflow Run %s has unsupported subject kind %q", run.ID, run.SubjectKind)
	}
}
