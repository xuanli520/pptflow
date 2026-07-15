package workflowadapter

import (
	"strings"
	"testing"
)

func TestStageOperationPayloadStrictDecodeAndCanonicalRoundTrip(t *testing.T) {
	raw := []byte(`{"kind":"agent.turn","agent_id":"repair_agent","model_id":"model_v2","max_turns":3}`)
	payload, err := ParseStageOperationPayloadJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	agent, ok := payload.(AgentTurnOperationPayload)
	if !ok || agent.AgentID != "repair_agent" || agent.ModelID != "model_v2" || agent.MaxTurns != 3 {
		t.Fatalf("parsed payload = %#v", payload)
	}
	canonical, err := CanonicalStageOperationPayloadJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(raw) {
		t.Fatalf("canonical payload = %s, want %s", canonical, raw)
	}
}

func TestStageOperationPayloadRejectsMalformedDocuments(t *testing.T) {
	validDigest := strings.Repeat("a", 64)
	for name, raw := range map[string][]byte{
		"unknown field":         []byte(`{"kind":"local.command","command_id":"harbor-stage","arguments":[],"extra":true}`),
		"missing command":       []byte(`{"kind":"local.command","arguments":[]}`),
		"missing arguments":     []byte(`{"kind":"local.command","command_id":"harbor-stage"}`),
		"null arguments":        []byte(`{"kind":"local.command","command_id":"harbor-stage","arguments":null}`),
		"duplicate key":         []byte(`{"kind":"agent.turn","agent_id":"one","agent_id":"two","model_id":"model","max_turns":1}`),
		"bad image pin":         []byte(`{"kind":"container.command","image_digest":"registry/example:latest","command":["run"]}`),
		"bad turn limit":        []byte(`{"kind":"agent.turn","agent_id":"agent","model_id":"model","max_turns":0}`),
		"unknown discriminator": []byte(`{"kind":"other.operation"}`),
		"invalid digest hex":    []byte(`{"kind":"container.command","image_digest":"registry/example@sha256:` + validDigest[:63] + `z","command":["run"]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseStageOperationPayloadJSON(raw); err == nil {
				t.Fatalf("malformed payload was accepted: %s", raw)
			}
		})
	}
}

func TestStageOperationBindingPayloadIsStrictAndCloned(t *testing.T) {
	binding := StageOperationBinding{
		ProviderID: "provider-local", OperationID: "repo_prepare", Version: "1",
		Payload: LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{"repo_prepare"}},
	}
	clone := binding.Clone()
	clonePayload := clone.Payload.(LocalCommandOperationPayload)
	clonePayload.Arguments[0] = "changed"
	clone.Payload = clonePayload
	original := binding.Payload.(LocalCommandOperationPayload)
	if original.Arguments[0] != "repo_prepare" {
		t.Fatalf("clone mutated original arguments: %#v", original.Arguments)
	}

	if err := binding.validate(); err != nil {
		t.Fatal(err)
	}
	if err := (StageOperationBinding{ProviderID: "provider", OperationID: "operation", Version: "1"}).validate(); err == nil {
		t.Fatal("binding without a typed payload was accepted")
	}
}

func TestCloneStageOperationPayloadPreservesExplicitEmptyArgumentArrays(t *testing.T) {
	payload := LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{}}
	clone, ok := CloneStageOperationPayload(payload).(LocalCommandOperationPayload)
	if !ok {
		t.Fatalf("clone type = %T, want LocalCommandOperationPayload", CloneStageOperationPayload(payload))
	}
	if clone.Arguments == nil || len(clone.Arguments) != 0 {
		t.Fatalf("explicit empty local command arguments became %#v", clone.Arguments)
	}
	if err := (StageOperationBinding{ProviderID: "provider-local", OperationID: "stage", Version: "1", Payload: clone}).validate(); err != nil {
		t.Fatalf("cloned explicit empty local command payload did not validate: %v", err)
	}
}
