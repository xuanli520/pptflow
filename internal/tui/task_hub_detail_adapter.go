package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var _ TaskHubDetailReader = (*AppTaskHubLifecycleAdapter)(nil)

// QueryTaskHubDetail projects a joined lifecycle inspection snapshot into the
// UI-safe Task/Run detail model. It performs no writes and never reads a
// workspace, revision snapshot, artifact payload, or package path from TUI.
func (adapter *AppTaskHubLifecycleAdapter) QueryTaskHubDetail(ctx context.Context, query TaskHubDetailQuery) (TaskHubDetail, error) {
	services, err := adapter.lifecycleServices()
	if err != nil {
		return TaskHubDetail{}, err
	}
	if services.Inspection == nil {
		return TaskHubDetail{}, fmt.Errorf("Task Hub lifecycle inspection service is unavailable")
	}
	dataStore := services.Store()
	if dataStore == nil {
		return TaskHubDetail{}, fmt.Errorf("Task Hub read-only lifecycle store is unavailable")
	}
	inspection, err := services.Inspection.ReadTaskDetail(ctx, app.TaskInspectionQuery{
		TaskID: strings.TrimSpace(query.TaskID),
		RunID:  strings.TrimSpace(query.RunID),
	})
	if err != nil {
		return TaskHubDetail{}, fmt.Errorf("read Task Hub detail: %w", err)
	}
	detail := TaskHubDetail{
		Task: TaskHubDetailTask{
			TaskID:            inspection.Task.ID,
			Slug:              inspection.Task.Slug,
			Name:              taskHubTaskName(inspection.Task),
			Lifecycle:         string(inspection.Task.LifecycleState),
			CurrentRevisionID: inspection.Task.CurrentRevisionID,
			SourceRepo:        inspection.Task.SourceRepo,
			SourceCommit:      inspection.Task.SourceCommit,
			CreatedAt:         inspection.Task.CreatedAt,
			UpdatedAt:         inspection.Task.UpdatedAt,
		},
		SelectedRunID: inspection.SelectedRunID,
		ObservedAt:    inspection.ObservedAt,
	}
	revisionsByID := make(map[string]store.TaskRevision, len(inspection.Revisions))
	for _, revision := range inspection.Revisions {
		revisionsByID[revision.ID] = revision
		detail.Revisions = append(detail.Revisions, TaskHubRevisionFact{
			RevisionID:                 revision.ID,
			VersionNumber:              revision.VersionNumber,
			ParentRevisionID:           revision.ParentRevisionID,
			Origin:                     string(revision.Origin),
			State:                      string(revision.State),
			TaskDigest:                 revision.TaskDigest,
			ValidationEvidenceManifest: revision.ValidationEvidenceManifest,
			ChangeSummary:              revision.ChangeSummary,
			Current:                    revision.ID == inspection.Task.CurrentRevisionID,
			CreatedAt:                  revision.CreatedAt,
			StateUpdatedAt:             revision.StateUpdatedAt,
		})
	}
	for _, runInspection := range inspection.Runs {
		run := runInspection.Run
		// Project the immutable manifest through the narrow TUI-safe reader.
		// The raw JSON never enters TaskHubDetail, and an invalid binding is
		// retained only as a conservative status rather than a partial contract.
		detail.FrozenExecutions = append(detail.FrozenExecutions, taskHubFrozenExecutionFromRun(run))
		projection := TaskHubRunFact{
			RunID:               run.ID,
			RevisionID:          run.RevisionID,
			ParentRunID:         run.ParentRunID,
			Status:              string(run.Status),
			Trigger:             run.Trigger,
			ExecutionEpoch:      run.ExecutionEpoch,
			WorkflowTemplateID:  run.WorkflowTemplateID,
			WorkflowTemplateVer: run.WorkflowTemplateVersion,
			ResolvedProfileHash: run.ResolvedProfileHash,
			DefinitionHash:      run.DefinitionHash,
			CreatedAt:           run.CreatedAt,
			StartedAt:           taskHubDetailOptionalTime(run.StartedAt),
			FinishedAt:          taskHubDetailOptionalTime(run.FinishedAt),
		}
		for _, stage := range runInspection.Stages {
			projection.Stages = append(projection.Stages, TaskHubStageFact{
				StageAttemptID:     stage.ID,
				RetryOfStageID:     stage.RetryOfStageAttemptID,
				StageKey:           stage.StageKey,
				StageGroup:         stage.StageGroup,
				Ordinal:            stage.Ordinal,
				ExecutionState:     string(stage.ExecutionStatus),
				Verdict:            string(stage.Verdict),
				FailureClass:       stage.FailureClass,
				HasRecordedError:   strings.TrimSpace(stage.ErrorText) != "",
				ArtifactManifestID: stage.ArtifactManifestID,
				CreatedAt:          stage.CreatedAt,
				StartedAt:          taskHubDetailOptionalTime(stage.StartedAt),
				FinishedAt:         taskHubDetailOptionalTime(stage.FinishedAt),
			})
		}
		detail.Runs = append(detail.Runs, projection)
		if taskHubIsCodeEdgePhase1Run(run) {
			record, recordErr := dataStore.GetCodeEdgeComplianceRecordForRun(ctx, run.ID)
			if recordErr != nil {
				return TaskHubDetail{}, fmt.Errorf("read CodeEdge final compliance for Run %s: %w", run.ID, recordErr)
			}
			revision, found := revisionsByID[run.RevisionID]
			if !found {
				return TaskHubDetail{}, fmt.Errorf("CodeEdge Run %s references unavailable revision %s", run.ID, run.RevisionID)
			}
			detail.CodeEdgeCompliance = append(detail.CodeEdgeCompliance, taskHubCodeEdgeComplianceFact(run, revision, record))
		}
	}
	for _, release := range inspection.Releases {
		detail.Releases = append(detail.Releases, TaskHubReleaseFact{
			ReleaseID:      release.ID,
			ReleaseVersion: release.ReleaseVersion,
			RevisionID:     release.RevisionID,
			TaskDigest:     release.TaskDigest,
			EvidenceRef:    release.EvidenceRef,
			PublishedAt:    release.PublishedAt,
			WithdrawnAt:    taskHubDetailOptionalTime(release.WithdrawnAt),
		})
	}
	for _, artifact := range inspection.Artifacts {
		projection := TaskHubArtifactFact{
			ManifestID:          artifact.Manifest.ID,
			RevisionID:          artifact.Manifest.SubjectRevisionID,
			SubjectDigest:       artifact.Manifest.SubjectDigest,
			WorkflowFingerprint: artifact.Manifest.WorkflowFingerprint,
			CreatedAt:           artifact.Manifest.CreatedAt,
		}
		for _, reference := range artifact.Refs {
			projection.Refs = append(projection.Refs, TaskHubArtifactRefFact{
				ArtifactKey:   reference.ArtifactKey,
				ContentDigest: reference.ContentDigest,
				SchemaVersion: reference.SchemaVersion,
				RunID:         reference.RunID,
				StageKey:      reference.StageKey,
				AttemptID:     reference.AttemptID,
				TurnOrdinal:   reference.TurnOrdinal,
			})
		}
		detail.Artifacts = append(detail.Artifacts, projection)
	}
	for _, review := range inspection.Reviews {
		projection := TaskHubReviewFact{
			ReviewRequestID:  review.Request.ID,
			RevisionID:       review.Request.RevisionID,
			State:            review.Request.State,
			EvidenceManifest: review.Request.EvidenceManifestDigest,
			CreatedAt:        review.Request.CreatedAt,
			ClosedAt:         taskHubDetailOptionalTime(review.Request.ClosedAt),
		}
		for _, decision := range review.Decisions {
			projection.Decisions = append(projection.Decisions, TaskHubReviewDecisionFact{
				DecisionID:             decision.ID,
				Action:                 string(decision.Action),
				ExpectedRevisionDigest: decision.ExpectedRevisionDigest,
				CreatedAt:              decision.CreatedAt,
			})
		}
		detail.Reviews = append(detail.Reviews, projection)
	}
	for _, repair := range inspection.Repairs {
		projection := TaskHubRepairFact{
			RepairSessionID: repair.Session.ID,
			SubjectID:       repair.Session.SubjectID,
			BaseRevisionID:  repair.Session.BaseRevisionID,
			Status:          string(repair.Session.Status),
			MaxRounds:       repair.Session.MaxRounds,
			CreatedAt:       repair.Session.CreatedAt,
			UpdatedAt:       repair.Session.UpdatedAt,
		}
		for _, change := range repair.Changes {
			changeProjection := TaskHubRepairChangeFact{
				PreparedChangeID: change.Change.ID,
				RoundOrdinal:     change.Change.RoundOrdinal,
				ProviderID:       change.Change.ProviderID,
				BeforeDigest:     change.Change.BeforeDigest,
				AfterDigest:      change.Change.AfterDigest,
				CreatedAt:        change.Change.CreatedAt,
			}
			for _, receipt := range change.Receipts {
				changeProjection.Receipts = append(changeProjection.Receipts, TaskHubMutationReceiptFact{
					ReceiptID: receipt.ID,
					Outcome:   string(receipt.Outcome),
					CreatedAt: receipt.CreatedAt,
				})
			}
			projection.Changes = append(projection.Changes, changeProjection)
		}
		detail.Repairs = append(detail.Repairs, projection)
	}
	return detail, nil
}

func taskHubDetailOptionalTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}

func taskHubCodeEdgeComplianceFact(run store.WorkflowRun, revision store.TaskRevision, record *store.CodeEdgeComplianceRecord) TaskHubCodeEdgeComplianceFact {
	fact := TaskHubCodeEdgeComplianceFact{RunID: run.ID, State: TaskHubCodeEdgeComplianceNotRecorded}
	if record == nil {
		return fact
	}
	fact.ComplianceRecordID = record.ID
	fact.RecordedAt = record.CreatedAt
	if record.RunID != run.ID || record.TaskID != run.TaskID || record.RevisionID != run.RevisionID || record.RevisionID != revision.ID || record.TaskDigest != revision.TaskDigest ||
		workflowkit.Fingerprint(strings.TrimSpace(record.DecisionFingerprint)).Validate() != nil {
		fact.State = TaskHubCodeEdgeComplianceInvalid
		return fact
	}
	switch record.Status {
	case store.CodeEdgeComplianceApproved:
		if workflowkit.Fingerprint(strings.TrimSpace(record.AuthorizationFingerprint)).Validate() != nil {
			fact.State = TaskHubCodeEdgeComplianceInvalid
			return fact
		}
		fact.State = TaskHubCodeEdgeComplianceApproved
	case store.CodeEdgeComplianceRejected:
		if strings.TrimSpace(record.AuthorizationFingerprint) != "" {
			fact.State = TaskHubCodeEdgeComplianceInvalid
			return fact
		}
		fact.State = TaskHubCodeEdgeComplianceRejected
	default:
		fact.State = TaskHubCodeEdgeComplianceInvalid
		return fact
	}
	fact.RevisionID = record.RevisionID
	fact.TaskDigest = record.TaskDigest
	fact.DecisionFingerprint = record.DecisionFingerprint
	fact.AuthorizationFingerprint = record.AuthorizationFingerprint
	return fact
}
