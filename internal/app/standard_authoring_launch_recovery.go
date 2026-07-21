package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringLaunchCaptureFailureCode    = "authoring.source_capture_failed"
	standardAuthoringLaunchCaptureFailureSummary = "Standard 创题源码捕获失败，尚未创建 Task。"
)

// standardAuthoringLaunchRequestRecord makes a prepared authoring.start
// operation independently retryable after its submitting TUI process exits.
// The store keeps only a request fingerprint; this immutable, descriptor-rooted
// record retains the canonical input required to reproduce that fingerprint.
type standardAuthoringLaunchRequestRecord struct {
	Format               string                  `json:"format"`
	Version              string                  `json:"version"`
	LifecycleOperationID string                  `json:"lifecycle_operation_id"`
	RequestFingerprint   workflowkit.Fingerprint `json:"request_fingerprint"`
	RepositoryURL        string                  `json:"repository_url"`
	CommitSHA            string                  `json:"commit_sha"`
	BaseImage            string                  `json:"base_image"`
	TaskType             string                  `json:"task_type"`
	Application          string                  `json:"application"`
	Objective            string                  `json:"objective"`
	Slug                 string                  `json:"slug"`
	Title                string                  `json:"title"`
	MetadataJSON         string                  `json:"metadata_json"`
}

func newStandardAuthoringLaunchRequestRecord(operation store.LifecycleOperation, command StandardAuthoringLaunchCommand, metadata string) (standardAuthoringLaunchRequestRecord, error) {
	record := standardAuthoringLaunchRequestRecord{
		Format:               standardAuthoringLaunchRequestRecordFormat,
		Version:              standardAuthoringLaunchRequestRecordVersion,
		LifecycleOperationID: operation.ID,
		RequestFingerprint:   workflowkit.Fingerprint(operation.RequestFingerprint),
		RepositoryURL:        command.RepositoryURL,
		CommitSHA:            command.CommitSHA,
		BaseImage:            command.BaseImage,
		TaskType:             command.TaskType,
		Application:          command.Application,
		Objective:            command.Objective,
		Slug:                 strings.TrimSpace(command.Slug),
		Title:                strings.TrimSpace(command.Title),
		MetadataJSON:         metadata,
	}
	if _, err := record.Command(operation); err != nil {
		return standardAuthoringLaunchRequestRecord{}, err
	}
	return record, nil
}

func (record standardAuthoringLaunchRequestRecord) request() standardAuthoringLaunchRequest {
	return standardAuthoringLaunchRequest{
		RepositoryURL: record.RepositoryURL,
		CommitSHA:     record.CommitSHA,
		BaseImage:     record.BaseImage,
		TaskType:      record.TaskType,
		Application:   record.Application,
		Objective:     record.Objective,
		Slug:          record.Slug,
		Title:         record.Title,
		MetadataJSON:  record.MetadataJSON,
	}
}

func (record standardAuthoringLaunchRequestRecord) Validate() error {
	if record.Format != standardAuthoringLaunchRequestRecordFormat || record.Version != standardAuthoringLaunchRequestRecordVersion {
		return errors.New("invalid Standard authoring launch request record format")
	}
	if err := store.ValidateUUIDv7(record.LifecycleOperationID); err != nil {
		return fmt.Errorf("Standard authoring launch request lifecycle operation ID: %w", err)
	}
	if err := record.RequestFingerprint.Validate(); err != nil {
		return fmt.Errorf("Standard authoring launch request fingerprint: %w", err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: record.LifecycleOperationID, Actor: "record-validation", Reason: "record-validation"},
		RepositoryURL:                record.RepositoryURL,
		CommitSHA:                    record.CommitSHA,
		BaseImage:                    record.BaseImage,
		TaskType:                     record.TaskType,
		Application:                  record.Application,
		Objective:                    record.Objective,
		Slug:                         record.Slug,
		Title:                        record.Title,
		MetadataJSON:                 record.MetadataJSON,
	}
	coordinate, err := standardAuthoringLaunchCoordinate(command)
	if err != nil || coordinate.RepositoryURL != command.RepositoryURL || coordinate.CommitSHA != command.CommitSHA {
		return errors.New("Standard authoring launch request source coordinate is not canonical")
	}
	environment, err := workflowadapter.NewStandardAuthoringEnvironmentPolicy(command.BaseImage)
	if err != nil || environment.BaseImage != command.BaseImage {
		return errors.New("Standard authoring launch request base image is not canonical")
	}
	brief, err := workflowadapter.NewStandardAuthoringBrief(command.TaskType, command.Application, command.Objective)
	if err != nil || brief.TaskType != command.TaskType || brief.Application != command.Application || brief.Objective != command.Objective {
		return errors.New("Standard authoring launch request brief is not canonical")
	}
	metadata, err := canonicalStandardAuthoringMetadata(command.MetadataJSON)
	if err != nil || metadata != command.MetadataJSON {
		return errors.New("Standard authoring launch request metadata is not canonical")
	}
	if strings.TrimSpace(command.Slug) != command.Slug || strings.TrimSpace(command.Title) != command.Title || command.Slug == "" || command.Title == "" {
		return errors.New("Standard authoring launch request Task identity is not canonical")
	}
	return nil
}

// Command verifies that this immutable record is the exact semantic payload of
// operation before returning a command whose original idempotency key, actor,
// and reason can safely be replayed.
func (record standardAuthoringLaunchRequestRecord) Command(operation store.LifecycleOperation) (StandardAuthoringLaunchCommand, error) {
	if err := record.Validate(); err != nil {
		return StandardAuthoringLaunchCommand{}, err
	}
	if operation.Action != string(standardAuthoringLaunchAction) || operation.State != store.LifecycleOperationPrepared ||
		record.LifecycleOperationID != operation.ID || record.RequestFingerprint != workflowkit.Fingerprint(operation.RequestFingerprint) {
		return StandardAuthoringLaunchCommand{}, fmt.Errorf("%w: Standard authoring launch request record does not match lifecycle operation", store.ErrIdempotencyConflict)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: operation.IdempotencyKey,
			Actor:          operation.Actor,
			Reason:         operation.Reason,
		},
		RepositoryURL: record.RepositoryURL,
		CommitSHA:     record.CommitSHA,
		BaseImage:     record.BaseImage,
		TaskType:      record.TaskType,
		Application:   record.Application,
		Objective:     record.Objective,
		Slug:          record.Slug,
		Title:         record.Title,
		MetadataJSON:  record.MetadataJSON,
	}
	if err := validateStandardAuthoringLaunchCommand(command); err != nil {
		return StandardAuthoringLaunchCommand{}, err
	}
	fingerprint, err := lifecycleMutationFingerprint(standardAuthoringLaunchAction, command.LifecycleMutationCommandBase, record.request())
	if err != nil {
		return StandardAuthoringLaunchCommand{}, err
	}
	if fingerprint != operation.RequestFingerprint {
		return StandardAuthoringLaunchCommand{}, fmt.Errorf("%w: Standard authoring launch request fingerprint", store.ErrIdempotencyConflict)
	}
	return command, nil
}

func (record standardAuthoringLaunchRequestRecord) CanonicalJSON() ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(record)
}

func readStandardAuthoringLaunchRequestRecordAt(directory *os.File) (standardAuthoringLaunchRequestRecord, bool, error) {
	raw, found, err := standardAuthoringReadNewImmutableFileAt(directory, standardAuthoringLaunchRequestRecordFileName, standardAuthoringLaunchRequestRecordMaxBytes)
	if err != nil {
		return standardAuthoringLaunchRequestRecord{}, false, fmt.Errorf("read Standard authoring launch request record: %w", err)
	}
	if !found {
		return standardAuthoringLaunchRequestRecord{}, false, nil
	}
	var record standardAuthoringLaunchRequestRecord
	if err := decodeStrictJSON(string(raw), &record); err != nil {
		return standardAuthoringLaunchRequestRecord{}, false, fmt.Errorf("decode Standard authoring launch request record: %w", err)
	}
	canonical, err := record.CanonicalJSON()
	if err != nil || !bytes.Equal(raw, canonical) {
		return standardAuthoringLaunchRequestRecord{}, false, errors.New("Standard authoring launch request record is not canonical")
	}
	return record, true, nil
}

func ensureStandardAuthoringLaunchRequestRecord(directory *os.File, operation store.LifecycleOperation, command StandardAuthoringLaunchCommand, metadata string) error {
	expected, err := newStandardAuthoringLaunchRequestRecord(operation, command, metadata)
	if err != nil {
		return err
	}
	canonical, err := expected.CanonicalJSON()
	if err != nil {
		return err
	}
	if stored, found, err := readStandardAuthoringLaunchRequestRecordAt(directory); err != nil {
		return err
	} else if found {
		storedCanonical, canonicalErr := stored.CanonicalJSON()
		if canonicalErr != nil || !bytes.Equal(storedCanonical, canonical) {
			return fmt.Errorf("%w: Standard authoring launch request record", store.ErrIdempotencyConflict)
		}
		_, err := stored.Command(operation)
		return err
	}
	if err := standardAuthoringWriteNewImmutableFileAt(directory, standardAuthoringLaunchRequestRecordFileName, canonical, 0o640); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("write Standard authoring launch request record: %w", err)
		}
		stored, found, readErr := readStandardAuthoringLaunchRequestRecordAt(directory)
		if readErr != nil {
			return readErr
		}
		if !found {
			return errors.New("Standard authoring launch request record appeared then disappeared")
		}
		storedCanonical, canonicalErr := stored.CanonicalJSON()
		if canonicalErr != nil || !bytes.Equal(storedCanonical, canonical) {
			return fmt.Errorf("%w: Standard authoring launch request record", store.ErrIdempotencyConflict)
		}
		_, err := stored.Command(operation)
		return err
	}
	return nil
}

// standardAuthoringLaunchCaptureFailure is a deliberately fixed-code failure
// marker. It carries no provider or Git output, so a restarted control plane
// can expose an actionable status without persisting repository-controlled text.
type standardAuthoringLaunchCaptureFailure struct {
	Format                 string                  `json:"format"`
	Version                string                  `json:"version"`
	LifecycleOperationID   string                  `json:"lifecycle_operation_id"`
	RequestFingerprint     workflowkit.Fingerprint `json:"request_fingerprint"`
	PreparationFingerprint workflowkit.Fingerprint `json:"preparation_fingerprint"`
	Code                   string                  `json:"code"`
	Summary                string                  `json:"summary"`
}

func newStandardAuthoringLaunchCaptureFailure(operation store.LifecycleOperation, preparation standardAuthoringLaunchPreparation) standardAuthoringLaunchCaptureFailure {
	return standardAuthoringLaunchCaptureFailure{
		Format:                 standardAuthoringLaunchCaptureFailureFormat,
		Version:                standardAuthoringLaunchCaptureFailureVersion,
		LifecycleOperationID:   operation.ID,
		RequestFingerprint:     workflowkit.Fingerprint(operation.RequestFingerprint),
		PreparationFingerprint: preparation.PreparationFingerprint,
		Code:                   standardAuthoringLaunchCaptureFailureCode,
		Summary:                standardAuthoringLaunchCaptureFailureSummary,
	}
}

func (failure standardAuthoringLaunchCaptureFailure) Validate(operation store.LifecycleOperation, preparation standardAuthoringLaunchPreparation) error {
	if failure.Format != standardAuthoringLaunchCaptureFailureFormat || failure.Version != standardAuthoringLaunchCaptureFailureVersion ||
		failure.Code != standardAuthoringLaunchCaptureFailureCode || failure.Summary != standardAuthoringLaunchCaptureFailureSummary {
		return errors.New("invalid Standard authoring source capture failure format")
	}
	if err := store.ValidateUUIDv7(failure.LifecycleOperationID); err != nil {
		return fmt.Errorf("Standard authoring source capture failure lifecycle operation ID: %w", err)
	}
	if err := failure.RequestFingerprint.Validate(); err != nil {
		return fmt.Errorf("Standard authoring source capture failure request fingerprint: %w", err)
	}
	if err := failure.PreparationFingerprint.Validate(); err != nil {
		return fmt.Errorf("Standard authoring source capture failure preparation fingerprint: %w", err)
	}
	if operation.Action != string(standardAuthoringLaunchAction) || operation.State != store.LifecycleOperationPrepared ||
		failure.LifecycleOperationID != operation.ID || failure.RequestFingerprint != workflowkit.Fingerprint(operation.RequestFingerprint) ||
		failure.PreparationFingerprint != preparation.PreparationFingerprint {
		return fmt.Errorf("%w: Standard authoring source capture failure record", store.ErrIdempotencyConflict)
	}
	return nil
}

func (failure standardAuthoringLaunchCaptureFailure) CanonicalJSON() ([]byte, error) {
	if failure.Format != standardAuthoringLaunchCaptureFailureFormat || failure.Version != standardAuthoringLaunchCaptureFailureVersion ||
		failure.Code != standardAuthoringLaunchCaptureFailureCode || failure.Summary != standardAuthoringLaunchCaptureFailureSummary {
		return nil, errors.New("invalid Standard authoring source capture failure format")
	}
	if err := store.ValidateUUIDv7(failure.LifecycleOperationID); err != nil {
		return nil, err
	}
	if err := failure.RequestFingerprint.Validate(); err != nil {
		return nil, err
	}
	if err := failure.PreparationFingerprint.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(failure)
}

func readStandardAuthoringLaunchCaptureFailureAt(directory *os.File) (standardAuthoringLaunchCaptureFailure, bool, error) {
	raw, found, err := standardAuthoringReadNewImmutableFileAt(directory, standardAuthoringLaunchCaptureFailureFileName, standardAuthoringLaunchCaptureFailureMaxBytes)
	if err != nil {
		return standardAuthoringLaunchCaptureFailure{}, false, fmt.Errorf("read Standard authoring source capture failure: %w", err)
	}
	if !found {
		return standardAuthoringLaunchCaptureFailure{}, false, nil
	}
	var failure standardAuthoringLaunchCaptureFailure
	if err := decodeStrictJSON(string(raw), &failure); err != nil {
		return standardAuthoringLaunchCaptureFailure{}, false, fmt.Errorf("decode Standard authoring source capture failure: %w", err)
	}
	canonical, err := failure.CanonicalJSON()
	if err != nil || !bytes.Equal(raw, canonical) {
		return standardAuthoringLaunchCaptureFailure{}, false, errors.New("Standard authoring source capture failure is not canonical")
	}
	return failure, true, nil
}

func ensureStandardAuthoringLaunchCaptureFailure(directory *os.File, operation store.LifecycleOperation, preparation standardAuthoringLaunchPreparation) error {
	expected := newStandardAuthoringLaunchCaptureFailure(operation, preparation)
	canonical, err := expected.CanonicalJSON()
	if err != nil {
		return err
	}
	if stored, found, err := readStandardAuthoringLaunchCaptureFailureAt(directory); err != nil {
		return err
	} else if found {
		if err := stored.Validate(operation, preparation); err != nil {
			return err
		}
		storedCanonical, canonicalErr := stored.CanonicalJSON()
		if canonicalErr != nil || !bytes.Equal(storedCanonical, canonical) {
			return fmt.Errorf("%w: Standard authoring source capture failure", store.ErrIdempotencyConflict)
		}
		return nil
	}
	if err := standardAuthoringWriteNewImmutableFileAt(directory, standardAuthoringLaunchCaptureFailureFileName, canonical, 0o640); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("write Standard authoring source capture failure: %w", err)
		}
		stored, found, readErr := readStandardAuthoringLaunchCaptureFailureAt(directory)
		if readErr != nil {
			return readErr
		}
		if !found {
			return errors.New("Standard authoring source capture failure appeared then disappeared")
		}
		return stored.Validate(operation, preparation)
	}
	return nil
}

func (service *StandardAuthoringLaunchService) durableCaptureFailure(directory *os.File, operation store.LifecycleOperation, preparation standardAuthoringLaunchPreparation, cause error) error {
	if cause == nil {
		return nil
	}
	if err := ensureStandardAuthoringLaunchCaptureFailure(directory, operation, preparation); err != nil {
		return fmt.Errorf("%w; persist durable Standard authoring source capture failure: %v", cause, err)
	}
	return cause
}

type standardAuthoringFailedPreTaskLaunch struct {
	Operation store.LifecycleOperation
	Request   standardAuthoringLaunchRequestRecord
	Failure   standardAuthoringLaunchCaptureFailure
}

func verifyStandardAuthoringPreparedLaunchEvidence(operation store.LifecycleOperation, preparation standardAuthoringLaunchPreparation) error {
	ids, err := standardAuthoringLaunchIdentities(operation.IdempotencyKey)
	if err != nil {
		return err
	}
	if operation.Action != string(standardAuthoringLaunchAction) || operation.State != store.LifecycleOperationPrepared ||
		preparation.LifecycleOperationID != operation.ID || preparation.RequestedSourceID != ids.SourceID ||
		preparation.TargetTaskID != operation.TaskID || preparation.TargetTaskID != ids.TaskID ||
		preparation.AuthoringSessionID != ids.SessionID || preparation.RunID != operation.RunID || preparation.RunID != ids.RunID ||
		preparation.EnvironmentPolicyArtifactID != ids.EnvironmentPolicyArtifactID || preparation.BriefArtifactID != ids.BriefArtifactID {
		return fmt.Errorf("%w: Standard authoring launch preparation does not match lifecycle operation", store.ErrIdempotencyConflict)
	}
	if _, err := preparation.DeploymentDefinition(); err != nil {
		return err
	}
	return nil
}

func (service *StandardAuthoringLaunchService) listFailedPreTaskLaunches(ctx context.Context) ([]standardAuthoringFailedPreTaskLaunch, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return nil, ErrStandardAuthoringLaunchUnavailable
	}
	operations, err := service.core.store.ListPreparedLifecycleOperationsByAction(ctx, string(standardAuthoringLaunchAction))
	if err != nil {
		return nil, fmt.Errorf("list prepared Standard authoring launch operations: %w", err)
	}
	launches := make([]standardAuthoringFailedPreTaskLaunch, 0)
	for _, operation := range operations {
		task, err := service.core.store.GetTaskV2(ctx, operation.TaskID)
		if err != nil {
			return nil, fmt.Errorf("read prepared Standard authoring launch Task %s: %w", operation.TaskID, err)
		}
		if task != nil {
			continue
		}
		directory, err := service.openExistingStandardAuthoringLaunchOperationDirectory(operation.ID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		request, requestFound, requestErr := readStandardAuthoringLaunchRequestRecordAt(directory)
		if requestErr != nil {
			_ = directory.Close()
			return nil, requestErr
		}
		preparation, preparationFound, preparationErr := readStandardAuthoringLaunchPreparationAt(directory)
		if preparationErr != nil {
			_ = directory.Close()
			return nil, preparationErr
		}
		failure, failureFound, failureErr := readStandardAuthoringLaunchCaptureFailureAt(directory)
		if failureErr != nil {
			_ = directory.Close()
			return nil, failureErr
		}
		receipt, receiptFound, receiptErr := readStandardAuthoringLaunchCaptureReceiptAt(directory)
		closeErr := directory.Close()
		if receiptErr != nil {
			return nil, receiptErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if !requestFound || !preparationFound || !failureFound || receiptFound {
			continue
		}
		if _, err := request.Command(operation); err != nil {
			return nil, err
		}
		if err := verifyStandardAuthoringPreparedLaunchEvidence(operation, preparation); err != nil {
			return nil, err
		}
		if err := failure.Validate(operation, preparation); err != nil {
			return nil, err
		}
		_ = receipt
		launches = append(launches, standardAuthoringFailedPreTaskLaunch{Operation: operation, Request: request, Failure: failure})
	}
	return launches, nil
}

func (service *StandardAuthoringLaunchService) retryFailedPreTaskLaunch(ctx context.Context, operationID string) (LifecycleMutationReceipt, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return LifecycleMutationReceipt{}, ErrStandardAuthoringLaunchUnavailable
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(operationID)); err != nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("Standard authoring launch operation ID: %w", err)
	}
	operation, err := service.core.store.GetLifecycleOperation(ctx, strings.TrimSpace(operationID))
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if operation == nil || operation.Action != string(standardAuthoringLaunchAction) || operation.State != store.LifecycleOperationPrepared {
		return LifecycleMutationReceipt{}, fmt.Errorf("%w: retryable Standard authoring launch %s", ErrLifecycleNotFound, operationID)
	}
	task, err := service.core.store.GetTaskV2(ctx, operation.TaskID)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if task != nil {
		return LifecycleMutationReceipt{}, fmt.Errorf("retryable Standard authoring launch %s already created Task %s", operationID, task.ID)
	}
	directory, err := service.openExistingStandardAuthoringLaunchOperationDirectory(operation.ID)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	request, requestFound, requestErr := readStandardAuthoringLaunchRequestRecordAt(directory)
	preparation, preparationFound, preparationErr := readStandardAuthoringLaunchPreparationAt(directory)
	failure, failureFound, failureErr := readStandardAuthoringLaunchCaptureFailureAt(directory)
	_, receiptFound, receiptErr := readStandardAuthoringLaunchCaptureReceiptAt(directory)
	closeErr := directory.Close()
	if requestErr != nil || preparationErr != nil || failureErr != nil || receiptErr != nil || closeErr != nil {
		return LifecycleMutationReceipt{}, errors.Join(requestErr, preparationErr, failureErr, receiptErr, closeErr)
	}
	if !requestFound || !preparationFound || !failureFound || receiptFound {
		return LifecycleMutationReceipt{}, fmt.Errorf("%w: Standard authoring launch %s has no retryable source capture failure", ErrLifecycleNotFound, operationID)
	}
	command, err := request.Command(*operation)
	if err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if err := verifyStandardAuthoringPreparedLaunchEvidence(*operation, preparation); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	if err := failure.Validate(*operation, preparation); err != nil {
		return LifecycleMutationReceipt{}, err
	}
	return service.Start(ctx, command)
}
