package tasklifecycle

import (
	"context"
	"errors"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/pipeline"
)

type RecoveryScope string

const (
	RecoveryScopeAll         RecoveryScope = "all"
	RecoveryScopeStale       RecoveryScope = "stale"
	RecoveryScopeOrphaned    RecoveryScope = "orphaned"
	RecoveryScopeTaskOrphan  RecoveryScope = "task_orphaned"
	RecoveryScopeInterrupted RecoveryScope = "interrupted"
)

type RecoveryRequest struct {
	Scope  RecoveryScope
	TaskID string
	Refs   []pipeline.RunReference
	Reason string
}

func (m *Manager) Recover(ctx context.Context, req RecoveryRequest) (pipeline.RecoveryResult, error) {
	if m == nil || m.store == nil {
		return pipeline.RecoveryResult{}, errors.New("task lifecycle manager unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	service := pipeline.NewRecoveryService(m.store, m.cfg)
	switch req.Scope {
	case "", RecoveryScopeAll:
		orphaned, orphanErr := service.RecoverOrphanedRuns(ctx)
		staleErr := service.RecoverStaleRuns(ctx)
		repairErr := m.store.RepairTaskStates(ctx)
		return orphaned, errors.Join(orphanErr, staleErr, repairErr)
	case RecoveryScopeStale:
		err := service.RecoverStaleRuns(ctx)
		repairErr := m.store.RepairTaskStates(ctx)
		return pipeline.RecoveryResult{}, errors.Join(err, repairErr)
	case RecoveryScopeOrphaned:
		result, err := service.RecoverOrphanedRuns(ctx)
		repairErr := m.store.RepairTaskStates(ctx)
		return result, errors.Join(err, repairErr)
	case RecoveryScopeTaskOrphan:
		taskID, err := NormalizeTaskID(strings.TrimSpace(req.TaskID))
		if err != nil {
			return pipeline.RecoveryResult{}, err
		}
		var result pipeline.RecoveryResult
		_, err = m.withTaskLock(taskID, func() (Result, error) {
			var recoverErr error
			result, recoverErr = service.RecoverOrphanedRunForTask(ctx, taskID)
			repairErr := m.store.RepairTaskStates(ctx)
			return Result{}, errors.Join(recoverErr, repairErr)
		})
		return result, err
	case RecoveryScopeInterrupted:
		result, err := service.RecoverInterruptedRuns(ctx, req.Refs, req.Reason)
		repairErr := m.store.RepairTaskStates(ctx)
		return result, errors.Join(err, repairErr)
	default:
		return pipeline.RecoveryResult{}, errors.New("unknown recovery scope: " + string(req.Scope))
	}
}
