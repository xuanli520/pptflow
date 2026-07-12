package runmodel

import (
	"encoding/json"
	"time"

	"github.com/purplevoid/harbor-factory/internal/redact"
)

type ArtifactRef struct {
	ID        string            `json:"id,omitempty"`
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Type      string            `json:"type,omitempty"`
	Producer  string            `json:"producer,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	SHA256    string            `json:"sha256,omitempty"`
	SizeBytes int64             `json:"size_bytes,omitempty"`
	Content   string            `json:"content,omitempty"`
	CreatedAt time.Time         `json:"created_at,omitempty"`
}

type ChecklistItem struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Critical bool   `json:"critical"`
	Passed   bool   `json:"passed"`
}

type GateRequest struct {
	RequestID string          `json:"request_id"`
	GateID    string          `json:"gate_id"`
	GateName  string          `json:"gate_name"`
	NodeID    string          `json:"node_id"`
	Message   string          `json:"message"`
	Checklist []ChecklistItem `json:"checklist,omitempty"`
	Artifacts []ArtifactRef   `json:"artifacts,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type NodeStatus = string

const (
	NodePending   NodeStatus = "pending"
	NodeRunning   NodeStatus = "running"
	NodeSucceeded NodeStatus = "succeeded"
	NodeFailed    NodeStatus = "failed"
	NodeCanceled  NodeStatus = "canceled"
	NodeSkipped   NodeStatus = "skipped"
	NodeRequeued  NodeStatus = "requeued"
)

type Event struct {
	Sequence  uint64         `json:"sequence,omitempty"`
	RunID     string         `json:"run_id,omitempty"`
	NodeID    string         `json:"node_id,omitempty"`
	Type      string         `json:"type"`
	Message   string         `json:"message,omitempty"`
	Status    NodeStatus     `json:"status,omitempty"`
	Path      string         `json:"path,omitempty"`
	Attempt   int            `json:"attempt,omitempty"`
	Revision  int            `json:"revision,omitempty"`
	Artifacts []ArtifactRef  `json:"artifacts,omitempty"`
	Logs      []ArtifactRef  `json:"logs,omitempty"`
	Gate      *GateRequest   `json:"gate,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

func RedactEvent(event Event) Event {
	if event.Gate == nil {
		if rawGate, ok := event.Fields["gate"]; ok {
			raw, err := json.Marshal(rawGate)
			var gate GateRequest
			if err == nil && json.Unmarshal(raw, &gate) == nil && gate.GateID != "" {
				event.Gate = &gate
			}
		}
	}
	event.RunID = redact.Text(event.RunID)
	event.NodeID = redact.Text(event.NodeID)
	event.Type = redact.Text(event.Type)
	event.Message = redact.Text(event.Message)
	event.Status = redact.Text(event.Status)
	event.Path = redact.Text(event.Path)
	event.Artifacts = redactArtifacts(event.Artifacts)
	event.Logs = redactArtifacts(event.Logs)
	if event.Gate != nil {
		gate := redactGate(*event.Gate)
		event.Gate = &gate
	}
	event.Fields = redactMap(event.Fields)
	return event
}

func CloneEvent(event Event) Event {
	event.Artifacts = cloneArtifacts(event.Artifacts)
	event.Logs = cloneArtifacts(event.Logs)
	if event.Gate != nil {
		gate := *event.Gate
		if len(gate.Checklist) > 0 {
			gate.Checklist = append([]ChecklistItem(nil), gate.Checklist...)
		}
		gate.Artifacts = cloneArtifacts(gate.Artifacts)
		event.Gate = &gate
	}
	if len(event.Fields) > 0 {
		fields := make(map[string]any, len(event.Fields))
		for key, value := range event.Fields {
			fields[key] = value
		}
		event.Fields = fields
	}
	return event
}

func cloneArtifacts(input []ArtifactRef) []ArtifactRef {
	if len(input) == 0 {
		return nil
	}
	output := make([]ArtifactRef, len(input))
	for index, artifact := range input {
		if len(artifact.Metadata) > 0 {
			metadata := make(map[string]string, len(artifact.Metadata))
			for key, value := range artifact.Metadata {
				metadata[key] = value
			}
			artifact.Metadata = metadata
		}
		output[index] = artifact
	}
	return output
}

func redactArtifacts(input []ArtifactRef) []ArtifactRef {
	if len(input) == 0 {
		return nil
	}
	output := make([]ArtifactRef, len(input))
	for index, artifact := range input {
		artifact.ID = redact.Text(artifact.ID)
		artifact.Name = redact.Text(artifact.Name)
		artifact.Path = redact.Text(artifact.Path)
		artifact.Type = redact.Text(artifact.Type)
		artifact.Producer = redact.Text(artifact.Producer)
		artifact.SHA256 = redact.Text(artifact.SHA256)
		artifact.Content = redact.Text(artifact.Content)
		if len(artifact.Metadata) > 0 {
			metadata := make(map[string]string, len(artifact.Metadata))
			for key, value := range artifact.Metadata {
				cleanKey := redact.Text(key)
				if redact.SensitiveKey(key) {
					metadata[cleanKey] = "<redacted>"
				} else {
					metadata[cleanKey] = redact.Text(value)
				}
			}
			artifact.Metadata = metadata
		}
		output[index] = artifact
	}
	return output
}

func redactGate(gate GateRequest) GateRequest {
	gate.RequestID = redact.Text(gate.RequestID)
	gate.GateID = redact.Text(gate.GateID)
	gate.GateName = redact.Text(gate.GateName)
	gate.NodeID = redact.Text(gate.NodeID)
	gate.Message = redact.Text(gate.Message)
	if len(gate.Checklist) > 0 {
		checklist := make([]ChecklistItem, len(gate.Checklist))
		for index, item := range gate.Checklist {
			item.ID = redact.Text(item.ID)
			item.Label = redact.Text(item.Label)
			checklist[index] = item
		}
		gate.Checklist = checklist
	}
	gate.Artifacts = redactArtifacts(gate.Artifacts)
	return gate
}

func redactMap(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		cleanKey := redact.Text(key)
		if redact.SensitiveKey(key) {
			output[cleanKey] = "<redacted>"
		} else {
			output[cleanKey] = redactValue(value)
		}
	}
	return output
}

func redactValue(value any) any {
	switch typed := value.(type) {
	case nil, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return typed
	case string:
		return redact.Text(typed)
	case []string:
		output := make([]string, len(typed))
		for index, item := range typed {
			output[index] = redact.Text(item)
		}
		return output
	case []any:
		output := make([]any, len(typed))
		for index, item := range typed {
			output[index] = redactValue(item)
		}
		return output
	case map[string]string:
		output := make(map[string]string, len(typed))
		for key, item := range typed {
			cleanKey := redact.Text(key)
			if redact.SensitiveKey(key) {
				output[cleanKey] = "<redacted>"
			} else {
				output[cleanKey] = redact.Text(item)
			}
		}
		return output
	case map[string]any:
		return redactMap(typed)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return redact.Text("<unserializable event field>")
	}
	var generic any
	if json.Unmarshal(raw, &generic) != nil {
		return redact.Text(string(raw))
	}
	return redactValue(generic)
}
