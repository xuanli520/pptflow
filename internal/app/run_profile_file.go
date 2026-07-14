package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

// StartRunFromProfileFileRequest is the explicit local-file boundary used by
// CLI and TUI run-start commands. A run never derives a profile from defaults
// or mutable UI state.
type StartRunFromProfileFileRequest struct {
	ID                string
	TaskID            string
	RevisionID        string
	ProfilePath       string
	ExecutionSpecPath string
	ParentRunID       string
	Trigger           string
	ExecutionEpoch    int
	Actor             string
	Reason            string
}

// ReadExecutionProfileFile parses one explicit local profile file in the
// application layer. It returns the parsed value only after the format's
// strict decoder has rejected trailing/ambiguous JSON and invalid durations.
func ReadExecutionProfileFile(path string) (workflowadapter.ExecutionProfile, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("execution profile path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("resolve execution profile path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("inspect execution profile file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("execution profile path is not a regular file")
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("read execution profile file: %w", err)
	}
	profile, err := workflowadapter.ParseExecutionProfileJSON(raw)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("parse execution profile file: %w", err)
	}
	return profile, nil
}

// ReadRunExecutionSpecFile parses one explicit local V2 execution-spec file.
// The caller must later bind Selection to its captured TaskRevision checkpoint;
// parsing alone deliberately does not authorize a subject choice.
func ReadRunExecutionSpecFile(path string) (workflowadapter.RunExecutionSpec, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("execution specification path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("resolve execution specification path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("inspect execution specification file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("execution specification path is not a regular file")
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("read execution specification file: %w", err)
	}
	specification, err := workflowadapter.ParseRunExecutionSpecJSON(raw)
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("parse execution specification file: %w", err)
	}
	return specification, nil
}

// StartRunFromProfileFile freezes the exact parsed profile through StartRun.
// The profile file is read only here; no caller can substitute hidden numeric
// defaults or override a frozen policy at confirmation time.
func (service *RunService) StartRunFromProfileFile(ctx context.Context, request StartRunFromProfileFileRequest) (store.WorkflowRun, error) {
	profile, err := ReadExecutionProfileFile(request.ProfilePath)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	specification, err := ReadRunExecutionSpecFile(request.ExecutionSpecPath)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	return service.StartRun(ctx, StartRunRequest{
		ID:             request.ID,
		TaskID:         request.TaskID,
		RevisionID:     request.RevisionID,
		Profile:        profile,
		ExecutionSpec:  specification,
		ParentRunID:    request.ParentRunID,
		Trigger:        request.Trigger,
		ExecutionEpoch: request.ExecutionEpoch,
		Actor:          request.Actor,
		Reason:         request.Reason,
	})
}
