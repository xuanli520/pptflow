package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

func TestTaskHubStandardAuthoringPhase1HandoffFactUsesConservativeStates(t *testing.T) {
	parent, task, revision, handoff, child := taskHubStandardAuthoringHandoffProjectionFixture()
	revisions := map[string]store.TaskRevision{revision.ID: revision}
	original := app.DurableJobInspection{Job: store.DurableJob{ID: "019f6207-2345-7000-8000-000000000007", CommandType: taskHubStandardAuthoringHandoffCommandType, State: store.JobQueued}}
	redrive := app.DurableJobInspection{Job: store.DurableJob{ID: "019f6207-2345-7000-8000-000000000008", CommandType: taskHubStandardAuthoringHandoffRedriveCommandType, State: store.JobQueued}}

	tests := []struct {
		name        string
		jobs        []app.DurableJobInspection
		handoff     *store.AuthoringPhase1Handoff
		child       *store.WorkflowRun
		lookupErr   error
		wantState   TaskHubStandardAuthoringPhase1HandoffState
		wantJob     string
		wantRedrive string
	}{
		{name: "not recorded", wantState: TaskHubStandardAuthoringPhase1HandoffNotRecorded},
		{name: "queued original", jobs: []app.DurableJobInspection{original}, wantState: TaskHubStandardAuthoringPhase1HandoffPending, wantJob: string(store.JobQueued)},
		{
			name: "definition hold with queued redrive",
			jobs: []app.DurableJobInspection{
				{Job: store.DurableJob{ID: original.Job.ID, CommandType: original.Job.CommandType, State: store.JobInDoubt}}, redrive,
			},
			wantState: TaskHubStandardAuthoringPhase1HandoffPending, wantJob: string(store.JobInDoubt), wantRedrive: string(store.JobQueued),
		},
		{
			name: "bound child", jobs: []app.DurableJobInspection{{Job: store.DurableJob{ID: original.Job.ID, CommandType: original.Job.CommandType, State: store.JobSucceeded}}},
			wantState: TaskHubStandardAuthoringPhase1HandoffBound, wantJob: string(store.JobSucceeded), handoff: &handoff, child: &child,
		},
		{
			name: "invalid child lineage", jobs: []app.DurableJobInspection{{Job: store.DurableJob{ID: original.Job.ID, CommandType: original.Job.CommandType, State: store.JobSucceeded}}},
			wantState: TaskHubStandardAuthoringPhase1HandoffInvalid, wantJob: string(store.JobSucceeded), handoff: &handoff,
			child: &store.WorkflowRun{ID: child.ID, ParentRunID: "019f6207-2345-7000-8000-000000000099", SubjectKind: child.SubjectKind, TaskID: child.TaskID, RevisionID: child.RevisionID, SubjectDigest: child.SubjectDigest, WorkflowTemplateID: child.WorkflowTemplateID, WorkflowTemplateVersion: child.WorkflowTemplateVersion},
		},
		{name: "lookup unavailable", jobs: []app.DurableJobInspection{original}, lookupErr: errors.New("storage endpoint and secret detail must not leak"), wantState: TaskHubStandardAuthoringPhase1HandoffUnavailable, wantJob: string(store.JobQueued)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fact := taskHubStandardAuthoringPhase1HandoffFact(parent, task, revisions, testCase.jobs, testCase.handoff, testCase.child, testCase.lookupErr)
			if fact.State != testCase.wantState || fact.JobState != testCase.wantJob || fact.RedriveJobState != testCase.wantRedrive {
				t.Fatalf("handoff fact = %+v; want state=%s job=%s redrive=%s", fact, testCase.wantState, testCase.wantJob, testCase.wantRedrive)
			}
			if testCase.lookupErr != nil && strings.Contains(fact.HandoffID+fact.ChildRunID+fact.HandoffFingerprint, "secret") {
				t.Fatalf("unavailable fact leaked lookup detail: %+v", fact)
			}
		})
	}
}

func TestTaskHubStandardAuthoringHandoffRowsExplainInDoubtAndQueuedRedriveWithoutMutation(t *testing.T) {
	parent, _, _, handoff, _ := taskHubStandardAuthoringHandoffProjectionFixture()
	detail := TaskHubDetail{
		SelectedRunID: parent.ID,
		Runs: []TaskHubRunFact{{
			RunID: parent.ID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
			WorkflowTemplateVer: workflowadapter.StandardAuthoringWorkflowTemplateVersion,
		}},
		FrozenExecutions: []TaskHubFrozenExecutionFact{{RunID: parent.ID, State: TaskHubFrozenExecutionBound}},
		StandardAuthoringPhase1Handoffs: []TaskHubStandardAuthoringPhase1HandoffFact{{
			AuthoringRunID: parent.ID, State: TaskHubStandardAuthoringPhase1HandoffPending,
			HandoffID: handoff.ID, ChildRunID: handoff.ChildRunID, HandoffFingerprint: handoff.HandoffFingerprint,
			JobState: string(store.JobInDoubt), RedriveJobState: string(store.JobQueued),
		}},
	}
	overlay := newTaskHubDetailOverlay(TaskHubDetailQuery{RunID: parent.ID})
	overlay.Loading = false
	overlay.Detail = detail
	rendered := strings.Join(overlay.frozenExecutionRows(), "\n")
	for _, required := range []string{
		"Standard -> Phase-1 handoff", "已显式 redrive Phase-1 handoff", "durable job 状态：in_doubt", "最新 redrive delivery 状态：queued",
		handoff.ID, handoff.ChildRunID,
	} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("handoff rows omitted %q:\n%s", required, rendered)
		}
	}
	for _, forbidden := range []string{"storage endpoint", "secret detail", "payload_json", "provider"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("handoff rows leaked %q:\n%s", forbidden, rendered)
		}
	}
}

func taskHubStandardAuthoringHandoffProjectionFixture() (store.WorkflowRun, store.TaskV2, store.TaskRevision, store.AuthoringPhase1Handoff, store.WorkflowRun) {
	const (
		parentID    = "019f6207-2345-7000-8000-000000000001"
		sessionID   = "019f6207-2345-7000-8000-000000000002"
		sourceID    = "019f6207-2345-7000-8000-000000000003"
		taskID      = "019f6207-2345-7000-8000-000000000004"
		revisionID  = "019f6207-2345-7000-8000-000000000005"
		handoffID   = "019f6207-2345-7000-8000-000000000006"
		childID     = "019f6207-2345-7000-8000-000000000009"
		digest      = "harbor.task.v2:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		fingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	parent := store.WorkflowRun{
		ID: parentID, SubjectKind: store.WorkflowRunSubjectAuthoringSession, SubjectID: sourceID, AuthoringSessionID: sessionID,
		WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.StandardAuthoringWorkflowTemplateVersion,
	}
	task := store.TaskV2{ID: taskID}
	revision := store.TaskRevision{ID: revisionID, TaskID: taskID, TaskDigest: digest}
	handoff := store.AuthoringPhase1Handoff{
		ID: handoffID, AuthoringRunID: parentID, AuthoringSessionID: sessionID, AuthoringSourceID: sourceID,
		HandoffArtifactID: "019f6207-2345-7000-8000-000000000010", HandoffFingerprint: fingerprint,
		TaskID: taskID, RevisionID: revisionID, TaskDigest: digest, ChildRunID: childID,
	}
	child := store.WorkflowRun{
		ID: childID, ParentRunID: parentID, SubjectKind: store.WorkflowRunSubjectTaskRevision, TaskID: taskID, RevisionID: revisionID, SubjectDigest: digest,
		WorkflowTemplateID: workflowadapter.CodeEdgePhase1WorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.CodeEdgePhase1WorkflowTemplateVersion,
	}
	return parent, task, revision, handoff, child
}
