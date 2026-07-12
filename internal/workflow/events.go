package workflow

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/runmodel"
)

// EventRecorder is a concurrent event sink with optional durable JSONL
// persistence and best-effort live subscriptions. Disk persistence happens
// before subscribers are notified.
type EventRecorder struct {
	mu          sync.Mutex
	events      []Event
	sequence    uint64
	path        string
	subscribers map[uint64]chan Event
	nextSubID   uint64
}

func NewEventRecorder() *EventRecorder {
	return &EventRecorder{subscribers: map[uint64]chan Event{}}
}

func NewPersistentEventRecorder(path string) (*EventRecorder, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("event log path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, err
	}
	recorder := &EventRecorder{path: abs, subscribers: map[uint64]chan Event{}}
	if err := recorder.load(); err != nil {
		return nil, err
	}
	return recorder, nil
}

func (r *EventRecorder) Emit(ctx context.Context, event Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("event recorder is nil")
	}
	event = runmodel.RedactEvent(event)
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if event.Sequence == 0 {
		r.sequence++
		event.Sequence = r.sequence
	} else if event.Sequence > r.sequence {
		r.sequence = event.Sequence
	}
	if r.path != "" {
		if err := appendJSONLine(r.path, event); err != nil {
			return err
		}
	}
	r.events = append(r.events, cloneEvent(event))
	for _, subscriber := range r.subscribers {
		select {
		case subscriber <- cloneEvent(event):
		default:
		}
	}
	return nil
}

func (r *EventRecorder) Events() []Event {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Event, 0, len(r.events))
	for _, event := range r.events {
		result = append(result, cloneEvent(event))
	}
	return result
}

func (r *EventRecorder) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer < 1 {
		buffer = 1
	}
	channel := make(chan Event, buffer)
	if r == nil {
		close(channel)
		return channel, func() {}
	}
	r.mu.Lock()
	r.nextSubID++
	id := r.nextSubID
	r.subscribers[id] = channel
	r.mu.Unlock()
	var once sync.Once
	return channel, func() {
		once.Do(func() {
			r.mu.Lock()
			if existing, ok := r.subscribers[id]; ok {
				delete(r.subscribers, id)
				close(existing)
			}
			r.mu.Unlock()
		})
	}
}

func (r *EventRecorder) load() error {
	file, err := os.Open(r.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(text), &event); err != nil {
			return fmt.Errorf("parse event log line %d: %w", line, err)
		}
		if event.Sequence == 0 {
			r.sequence++
			event.Sequence = r.sequence
		} else if event.Sequence > r.sequence {
			r.sequence = event.Sequence
		}
		r.events = append(r.events, runmodel.RedactEvent(event))
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return r.rewriteSanitizedLog()
}

func (r *EventRecorder) rewriteSanitizedLog() error {
	if r.path == "" || len(r.events) == 0 {
		return nil
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for _, event := range r.events {
		if err := encoder.Encode(runmodel.RedactEvent(event)); err != nil {
			return err
		}
	}
	_, _, err := atomicWriteReader(r.path, &output, 0o600)
	return err
}

func appendJSONLine(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func cloneEvent(event Event) Event {
	return runmodel.CloneEvent(event)
}

type eventFanout struct {
	primary  *EventRecorder
	external EventSink
}

func (s eventFanout) Emit(ctx context.Context, event Event) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	event = runmodel.RedactEvent(event)
	if err := s.primary.Emit(ctx, event); err != nil {
		return err
	}
	if s.external != nil {
		if recorder, ok := s.external.(*EventRecorder); ok && recorder == s.primary {
			return nil
		}
		return s.external.Emit(ctx, event)
	}
	return nil
}
