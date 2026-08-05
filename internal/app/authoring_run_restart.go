package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// standardAuthoringRestartRequest is the durable caller-selected payload of
// one authoring.restart operation. It names only the immutable source Run;
// every frozen input is copied from that Run's manifest and directory, never
// re-selected by the operator.
type standardAuthoringRestartRequest struct {
	SourceRunID string `json:"source_run_id"`
	Reason      string `json:"reason"`
}

// RestartAuthoringRunCommand re-runs one terminal authoring Run with the same
// frozen source snapshot, launch configuration, profile, and deployment
// catalog. It never re-captures Git and never accepts mutable launch input.
// The new attempt gets its own draft Task, session, and Run so the store's
// one-session-per-Task and one-Run-per-session invariants hold; the new Run
// manifest records the restart lineage.
type RestartAuthoringRunCommand struct {
	LifecycleMutationCommandBase
	SourceRunID string
}

// RestartAuthoringRun is the operator-facing 重跑 entry for an authoring Run
// that reached a terminal outcome (content rejection, repair exhaustion, or
// an already-completed materialization). The old Run, session, and Task
// remain immutable history; the new Run is a fresh attempt over the same
// frozen source snapshot and launch configuration.
func (service *StandardAuthoringLaunchService) RestartAuthoringRun(ctx context.Context, command RestartAuthoringRunCommand) (LifecycleMutationReceipt, error) {
	if service == nil || service.core == nil || service.core.store == nil || service.core.objects == nil {
		return LifecycleMutationReceipt{}, ErrStandardAuthoringLaunchUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sourceRunID := strings.TrimSpace(command.SourceRunID)
	if err := store.ValidateUUIDv7(sourceRunID); err != nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("Standard authoring restart source run ID: %w", err)
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(command.IdempotencyKey)); err != nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("Standard authoring restart idempotency key: %w", err)
	}
	if strings.TrimSpace(command.Actor) == "" || strings.TrimSpace(command.Reason) == "" {
		return LifecycleMutationReceipt{}, fmt.Errorf("Standard authoring restart actor and reason are required")
	}
	oldRun, err := service.core.store.GetWorkflowRun(ctx, sourceRunID)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if oldRun == nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("%w: authoring Run %s", ErrLifecycleNotFound, sourceRunID)
	}
	if oldRun.SubjectKind != store.WorkflowRunSubjectAuthoringSession || oldRun.AuthoringSessionID == "" {
		return LifecycleMutationReceipt{}, fmt.Errorf("Run %s is not a Standard authoring Run", sourceRunID)
	}
	if !restartableAuthoringRunStatus(oldRun.Status) {
		return LifecycleMutationReceipt{}, fmt.Errorf("authoring Run %s is %s; only terminal Runs can be restarted (recoverable failures use 断点恢复)", sourceRunID, oldRun.Status)
	}
	session, err := service.core.store.GetAuthoringSession(ctx, oldRun.AuthoringSessionID)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if session == nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("%w: authoring session %s", ErrLifecycleNotFound, oldRun.AuthoringSessionID)
	}
	source, err := service.core.store.GetAuthoringSource(ctx, session.SourceID)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if source == nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("%w: authoring source %s", ErrLifecycleNotFound, session.SourceID)
	}
	task, err := service.core.store.GetTaskV2(ctx, session.TargetTaskID)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if task == nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("%w: authoring draft task %s", ErrLifecycleNotFound, session.TargetTaskID)
	}
	if task.LifecycleState != store.TaskLifecycleDraft || task.CurrentRevisionID != "" {
		return LifecycleMutationReceipt{}, fmt.Errorf("authoring task %s already materialized a revision; restart is only possible before materialization, start a new 创题 instead", task.ID)
	}
	oldManifest, err := decodeRunManifest(*oldRun)
	if err != nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("decode source authoring Run manifest: %w", err)
	}
	oldContract, err := standardAuthoringContractInputFromSession(ctx, service.core.objects, *session)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	ids, err := standardAuthoringRestartIdentities(command.IdempotencyKey)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	expected := command.Expected
	expected.RunID = oldRun.ID
	expected.RunVersion = oldRun.Version
	expected.RunDefinitionHash = oldRun.DefinitionHash

	mutations := &LifecycleMutationService{core: service.core}
	op, replay, err := mutations.begin(ctx, standardAuthoringRestartAction, LifecycleMutationCommandBase{
		IdempotencyKey: command.IdempotencyKey, Actor: command.Actor, Reason: command.Reason, Expected: expected,
	}, standardAuthoringRestartRequest{SourceRunID: sourceRunID, Reason: strings.TrimSpace(command.Reason)}, lifecycleOperationTargets{TaskID: ids.TaskID, RunID: ids.RunID})
	if err != nil || replay != nil {
		return lifecycleReplayResult(replay, err)
	}
	if op.TaskID != ids.TaskID || op.RunID != ids.RunID || op.Actor != strings.TrimSpace(command.Actor) || op.Reason != strings.TrimSpace(command.Reason) {
		return LifecycleMutationReceipt{}, fmt.Errorf("%w: Standard authoring restart lifecycle operation %s", store.ErrIdempotencyConflict, op.ID)
	}
	if receipt, found, replayErr := mutations.ReplayCompleted(ctx, standardAuthoringRestartAction, command.IdempotencyKey); replayErr != nil {
		return LifecycleMutationReceipt{}, replayErr
	} else if found {
		return receipt, nil
	}

	newTask, err := service.createRestartAuthoringDraftTask(ctx, ids.TaskID, *task, *source, command.Actor, command.Reason)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	newContract, err := service.createRestartAuthoringContract(ctx, oldContract.Contract, *task, newTask, ids.ContractArtifactID)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	frozen, err := service.restartStandardAuthoringFrozenDefinition(ctx, *oldRun, *session, oldManifest, *newContract, ids.SessionID)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	var oldSessionManifest standardAuthoringSessionManifest
	if err := decodeStrictJSON(session.SessionManifestJSON, &oldSessionManifest); err != nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("decode source authoring session manifest: %w", err)
	}
	preparation := standardAuthoringLaunchPreparation{
		Format:                      standardAuthoringLaunchPreparationFormat,
		Version:                     standardAuthoringLaunchPreparationVersion,
		LifecycleOperationID:        op.ID,
		RequestedSourceID:           oldSessionManifest.RequestedSourceID,
		TargetTaskID:                newTask.ID,
		AuthoringSessionID:          ids.SessionID,
		RunID:                       ids.RunID,
		WorkflowTemplateID:          session.WorkflowTemplateID,
		WorkflowTemplateVersion:     session.WorkflowTemplateVersion,
		SourceSnapshotSchemaVersion: source.SnapshotSchemaVersion,
		ExecutionProfile:            append(json.RawMessage(nil), frozen.ProfileCanonical...),
		ProfileFingerprint:          frozen.ProfileFingerprint,
		DeploymentCatalogReceipt:    append(json.RawMessage(nil), frozen.DeploymentCatalogReceipt...),
		PreparationFingerprint:      oldSessionManifest.PreparationFingerprint,
	}
	sessionManifestJSON, err := standardAuthoringSessionManifestJSON(*source, newTask, ids.SessionID, op.ID, preparation, frozen.frozenDefinition(), *newContract)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	var sessionManifest standardAuthoringSessionManifest
	if err := decodeStrictJSON(sessionManifestJSON, &sessionManifest); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	sessionManifest.RestartOfRunID = oldRun.ID
	encodedSessionManifest, err := json.Marshal(sessionManifest)
	if err != nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("encode restart authoring session manifest: %w", err)
	}
	newSession, err := service.core.store.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		ID:                      ids.SessionID,
		SourceID:                source.ID,
		TargetTaskID:            newTask.ID,
		WorkflowTemplateID:      session.WorkflowTemplateID,
		WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		SessionManifestJSON:     string(encodedSessionManifest),
		IdempotencyKey:          standardAuthoringRestartChildKey(command.IdempotencyKey, "session"),
		Actor:                   command.Actor,
		Reason:                  command.Reason,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if err := verifyStandardAuthoringLaunchSession(newSession, *source, newTask, op.ID, preparation, frozen.frozenDefinition(), *newContract); err != nil {
		return LifecycleMutationReceipt{}, err
	}

	run, err := (&RunService{core: service.core}).StartAuthoringRun(ctx, StartAuthoringRunRequest{
		ID:                       ids.RunID,
		AuthoringSessionID:       newSession.ID,
		Profile:                  frozen.Profile,
		ExecutionSpec:            frozen.ExecutionSpec,
		ProfileFingerprint:       frozen.ProfileFingerprint,
		ExecutionSpecFingerprint: frozen.ExecutionSpecFingerprint,
		DeploymentCatalogReceipt: append([]byte(nil), frozen.DeploymentCatalogReceipt...),
		RestartOfRunID:           oldRun.ID,
		Trigger:                  standardAuthoringRestartTrigger,
		Actor:                    command.Actor,
		Reason:                   command.Reason,
	})
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return mutations.complete(ctx, op, LifecycleMutationReceipt{
		Action:               standardAuthoringRestartAction,
		TaskID:               newTask.ID,
		TaskVersion:          newTask.Version,
		RunID:                run.ID,
		RunVersion:           run.Version,
		AuthoringSourceID:    source.ID,
		AuthoringSessionID:   newSession.ID,
		SourceSnapshotDigest: source.SnapshotContentDigest,
		Summary:              "已复用冻结源码与同一创题配置重跑（新 Run 保留旧 Run 为历史）",
	})
}

// restartableAuthoringRunStatus admits only immutable historical outcomes.
// Recoverable failures and interruptions belong to 断点恢复; queued, running,
// and in-doubt Runs must not be cloned while they can still move.
func restartableAuthoringRunStatus(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunSucceeded, store.WorkflowRunFailedTerminal, store.WorkflowRunCanceled:
		return true
	default:
		return false
	}
}

type standardAuthoringRestartIDs struct {
	TaskID             string
	SessionID          string
	RunID              string
	ContractArtifactID string
}

func standardAuthoringRestartIdentities(idempotencyKey string) (standardAuthoringRestartIDs, error) {
	if err := store.ValidateUUIDv7(strings.TrimSpace(idempotencyKey)); err != nil {
		return standardAuthoringRestartIDs{}, err
	}
	return standardAuthoringRestartIDs{
		TaskID:             standardAuthoringRestartIdentity(idempotencyKey, "task"),
		SessionID:          standardAuthoringRestartIdentity(idempotencyKey, "session"),
		RunID:              standardAuthoringRestartIdentity(idempotencyKey, "run"),
		ContractArtifactID: standardAuthoringRestartIdentity(idempotencyKey, "authoring-contract"),
	}, nil
}

// createRestartAuthoringDraftTask clones the source Run's still-draft Task so
// the restart attempt keeps the store's one-session-per-Task invariant. The
// clone carries the same slug, title, metadata, and frozen source binding;
// only its identity is fresh.
func (service *StandardAuthoringLaunchService) createRestartAuthoringDraftTask(ctx context.Context, taskID string, oldTask store.TaskV2, source store.AuthoringSource, actor, reason string) (store.TaskV2, error) {
	existing, err := service.core.store.GetTaskV2(ctx, taskID)
	if err != nil {
		return store.TaskV2{}, err
	}
	if existing != nil {
		if !restartAuthoringTaskMatches(*existing, oldTask, source) {
			return store.TaskV2{}, fmt.Errorf("%w: restart authoring draft Task %s", store.ErrIdempotencyConflict, taskID)
		}
		return *existing, nil
	}
	created, err := service.core.store.CreateTaskV2(ctx, store.CreateTaskV2Request{
		ID:             taskID,
		Slug:           oldTask.Slug,
		Title:          oldTask.Title,
		MetadataJSON:   oldTask.MetadataJSON,
		SourceRepo:     source.RepositoryURL,
		SourceCommit:   source.CommitSHA,
		LifecycleState: store.TaskLifecycleDraft,
		Actor:          actor,
		Reason:         reason,
	})
	if err != nil {
		return store.TaskV2{}, err
	}
	if !restartAuthoringTaskMatches(created, oldTask, source) {
		return store.TaskV2{}, fmt.Errorf("%w: restart authoring draft Task %s", store.ErrIdempotencyConflict, created.ID)
	}
	return created, nil
}

func restartAuthoringTaskMatches(task, oldTask store.TaskV2, source store.AuthoringSource) bool {
	return task.Slug == oldTask.Slug && task.Title == oldTask.Title && task.MetadataJSON == oldTask.MetadataJSON &&
		task.SourceRepo == source.RepositoryURL && task.SourceCommit == source.CommitSHA &&
		task.LifecycleState == store.TaskLifecycleDraft && task.CurrentRevisionID == ""
}

// createRestartAuthoringContract rebuilds the immutable root contract for the
// cloned Task. Only the Task identity changes: every launch configuration
// fact (base image, objective, profile fingerprint, source binding) is copied
// from the source Run's contract object.
func (service *StandardAuthoringLaunchService) createRestartAuthoringContract(ctx context.Context, oldContract workflowadapter.AuthoringContract, oldTask, newTask store.TaskV2, artifactID string) (*standardAuthoringContractInput, error) {
	if oldContract.Task.ID != oldTask.ID {
		return nil, fmt.Errorf("source authoring contract does not bind its draft Task")
	}
	rebuilt := oldContract
	rebuilt.Task.ID = newTask.ID
	contract, err := newStandardAuthoringContractInput(artifactID, rebuilt)
	if err != nil {
		return nil, err
	}
	object, err := service.core.objects.PutBytes(ctx, contract.CanonicalJSON)
	if err != nil {
		return nil, fmt.Errorf("store restart Standard authoring contract: %w", err)
	}
	if object.Digest != contract.ContentDigest || object.SizeBytes != int64(len(contract.CanonicalJSON)) {
		return nil, fmt.Errorf("stored restart Standard authoring contract does not match its canonical binding")
	}
	return &contract, nil
}

// restartStandardAuthoringFrozenDefinition freezes the new attempt's inputs:
// the canonical profile bytes are copied from the source Run's managed
// directory, the execution specification is rebound to the fresh session and
// contract artifact, and the deployment catalog receipt is reused
// byte-for-byte. The new definition fingerprint is derived from these exact
// inputs, never guessed.
func (service *StandardAuthoringLaunchService) restartStandardAuthoringFrozenDefinition(ctx context.Context, oldRun store.WorkflowRun, oldSession store.AuthoringSession, oldManifest runManifest, contract standardAuthoringContractInput, newSessionID string) (*restartAuthoringFrozenDefinition, error) {
	profileRaw, err := readManagedRunExecutionInputFile(filepath.Join(service.core.layout.runDirectory(oldRun.ID), runExecutionProfileFileName), "source authoring execution profile")
	if err != nil {
		return nil, err
	}
	profile, err := workflowadapter.ParseExecutionProfileJSON(profileRaw)
	if err != nil {
		return nil, fmt.Errorf("parse source authoring execution profile: %w", err)
	}
	profileCanonical, err := profile.CanonicalJSON()
	if err != nil || !bytes.Equal(profileRaw, profileCanonical) {
		return nil, fmt.Errorf("source authoring execution profile is not canonical")
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil || profileFingerprint != oldManifest.Inputs.ProfileFingerprint {
		return nil, fmt.Errorf("source authoring execution profile fingerprint does not match its run manifest")
	}
	specification, _, _, err := canonicalFrozenRunExecutionSpec(oldManifest, oldRun)
	if err != nil {
		return nil, err
	}
	if specification.Selection.Kind != workflowadapter.RunSelectionAuthoringSession {
		return nil, fmt.Errorf("source authoring Run execution specification is not an authoring selection")
	}
	specification.Selection.AuthoringSessionID = newSessionID
	for index := range specification.References.Checkouts {
		specification.References.Checkouts[index].RevisionID = newSessionID
	}
	specification, err = specification.BindManagedArtifactInput(workflowadapter.AuthoringContractArtifact, contract.artifactReference())
	if err != nil {
		return nil, fmt.Errorf("rebind restart Standard authoring contract: %w", err)
	}
	specCanonical, specFingerprint, err := canonicalExecutionSpec(specification)
	if err != nil {
		return nil, err
	}
	if err := validateRunExecutionSpecOperationResolver(specification, service.core.operationResolver); err != nil {
		return nil, fmt.Errorf("restart execution specification operation resolver: %w", err)
	}
	if err := service.core.validateDeploymentCatalogExecutionSpec(specification); err != nil {
		return nil, fmt.Errorf("restart execution specification deployment catalog: %w", err)
	}
	receipt := append([]byte(nil), oldManifest.DeploymentCatalogReceipt...)
	definitionFingerprint, err := standardAuthoringDefinitionFingerprint(profileCanonical, specCanonical, receipt)
	if err != nil {
		return nil, err
	}
	return &restartAuthoringFrozenDefinition{
		Profile: profile, ProfileCanonical: profileCanonical, ProfileFingerprint: profileFingerprint,
		ExecutionSpec: specification, ExecutionSpecFingerprint: specFingerprint,
		DeploymentCatalogReceipt: receipt,
		Fingerprint:              definitionFingerprint,
	}, nil
}

type restartAuthoringFrozenDefinition struct {
	Profile                  workflowadapter.ExecutionProfile
	ProfileCanonical         []byte
	ProfileFingerprint       workflowkit.Fingerprint
	ExecutionSpec            workflowadapter.RunExecutionSpec
	ExecutionSpecFingerprint workflowkit.Fingerprint
	DeploymentCatalogReceipt []byte
	Fingerprint              workflowkit.Fingerprint
}

func (definition *restartAuthoringFrozenDefinition) frozenDefinition() standardAuthoringFrozenDefinition {
	return standardAuthoringFrozenDefinition{
		Profile: definition.Profile, ExecutionSpec: definition.ExecutionSpec,
		ProfileFingerprint: definition.ProfileFingerprint, ExecutionSpecFingerprint: definition.ExecutionSpecFingerprint,
		DeploymentCatalogReceipt: definition.DeploymentCatalogReceipt,
		Fingerprint:              definition.Fingerprint,
	}
}
