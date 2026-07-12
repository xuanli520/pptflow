package workflow

import (
	"context"
	"sync"
)

type checkpointManager struct {
	mu        sync.Mutex
	persistMu sync.Mutex
	result    RunResult
	events    *EventRecorder
	store     ArtifactStore
	callback  func(context.Context, RunResult) error
}

func newCheckpointManager(result RunResult, events *EventRecorder, store ArtifactStore, callback func(context.Context, RunResult) error) *checkpointManager {
	return &checkpointManager{result: cloneRunResult(result), events: events, store: store, callback: callback}
}

func (m *checkpointManager) update(result RunResult) {
	m.mu.Lock()
	m.result = cloneRunResult(result)
	m.mu.Unlock()
}

func (m *checkpointManager) revision() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.result.Revision
}

func (m *checkpointManager) setActiveAttempt(attempt int) {
	m.mu.Lock()
	m.result.ActiveAttempt = attempt
	m.mu.Unlock()
}

func (m *checkpointManager) persist(ctx context.Context) error {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.Lock()
	snapshot := cloneRunResult(m.result)
	m.mu.Unlock()
	snapshot.Events = eventsForRun(m.events.Events(), snapshot.RunID)
	if _, err := m.store.PutJSON(ctx, "run_result.json", "workflow_run_result", "workflow.engine", snapshot); err != nil {
		return err
	}
	if m.callback != nil {
		return m.callback(ctx, cloneRunResult(snapshot))
	}
	return nil
}

type checkpointEventSink struct {
	delegate   EventSink
	checkpoint *checkpointManager
}

func (s checkpointEventSink) Emit(ctx context.Context, event Event) error {
	event.Revision = s.checkpoint.revision()
	if err := s.delegate.Emit(ctx, event); err != nil {
		return err
	}
	if event.Type == "node_attempt_started" {
		s.checkpoint.setActiveAttempt(event.Attempt)
	}
	switch event.Type {
	case "node_started", "node_attempt_started", "node_attempt_failed", "gate_requested":
		return s.checkpoint.persist(context.WithoutCancel(ctx))
	default:
		return nil
	}
}

func cloneRunResult(result RunResult) RunResult {
	if len(result.Nodes) > 0 {
		nodes := make([]NodeRun, 0, len(result.Nodes))
		for _, run := range result.Nodes {
			nodes = append(nodes, cloneNodeRun(run))
		}
		result.Nodes = nodes
	}
	if len(result.Artifacts) > 0 {
		artifacts := make([]ArtifactRef, 0, len(result.Artifacts))
		for _, artifact := range result.Artifacts {
			artifacts = append(artifacts, cloneArtifactRef(artifact))
		}
		result.Artifacts = artifacts
	}
	if len(result.Events) > 0 {
		events := make([]Event, 0, len(result.Events))
		for _, event := range result.Events {
			events = append(events, cloneEvent(event))
		}
		result.Events = events
	}
	return result
}

func cloneNodeRun(run NodeRun) NodeRun {
	if len(run.Artifacts) > 0 {
		artifacts := make([]ArtifactRef, 0, len(run.Artifacts))
		for _, artifact := range run.Artifacts {
			artifacts = append(artifacts, cloneArtifactRef(artifact))
		}
		run.Artifacts = artifacts
	}
	return run
}
