package workflow

import (
	"context"
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
	nodes, err := orderedWorkflowNodes(req.Workflow)
	if err != nil {
		return RunResult{}, err
	}
	runID := strings.TrimSpace(req.RunID)
	externalLayout := runID != ""
	if runID == "" {
		runID = newRunID(req.Workflow.ID)
	}
	store := req.Store
	if store == nil {
		artifactRoot := req.ArtifactRoot
		if !externalLayout {
			artifactRoot = filepath.Join(artifactRoot, runID)
		}
		store, err = NewFileArtifactStore(artifactRoot)
		if err != nil {
			return RunResult{}, err
		}
	}
	workspaceRoot, err := prepareWorkspace(req.WorkspaceRoot, runID, externalLayout)
	if err != nil {
		return RunResult{}, err
	}
	events, sink, err := engineEventSink(req.Events, workspaceRoot, externalLayout)
	if err != nil {
		return RunResult{}, err
	}
	prior := clonePrior(req.Prior)
	revision := req.Revision
	result := RunResult{
		RunID:         runID,
		WorkflowID:    req.Workflow.ID,
		Status:        RunRunning,
		Revision:      revision,
		ArtifactRoot:  store.Root(),
		WorkspaceRoot: workspaceRoot,
		StartedAt:     start,
	}
	result.Nodes, result.Artifacts = priorSnapshot(nodes, prior)
	checkpoint := newCheckpointManager(result, events, store, req.Checkpoint)
	sink = checkpointEventSink{delegate: sink, checkpoint: checkpoint}
	if err := sink.Emit(ctx, Event{RunID: runID, Type: "run_started", Message: req.Workflow.Name, Revision: revision}); err != nil {
		return result, err
	}
	if err := checkpoint.persist(context.WithoutCancel(ctx)); err != nil {
		return failCheckpoint(result, checkpoint, fmt.Errorf("persist initial workflow checkpoint: %w", err))
	}
	changed := map[string]bool{}
	for index := 0; index < len(nodes); {
		spec := nodes[index]
		result.ActiveNodeID = spec.ID
		result.ActiveAttempt = 0
		result.Revision = revision
		checkpoint.update(result)
		nodeRun := NodeRun{NodeID: spec.ID, Kind: spec.Kind, Name: spec.Name, Status: NodePending}
		if reusable, ok := prior[spec.ID]; ok && reusable.Status == NodeSucceeded && dependenciesSucceeded(spec, prior) && !dependencyChanged(spec, changed) && artifactsAvailable(ctx, store, reusable.Artifacts) {
			reusable.Revision = revision
			result.Nodes = upsertNodeRun(result.Nodes, reusable)
			result.Artifacts = replaceProducerArtifacts(result.Artifacts, spec.ID, reusable.Artifacts)
			result.ActiveNodeID = ""
			result.ActiveAttempt = 0
			if err := sink.Emit(ctx, Event{RunID: runID, NodeID: spec.ID, Type: "node_reused", Status: NodeSucceeded, Message: "reused durable node result", Artifacts: reusable.Artifacts, Revision: revision}); err != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("emit node reuse: %w", err))
			}
			checkpoint.update(result)
			if err := checkpoint.persist(context.WithoutCancel(ctx)); err != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("persist reused node %s: %w", spec.ID, err))
			}
			index++
			continue
		}
		if blockedBy := blockedDependency(spec, prior); blockedBy != "" {
			changed[spec.ID] = true
			nodeRun.Status = NodeSkipped
			nodeRun.Revision = revision
			nodeRun.Error = "dependency failed: " + blockedBy
			result.Nodes = upsertNodeRun(result.Nodes, nodeRun)
			result.Artifacts = replaceProducerArtifacts(result.Artifacts, spec.ID, nil)
			prior[spec.ID] = nodeRun
			result.ActiveNodeID = ""
			result.ActiveAttempt = 0
			if err := sink.Emit(ctx, Event{RunID: runID, NodeID: spec.ID, Type: "node_skipped", Status: NodeSkipped, Message: nodeRun.Error, Revision: revision}); err != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("emit node skip: %w", err))
			}
			checkpoint.update(result)
			if err := checkpoint.persist(context.WithoutCancel(ctx)); err != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("persist skipped node %s: %w", spec.ID, err))
			}
			index++
			continue
		}
		plugin, err := e.registry.Lookup(spec)
		if err == nil {
			err = plugin.Validate(spec)
		}
		if err != nil {
			changed[spec.ID] = true
			nodeRun.Status = NodeFailed
			nodeRun.Revision = revision
			nodeRun.Error = err.Error()
			nodeRun.Metrics = NodeMetrics{FailureType: "validation"}
			result.Nodes = upsertNodeRun(result.Nodes, nodeRun)
			result.Artifacts = replaceProducerArtifacts(result.Artifacts, spec.ID, nil)
			prior[spec.ID] = nodeRun
			result.ActiveNodeID = ""
			result.ActiveAttempt = 0
			if emitErr := sink.Emit(ctx, Event{RunID: runID, NodeID: spec.ID, Type: "node_failed", Status: NodeFailed, Message: nodeRun.Error, Revision: revision}); emitErr != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("emit validation failure: %w", emitErr))
			}
			checkpoint.update(result)
			if err := checkpoint.persist(context.WithoutCancel(ctx)); err != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("persist failed node %s: %w", spec.ID, err))
			}
			index++
			continue
		}
		inputs, err := resolveNodeInputs(ctx, store, spec, prior)
		if err != nil {
			changed[spec.ID] = true
			nodeRun.Status, nodeRun.Error, nodeRun.Metrics.FailureType = NodeFailed, err.Error(), string(FailurePermanent)
			nodeRun.Revision = revision
			result.Nodes = upsertNodeRun(result.Nodes, nodeRun)
			result.Artifacts = replaceProducerArtifacts(result.Artifacts, spec.ID, nil)
			prior[spec.ID] = nodeRun
			result.ActiveNodeID = ""
			result.ActiveAttempt = 0
			if emitErr := sink.Emit(ctx, Event{RunID: runID, NodeID: spec.ID, Type: "node_failed", Status: NodeFailed, Message: err.Error(), Revision: revision}); emitErr != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("emit input failure: %w", emitErr))
			}
			checkpoint.update(result)
			if err := checkpoint.persist(context.WithoutCancel(ctx)); err != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("persist failed node %s: %w", spec.ID, err))
			}
			index++
			continue
		}
		run, artifacts, err := e.executeNode(ctx, plugin, NodeRequest{
			RunID:         runID,
			Spec:          spec,
			ArtifactRoot:  store.Root(),
			WorkspaceRoot: workspaceRoot,
			Input:         req.Input,
			Inputs:        inputs,
			Revision:      revision,
			Store:         store,
			Events:        sink,
			Runtimes:      e.runtimes,
			Prior:         clonePrior(prior),
		})
		var checkpointErr checkpointPersistError
		if errors.As(err, &checkpointErr) {
			return failCheckpoint(result, checkpoint, checkpointErr.err)
		}
		// If the node_started checkpoint itself fails, keep the prior durable
		// snapshot because no plugin work has begun.
		if err == nil || run.Status != NodeRunning || run.Attempt > 0 {
			result.Nodes = upsertNodeRun(result.Nodes, run)
		}
		changed[spec.ID] = true
		result.Artifacts = replaceProducerArtifacts(result.Artifacts, spec.ID, artifacts)
		prior[spec.ID] = run
		result.ActiveNodeID = ""
		result.ActiveAttempt = 0
		if err != nil {
			checkpoint.update(result)
			if checkpointErr := checkpoint.persist(context.WithoutCancel(ctx)); checkpointErr != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("persist failed node %s: %w", spec.ID, checkpointErr))
			}
			if ctx.Err() != nil {
				break
			}
			index++
			continue
		}
		checkpoint.update(result)
		if checkpointErr := checkpoint.persist(context.WithoutCancel(ctx)); checkpointErr != nil {
			return failCheckpoint(result, checkpoint, fmt.Errorf("persist completed node %s: %w", spec.ID, checkpointErr))
		}
		index++
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		result.Status = RunCancelled
	} else if currentRunFailed(nodes, prior) {
		result.Status = RunFailed
	} else {
		result.Status = RunSucceeded
	}
	result.ActiveNodeID = ""
	result.ActiveAttempt = 0
	result.Revision = revision
	result.Nodes = orderNodeRuns(nodes, result.Nodes)
	terminalType := "run_succeeded"
	terminalMessage := "workflow succeeded"
	if result.Status == RunCancelled {
		terminalType, terminalMessage = "run_canceled", "workflow canceled"
	} else if result.Status != RunSucceeded {
		terminalType, terminalMessage = "run_failed", "workflow failed"
	}
	if err := sink.Emit(context.WithoutCancel(ctx), Event{RunID: runID, Type: terminalType, Message: terminalMessage}); err != nil {
		return result, err
	}
	result.Events = events.Events()
	result.Events = eventsForRun(result.Events, runID)
	result.FinishedAt = time.Now().UTC()
	result.DurationMS = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
	checkpoint.update(result)
	if err := checkpoint.persist(context.WithoutCancel(ctx)); err != nil {
		return result, fmt.Errorf("persist terminal workflow checkpoint: %w", err)
	}
	if err := writeResultFiles(context.WithoutCancel(ctx), store, result); err != nil {
		return result, err
	}
	return result, resultError(result)
}

func (e *Engine) executeNode(ctx context.Context, plugin Plugin, req NodeRequest) (NodeRun, []ArtifactRef, error) {
	spec := req.Spec
	attempts := spec.Policy.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	nodeCtx := ctx
	nodeCancel := func() {}
	if spec.Policy.MaxElapsedSeconds > 0 {
		nodeCtx, nodeCancel = context.WithTimeout(ctx, time.Duration(spec.Policy.MaxElapsedSeconds)*time.Second)
	}
	defer nodeCancel()
	run := NodeRun{NodeID: spec.ID, Kind: spec.Kind, Name: spec.Name, Status: NodeRunning, Revision: req.Revision, StartedAt: time.Now().UTC()}
	if err := req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_started", Status: NodeRunning, Revision: req.Revision, Message: spec.Name}); err != nil {
		return run, nil, err
	}
	var lastErr error
	var artifacts []ArtifactRef
	for attempt := 1; attempt <= attempts; attempt++ {
		req.Attempt = attempt
		run.Attempt = attempt
		if err := req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_attempt_started", Status: NodeRunning, Attempt: attempt, Revision: req.Revision}); err != nil {
			return run, artifacts, err
		}
		attemptCtx := nodeCtx
		cancel := func() {}
		if spec.Policy.TimeoutSeconds > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, time.Duration(spec.Policy.TimeoutSeconds)*time.Second)
		}
		out, err := plugin.Execute(attemptCtx, req)
		cancel()
		run.Metrics = out.Metrics
		run.Metrics.RetryCount = attempt - 1
		// A plugin can write a diagnostic report before returning an error. Keep
		// that evidence in the state machine so retry, repair, and the UI can
		// explain the failure instead of relying on a workspace-only file.
		if len(out.Artifacts) > 0 {
			run.Artifacts = append(run.Artifacts, out.Artifacts...)
			artifacts = append(artifacts, out.Artifacts...)
		}
		if err == nil {
			run.Status = NodeSucceeded
			run.FinishedAt = time.Now().UTC()
			run.DurationMS = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
			if emitErr := req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_succeeded", Status: run.Status, Attempt: attempt, Revision: req.Revision, Artifacts: out.Artifacts}); emitErr != nil {
				return run, artifacts, emitErr
			}
			return run, artifacts, nil
		}
		lastErr = err
		kind, retryable := classifyFailure(err)
		run.Metrics.FailureType = string(kind)
		if emitErr := req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_attempt_failed", Status: NodeFailed, Attempt: attempt, Revision: req.Revision, Message: err.Error(), Fields: map[string]any{"failure_type": kind, "retryable": retryable}}); emitErr != nil {
			return run, artifacts, emitErr
		}
		if attempt == attempts || !policyAllowsRetry(spec.Policy, kind, retryable) {
			break
		}
		delay := retryBackoffDuration(spec.Policy, attempt)
		if emitErr := req.Events.Emit(ctx, Event{
			RunID: req.RunID, NodeID: spec.ID, Type: "node_retry_scheduled", Status: NodeRunning,
			Attempt: attempt + 1, Revision: req.Revision,
			Message: fmt.Sprintf("retrying after %s", delay),
			Fields: map[string]any{
				"failed_attempt": attempt, "next_attempt": attempt + 1,
				"backoff_ms": delay.Milliseconds(), "failure_type": kind,
			},
		}); emitErr != nil {
			return run, artifacts, emitErr
		}
		if err := waitRetryBackoff(nodeCtx, delay); err != nil {
			lastErr = err
			break
		}
	}
	run.Status = NodeFailed
	eventType := "node_failed"
	if run.Metrics.FailureType == string(FailureCanceled) {
		run.Status = NodeCanceled
		eventType = "node_canceled"
	}
	run.Error = lastErr.Error()
	if run.Metrics.FailureType == "" {
		run.Metrics.FailureType = "execution"
	}
	run.FinishedAt = time.Now().UTC()
	run.DurationMS = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	if emitErr := req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: eventType, Status: run.Status, Attempt: run.Metrics.RetryCount + 1, Revision: req.Revision, Message: run.Error}); emitErr != nil {
		return run, artifacts, emitErr
	}
	return run, artifacts, lastErr
}

func validateRunRequest(req RunRequest) error {
	if strings.TrimSpace(req.Workflow.ID) == "" {
		return fmt.Errorf("workflow id is required")
	}
	if len(req.Workflow.Nodes) == 0 {
		return fmt.Errorf("workflow must contain nodes")
	}
	if req.Store == nil && strings.TrimSpace(req.ArtifactRoot) == "" {
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
		if err := validateNodeBudget(node); err != nil {
			return err
		}
	}
	for _, node := range req.Workflow.Nodes {
		for _, dep := range node.DependsOn {
			if !seen[dep] {
				return fmt.Errorf("node %s depends on unknown node %s", node.ID, dep)
			}
		}
	}
	if _, err := orderedWorkflowNodes(req.Workflow); err != nil {
		return err
	}
	return nil
}

func validateNodeBudget(node NodeSpec) error {
	policy := node.Policy
	hasTurnBudget := policy.TurnTimeoutSeconds > 0 || policy.MaxTurns > 0 ||
		policy.StartupGraceSeconds > 0 || policy.ShutdownGraceSeconds > 0
	if !hasTurnBudget && policy.MaxElapsedSeconds == 0 {
		return nil
	}
	attempts := policy.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	if hasTurnBudget {
		if policy.TurnTimeoutSeconds <= 0 || policy.MaxTurns <= 0 {
			return fmt.Errorf("node %s has an incomplete turn budget", node.ID)
		}
		if policy.TimeoutSeconds <= 0 {
			return fmt.Errorf("node %s has a turn budget but no attempt timeout", node.ID)
		}
		requiredAttempt := time.Duration(policy.MaxTurns*policy.TurnTimeoutSeconds+policy.StartupGraceSeconds+policy.ShutdownGraceSeconds) * time.Second
		if time.Duration(policy.TimeoutSeconds)*time.Second < requiredAttempt {
			return fmt.Errorf("node %s attempt timeout %s is smaller than %d turns of %s plus startup/shutdown grace %s", node.ID, time.Duration(policy.TimeoutSeconds)*time.Second, policy.MaxTurns, time.Duration(policy.TurnTimeoutSeconds)*time.Second, time.Duration(policy.StartupGraceSeconds+policy.ShutdownGraceSeconds)*time.Second)
		}
	}
	if policy.MaxElapsedSeconds <= 0 {
		return fmt.Errorf("node %s has an execution budget but no max elapsed timeout", node.ID)
	}
	if policy.TimeoutSeconds <= 0 {
		return fmt.Errorf("node %s has max elapsed timeout but no attempt timeout", node.ID)
	}
	requiredElapsed := time.Duration(attempts*policy.TimeoutSeconds) * time.Second
	for failedAttempt := 1; failedAttempt < attempts; failedAttempt++ {
		requiredElapsed += retryBackoffDuration(policy, failedAttempt)
	}
	if time.Duration(policy.MaxElapsedSeconds)*time.Second < requiredElapsed {
		return fmt.Errorf("node %s max elapsed %s is smaller than %d attempts plus retry backoff %s", node.ID, time.Duration(policy.MaxElapsedSeconds)*time.Second, attempts, requiredElapsed)
	}
	return nil
}

func orderedWorkflowNodes(workflow WorkflowDefinition) ([]NodeSpec, error) {
	nodes := append([]NodeSpec(nil), workflow.Nodes...)
	indexByID := map[string]int{}
	for i, node := range nodes {
		indexByID[node.ID] = i
	}
	depsByID := map[string][]string{}
	for _, node := range nodes {
		depsByID[node.ID] = dedupeStrings(node.DependsOn)
	}
	for _, edge := range workflow.Edges {
		from := strings.TrimSpace(edge.From)
		to := strings.TrimSpace(edge.To)
		if from == "" || to == "" {
			return nil, fmt.Errorf("workflow edge requires from and to")
		}
		if from == to {
			return nil, fmt.Errorf("workflow edge %s -> %s creates self dependency", from, to)
		}
		if _, ok := indexByID[from]; !ok {
			return nil, fmt.Errorf("workflow edge depends on unknown node %s", from)
		}
		toIndex, ok := indexByID[to]
		if !ok {
			return nil, fmt.Errorf("workflow edge targets unknown node %s", to)
		}
		depsByID[to] = appendUnique(depsByID[to], from)
		nodes[toIndex].DependsOn = depsByID[to]
	}
	for _, node := range nodes {
		for _, dep := range depsByID[node.ID] {
			if dep == node.ID {
				return nil, fmt.Errorf("node %s depends on itself", node.ID)
			}
			if _, ok := indexByID[dep]; !ok {
				return nil, fmt.Errorf("node %s depends on unknown node %s", node.ID, dep)
			}
		}
	}
	done := map[string]bool{}
	ordered := make([]NodeSpec, 0, len(nodes))
	for len(ordered) < len(nodes) {
		progress := false
		for _, node := range nodes {
			if done[node.ID] {
				continue
			}
			blocked := false
			for _, dep := range depsByID[node.ID] {
				if !done[dep] {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			ordered = append(ordered, node)
			done[node.ID] = true
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("workflow dependency graph contains a cycle")
		}
	}
	return ordered, nil
}

func dedupeStrings(values []string) []string {
	result := []string{}
	for _, value := range values {
		result = appendUnique(result, strings.TrimSpace(value))
	}
	return result
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
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

func prepareWorkspace(root, runID string, exact bool) (string, error) {
	root = filepath.Clean(root)
	path := root
	if !exact {
		path = filepath.Join(root, runID)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	return abs, nil
}

func writeResultFiles(ctx context.Context, store ArtifactStore, result RunResult) error {
	if _, err := store.PutJSON(ctx, "run_result.json", "workflow_run_result", "workflow.engine", result); err != nil {
		return err
	}
	_, err := store.PutJSON(ctx, "event_log.json", "workflow_event_log", "workflow.engine", result.Events)
	return err
}

func engineEventSink(external EventSink, workspaceRoot string, persistent bool) (*EventRecorder, EventSink, error) {
	if recorder, ok := external.(*EventRecorder); ok && recorder != nil {
		return recorder, recorder, nil
	}
	var recorder *EventRecorder
	var err error
	if persistent {
		recorder, err = NewPersistentEventRecorder(filepath.Join(workspaceRoot, "event_log.jsonl"))
	} else {
		recorder = NewEventRecorder()
	}
	if err != nil {
		return nil, nil, err
	}
	if external == nil {
		return recorder, recorder, nil
	}
	return recorder, eventFanout{primary: recorder, external: external}, nil
}

func eventsForRun(events []Event, runID string) []Event {
	result := make([]Event, 0, len(events))
	for _, event := range events {
		if event.RunID == "" || event.RunID == runID {
			result = append(result, event)
		}
	}
	return result
}

func dependenciesSucceeded(spec NodeSpec, prior map[string]NodeRun) bool {
	return blockedDependency(spec, prior) == ""
}

func dependencyChanged(spec NodeSpec, changed map[string]bool) bool {
	for _, dependency := range spec.DependsOn {
		if changed[dependency] {
			return true
		}
	}
	return false
}

func withoutProducerArtifacts(artifacts []ArtifactRef, producers []string) []ArtifactRef {
	invalid := make(map[string]bool, len(producers))
	for _, producer := range producers {
		invalid[producer] = true
	}
	kept := make([]ArtifactRef, 0, len(artifacts))
	for _, artifact := range artifacts {
		if !invalid[artifact.Producer] {
			kept = append(kept, artifact)
		}
	}
	return kept
}

func upsertNodeRun(runs []NodeRun, updated NodeRun) []NodeRun {
	for index := range runs {
		if runs[index].NodeID == updated.NodeID {
			runs[index] = cloneNodeRun(updated)
			return runs
		}
	}
	return append(runs, cloneNodeRun(updated))
}

func orderNodeRuns(nodes []NodeSpec, runs []NodeRun) []NodeRun {
	byID := make(map[string]NodeRun, len(runs))
	for _, run := range runs {
		byID[run.NodeID] = run
	}
	ordered := make([]NodeRun, 0, len(byID))
	for _, spec := range nodes {
		if run, ok := byID[spec.ID]; ok {
			ordered = append(ordered, cloneNodeRun(run))
		}
	}
	return ordered
}

func replaceProducerArtifacts(artifacts []ArtifactRef, producer string, replacements []ArtifactRef) []ArtifactRef {
	kept := withoutProducerArtifacts(artifacts, []string{producer})
	for _, artifact := range replacements {
		kept = append(kept, cloneArtifactRef(artifact))
	}
	return kept
}

func priorSnapshot(nodes []NodeSpec, prior map[string]NodeRun) ([]NodeRun, []ArtifactRef) {
	runs := make([]NodeRun, 0, len(prior))
	var artifacts []ArtifactRef
	for _, spec := range nodes {
		run, ok := prior[spec.ID]
		if !ok {
			continue
		}
		run = cloneNodeRun(run)
		runs = append(runs, run)
		artifacts = append(artifacts, run.Artifacts...)
	}
	return runs, artifacts
}

func currentRunFailed(nodes []NodeSpec, prior map[string]NodeRun) bool {
	for _, spec := range nodes {
		run, ok := prior[spec.ID]
		if !ok || run.Status != NodeSucceeded {
			return true
		}
	}
	return false
}

func failCheckpoint(result RunResult, checkpoint *checkpointManager, err error) (RunResult, error) {
	result.Status = RunFailed
	result.ActiveNodeID = ""
	result.ActiveAttempt = 0
	result.FinishedAt = time.Now().UTC()
	result.DurationMS = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
	result.Events = eventsForRun(checkpoint.events.Events(), result.RunID)
	checkpoint.update(result)
	_ = checkpoint.persist(context.Background())
	return result, err
}

func artifactsAvailable(ctx context.Context, store ArtifactStore, refs []ArtifactRef) bool {
	for _, ref := range refs {
		reader, _, err := store.Get(ctx, ref)
		if err != nil {
			return false
		}
		_ = reader.Close()
	}
	return true
}

func resolveNodeInputs(ctx context.Context, store ArtifactStore, spec NodeSpec, prior map[string]NodeRun) ([]ArtifactRef, error) {
	available, err := store.List(ctx, "")
	if err != nil {
		return nil, err
	}
	byID, byName := map[string]ArtifactRef{}, map[string]ArtifactRef{}
	for _, ref := range available {
		byID[ref.ID], byName[filepath.ToSlash(ref.Name)] = ref, ref
	}
	resolved := make([]ArtifactRef, 0, len(spec.Inputs))
	seen := map[string]bool{}
	add := func(ref ArtifactRef) {
		key := ref.ID + "\x00" + filepath.ToSlash(ref.Name)
		if !seen[key] {
			seen[key] = true
			resolved = append(resolved, ref)
		}
	}
	for _, requested := range spec.Inputs {
		ref, ok := byID[requested.ID]
		if !ok {
			ref, ok = byName[filepath.ToSlash(requested.Name)]
		}
		if !ok && requested.Path != "" {
			reader, _, getErr := store.Get(ctx, requested)
			if getErr == nil {
				_ = reader.Close()
				ref, ok = requested, true
			}
		}
		if !ok {
			return nil, fmt.Errorf("node %s input artifact not found: id=%q name=%q", spec.ID, requested.ID, requested.Name)
		}
		add(ref)
	}
	for _, dependency := range spec.DependsOn {
		for _, ref := range prior[dependency].Artifacts {
			add(ref)
		}
	}
	return resolved, nil
}

func policyAllowsRetry(policy NodePolicy, kind FailureKind, classifiedRetryable bool) bool {
	if len(policy.Retryable) == 0 {
		// Unknown errors must not become blind retries. A plugin can still opt in
		// by returning a ClassifiedError with Retryable=true.
		return classifiedRetryable
	}
	for _, allowed := range policy.Retryable {
		if allowed == kind {
			return true
		}
	}
	return false
}

func retryBackoffDuration(policy NodePolicy, failedAttempt int) time.Duration {
	base := policy.RetryBackoffMS
	if base <= 0 {
		base = 100
	}
	delay := time.Duration(base) * time.Millisecond
	for i := 1; i < failedAttempt; i++ {
		delay *= 2
	}
	if max := policy.RetryMaxBackoffMS; max > 0 && delay > time.Duration(max)*time.Millisecond {
		delay = time.Duration(max) * time.Millisecond
	}
	return delay
}

func waitRetryBackoff(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func classifyFailure(err error) (FailureKind, bool) {
	if err == nil {
		return FailureUnknown, false
	}
	var classified ClassifiedError
	if errors.As(err, &classified) {
		return classified.FailureKind(), classified.Retryable()
	}
	if errors.Is(err, context.Canceled) {
		return FailureCanceled, false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureTimeout, true
	}
	return FailureUnknown, false
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
		if len(value.Artifacts) > 0 {
			artifacts := make([]ArtifactRef, 0, len(value.Artifacts))
			for _, artifact := range value.Artifacts {
				artifacts = append(artifacts, cloneArtifactRef(artifact))
			}
			value.Artifacts = artifacts
		}
		output[key] = value
	}
	return output
}
