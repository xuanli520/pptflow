package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Engine struct {
	registry *Registry
	runtimes Runtimes
}

func NewEngine(registry *Registry, runtimes Runtimes) *Engine {
	return &Engine{registry: registry, runtimes: runtimes}
}

func (e *Engine) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	start := time.Now().UTC()
	if err := validateRunRequest(req); err != nil {
		return RunResult{}, err
	}
	if e == nil || e.registry == nil {
		return RunResult{}, fmt.Errorf("workflow engine registry is required")
	}
	runID := newRunID(req.Workflow.ID)
	store, err := NewFileArtifactStore(filepath.Join(req.ArtifactRoot, runID))
	if err != nil {
		return RunResult{}, err
	}
	workspaceRoot, err := prepareWorkspace(req.WorkspaceRoot, runID)
	if err != nil {
		return RunResult{}, err
	}
	events := NewEventRecorder()
	result := RunResult{
		RunID:         runID,
		WorkflowID:    req.Workflow.ID,
		Status:        RunSucceeded,
		ArtifactRoot:  store.Root(),
		WorkspaceRoot: workspaceRoot,
		StartedAt:     start,
	}
	_ = events.Emit(ctx, Event{RunID: runID, Type: "run_started", Message: req.Workflow.Name})
	prior := map[string]NodeRun{}
	for _, spec := range req.Workflow.Nodes {
		nodeRun := NodeRun{NodeID: spec.ID, Kind: spec.Kind, Name: spec.Name, Status: NodePending}
		if blockedBy := blockedDependency(spec, prior); blockedBy != "" {
			nodeRun.Status = NodeSkipped
			nodeRun.Error = "dependency failed: " + blockedBy
			result.Nodes = append(result.Nodes, nodeRun)
			prior[spec.ID] = nodeRun
			result.Status = RunFailed
			continue
		}
		plugin, err := e.registry.Lookup(spec)
		if err == nil {
			err = plugin.Validate(spec)
		}
		if err != nil {
			nodeRun.Status = NodeFailed
			nodeRun.Error = err.Error()
			nodeRun.Metrics = NodeMetrics{FailureType: "validation"}
			result.Nodes = append(result.Nodes, nodeRun)
			prior[spec.ID] = nodeRun
			result.Status = RunFailed
			break
		}
		run, artifacts, err := e.executeNode(ctx, plugin, NodeRequest{
			RunID:         runID,
			Spec:          spec,
			ArtifactRoot:  store.Root(),
			WorkspaceRoot: workspaceRoot,
			Input:         req.Input,
			Store:         store,
			Events:        events,
			Runtimes:      e.runtimes,
			Prior:         clonePrior(prior),
		})
		result.Nodes = append(result.Nodes, run)
		result.Artifacts = append(result.Artifacts, artifacts...)
		prior[spec.ID] = run
		if err != nil {
			result.Status = RunFailed
			break
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		result.Status = RunCancelled
	}
	result.Events = events.Events()
	result.FinishedAt = time.Now().UTC()
	result.DurationMS = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
	_ = writeResultFiles(store.Root(), result)
	return result, resultError(result)
}

func (e *Engine) executeNode(ctx context.Context, plugin Plugin, req NodeRequest) (NodeRun, []ArtifactRef, error) {
	spec := req.Spec
	attempts := spec.Policy.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	run := NodeRun{NodeID: spec.ID, Kind: spec.Kind, Name: spec.Name, Status: NodeRunning, StartedAt: time.Now().UTC()}
	_ = req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_started", Message: spec.Name})
	var lastErr error
	var artifacts []ArtifactRef
	for attempt := 1; attempt <= attempts; attempt++ {
		attemptCtx := ctx
		cancel := func() {}
		if spec.Policy.TimeoutSeconds > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, time.Duration(spec.Policy.TimeoutSeconds)*time.Second)
		}
		out, err := plugin.Execute(attemptCtx, req)
		cancel()
		run.Metrics = out.Metrics
		run.Metrics.RetryCount = attempt - 1
		if err == nil {
			run.Status = NodeSucceeded
			run.Artifacts = append(run.Artifacts, out.Artifacts...)
			artifacts = append(artifacts, out.Artifacts...)
			run.FinishedAt = time.Now().UTC()
			run.DurationMS = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
			_ = req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_succeeded"})
			return run, artifacts, nil
		}
		lastErr = err
		_ = req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_attempt_failed", Message: err.Error(), Fields: map[string]any{"attempt": attempt}})
	}
	run.Status = NodeFailed
	run.Error = lastErr.Error()
	if run.Metrics.FailureType == "" {
		run.Metrics.FailureType = "execution"
	}
	run.FinishedAt = time.Now().UTC()
	run.DurationMS = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	_ = req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_failed", Message: run.Error})
	return run, artifacts, lastErr
}

func validateRunRequest(req RunRequest) error {
	if strings.TrimSpace(req.Workflow.ID) == "" {
		return fmt.Errorf("workflow id is required")
	}
	if len(req.Workflow.Nodes) == 0 {
		return fmt.Errorf("workflow must contain nodes")
	}
	if strings.TrimSpace(req.ArtifactRoot) == "" {
		return fmt.Errorf("artifact root is required")
	}
	if strings.TrimSpace(req.WorkspaceRoot) == "" {
		return fmt.Errorf("workspace root is required")
	}
	if req.Workflow.Policy.MaxNodes > 0 && len(req.Workflow.Nodes) > req.Workflow.Policy.MaxNodes {
		return fmt.Errorf("workflow contains %d nodes, max is %d", len(req.Workflow.Nodes), req.Workflow.Policy.MaxNodes)
	}
	seen := map[string]bool{}
	for _, node := range req.Workflow.Nodes {
		if strings.TrimSpace(node.ID) == "" {
			return fmt.Errorf("node id is required")
		}
		if seen[node.ID] {
			return fmt.Errorf("duplicate node id %s", node.ID)
		}
		seen[node.ID] = true
	}
	for _, node := range req.Workflow.Nodes {
		for _, dep := range node.DependsOn {
			if !seen[dep] {
				return fmt.Errorf("node %s depends on unknown node %s", node.ID, dep)
			}
		}
	}
	return nil
}

func blockedDependency(spec NodeSpec, prior map[string]NodeRun) string {
	for _, dep := range spec.DependsOn {
		run, ok := prior[dep]
		if !ok || run.Status != NodeSucceeded {
			return dep
		}
	}
	return ""
}

func prepareWorkspace(root, runID string) (string, error) {
	root = filepath.Clean(root)
	abs, err := filepath.Abs(filepath.Join(root, runID))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	return abs, nil
}

func writeResultFiles(root string, result RunResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "run_result.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	events, err := json.MarshalIndent(result.Events, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "event_log.json"), append(events, '\n'), 0o644)
}

func resultError(result RunResult) error {
	if result.Status == RunSucceeded {
		return nil
	}
	for _, node := range result.Nodes {
		if node.Status == NodeFailed && node.Error != "" {
			return fmt.Errorf("node %s failed: %s", node.NodeID, node.Error)
		}
	}
	if result.Status == RunCancelled {
		return context.Canceled
	}
	return fmt.Errorf("workflow failed")
}

func newRunID(workflowID string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, workflowID)
	clean = strings.Trim(clean, "-_")
	if clean == "" {
		clean = "run"
	}
	now := time.Now().UTC()
	return fmt.Sprintf("%s-%s-%d", clean, now.Format("20060102T150405Z"), now.Nanosecond())
}

func clonePrior(input map[string]NodeRun) map[string]NodeRun {
	output := make(map[string]NodeRun, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
