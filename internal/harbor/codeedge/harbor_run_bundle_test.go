package codeedge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestHarborRunBundleV018CanonicalRoundTripAndStrictRejection(t *testing.T) {
	fixture := newHarborRunBundleFixture(t)
	bundle, err := CaptureHarborRunBundleV018(fixture.request())
	if err != nil {
		t.Fatalf("CaptureHarborRunBundleV018() error = %v", err)
	}
	raw, err := bundle.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseHarborRunBundleV018(raw)
	if err != nil {
		t.Fatalf("ParseHarborRunBundleV018() error = %v", err)
	}
	if parsed.SourceTaskSnapshotDigest != fixture.digest || parsed.MaterializedTaskRootV2Digest != fixture.digest {
		t.Fatalf("bundle V2 task digests = %q/%q, want %q", parsed.SourceTaskSnapshotDigest, parsed.MaterializedTaskRootV2Digest, fixture.digest)
	}
	if _, err := ParseHarborRunBundleV018(append(append([]byte(nil), raw...), ' ')); !errors.Is(err, ErrInvalidHarborRunBundle) {
		t.Fatalf("non-canonical trailing whitespace error = %v, want ErrInvalidHarborRunBundle", err)
	}
	duplicate := []byte(`{"format":"harbor.run-bundle.v0.18","format":"forged"}`)
	if _, err := ParseHarborRunBundleV018(duplicate); !errors.Is(err, ErrInvalidHarborRunBundle) {
		t.Fatalf("duplicate JSON error = %v, want ErrInvalidHarborRunBundle", err)
	}

	unsafe := parsed.clone()
	unsafe.Paths[0].Path = "../escape"
	unsafe.Files[0].Path = "../escape"
	if _, err := unsafe.CanonicalJSON(); !errors.Is(err, ErrInvalidHarborRunBundle) {
		t.Fatalf("unsafe path canonicalization error = %v, want ErrInvalidHarborRunBundle", err)
	}
}

func TestCaptureHarborRunBundleV018RejectsMismatchedTaskRootAndSecrets(t *testing.T) {
	t.Run("mismatched task root", func(t *testing.T) {
		fixture := newHarborRunBundleFixture(t)
		wrongRoot := writeHarborRunBundleTask(t)
		request := fixture.request()
		request.MaterializedTaskRoot = wrongRoot
		if _, err := CaptureHarborRunBundleV018(request); !errors.Is(err, ErrInvalidHarborRunBundle) {
			t.Fatalf("mismatched task root error = %v, want ErrInvalidHarborRunBundle", err)
		}
	})
	t.Run("secret in captured text", func(t *testing.T) {
		fixture := newHarborRunBundleFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.jobRoot, "job.log"), []byte("OPENAI_API_KEY=raw-api-value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := CaptureHarborRunBundleV018(fixture.request()); !errors.Is(err, ErrHarborRunBundleSecret) {
			t.Fatalf("secret capture error = %v, want ErrHarborRunBundleSecret", err)
		}
	})
}

func TestHarborRunBundleV018InspectionRejectsCrossJobTrial(t *testing.T) {
	fixture := newHarborRunBundleFixture(t)
	trialPath := filepath.Join(fixture.jobRoot, fixture.trialDirectories[0], "config.json")
	writeHarborRunBundleJSON(t, trialPath, map[string]any{"job_id": "other-job"})
	if _, err := CaptureHarborRunBundleV018(fixture.request()); !errors.Is(err, ErrInvalidHarborRunBundle) {
		t.Fatalf("cross-job trial capture error = %v, want ErrInvalidHarborRunBundle", err)
	}
}

func TestHarborRunBundleV018InspectionAuthenticatesStrictAggregateInternalRetryCount(t *testing.T) {
	t.Run("positive aggregate remains job scoped", func(t *testing.T) {
		fixture := newHarborRunBundleFixture(t)
		updateHarborRunBundleJob(t, fixture, func(job map[string]any) {
			job["stats"].(map[string]any)["n_retries"] = 2
		})

		bundle, err := CaptureHarborRunBundleV018(fixture.request())
		if err != nil {
			t.Fatalf("CaptureHarborRunBundleV018() error = %v", err)
		}
		inspection, err := InspectHarborRunBundleV018(bundle)
		if err != nil {
			t.Fatalf("InspectHarborRunBundleV018() error = %v", err)
		}
		if job := inspection.Job(); job.InternalRetryCount != 2 {
			t.Fatalf("job InternalRetryCount = %d, want 2", job.InternalRetryCount)
		}
		if trials := inspection.Trials(); len(trials) != harborRunBundleExpectedTrialCount {
			t.Fatalf("final logical trials = %d, want %d; aggregate retries must not create inferred trial facts", len(trials), harborRunBundleExpectedTrialCount)
		}
	})

	for _, invalid := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "missing",
			mutate: func(job map[string]any) {
				delete(job["stats"].(map[string]any), "n_retries")
			},
		},
		{
			name: "negative",
			mutate: func(job map[string]any) {
				job["stats"].(map[string]any)["n_retries"] = -1
			},
		},
		{
			name: "fractional",
			mutate: func(job map[string]any) {
				job["stats"].(map[string]any)["n_retries"] = 1.5
			},
		},
		{
			name: "string",
			mutate: func(job map[string]any) {
				job["stats"].(map[string]any)["n_retries"] = "2"
			},
		},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			fixture := newHarborRunBundleFixture(t)
			updateHarborRunBundleJob(t, fixture, invalid.mutate)
			if _, err := CaptureHarborRunBundleV018(fixture.request()); !errors.Is(err, ErrInvalidHarborRunBundle) {
				t.Fatalf("invalid stats.n_retries error = %v, want ErrInvalidHarborRunBundle", err)
			}
		})
	}
}

func TestHarborRunBundleV018InspectionPreservesHarborNaiveJobTimestamp(t *testing.T) {
	t.Run("accepts Harbor 0.18 naive timestamp without inventing a timezone", func(t *testing.T) {
		fixture := newHarborRunBundleFixture(t)
		const finishedAt = "2026-07-22T14:32:28.056110"
		updateHarborRunBundleJob(t, fixture, func(job map[string]any) {
			job["finished_at"] = finishedAt
		})
		bundle, err := CaptureHarborRunBundleV018(fixture.request())
		if err != nil {
			t.Fatalf("capture Harbor 0.18 naive timestamp: %v", err)
		}
		inspection, err := InspectHarborRunBundleV018(bundle)
		if err != nil {
			t.Fatalf("inspect Harbor 0.18 naive timestamp: %v", err)
		}
		if got := inspection.Job().FinishedAt; got != finishedAt {
			t.Fatalf("observed job finished_at = %q, want exact raw value %q", got, finishedAt)
		}
	})

	t.Run("rejects malformed naive timestamp", func(t *testing.T) {
		fixture := newHarborRunBundleFixture(t)
		updateHarborRunBundleJob(t, fixture, func(job map[string]any) {
			job["finished_at"] = "2026-07-22 14:32:28"
		})
		if _, err := CaptureHarborRunBundleV018(fixture.request()); !errors.Is(err, ErrInvalidHarborRunBundle) {
			t.Fatalf("malformed naive timestamp error = %v, want ErrInvalidHarborRunBundle", err)
		}
	})
}

func updateHarborRunBundleJob(t *testing.T, fixture harborRunBundleFixture, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(fixture.jobRoot, "result.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	mutate(job)
	writeHarborRunBundleJSON(t, path, job)
}

type harborRunBundleFixture struct {
	taskRoot         string
	jobRoot          string
	digest           workflowkit.SubjectDigest
	trialDirectories []string
}

func newHarborRunBundleFixture(t *testing.T) harborRunBundleFixture {
	t.Helper()
	taskRoot := writeHarborRunBundleTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	jobRoot := filepath.Join(t.TempDir(), "job")
	if err := os.MkdirAll(jobRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeHarborRunBundleJSON(t, filepath.Join(jobRoot, "config.json"), map[string]any{
		"n_attempts": 4,
		"tasks":      []any{map[string]any{"path": taskRoot}},
		"datasets":   []any{},
		"agents":     []any{map[string]any{"name": "claude-code", "model_name": "qwen"}},
	})
	writeHarborRunBundleJSON(t, filepath.Join(jobRoot, "lock.json"), map[string]any{
		"schema_version": 2, "harbor": map[string]any{"version": "0.18.0"}, "trials": []any{},
	})
	writeHarborRunBundleJSON(t, filepath.Join(jobRoot, "result.json"), map[string]any{
		"id": "job-1", "started_at": "2026-07-14T00:00:00Z", "finished_at": "2026-07-14T00:10:00Z", "n_total_trials": 4,
		"stats": map[string]any{
			"n_running_trials": 0, "n_pending_trials": 0, "n_retries": 0,
			"evals": map[string]any{"claude-code__qwen__adhoc": map[string]any{"pass_at_k": map[string]any{"2": 0.25, "4": 0.25}}},
		},
	})
	lockDigest := "sha256:" + strings.Repeat("a", 64)
	directories := make([]string, 0, 4)
	for ordinal := 1; ordinal <= 4; ordinal++ {
		directory := "task__generated-" + string(rune('a'+ordinal-1))
		directories = append(directories, directory)
		root := filepath.Join(jobRoot, directory)
		if err := os.MkdirAll(filepath.Join(root, "agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeHarborRunBundleJSON(t, filepath.Join(root, "config.json"), map[string]any{"job_id": "job-1", "trial_name": directory})
		writeHarborRunBundleJSON(t, filepath.Join(root, "lock.json"), map[string]any{"task": map[string]any{"digest": lockDigest}})
		var exceptionInfo any
		if ordinal == 4 {
			exceptionInfo = map[string]any{"exception_type": "NetworkError"}
		}
		writeHarborRunBundleJSON(t, filepath.Join(root, "result.json"), map[string]any{
			"id": "trial-id-" + string(rune('a'+ordinal-1)), "trial_name": directory,
			"task_checksum": "dirhash-task-checksum-" + string(rune('a'+ordinal-1)),
			"config":        map[string]any{"job_id": "job-1"},
			"started_at":    "2026-07-14T00:00:00Z", "finished_at": "2026-07-14T00:01:00Z",
			"exception_info":  exceptionInfo,
			"agent_info":      map[string]any{"name": "claude-code", "version": "1.0.0", "model_info": map[string]any{"name": "qwen"}},
			"verifier_result": map[string]any{"rewards": map[string]any{"reward": map[bool]int{true: 1, false: 0}[ordinal == 1]}},
		})
		writeHarborRunBundleJSON(t, filepath.Join(root, "agent", "trajectory.json"), map[string]any{"final_metrics": map[string]any{"total_steps": 20 + ordinal}})
	}
	return harborRunBundleFixture{taskRoot: taskRoot, jobRoot: jobRoot, digest: workflowkit.SubjectDigest(digest), trialDirectories: directories}
}

func (fixture harborRunBundleFixture) request() HarborRunBundleCaptureRequest {
	return HarborRunBundleCaptureRequest{
		JobDirectory: fixture.jobRoot, MaterializedTaskRoot: fixture.taskRoot, FrozenTaskSnapshotDigest: fixture.digest,
		HarborCLI: HarborCLIIdentity{CommandID: "harbor", Version: "0.18.0", ContentFingerprint: workflowkit.SHA256Fingerprint([]byte("harbor-0.18.0-fixture"))},
	}
}

func writeHarborRunBundleTask(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "task")
	files := map[string][]byte{
		"instruction.md":         []byte("Fix the issue.\n"),
		"task.toml":              []byte("[task]\nname = \"fixture/task\"\n"),
		"tests_analysis.md":      []byte("analysis\n"),
		"environment/Dockerfile": []byte("FROM alpine:3.22\n"),
		"solution/solve.sh":      []byte("#!/bin/sh\nexit 0\n"),
		"tests/test.sh":          []byte("#!/bin/sh\nexit 0\n"),
	}
	for relative, content := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(relative, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, content, mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func writeHarborRunBundleJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
