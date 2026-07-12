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
	checkpoint := newCheckpointManager(result, events, store, req.Checkpoint)
	sink = checkpointEventSink{delegate: sink, checkpoint: checkpoint}
	if err := sink.Emit(ctx, Event{RunID: runID, Type: "run_started", Message: req.Workflow.Name, Revision: revision}); err != nil {
		return result, err
	}
	if err := checkpoint.persist(context.WithoutCancel(ctx)); err != nil {
		return failCheckpoint(result, checkpoint, fmt.Errorf("persist initial workflow checkpoint: %w", err))
	}
	prior := clonePrior(req.Prior)
	changed := map[string]bool{}
	maxRevisions := boundedMaxRevisions(req.Workflow.Policy.MaxRevisions)
	for index := 0; index < len(nodes); {
		spec := nodes[index]
		result.ActiveNodeID = spec.ID
		result.ActiveAttempt = 0
		result.Revision = revision
		checkpoint.update(result)
		nodeRun := NodeRun{NodeID: spec.ID, Kind: spec.Kind, Name: spec.Name, Status: NodePending}
		if reusable, ok := prior[spec.ID]; ok && reusable.Status == NodeSucceeded && dependenciesSucceeded(spec, prior) && !dependencyChanged(spec, changed) && artifactsAvailable(ctx, store, reusable.Artifacts) {
			reusable.Revision = revision
			result.Nodes = append(result.Nodes, reusable)
			result.Artifacts = append(result.Artifacts, reusable.Artifacts...)
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
			result.Nodes = append(result.Nodes, nodeRun)
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
			result.Nodes = append(result.Nodes, nodeRun)
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
			result.Nodes = append(result.Nodes, nodeRun)
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
		run, artifacts, directive, err := e.executeNode(ctx, plugin, NodeRequest{
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
		result.Nodes = append(result.Nodes, run)
		changed[spec.ID] = true
		result.Artifacts = append(result.Artifacts, artifacts...)
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
		if directive != nil {
			target, affected, directiveErr := validateDirective(*directive, index, nodes)
			if directiveErr != nil {
				return failCheckpoint(result, checkpoint, directiveErr)
			}
			if revision >= maxRevisions {
				return failCheckpoint(result, checkpoint, fmt.Errorf("workflow revision limit reached: maximum %d", maxRevisions))
			}
			revision++
			if err := invalidateRevision(ctx, store, prior, changed, affected); err != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("invalidate revision artifacts: %w", err))
			}
			result.Artifacts = withoutProducerArtifacts(result.Artifacts, affected)
			result.Revision = revision
			checkpoint.update(result)
			if emitErr := sink.Emit(ctx, Event{RunID: runID, NodeID: spec.ID, Type: "revision_started", Status: NodeRequeued, Revision: revision, Message: directive.Reason, Fields: map[string]any{"restart_from": nodes[target].ID, "invalidated_nodes": affected}}); emitErr != nil {
				return failCheckpoint(result, checkpoint, emitErr)
			}
			if checkpointErr := checkpoint.persist(context.WithoutCancel(ctx)); checkpointErr != nil {
				return failCheckpoint(result, checkpoint, fmt.Errorf("persist revision %d: %w", revision, checkpointErr))
			}
			index = target
			continue
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
	terminalType := "run_succeeded"
	terminalMessage := "workflow succeeded"
	if result.Status == RunCancelled {
		terminalType, terminalMessage = "run_cancelled", "workflow cancelled"
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

func (e *Engine) executeNode(ctx context.Context, plugin Plugin, req NodeRequest) (NodeRun, []ArtifactRef, *NodeDirective, error) {
	spec := req.Spec
	attempts := spec.Policy.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	run := NodeRun{NodeID: spec.ID, Kind: spec.Kind, Name: spec.Name, Status: NodeRunning, Revision: req.Revision, StartedAt: time.Now().UTC()}
	if err := req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_started", Status: NodeRunning, Revision: req.Revision, Message: spec.Name}); err != nil {
		return run, nil, nil, err
	}
	var lastErr error
	var artifacts []ArtifactRef
	for attempt := 1; attempt <= attempts; attempt++ {
		req.Attempt = attempt
		run.Attempt = attempt
		if err := req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_attempt_started", Status: NodeRunning, Attempt: attempt, Revision: req.Revision}); err != nil {
			return run, artifacts, nil, err
		}
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
			eventType := "node_succeeded"
			if out.Directive != nil {
				run.Status = NodeRequeued
				eventType = "node_requeued"
			}
			run.Artifacts = append(run.Artifacts, out.Artifacts...)
			artifacts = append(artifacts, out.Artifacts...)
			run.FinishedAt = time.Now().UTC()
			run.DurationMS = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
			if emitErr := req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: eventType, Status: run.Status, Attempt: attempt, Revision: req.Revision, Artifacts: out.Artifacts}); emitErr != nil {
				return run, artifacts, nil, emitErr
			}
			return run, artifacts, out.Directive, nil
		}
		lastErr = err
		kind, retryable := classifyFailure(err)
		run.Metrics.FailureType = string(kind)
		if emitErr := req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: spec.ID, Type: "node_attempt_failed", Status: NodeFailed, Attempt: attempt, Revision: req.Revision, Message: err.Error(), Fields: map[string]any{"failure_type": kind, "retryable": retryable}}); emitErr != nil {
			return run, artifacts, nil, emitErr
		}
		if attempt == attempts || !policyAllowsRetry(spec.Policy, kind, retryable) {
			break
		}
		if err := waitRetryBackoff(ctx, spec.Policy, attempt); err != nil {
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
		return run, artifacts, nil, emitErr
	}
	return run, artifacts, nil, lastErr
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
	if req.Workflow.Policy.MaxRevisions < 0 {
		return fmt.Errorf("workflow max revisions must be non-negative")
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
	if _, err := orderedWorkflowNodes(req.Workflow); err != nil {
		return err
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

func boundedMaxRevisions(configured int) int {
	if configured <= 0 || configured > 5 {
		return 5
	}
	return configured
}

func validateDirective(directive NodeDirective, currentIndex int, nodes []NodeSpec) (int, []string, error) {
	if directive.Action != DirectiveRequeue {
		return 0, nil, fmt.Errorf("unsupported node directive action %q", directive.Action)
	}
	restartFrom := strings.TrimSpace(directive.RestartFrom)
	target := -1
	for index, spec := range nodes {
		if spec.ID == restartFrom {
			target = index
			break
		}
	}
	if target < 0 {
		return 0, nil, fmt.Errorf("requeue directive targets unknown node %q", restartFrom)
	}
	if target > currentIndex {
		return 0, nil, fmt.Errorf("requeue directive must restart the current or an earlier node: %s", restartFrom)
	}
	affectedSet := map[string]bool{nodes[target].ID: true}
	for changed := true; changed; {
		changed = false
		for _, spec := range nodes {
			if affectedSet[spec.ID] {
				continue
			}
			for _, dependency := range spec.DependsOn {
				if affectedSet[dependency] {
					affectedSet[spec.ID] = true
					changed = true
					break
				}
			}
		}
	}
	affected := make([]string, 0, len(affectedSet))
	for _, spec := range nodes {
		if affectedSet[spec.ID] {
			affected = append(affected, spec.ID)
		}
	}
	return target, affected, nil
}

func invalidateRevision(ctx context.Context, store ArtifactStore, prior map[string]NodeRun, changed map[string]bool, affected []string) error {
	for _, nodeID := range affected {
		delete(prior, nodeID)
		changed[nodeID] = true
	}
	if invalidator, ok := store.(ArtifactInvalidator); ok {
		return invalidator.InvalidateProducers(ctx, affected)
	}
	return nil
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
		// Preserve the historical MaxAttempts behavior for unclassified plugin
		// errors while always honoring explicit permanent/cancellation signals.
		return classifiedRetryable || kind == FailureUnknown
	}
	for _, allowed := range policy.Retryable {
		if allowed == kind {
			return true
		}
	}
	return false
}

func waitRetryBackoff(ctx context.Context, policy NodePolicy, failedAttempt int) error {
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
