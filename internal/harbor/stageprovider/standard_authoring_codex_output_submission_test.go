package stageprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringCodexOutputSubmissionRejectsInvalidCandidatesWithoutPersistingContent(t *testing.T) {
	t.Parallel()
	const secret = "invalid-candidate-must-not-escape"
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	encoded := base64.StdEncoding.EncodeToString([]byte("candidate bytes"))
	cases := []struct {
		name        string
		raw         json.RawMessage
		wantError   string
		wantCharges int
	}{
		{
			name:      "trailing garbage",
			raw:       json.RawMessage(`{"verdict":"pass","artifacts":[{"content_base64":"` + encoded + `"}]} {"secret":"` + secret + `"}`),
			wantError: "invalid_json", wantCharges: 1,
		},
		{
			name:      "duplicate key",
			raw:       json.RawMessage(`{"verdict":"pass","verdict":"pass","artifacts":[{"content_base64":"` + encoded + `"}]}`),
			wantError: "invalid_json", wantCharges: 1,
		},
		{
			name:      "wrong verdict",
			raw:       json.RawMessage(`{"verdict":"reject","artifacts":[{"content_base64":"` + encoded + `"}]}`),
			wantError: "wrong_verdict", wantCharges: 1,
		},
		{
			name:      "missing verdict",
			raw:       json.RawMessage(`{"artifacts":[{"content_base64":"` + encoded + `"}]}`),
			wantError: "wrong_verdict", wantCharges: 1,
		},
		{
			name:      "null verdict",
			raw:       json.RawMessage(`{"verdict":null,"artifacts":[{"content_base64":"` + encoded + `"}]}`),
			wantError: "wrong_verdict", wantCharges: 1,
		},
		{
			name:      "wrong artifact count",
			raw:       json.RawMessage(`{"verdict":"pass","artifacts":[]}`),
			wantError: "artifact_identity_mismatch", wantCharges: 1,
		},
		{
			name:      "missing artifacts",
			raw:       json.RawMessage(`{"verdict":"pass"}`),
			wantError: "artifact_identity_mismatch", wantCharges: 1,
		},
		{
			name:      "null artifacts",
			raw:       json.RawMessage(`{"verdict":"pass","artifacts":null}`),
			wantError: "artifact_identity_mismatch", wantCharges: 1,
		},
		{
			name:      "missing base64",
			raw:       json.RawMessage(`{"verdict":"pass","artifacts":[{}]}`),
			wantError: "invalid_content_encoding", wantCharges: 1,
		},
		{
			name:      "null base64",
			raw:       json.RawMessage(`{"verdict":"pass","artifacts":[{"content_base64":null}]}`),
			wantError: "invalid_content_encoding", wantCharges: 1,
		},
		{
			name:      "non-string base64",
			raw:       json.RawMessage(`{"verdict":"pass","artifacts":[{"content_base64":7}]}`),
			wantError: "invalid_json", wantCharges: 1,
		},
		{
			name:      "invalid base64",
			raw:       json.RawMessage(`{"verdict":"pass","artifacts":[{"content_base64":"not base64"}]}`),
			wantError: "invalid_content_encoding", wantCharges: 1,
		},
		{
			name:      "caller supplied artifact identity",
			raw:       json.RawMessage(`{"verdict":"pass","artifacts":[{"content_base64":"` + encoded + `","name":"other","schema_version":"other","path":"/tmp/other"}]}`),
			wantError: "invalid_json", wantCharges: 1,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stage := standardAuthoringCodexTestStage(1)
			request, _, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
			submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 3, func() time.Time { return now }, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := submission.beginTurn(1); err != nil {
				t.Fatal(err)
			}

			response, err := submission.dynamicTool().Handler(context.Background(), testCase.raw)
			if err != nil {
				t.Fatal(err)
			}
			receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
			if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != testCase.wantError || receipt.Digest != workflowkit.SHA256Fingerprint(testCase.raw) {
				t.Fatalf("receipt = %+v, want rejected %q with raw digest", receipt, testCase.wantError)
			}
			if strings.Contains(string(response), secret) {
				t.Fatalf("rejection response leaked invalid candidate content")
			}
			if _, accepted := submission.acceptedResult(); accepted {
				t.Fatal("invalid candidate became an accepted artifact")
			}
			if len(*usages) != testCase.wantCharges {
				t.Fatalf("usage records = %+v, want %d charged submission", *usages, testCase.wantCharges)
			}
			for _, usage := range *usages {
				if usage.Dimension != standardAuthoringCodexOutputSubmissionQuotaDimension || strings.Contains(usage.OperationKey, secret) {
					t.Fatalf("invalid candidate usage = %+v, want secret-free output-submission charge", usage)
				}
			}
		})
	}
}

func TestStandardAuthoringCodexFixedFileSubmissionPublishesOnlyHostReadBytes(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name     string
		stageKey workflowkit.StageKey
		content  []byte
	}{
		{name: "solve", stageKey: workflowkit.StageKey(workflowadapter.SolveGen), content: []byte("#!/bin/sh\nset -eu\nprintf 'solution\\n'\n")},
		{name: "test", stageKey: workflowkit.StageKey(workflowadapter.TestGen), content: []byte("#!/bin/sh\nset -eu\nprintf 'tests\\n'\n")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stage := standardAuthoringCodexFixedFileTestStage(t, testCase.stageKey)
			taskRoot := t.TempDir()
			relative, outputName, _, ok := standardAuthoringCodexFixedFileStageContract(stage)
			if !ok {
				t.Fatalf("fixed-file stage contract missing for %q", stage.Key)
			}
			path := filepath.Join(taskRoot, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, testCase.content, 0o750); err != nil {
				t.Fatal(err)
			}
			request, _, usages := standardAuthoringCodexFixedFileTestRequest(t, stage, now)
			submission, err := newStandardAuthoringCodexFixedFileSubmission(request, taskRoot, 1024, 1, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			if err := submission.beginTurn(1); err != nil {
				t.Fatal(err)
			}
			tool := submission.dynamicTool()
			var schema map[string]any
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			properties, ok := schema["properties"].(map[string]any)
			if !ok || len(properties) != 1 || properties["verdict"] == nil || schema["additionalProperties"] != false {
				t.Fatalf("fixed-file tool schema = %#v", schema)
			}
			response, err := tool.Handler(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
			if err != nil {
				t.Fatal(err)
			}
			receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
			if !receipt.Accepted || len(receipt.Errors) != 0 || receipt.Digest != workflowkit.SHA256Fingerprint(testCase.content) {
				t.Fatalf("fixed-file receipt = %+v", receipt)
			}
			accepted, found := submission.acceptedResult()
			if !found || accepted.Outcome.Verdict != workflowkit.VerdictPass || len(accepted.Artifacts) != 1 ||
				accepted.Artifacts[0].Name != outputName || string(accepted.Artifacts[0].Content) != string(testCase.content) {
				t.Fatalf("accepted fixed-file result = %+v", accepted)
			}
			if len(*usages) != 1 || (*usages)[0].Dimension != standardAuthoringCodexOutputSubmissionQuotaDimension {
				t.Fatalf("fixed-file charges = %+v", *usages)
			}
		})
	}
}

func TestStandardAuthoringCodexOutputSchemaAssetsArePinnedByTemplateAndStage(t *testing.T) {
	fixedTemplate := workflowadapter.StandardAuthoringFixedFileTemplateReference()
	legacyTemplate := workflowadapter.StandardAuthoringHarnessTemplateReference()
	fixedSchema := standardAuthoringCodexFixedFileOutputSchemaTemplate()
	legacySchema := standardAuthoringCodexOutputSchemaTemplate()
	for _, key := range []workflowkit.StageKey{
		workflowkit.StageKey(workflowadapter.SolveGen),
		workflowkit.StageKey(workflowadapter.TestGen),
	} {
		if err := ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(fixedTemplate, key, fixedSchema); err != nil {
			t.Fatalf("validate fixed schema for %q: %v", key, err)
		}
		if err := ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(fixedTemplate, key, legacySchema); err == nil {
			t.Fatalf("accepted legacy base64 schema for fixed-file stage %q", key)
		}
		if got := StandardAuthoringCodexOutputSchemaFingerprintForTemplateStage(fixedTemplate, key); got != StandardAuthoringCodexFixedFileOutputSchemaFingerprint() {
			t.Fatalf("fixed schema fingerprint for %q = %q", key, got)
		}
		if err := ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(legacyTemplate, key, legacySchema); err != nil {
			t.Fatalf("validate legacy schema for %q: %v", key, err)
		}
		if err := ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(legacyTemplate, key, fixedSchema); err == nil {
			t.Fatalf("accepted fixed-file schema for frozen 1.7 stage %q", key)
		}
	}
	if err := ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(fixedTemplate, workflowkit.StageKey(workflowadapter.RepoAnalyze), legacySchema); err != nil {
		t.Fatalf("validate ordinary fixed-template stage schema: %v", err)
	}
}

func TestStandardAuthoringCodexFixedFileSubmissionRejectsInvalidOrUnsafeCandidates(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	newSubmission := func(t *testing.T, content []byte, maxBytes int) (*standardAuthoringCodexOutputSubmission, string, *[]workflowkit.StageUsage) {
		t.Helper()
		stage := standardAuthoringCodexFixedFileTestStage(t, workflowkit.StageKey(workflowadapter.SolveGen))
		taskRoot := t.TempDir()
		relative, _, _, ok := standardAuthoringCodexFixedFileStageContract(stage)
		if !ok {
			t.Fatal("solve stage is missing fixed-file contract")
		}
		path := filepath.Join(taskRoot, filepath.FromSlash(relative))
		if content != nil {
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, content, 0o750); err != nil {
				t.Fatal(err)
			}
		}
		request, _, usages := standardAuthoringCodexFixedFileTestRequest(t, stage, now)
		submission, err := newStandardAuthoringCodexFixedFileSubmission(request, taskRoot, maxBytes, 1, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		if err := submission.beginTurn(1); err != nil {
			t.Fatal(err)
		}
		return submission, path, usages
	}

	for _, testCase := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "missing verdict", raw: json.RawMessage(`{}`)},
		{name: "non-pass verdict", raw: json.RawMessage(`{"verdict":"needs_repair"}`)},
		{name: "extra field", raw: json.RawMessage(`{"verdict":"pass","artifacts":[]}`)},
		{name: "duplicate verdict", raw: json.RawMessage(`{"verdict":"pass","verdict":"pass"}`)},
		{name: "trailing JSON", raw: json.RawMessage(`{"verdict":"pass"} {}`)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			submission, _, usages := newSubmission(t, []byte("#!/bin/sh\nexit 0\n"), 1024)
			response, err := submission.dynamicTool().Handler(context.Background(), testCase.raw)
			if err != nil {
				t.Fatal(err)
			}
			receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
			if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "wrong_verdict" {
				t.Fatalf("invalid fixed-file receipt = %+v", receipt)
			}
			if _, accepted := submission.acceptedResult(); accepted || len(*usages) != 1 {
				t.Fatalf("invalid fixed-file submission accepted=%t charges=%+v", accepted, *usages)
			}
		})
	}

	t.Run("missing fixed file", func(t *testing.T) {
		submission, _, _ := newSubmission(t, nil, 1024)
		response, err := submission.dynamicTool().Handler(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
		if err != nil {
			t.Fatal(err)
		}
		receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
		if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "candidate_unavailable" {
			t.Fatalf("missing fixed-file receipt = %+v", receipt)
		}
	})

	t.Run("invalid shell script", func(t *testing.T) {
		submission, _, _ := newSubmission(t, []byte("exit 0\n"), 1024)
		response, err := submission.dynamicTool().Handler(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
		if err != nil {
			t.Fatal(err)
		}
		receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
		if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "solve_script_invalid" {
			t.Fatalf("invalid fixed script receipt = %+v", receipt)
		}
	})

	t.Run("oversized fixed file", func(t *testing.T) {
		submission, _, _ := newSubmission(t, []byte("#!/bin/sh\n"+strings.Repeat("x", 64)), 32)
		response, err := submission.dynamicTool().Handler(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
		if err != nil {
			t.Fatal(err)
		}
		receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
		if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "byte_limit_exceeded" {
			t.Fatalf("oversized fixed script receipt = %+v", receipt)
		}
	})

	for _, testCase := range []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{
			name: "symlink",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(filepath.Dir(path)), "outside-script")
				if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
		{
			name: "hardlink",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(filepath.Dir(path)), "outside-script")
				if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(target, path); err != nil {
					t.Skipf("hardlinks unavailable: %v", err)
				}
			},
		},
	} {
		t.Run(testCase.name+" is rejected before publication", func(t *testing.T) {
			submission, path, _ := newSubmission(t, []byte("#!/bin/sh\nexit 0\n"), 1024)
			testCase.mutate(t, path)
			response, err := submission.dynamicTool().Handler(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
			if err != nil {
				t.Fatal(err)
			}
			receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
			if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "candidate_unavailable" {
				t.Fatalf("unsafe fixed script receipt = %+v", receipt)
			}
		})
	}

	t.Run("replacement between validation and publication", func(t *testing.T) {
		original := []byte("#!/bin/sh\nprintf 'original\\n'\n")
		replacement := []byte("#!/bin/sh\nprintf 'replacement\\n'\n")
		submission, path, _ := newSubmission(t, original, 1024)
		reads := 0
		submission.readFixedFile = func(taskRoot, relative string, maxBytes int64) ([]byte, error) {
			reads++
			content, err := authoringharness.ReadFixedFileWithLimit(taskRoot, relative, maxBytes)
			if err != nil || reads != 1 {
				return content, err
			}
			if err := os.Remove(path); err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, replacement, 0o750); err != nil {
				return nil, err
			}
			return content, nil
		}
		response, err := submission.dynamicTool().Handler(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
		if err != nil {
			t.Fatal(err)
		}
		receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
		if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "candidate_changed_after_validation" || reads != 2 {
			t.Fatalf("replacement receipt = %+v reads=%d", receipt, reads)
		}
		if _, accepted := submission.acceptedResult(); accepted {
			t.Fatal("replacement after validation was published")
		}
	})
}

func standardAuthoringCodexFixedFileTestStage(t *testing.T, key workflowkit.StageKey) workflowkit.StageDescriptor {
	t.Helper()
	expected, found := workflowadapter.StandardAuthoringFixedFileStageCatalog().Stage(key)
	if !found || len(expected.Outputs) != 1 {
		t.Fatalf("fixed-file catalog stage %q = %+v found=%t", key, expected, found)
	}
	stage := standardAuthoringCodexTestArtifactStage(1, key, expected.Outputs[0].Name)
	stage.Version = expected.Version
	stage.Plugin = workflowkit.PluginBinding{ID: expected.Plugin.ID, Version: expected.Plugin.Version}
	stage.Outputs = append([]workflowkit.ArtifactSpec(nil), expected.Outputs...)
	stage.Verdicts = expected.Verdicts.Clone()
	return stage
}

func standardAuthoringCodexFixedFileTestRequest(t *testing.T, stage workflowkit.StageDescriptor, now time.Time) (workflowkit.StageExecutionRequest, *[]workflowkit.StageCheckpoint, *[]workflowkit.StageUsage) {
	t.Helper()
	request, checkpoints, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
	template := workflowadapter.StandardAuthoringFixedFileTemplateReference()
	request.Execution.Workflow.ID = template.ID
	request.Execution.Workflow.Version = template.Version
	return request, checkpoints, usages
}

func TestStandardAuthoringCodexOutputSubmissionDerivesClosedSchemaFromFrozenStage(t *testing.T) {
	stage := standardAuthoringCodexTestStage(1)
	stage.Outputs = append(stage.Outputs, workflowkit.ArtifactSpec{Name: "second_output", SchemaVersion: "harbor.artifact.v1", Required: true})

	var schema map[string]any
	if err := json.Unmarshal(standardAuthoringCodexSubmissionSchema(stage), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$schema"] != "http://json-schema.org/draft-07/schema#" || schema["$id"] != "harbor.standard-authoring-codex-stage-output.v1" || schema["additionalProperties"] != false {
		t.Fatalf("derived top-level schema = %#v", schema)
	}
	required, ok := schema["required"].([]any)
	if !ok || len(required) != 2 || required[0] != "verdict" || required[1] != "artifacts" {
		t.Fatalf("derived required fields = %#v", schema["required"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("derived properties = %#v", schema["properties"])
	}
	verdict, ok := properties["verdict"].(map[string]any)
	if !ok || verdict["type"] != "string" {
		t.Fatalf("derived verdict property = %#v", properties["verdict"])
	}
	enum, ok := verdict["enum"].([]any)
	if !ok || len(enum) != 2 || enum[0] != string(workflowkit.VerdictNeedsRepair) || enum[1] != string(workflowkit.VerdictPass) {
		t.Fatalf("derived verdict enum = %#v", verdict["enum"])
	}
	artifacts, ok := properties["artifacts"].(map[string]any)
	if !ok || artifacts["type"] != "array" || artifacts["minItems"] != float64(2) || artifacts["maxItems"] != float64(2) {
		t.Fatalf("derived artifacts property = %#v", properties["artifacts"])
	}
	items, ok := artifacts["items"].(map[string]any)
	if !ok || items["type"] != "object" || items["additionalProperties"] != false {
		t.Fatalf("derived artifact item = %#v", artifacts["items"])
	}
	itemProperties, ok := items["properties"].(map[string]any)
	if !ok || len(itemProperties) != 1 || itemProperties["content_base64"] == nil {
		t.Fatalf("derived artifact fields = %#v", items["properties"])
	}
	for _, forbidden := range []string{"name", "schema_version", "path", "stage"} {
		if _, exposed := itemProperties[forbidden]; exposed {
			t.Fatalf("derived schema exposed host-owned %q field: %#v", forbidden, itemProperties)
		}
	}
}

func TestStandardAuthoringCodexOutputSubmissionCanonicalizesSemanticEquivalentCandidates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	canonical := standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("canonical artifact"))
	spaced := json.RawMessage(" \n\t" + string(canonical) + "\n")
	if workflowkit.SHA256Fingerprint(canonical) == workflowkit.SHA256Fingerprint(spaced) {
		t.Fatal("test inputs unexpectedly have the same raw digest")
	}

	acceptedDigests := make([]workflowkit.Fingerprint, 0, 2)
	for _, raw := range []json.RawMessage{canonical, spaced} {
		stage := standardAuthoringCodexTestStage(1)
		request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
		submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 1, func() time.Time { return now }, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := submission.beginTurn(1); err != nil {
			t.Fatal(err)
		}
		response, err := submission.dynamicTool().Handler(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
		if !receipt.Accepted || len(receipt.Errors) != 0 {
			t.Fatalf("receipt = %+v, want accepted candidate", receipt)
		}
		acceptedDigests = append(acceptedDigests, receipt.Digest)
	}
	if acceptedDigests[0] != acceptedDigests[1] {
		t.Fatalf("semantic candidates received different canonical digests: %q and %q", acceptedDigests[0], acceptedDigests[1])
	}
}

func TestStandardAuthoringCodexOutputSubmissionCanonicalizesASCIIWhitespaceInBase64(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	content := []byte("artifact bytes that are long enough to wrap")
	encoded := base64.StdEncoding.EncodeToString(content)
	spaced := encoded[:5] + " \t\r\n\v\f" + encoded[5:]
	raw := standardAuthoringCodexTestCandidateWithBase64(t, workflowkit.VerdictPass, spaced)

	stage := standardAuthoringCodexTestStage(1)
	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
	submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 1, func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := submission.beginTurn(1); err != nil {
		t.Fatal(err)
	}
	response, err := submission.dynamicTool().Handler(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
	if !receipt.Accepted || len(receipt.Errors) != 0 {
		t.Fatalf("receipt = %+v, want accepted candidate", receipt)
	}
	accepted, found := submission.acceptedResult()
	if !found || len(accepted.Artifacts) != 1 || string(accepted.Artifacts[0].Content) != string(content) {
		t.Fatalf("accepted result = %+v, want decoded artifact", accepted)
	}
	wantCanonical := standardAuthoringCodexCanonicalSubmission{
		Format: standardAuthoringCodexCanonicalSubmissionFormat, Version: standardAuthoringCodexCanonicalSubmissionVersion,
		StageKey: stage.Key, StageVersion: stage.Version, Verdict: workflowkit.VerdictPass,
		Artifacts: []standardAuthoringCodexCanonicalSubmissionArtifact{{Name: stage.Outputs[0].Name, SchemaVersion: stage.Outputs[0].SchemaVersion, ContentBase64: encoded}},
	}
	wantCanonicalBytes, err := json.Marshal(wantCanonical)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Digest != workflowkit.SHA256Fingerprint(wantCanonicalBytes) {
		t.Fatalf("digest = %q, want canonical digest %q", receipt.Digest, workflowkit.SHA256Fingerprint(wantCanonicalBytes))
	}
}

func TestCanonicalStandardAuthoringCodexBase64RejectsNonCanonicalSpelling(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"Zg", "Zh==", "-w==", "Zg==\u00a0"} {
		if canonical, _, err := canonicalStandardAuthoringCodexBase64(input); err == nil {
			t.Fatalf("input %q unexpectedly accepted as %q", input, canonical)
		}
	}
}

func standardAuthoringCodexTestCandidateWithBase64(t *testing.T, verdict workflowkit.Verdict, contentBase64 string) json.RawMessage {
	t.Helper()
	candidate := struct {
		Verdict   workflowkit.Verdict `json:"verdict"`
		Artifacts []struct {
			ContentBase64 string `json:"content_base64"`
		} `json:"artifacts"`
	}{
		Verdict: verdict,
		Artifacts: []struct {
			ContentBase64 string `json:"content_base64"`
		}{{ContentBase64: contentBase64}},
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(encoded)
}

func TestStandardAuthoringCodexOutputSubmissionEnforcesFrozenDockerfileEnvironmentPolicy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	policy := standardAuthoringCodexTestEnvironmentPolicy(t)
	valid := []byte("FROM " + policy.BaseImage + " AS build\nRUN printf '%s\\n' ready\n")
	cases := []struct {
		name       string
		content    []byte
		wantAccept bool
	}{
		{name: "exact immutable image", content: valid, wantAccept: true},
		{name: "different immutable image", content: []byte("FROM registry.example.com/team/other:1.2.3@sha256:" + strings.Repeat("b", 64) + "\n")},
		{name: "variable expansion", content: []byte("ARG BASE=" + policy.BaseImage + "\nFROM ${BASE}\n")},
		{name: "additional image", content: []byte("FROM " + policy.BaseImage + " AS build\nFROM registry.example.com/team/other:1.2.3@sha256:" + strings.Repeat("b", 64) + "\n")},
		{name: "no from instruction", content: []byte("RUN printf '%s\\n' missing\n")},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stage := standardAuthoringCodexTestDockerfileStage(1)
			request, _, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
			submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 1, func() time.Time { return now }, &policy)
			if err != nil {
				t.Fatal(err)
			}
			if err := submission.beginTurn(1); err != nil {
				t.Fatal(err)
			}
			response, err := submission.dynamicTool().Handler(context.Background(), standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, testCase.content))
			if err != nil {
				t.Fatal(err)
			}
			receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
			if receipt.Accepted != testCase.wantAccept {
				t.Fatalf("receipt = %+v, want accepted=%t", receipt, testCase.wantAccept)
			}
			if testCase.wantAccept {
				accepted, found := submission.acceptedResult()
				if !found || len(accepted.Artifacts) != 1 || string(accepted.Artifacts[0].Content) != string(testCase.content) {
					t.Fatalf("accepted Dockerfile result = %+v", accepted)
				}
			} else if len(receipt.Errors) != 1 || receipt.Errors[0] != "dockerfile_environment_policy_mismatch" {
				t.Fatalf("rejected Dockerfile receipt = %+v", receipt)
			}
			if len(*usages) != 1 || (*usages)[0].Dimension != standardAuthoringCodexOutputSubmissionQuotaDimension {
				t.Fatalf("usage records = %+v, want one charged submission", *usages)
			}
		})
	}
}

func TestStandardAuthoringCodexOutputSubmissionEnforcesStagePayloadContracts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		stageKey   workflowkit.StageKey
		outputName string
		valid      [][]byte
		invalid    [][]byte
		diagnostic string
	}{
		{
			name: "instruction", stageKey: workflowkit.StageKey(workflowadapter.InstructionGen), outputName: "instruction",
			valid: [][]byte{[]byte("# Implement the feature\n\nKeep the behavior local.\n"), []byte("[Public API](https://example.test) must remain compatible.\n")},
			invalid: [][]byte{
				[]byte(`{"format":"harbor.standard-authoring-instruction.v1","version":"1","content":"# Wrapped"}`),
				[]byte(`["wrapped instruction"]`), []byte("```markdown\n# Wrapped\n```\n"),
			},
			diagnostic: "instruction_invalid",
		},
		{
			name: "task TOML", stageKey: workflowkit.StageKey(workflowadapter.TaskTOMLGen), outputName: "task_toml",
			valid: [][]byte{[]byte("schema_version = \"1.0\"\n\n[metadata]\ncode_lang = \"rust\"\ntask_type = \"feature\"\napplication = \"backend\"\nis_0_to_1 = false\n\n[task]\nname = \"harbor/request-header-count-limit\"\ndescription = \"Add the requested middleware.\"\n\n[environment]\nbuild_timeout_sec = 900.0\nnetwork_mode = \"no-network\"\nworkdir = \"/workspace/source\"\n\n[verifier]\ntimeout_sec = 1800.0\n")},
			invalid: [][]byte{
				[]byte(`{"format":"harbor.artifact.v1","version":"1","task_toml":"[metadata]"}`),
				[]byte("[metadata]\ntask_type = \"feature\"\n[metadata]\napplication = \"backend\"\n"),
				[]byte("[metadata]\ncode_lang = \"rust\"\ntask_type = \"feature\"\napplication = \"backend\"\nis_0_to_1 = false\n"),
				[]byte("[metadata]\ncode_lang = \"rust\"\ntask_type = \"feature\"\napplication = \"backend\"\nis_0_to_1 = false\n\n[task]\ndescription = \"Add the requested middleware.\"\n\n[environment]\nbuild_timeout_sec = 900.0\nnetwork_mode = \"no-network\"\nworkdir = \"/workspace/source\"\n\n[verifier]\ntimeout_sec = 1800.0\n"),
				[]byte("[metadata]\ncode_lang = \"rust\"\ntask_type = \"feature\"\napplication = \"backend\"\nis_0_to_1 = false\n\n[task]\nname = \"not-a-harbor-task\"\ndescription = \"Add the requested middleware.\"\n\n[environment]\nbuild_timeout_sec = 900.0\nnetwork_mode = \"no-network\"\nworkdir = \"/workspace/source\"\n\n[verifier]\ntimeout_sec = 1800.0\n"),
				[]byte("[metadata]\ncode_lang = \"rust\"\ntask_type = \"feature\"\napplication = \"backend\"\nis_0_to_1 = false\n\n[task]\nname = \"harbor/request-header-count-limit\"\ndescription = \"Add the requested middleware.\"\n\n[environment]\ndockerfile = \"FROM rust:1.65\"\n\n[verification]\ncommands = [\"cargo test --workspace\"]\n"),
			},
			diagnostic: "task_toml_invalid",
		},
		{
			name: "solve script", stageKey: workflowkit.StageKey(workflowadapter.SolveGen), outputName: "solve_script",
			valid: [][]byte{[]byte("#!/usr/bin/env bash\nset -euo pipefail\nexit 0\n")},
			invalid: [][]byte{
				[]byte(`{"format":"harbor.artifact.v1","version":"1","solve_script":"#!/bin/sh\\nexit 0\\n"}`),
				[]byte("set -e\nexit 0\n"), []byte("#!/bin/sh\r\nexit 0\n"),
			},
			diagnostic: "solve_script_invalid",
		},
		{
			name: "test script", stageKey: workflowkit.StageKey(workflowadapter.TestGen), outputName: "test_script",
			valid: [][]byte{[]byte("#!/usr/bin/env bash\nset -euo pipefail\nexit 0\n")},
			invalid: [][]byte{
				[]byte(`{"format":"harbor.artifact.v1","version":"1","test_script":"#!/bin/sh\\nexit 0\\n"}`),
				[]byte("#!/bin/sh\n"),
			},
			diagnostic: "test_script_invalid",
		},
		{
			name: "tests analysis", stageKey: workflowkit.StageKey(workflowadapter.TestsAnalysis), outputName: "tests_analysis",
			valid: [][]byte{[]byte(`{"provided_information":"Visible inputs","theoretical_path":"Implement and test","passing_evidence":"Assertions cover the contract"}`)},
			invalid: [][]byte{
				[]byte(`{"format":"harbor.artifact.v1","provided_information":"Visible inputs","theoretical_path":"Implement and test","passing_evidence":"Assertions cover the contract"}`),
				[]byte(`{"provided_information":"first","provided_information":"second","theoretical_path":"path","passing_evidence":"evidence"}`),
				[]byte(`{"provided_information":"info","theoretical_path":"path","passing_evidence":"evidence"} {}`),
				[]byte(`{"provided_information":"info","theoretical_path":"path","passing_evidence":" "}`),
				[]byte(`{"provided_information":"\u0000","theoretical_path":"path","passing_evidence":"evidence"}`),
			},
			diagnostic: "tests_analysis_invalid",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			stage := standardAuthoringCodexTestArtifactStage(1, testCase.stageKey, testCase.outputName)
			for index, content := range testCase.valid {
				request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
				submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 1, func() time.Time { return now }, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := submission.beginTurn(1); err != nil {
					t.Fatal(err)
				}
				response, err := submission.dynamicTool().Handler(context.Background(), standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, content))
				if err != nil {
					t.Fatal(err)
				}
				receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
				if !receipt.Accepted || len(receipt.Errors) != 0 {
					t.Fatalf("valid candidate %d receipt = %+v", index, receipt)
				}
			}
			for index, content := range testCase.invalid {
				request, _, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
				submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 1, func() time.Time { return now }, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := submission.beginTurn(1); err != nil {
					t.Fatal(err)
				}
				response, err := submission.dynamicTool().Handler(context.Background(), standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, content))
				if err != nil {
					t.Fatal(err)
				}
				receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
				if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != testCase.diagnostic {
					t.Fatalf("invalid candidate %d receipt = %+v, want %q", index, receipt, testCase.diagnostic)
				}
				if _, accepted := submission.acceptedResult(); accepted || len(*usages) != 1 {
					t.Fatalf("invalid candidate %d accepted=%t usages=%+v", index, accepted, *usages)
				}
			}
		})
	}
}

func TestStandardAuthoringCodexOutputSubmissionAllowsDiagnosticContentForNonPassVerdicts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name              string
		stage             workflowkit.StageDescriptor
		content           []byte
		environmentPolicy *workflowadapter.StandardAuthoringEnvironmentPolicy
	}{
		{
			name:    "task TOML needs repair",
			stage:   standardAuthoringCodexTestArtifactStage(1, workflowkit.StageKey(workflowadapter.TaskTOMLGen), "task_toml"),
			content: []byte("unable to produce valid TOML because the approved design is inconsistent"),
		},
		{
			name:    "Dockerfile needs repair",
			stage:   standardAuthoringCodexTestDockerfileStage(1),
			content: []byte("the frozen environment policy conflicts with the approved task design"),
			environmentPolicy: func() *workflowadapter.StandardAuthoringEnvironmentPolicy {
				policy := standardAuthoringCodexTestEnvironmentPolicy(t)
				return &policy
			}(),
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request, _, _ := standardAuthoringCodexTestRequest(testCase.stage, []byte("frozen input"), now)
			submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 1, func() time.Time { return now }, testCase.environmentPolicy)
			if err != nil {
				t.Fatal(err)
			}
			if err := submission.beginTurn(1); err != nil {
				t.Fatal(err)
			}
			response, err := submission.dynamicTool().Handler(context.Background(), standardAuthoringCodexTestCandidate(t, workflowkit.VerdictNeedsRepair, testCase.content))
			if err != nil {
				t.Fatal(err)
			}
			receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
			if !receipt.Accepted || len(receipt.Errors) != 0 {
				t.Fatalf("needs-repair receipt = %+v", receipt)
			}
			accepted, found := submission.acceptedResult()
			if !found || accepted.Outcome.Verdict != workflowkit.VerdictNeedsRepair || len(accepted.Artifacts) != 1 || string(accepted.Artifacts[0].Content) != string(testCase.content) {
				t.Fatalf("accepted needs-repair result = %+v", accepted)
			}
		})
	}
}

func TestStandardAuthoringCodexSubmitToolDescriptionsMatchPayloadKind(t *testing.T) {
	raw := standardAuthoringCodexSubmitToolDescription(workflowkit.StageKey(workflowadapter.TaskTOMLGen))
	analysis := standardAuthoringCodexSubmitToolDescription(workflowkit.StageKey(workflowadapter.TestsAnalysis))
	structured := standardAuthoringCodexSubmitToolDescription(workflowkit.StageKey(workflowadapter.TaskDesign))
	if !strings.Contains(raw, "final raw file bytes") || !strings.Contains(raw, "never an extra JSON object") {
		t.Fatalf("raw-file tool description = %q", raw)
	}
	if !strings.Contains(analysis, "exactly one JSON object") || !strings.Contains(analysis, "provided_information") {
		t.Fatalf("tests-analysis tool description = %q", analysis)
	}
	if strings.Contains(structured, "final raw file bytes") || strings.Contains(structured, "exactly one JSON object") {
		t.Fatalf("generic structured-stage tool description = %q", structured)
	}
}

func TestStandardAuthoringCodexOutputSubmissionEnforcesLimitsAndCancellation(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	valid := standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("accepted artifact"))
	invalid := standardAuthoringCodexTestCandidate(t, workflowkit.Verdict("not-allowed"), []byte("rejected artifact"))

	t.Run("submission attempts are bounded independently from agent turns", func(t *testing.T) {
		stage := standardAuthoringCodexTestStage(1)
		request, _, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
		submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 2, func() time.Time { return now }, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := submission.beginTurn(1); err != nil {
			t.Fatal(err)
		}
		for index, raw := range []json.RawMessage{invalid, invalid, valid} {
			response, err := submission.dynamicTool().Handler(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
			if index == 2 {
				if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "submit_attempts_exhausted" || receipt.Remaining != 0 {
					t.Fatalf("exhausted receipt = %+v", receipt)
				}
			}
		}
		if len(*usages) != 2 {
			t.Fatalf("usage records = %+v, want two charged output submissions", *usages)
		}
		if _, accepted := submission.acceptedResult(); accepted {
			t.Fatal("candidate was accepted after submission attempts were exhausted")
		}
	})

	t.Run("byte limit rejects after charging the bounded submission attempt", func(t *testing.T) {
		stage := standardAuthoringCodexTestStage(1)
		request, _, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
		submission, err := newStandardAuthoringCodexOutputSubmission(request, len(valid)-1, 2, func() time.Time { return now }, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := submission.beginTurn(1); err != nil {
			t.Fatal(err)
		}
		response, err := submission.dynamicTool().Handler(context.Background(), valid)
		if err != nil {
			t.Fatal(err)
		}
		receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
		if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "byte_limit_exceeded" {
			t.Fatalf("byte limit receipt = %+v", receipt)
		}
		if len(*usages) != 1 || (*usages)[0].Dimension != standardAuthoringCodexOutputSubmissionQuotaDimension {
			t.Fatalf("byte-limited candidate usage = %+v, want one charged output submission", *usages)
		}
	})

	t.Run("quota failure disables future submissions", func(t *testing.T) {
		stage := standardAuthoringCodexTestStage(1)
		request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
		request.Charge = func(context.Context, workflowkit.StageUsage) error { return store.ErrQuotaExhausted }
		submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 2, func() time.Time { return now }, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := submission.beginTurn(1); err != nil {
			t.Fatal(err)
		}
		response, err := submission.dynamicTool().Handler(context.Background(), valid)
		if err != nil {
			t.Fatal(err)
		}
		receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
		if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "submission_quota_exhausted" || submission.failure() != standardAuthoringCodexSubmissionFailureQuota {
			t.Fatalf("quota receipt = %+v failure=%q", receipt, submission.failure())
		}
		response, err = submission.dynamicTool().Handler(context.Background(), valid)
		if err != nil {
			t.Fatal(err)
		}
		receipt = standardAuthoringCodexTestSubmissionReceipt(t, response)
		if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "submission_unavailable" {
			t.Fatalf("post-quota receipt = %+v", receipt)
		}
	})

	t.Run("expired lease is not reported as quota exhaustion", func(t *testing.T) {
		stage := standardAuthoringCodexTestStage(1)
		request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
		request.Charge = func(context.Context, workflowkit.StageUsage) error { return store.ErrQuotaLeaseExpired }
		submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 2, func() time.Time { return now }, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := submission.beginTurn(1); err != nil {
			t.Fatal(err)
		}
		response, err := submission.dynamicTool().Handler(context.Background(), valid)
		if err != nil {
			t.Fatal(err)
		}
		receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
		if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "submission_lease_lost" || submission.failure() != standardAuthoringCodexSubmissionFailureLease {
			t.Fatalf("expired lease receipt = %+v failure=%q", receipt, submission.failure())
		}
	})

	t.Run("expired context cannot accept after quota charge", func(t *testing.T) {
		stage := standardAuthoringCodexTestStage(1)
		request, _, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
		originalCharge := request.Charge
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request.Charge = func(chargeCtx context.Context, usage workflowkit.StageUsage) error {
			if err := originalCharge(chargeCtx, usage); err != nil {
				return err
			}
			cancel()
			return nil
		}
		submission, err := newStandardAuthoringCodexOutputSubmission(request, 64*1024, 2, func() time.Time { return now }, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := submission.beginTurn(1); err != nil {
			t.Fatal(err)
		}
		response, err := submission.dynamicTool().Handler(ctx, valid)
		if err != nil {
			t.Fatal(err)
		}
		receipt := standardAuthoringCodexTestSubmissionReceipt(t, response)
		if receipt.Accepted || len(receipt.Errors) != 1 || receipt.Errors[0] != "submission_timeout" {
			t.Fatalf("timeout receipt = %+v", receipt)
		}
		if _, accepted := submission.acceptedResult(); accepted {
			t.Fatal("timed-out submission accepted an artifact")
		}
		if len(*usages) != 1 || (*usages)[0].Dimension != standardAuthoringCodexOutputSubmissionQuotaDimension {
			t.Fatalf("timeout usage = %+v, want completed pre-timeout quota charge", *usages)
		}
	})
}

func TestStandardAuthoringCodexAgentTurnExecutorValidateAndSubmitUsesOnlyFirstAcceptedArtifact(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestStage(1)
	rejected := standardAuthoringCodexTestCandidate(t, workflowkit.Verdict("not-allowed"), []byte("rejected candidate"))
	accepted := standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("accepted artifact A"))
	overwrite := standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("candidate B must not win"))
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results: []agent.TurnResult{{
			Model: CodexAppServerProductionModelID,
			Text:  `{"verdict":"needs_repair","artifacts":[{"content_base64":"` + base64.StdEncoding.EncodeToString([]byte("free text B must not win")) + `"}]}`,
		}},
		submissions: [][]json.RawMessage{{rejected, accepted, overwrite}},
	}}
	executor, program := standardAuthoringCodexTestExecutor(t, runtime, now, 1)
	request, _, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(len(program.TurnPrompts))},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass || len(result.Artifacts) != 1 || string(result.Artifacts[0].Content) != "accepted artifact A" || result.Artifacts[0].TurnOrdinal != 1 {
		t.Fatalf("result = %+v, want only accepted artifact A", result)
	}
	if len(runtime.conversation.submissionErrors) != 3 {
		t.Fatalf("tool calls = %d, want invalid, accepted, and post-accept calls", len(runtime.conversation.submissionErrors))
	}
	for _, toolErr := range runtime.conversation.submissionErrors {
		if toolErr != nil {
			t.Fatalf("dynamic tool returned error: %v", toolErr)
		}
	}
	if len(runtime.conversation.submissionResponses) != 3 {
		t.Fatalf("tool responses = %d, want three", len(runtime.conversation.submissionResponses))
	}
	first := standardAuthoringCodexTestSubmissionReceipt(t, runtime.conversation.submissionResponses[0])
	second := standardAuthoringCodexTestSubmissionReceipt(t, runtime.conversation.submissionResponses[1])
	third := standardAuthoringCodexTestSubmissionReceipt(t, runtime.conversation.submissionResponses[2])
	if first.Accepted || len(first.Errors) != 1 || first.Errors[0] != "wrong_verdict" || second.Accepted == false || len(second.Errors) != 0 || third.Accepted || len(third.Errors) != 1 || third.Errors[0] != "already_accepted" {
		t.Fatalf("tool receipts = first:%+v second:%+v third:%+v", first, second, third)
	}
	if standardAuthoringCodexTestUsageCount(*usages, "agent_turn") != 1 || standardAuthoringCodexTestUsageCount(*usages, standardAuthoringCodexOutputSubmissionQuotaDimension) != 2 {
		t.Fatalf("usage records = %+v, want one agent turn and two output submissions", *usages)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorFailsWithoutToolSubmission(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestStage(1)
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results: []agent.TurnResult{{
			Model: CodexAppServerProductionModelID,
			Text:  string(standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("free text must not become an artifact"))),
		}},
	}}
	executor, program := standardAuthoringCodexTestExecutor(t, runtime, now, 1)
	request, _, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(len(program.TurnPrompts))},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusInfraFailed || result.Outcome.Failure != workflowkit.FailureProcess || result.ErrorText != standardAuthoringCodexSubmissionFailureAbsent || len(result.Artifacts) != 0 {
		t.Fatalf("free-text result = %+v, want missing output submission failure", result)
	}
	if standardAuthoringCodexTestUsageCount(*usages, "agent_turn") != 1 || standardAuthoringCodexTestUsageCount(*usages, standardAuthoringCodexOutputSubmissionQuotaDimension) != 0 {
		t.Fatalf("usage records = %+v, want no output-submission charge", *usages)
	}
}

func standardAuthoringCodexTestSubmissionReceipt(t *testing.T, raw json.RawMessage) standardAuthoringCodexSubmissionReceipt {
	t.Helper()
	var receipt standardAuthoringCodexSubmissionReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("decode submission receipt: %v", err)
	}
	return receipt
}

func standardAuthoringCodexTestUsageCount(usages []workflowkit.StageUsage, dimension string) int {
	count := 0
	for _, usage := range usages {
		if usage.Dimension == dimension {
			count++
		}
	}
	return count
}
