package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistentEventRecorderRedactsCanonicalEventBeforePersistenceAndPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event_log.jsonl")
	recorder, err := NewPersistentEventRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	subscription, unsubscribe := recorder.Subscribe(1)
	defer unsubscribe()
	secret := "unit-test-event-secret"
	event := Event{
		RunID: "run-1", NodeID: "final_review", Type: "gate_requested",
		Message: "API_KEY=" + secret,
		Artifacts: []ArtifactRef{{
			Name: "report.json", Path: "/tmp/report.json", Content: `{"TOKEN":"` + secret + `"}`,
			Metadata: map[string]string{"authorization": "Bearer " + secret},
		}},
		Logs: []ArtifactRef{{Name: "stderr.txt", Content: "PASSWORD=" + secret}},
		Gate: &GateRequest{
			RequestID: "request-1", GateID: "final_review", Message: "AUTH=" + secret,
			Checklist: []ChecklistItem{{ID: "credential", Label: "Bearer " + secret, Critical: true}},
			Artifacts: []ArtifactRef{{Name: "gate.txt", Content: "SECRET=" + secret}},
		},
		Fields: map[string]any{
			"notes":   "API_TOKEN=" + secret,
			"api_key": secret,
			"nested":  map[string]any{"authorization": secret},
		},
	}
	if err := recorder.Emit(context.Background(), event); err != nil {
		t.Fatal(err)
	}

	published := <-subscription
	publishedRaw, err := json.Marshal(published)
	if err != nil {
		t.Fatal(err)
	}
	persistedRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for label, raw := range map[string][]byte{"published event": publishedRaw, "event_log.jsonl": persistedRaw} {
		text := string(raw)
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaked secret: %s", label, text)
		}
		if !strings.Contains(text, "redacted") {
			t.Fatalf("%s did not retain redaction evidence: %s", label, text)
		}
	}

	events := recorder.Events()
	if len(events) != 1 || events[0].Gate == nil || len(events[0].Gate.Checklist) != 1 {
		t.Fatalf("canonical gate event was not retained: %+v", events)
	}
}

func TestPersistentEventRecorderMigratesLegacyFieldGateAndRedactsExistingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event_log.jsonl")
	secret := "legacy-event-secret"
	legacy := `{"run_id":"run-legacy","type":"gate_requested","fields":{"gate":{"request_id":"request-1","gate_id":"final_review","message":"API_KEY=` + secret + `","checklist":[{"id":"auth","label":"Bearer ` + secret + `","critical":true}]}}}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder, err := NewPersistentEventRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	events := recorder.Events()
	if len(events) != 1 || events[0].Gate == nil || events[0].Gate.GateID != "final_review" {
		t.Fatalf("legacy gate was not promoted to canonical event field: %+v", events)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "redacted") {
		t.Fatalf("legacy event log was not rewritten safely: %s", raw)
	}
}
