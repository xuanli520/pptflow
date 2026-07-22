package codeedge

import (
	"bytes"
	"testing"
	"time"
)

// TestHarborRunBundleV018TempFilesystemIntegration exercises the complete
// local capture boundary without invoking an external provider: a controlled
// task root and Harbor-shaped job tree are written to temp storage, captured,
// serialized, parsed, and then read through the typed/raw inspection API.
func TestHarborRunBundleV018TempFilesystemIntegration(t *testing.T) {
	fixture := newHarborRunBundleFixture(t)
	bundle, err := CaptureHarborRunBundleV018(fixture.request())
	if err != nil {
		t.Fatalf("capture temp Harbor job tree: %v", err)
	}
	raw, err := bundle.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := ParseAndInspectHarborRunBundleV018(raw)
	if err != nil {
		t.Fatalf("parse and inspect bundle: %v", err)
	}
	job := inspection.Job()
	if job.ID != "job-1" || job.TotalTrials != 4 || job.RunningTrials != 0 || job.PendingTrials != 0 || job.FinishedAt == "" {
		t.Fatalf("job facts = %+v", job)
	}
	if got := job.PassAtK["claude-code__qwen__adhoc"]["4"]; got != 0.25 {
		t.Fatalf("job pass@4 = %v, want 0.25", got)
	}
	trials := inspection.Trials()
	if len(trials) != 4 {
		t.Fatalf("trial facts = %+v, want four", trials)
	}
	sawNetworkError := false
	for _, trial := range trials {
		if trial.JobID != job.ID || trial.Elapsed != time.Minute || trial.LockTaskDigest == trial.TaskChecksum {
			t.Fatalf("trial facts lost distinct identity/timing: %+v", trial)
		}
		if trial.Evaluator.ModelName == nil || *trial.Evaluator.ModelName != "qwen" || trial.Evaluator.ModelProvider != nil {
			t.Fatalf("optional provider facts = %+v, want qwen/no-provider", trial.Evaluator)
		}
		if trial.TrajectoryTotalSteps == nil || *trial.TrajectoryTotalSteps < 21 {
			t.Fatalf("trajectory facts = %+v", trial)
		}
		sawNetworkError = sawNetworkError || trial.ExceptionType == "NetworkError"
		config, configErr := inspection.TrialConfigJSON(trial.ID)
		if configErr != nil || !bytes.Contains(config, []byte(`"job_id":"job-1"`)) {
			t.Fatalf("trial config reader = %q, %v", config, configErr)
		}
		trajectory, found, trajectoryErr := inspection.TrialTrajectoryJSON(trial.ID)
		if trajectoryErr != nil || !found || !bytes.Contains(trajectory, []byte(`"total_steps"`)) {
			t.Fatalf("trial trajectory reader = %q, found=%t, err=%v", trajectory, found, trajectoryErr)
		}
	}
	if !sawNetworkError {
		t.Fatal("inspection did not retain trial exception_info.exception_type")
	}

	first := trials[0]
	config, err := inspection.TrialConfigJSON(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	config[0] ^= 0xff
	again, err := inspection.TrialConfigJSON(first.ID)
	if err != nil || bytes.Equal(config, again) {
		t.Fatalf("trial raw reader exposed mutable captured bytes: %q / %q / %v", config, again, err)
	}
}
