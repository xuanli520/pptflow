package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// CandidateRetentionService owns the only allowed cleanup of a terminal
// RevisionCandidate checkout. It never deletes the candidate record, sealed
// revisions, immutable object-store evidence, UUID reservations, or audit
// history.
type CandidateRetentionService struct{ core *lifecycleServiceCore }

type GarbageCollectCandidateRequest struct {
	CandidateID              string
	ExpectedCandidateVersion int64
	IdempotencyKey           string
	Actor                    string
	Reason                   string
}

type CandidateGarbageCollectionResult struct {
	Candidate  store.RevisionCandidate                   `json:"candidate"`
	Operation  store.CandidateGarbageCollectionOperation `json:"operation"`
	Collected  bool                                      `json:"collected"`
	InProgress bool                                      `json:"in_progress"`
}

type SweepExpiredCandidateGarbageCollectionRequest struct {
	Limit  int
	Actor  string
	Reason string
}

type CandidateGarbageCollectionFailure struct {
	CandidateID string `json:"candidate_id"`
	Error       string `json:"error"`
}

// SweepExpiredCandidateGarbageCollectionResult is deliberately explicit
// about per-candidate failures. One unsafe filesystem path cannot prevent
// cleanup of other expired candidates, and every such failure is durably
// recorded on its operation before it is returned here.
type SweepExpiredCandidateGarbageCollectionResult struct {
	Results  []CandidateGarbageCollectionResult  `json:"results"`
	Failures []CandidateGarbageCollectionFailure `json:"failures,omitempty"`
}

// GarbageCollect performs one idempotent candidate checkout tombstone. The
// caller must provide the candidate version observed in its preflight and a
// UUIDv7 idempotency key that remains stable across retries.
func (service *CandidateRetentionService) GarbageCollect(ctx context.Context, request GarbageCollectCandidateRequest) (CandidateGarbageCollectionResult, error) {
	if service == nil || service.core == nil {
		return CandidateGarbageCollectionResult{}, fmt.Errorf("candidate retention service is not configured")
	}
	prepared, err := service.core.store.PrepareCandidateGarbageCollection(ctx, store.PrepareCandidateGarbageCollectionRequest{
		CandidateID:              request.CandidateID,
		ExpectedCandidateVersion: request.ExpectedCandidateVersion,
		IdempotencyKey:           request.IdempotencyKey,
		Actor:                    request.Actor,
		Reason:                   request.Reason,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return CandidateGarbageCollectionResult{}, fmt.Errorf("%w: candidate %s", ErrLifecycleNotFound, request.CandidateID)
		}
		return CandidateGarbageCollectionResult{}, err
	}
	result := candidateGarbageCollectionResult(prepared.Candidate, prepared.Operation)
	if prepared.Operation.State == store.CandidateGarbageCollectionCompleted {
		return result, nil
	}
	finalized, err := service.core.store.FinalizeCandidateGarbageCollection(ctx, store.FinalizeCandidateGarbageCollectionRequest{
		OperationID:     prepared.Operation.ID,
		ExpectedVersion: prepared.Operation.Version,
		Actor:           request.Actor,
		Reason:          request.Reason,
		RemoveDirectory: func() error {
			return removeManagedCandidateDirectory(ctx, service.core.layout, prepared.Candidate)
		},
	})
	if err == nil {
		return candidateGarbageCollectionResult(finalized.Candidate, finalized.Operation), nil
	}
	if !errors.Is(err, store.ErrCandidateGCFilesystem) {
		return result, err
	}
	failed, failureErr := service.core.store.RecordCandidateGarbageCollectionFailure(ctx, store.RecordCandidateGarbageCollectionFailureRequest{
		OperationID:     prepared.Operation.ID,
		ExpectedVersion: prepared.Operation.Version,
		Actor:           request.Actor,
		Reason:          request.Reason,
		ErrorText:       err.Error(),
	})
	if failureErr != nil {
		return result, fmt.Errorf("candidate garbage collection operation %s filesystem failure was not durably recorded: %w", prepared.Operation.ID, failureErr)
	}
	result.Operation = failed
	result.InProgress = true
	return result, fmt.Errorf("candidate garbage collection operation %s: %w", failed.ID, err)
}

// SweepExpired finds retention-eligible terminal candidates and assigns a
// fresh UUIDv7 idempotency key to every independent cleanup operation. A
// controlled worker can call this periodically without accessing SQLite or
// the managed filesystem directly.
func (service *CandidateRetentionService) SweepExpired(ctx context.Context, request SweepExpiredCandidateGarbageCollectionRequest) (SweepExpiredCandidateGarbageCollectionResult, error) {
	if service == nil || service.core == nil {
		return SweepExpiredCandidateGarbageCollectionResult{}, fmt.Errorf("candidate retention service is not configured")
	}
	if request.Limit < 0 {
		return SweepExpiredCandidateGarbageCollectionResult{}, fmt.Errorf("candidate garbage collection limit cannot be negative")
	}
	candidates, err := service.core.store.ListRevisionCandidatesReadyForGarbageCollection(ctx, request.Limit)
	if err != nil {
		return SweepExpiredCandidateGarbageCollectionResult{}, err
	}
	result := SweepExpiredCandidateGarbageCollectionResult{Results: make([]CandidateGarbageCollectionResult, 0, len(candidates))}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		key, err := store.NewUUIDv7()
		if err != nil {
			return result, fmt.Errorf("allocate candidate garbage collection idempotency key: %w", err)
		}
		collected, err := service.GarbageCollect(ctx, GarbageCollectCandidateRequest{
			CandidateID:              candidate.ID,
			ExpectedCandidateVersion: candidate.Version,
			IdempotencyKey:           key,
			Actor:                    request.Actor,
			Reason:                   request.Reason,
		})
		if err != nil {
			result.Failures = append(result.Failures, CandidateGarbageCollectionFailure{CandidateID: candidate.ID, Error: err.Error()})
			continue
		}
		result.Results = append(result.Results, collected)
	}
	return result, nil
}

func candidateGarbageCollectionResult(candidate store.RevisionCandidate, operation store.CandidateGarbageCollectionOperation) CandidateGarbageCollectionResult {
	return CandidateGarbageCollectionResult{
		Candidate:  candidate,
		Operation:  operation,
		Collected:  operation.State == store.CandidateGarbageCollectionCompleted,
		InProgress: operation.State == store.CandidateGarbageCollectionInProgress,
	}
}

// removeManagedCandidateDirectory uses descriptor-rooted filesystem access
// and refuses every symbolic-link or non-directory boundary. Candidate IDs
// are derived from the durable record, never from a caller-provided path.
func removeManagedCandidateDirectory(ctx context.Context, layout managedLayout, candidate store.RevisionCandidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.ValidateUUIDv7(candidate.TaskID); err != nil {
		return err
	}
	if err := store.ValidateUUIDv7(candidate.ID); err != nil {
		return err
	}
	if candidate.CheckoutRelpath != layout.candidateCheckoutRelpath(candidate.TaskID, candidate.ID) {
		return fmt.Errorf("candidate checkout path does not match managed layout")
	}
	rootInfo, err := os.Lstat(layout.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed root for candidate cleanup: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("refusing to clean candidate through an unsafe managed root")
	}
	root, err := os.OpenRoot(layout.root)
	if err != nil {
		return fmt.Errorf("open managed root for candidate cleanup: %w", err)
	}
	defer root.Close()

	tasksInfo, err := root.Lstat(managedTasksDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed task parent for candidate cleanup: %w", err)
	}
	if tasksInfo.Mode()&os.ModeSymlink != 0 || !tasksInfo.IsDir() {
		return fmt.Errorf("refusing to clean candidate through an unsafe managed task parent")
	}
	tasksRoot, err := root.OpenRoot(managedTasksDirectory)
	if err != nil {
		return fmt.Errorf("open managed task parent for candidate cleanup: %w", err)
	}
	defer tasksRoot.Close()

	taskInfo, err := tasksRoot.Lstat(candidate.TaskID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed task for candidate cleanup: %w", err)
	}
	if taskInfo.Mode()&os.ModeSymlink != 0 || !taskInfo.IsDir() {
		return fmt.Errorf("refusing to clean candidate through an unsafe managed task directory")
	}
	taskRoot, err := tasksRoot.OpenRoot(candidate.TaskID)
	if err != nil {
		return fmt.Errorf("open managed task for candidate cleanup: %w", err)
	}
	defer taskRoot.Close()

	candidatesInfo, err := taskRoot.Lstat("candidates")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed candidate parent: %w", err)
	}
	if candidatesInfo.Mode()&os.ModeSymlink != 0 || !candidatesInfo.IsDir() {
		return fmt.Errorf("refusing to clean through an unsafe candidate parent")
	}
	candidatesRoot, err := taskRoot.OpenRoot("candidates")
	if err != nil {
		return fmt.Errorf("open managed candidate parent: %w", err)
	}
	defer candidatesRoot.Close()

	candidateInfo, err := candidatesRoot.Lstat(candidate.ID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect candidate directory for cleanup: %w", err)
	}
	if candidateInfo.Mode()&os.ModeSymlink != 0 || !candidateInfo.IsDir() {
		return fmt.Errorf("refusing to clean an unsafe candidate directory")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := candidatesRoot.RemoveAll(candidate.ID); err != nil {
		return fmt.Errorf("remove managed candidate directory: %w", err)
	}
	return nil
}
