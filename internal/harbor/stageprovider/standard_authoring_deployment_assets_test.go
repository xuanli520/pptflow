package stageprovider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringDeploymentCatalogAndAssetsAreExactAndLoadable(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	deploymentRoot := filepath.Join(root, "deployments", "standard-authoring-1.8")
	catalogRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "operation-catalog.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalogDocument, err := ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		t.Fatalf("parse Standard authoring catalog: %v", err)
	}
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatalf("resolve Standard authoring catalog: %v", err)
	}
	if !catalog.Template().Equal(workflowadapter.StandardAuthoringCurrentTemplateReference()) {
		t.Fatalf("catalog template = %s@%s, want Standard authoring", catalog.Template().ID, catalog.Template().Version)
	}
	profileRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "execution-profile.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := workflowadapter.ParseExecutionProfileJSON(profileRaw)
	if err != nil {
		t.Fatalf("parse Standard authoring execution profile: %v", err)
	}
	compiled, err := workflowadapter.StandardAuthoringCurrentWorkflowTemplate().Compile(profile)
	if err != nil {
		t.Fatalf("compile Standard authoring execution profile: %v", err)
	}

	manifestRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
	if err != nil {
		t.Fatalf("parse Standard authoring asset manifest: %v", err)
	}
	if len(catalog.Catalog().Operations) != len(manifest.Operations) || len(manifest.Operations) != len(workflowadapter.StandardAuthoringFixedFileStageOrder()) {
		t.Fatalf("catalog/manifest operation coverage = %d/%d, want %d", len(catalog.Catalog().Operations), len(manifest.Operations), len(workflowadapter.StandardAuthoringFixedFileStageOrder()))
	}

	byStage := make(map[string]StandardAuthoringContractAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		byStage[string(entry.StageKey)] = entry
		for _, asset := range []StandardAuthoringContractAssetReference{entry.Prompt, entry.Schema} {
			assetPath := filepath.Join(deploymentRoot, filepath.FromSlash(asset.RelativePath))
			info, err := os.Lstat(assetPath)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				t.Fatalf("asset %q is not a regular non-symlink file: info=%v error=%v", asset.RelativePath, info, err)
			}
		}
	}

	agentStages := 0
	for _, registration := range catalog.Catalog().Operations {
		entry, found := byStage[string(registration.Stage.Key)]
		if !found {
			t.Fatalf("catalog stage %q has no typed asset entry", registration.Stage.Key)
		}
		payload, isAgentTurn := registration.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
		if !isAgentTurn {
			continue
		}
		agentStages++
		if !IsCodexAppServerProductionPayload(payload) {
			t.Fatalf("agent stage %q payload = %+v, want frozen %s/%s", registration.Stage.Key, payload, CodexAppServerProductionModelID, CodexAppServerProductionReasoningEffort)
		}
		stage, found := compiled.Descriptor.Stage(registration.Stage.Key)
		if !found {
			t.Fatalf("compiled descriptor omits agent stage %q", registration.Stage.Key)
		}
		claimedTurns, hasClaim := standardAuthoringLockedAgentTurnClaim(stage)
		if !hasClaim || payload.MaxTurns != stage.Budget.MaxTurns || int64(payload.MaxTurns) != claimedTurns {
			t.Fatalf("agent stage %q turn contract payload=%d budget=%d quota=%d present=%t", registration.Stage.Key, payload.MaxTurns, stage.Budget.MaxTurns, claimedTurns, hasClaim)
		}
		if registration.Stage.Key == workflowkit.StageKey(workflowadapter.TaskDesign) && payload.MaxTurns != workflowadapter.StandardAuthoringTaskDesignMaxTurns {
			t.Fatalf("task_design max turns = %d, want %d", payload.MaxTurns, workflowadapter.StandardAuthoringTaskDesignMaxTurns)
		}
		promptRaw, err := os.ReadFile(filepath.Join(deploymentRoot, filepath.FromSlash(entry.Prompt.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		program, err := ParseStandardAuthoringCodexTurnProgramAsset(promptRaw)
		if err != nil {
			t.Fatalf("parse canonical prompt program for %q: %v", registration.Stage.Key, err)
		}
		if program.ID != entry.Prompt.ID || program.Version != entry.Prompt.Version || len(program.TurnPrompts) != payload.MaxTurns {
			t.Fatalf("prompt program for %q does not match frozen manifest/payload", registration.Stage.Key)
		}
		usesFixedFileSchema := standardAuthoringCodexUsesFixedFileOutputSchema(catalog.Template(), registration.Stage.Key)
		if usesFixedFileSchema {
			if entry.Schema.ID != "standard-authoring.codex-fixed-file-submit-schema" || entry.Schema.Version != "1.0.0" || entry.Schema.RelativePath != "schemas/codex-fixed-file-submit.schema.json" {
				t.Fatalf("fixed-file agent stage %q schema = %q@%q path %q", registration.Stage.Key, entry.Schema.ID, entry.Schema.Version, entry.Schema.RelativePath)
			}
		} else if entry.Schema.ID != "standard-authoring.codex-stage-output-schema" || entry.Schema.Version != "1.0.0" || entry.Schema.RelativePath != "schemas/codex-stage-output.schema.json" {
			t.Fatalf("agent stage %q has non-Codex schema asset %q@%q", registration.Stage.Key, entry.Schema.ID, entry.Schema.Version)
		}
		schemaRaw, err := os.ReadFile(filepath.Join(deploymentRoot, filepath.FromSlash(entry.Schema.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(catalog.Template(), registration.Stage.Key, schemaRaw); err != nil {
			t.Fatalf("validate canonical Codex schema asset for %q: %v", registration.Stage.Key, err)
		}
	}
	if agentStages != 11 {
		t.Fatalf("catalog Codex agent stages = %d, want 11", agentStages)
	}
}

func TestStandardAuthoringHarnessV17DeploymentAssetsRemainFrozen(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	deploymentRoot := filepath.Join(root, "deployments", "standard-authoring")
	catalogRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "operation-catalog.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalogDocument, err := ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		t.Fatalf("parse frozen 1.7 catalog: %v", err)
	}
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatalf("resolve frozen 1.7 catalog: %v", err)
	}
	if !catalog.Template().Equal(workflowadapter.StandardAuthoringHarnessTemplateReference()) {
		t.Fatalf("frozen catalog template = %s@%s, want 1.7.0", catalog.Template().ID, catalog.Template().Version)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
	if err != nil {
		t.Fatalf("parse frozen 1.7 manifest: %v", err)
	}
	if !manifest.Template.Equal(workflowadapter.StandardAuthoringHarnessTemplateReference()) || len(manifest.Operations) != len(workflowadapter.StandardAuthoringHarnessStageOrder()) {
		t.Fatalf("frozen manifest = template:%+v operations:%d", manifest.Template, len(manifest.Operations))
	}
	byStage := make(map[workflowkit.StageKey]StandardAuthoringContractAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		byStage[entry.StageKey] = entry
	}
	for _, registration := range catalog.Catalog().Operations {
		if _, isAgentTurn := registration.Operation.Payload.(workflowadapter.AgentTurnOperationPayload); !isAgentTurn {
			continue
		}
		entry, found := byStage[registration.Stage.Key]
		if !found {
			t.Fatalf("frozen agent stage %q is absent from manifest", registration.Stage.Key)
		}
		if entry.Schema.ID != "standard-authoring.codex-stage-output-schema" || entry.Schema.Version != "1.0.0" || entry.Schema.RelativePath != "schemas/codex-stage-output.schema.json" {
			t.Fatalf("frozen agent stage %q schema = %+v", registration.Stage.Key, entry.Schema)
		}
		raw, err := os.ReadFile(filepath.Join(deploymentRoot, filepath.FromSlash(entry.Schema.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(catalog.Template(), registration.Stage.Key, raw); err != nil {
			t.Fatalf("validate frozen agent schema %q: %v", registration.Stage.Key, err)
		}
	}
}

func TestStandardAuthoringBriefPromptsFreezeScopeWithoutElevatingData(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	for _, test := range []struct {
		path    string
		version string
		extras  []string
	}{
		{path: "repo-analyze.json", version: "1.5.0", extras: []string{"exact spelling", "Cargo package names"}},
		{path: "task-design.json", version: "1.6.0", extras: []string{"Verify every repository-relative path", "character-for-character"}},
		{path: "generate-task-files.json", version: "1.4.0", extras: []string{"rechecked against the frozen source", "exact spelling exists"}},
		{path: "task-toml-generate.json", version: "1.9.0", extras: []string{"metadata.task_type", "metadata.code_lang", "metadata.is_0_to_1", "org/name", "build_timeout_sec", "network_mode", "[verifier]", "Never emit [verification]", "false"}},
	} {
		raw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "prompts", test.path))
		if err != nil {
			t.Fatal(err)
		}
		program, err := ParseStandardAuthoringCodexTurnProgramAsset(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", test.path, err)
		}
		joined := strings.Join(program.TurnPrompts, "\n")
		for _, required := range []string{"authoring_brief", "task_type", "application", "objective", "system", "instructions"} {
			if !strings.Contains(joined, required) {
				t.Fatalf("prompt %s omits frozen brief boundary %q", test.path, required)
			}
		}
		if program.Version != test.version {
			t.Fatalf("prompt %s version = %s, want %s", test.path, program.Version, test.version)
		}
		for _, extra := range test.extras {
			if !strings.Contains(joined, extra) {
				t.Fatalf("prompt %s omits required metadata rule %q", test.path, extra)
			}
		}
	}
}

func TestStandardAuthoringTaskTOMLPromptsUseHarborTaskConfigContract(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	for _, deployment := range []string{"standard-authoring", "standard-authoring-1.8"} {
		t.Run(deployment, func(t *testing.T) {
			deploymentRoot := filepath.Join(root, "deployments", deployment)
			raw, err := os.ReadFile(filepath.Join(deploymentRoot, "prompts", "task-toml-generate.json"))
			if err != nil {
				t.Fatal(err)
			}
			program, err := ParseStandardAuthoringCodexTurnProgramAsset(raw)
			if err != nil {
				t.Fatalf("parse task TOML prompt: %v", err)
			}
			if program.Version != "1.9.0" {
				t.Fatalf("task TOML prompt version = %s, want 1.9.0", program.Version)
			}
			joined := strings.Join(program.TurnPrompts, "\n")
			for _, required := range []string{
				"complete Harbor 0.18 task.toml", "runs before Dockerfile and test generation",
				"build_timeout_sec", "network_mode = \"no-network\"", "workdir = \"/workspace/source\"",
				"[verifier]", "timeout_sec", "Never emit [verification]", "environment.dockerfile",
				"environment/Dockerfile", "tests/test.sh",
			} {
				if !strings.Contains(joined, required) {
					t.Fatalf("task TOML prompt omits Harbor contract rule %q", required)
				}
			}
			for _, stale := range []string{"[environment] with a non-empty dockerfile string", "[verification] with a non-empty commands array"} {
				if strings.Contains(joined, stale) {
					t.Fatalf("task TOML prompt retains stale factory-only contract %q", stale)
				}
			}

			manifestRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"))
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
			if err != nil {
				t.Fatalf("parse task TOML contract manifest: %v", err)
			}
			for _, entry := range manifest.Operations {
				if entry.StageKey == workflowkit.StageKey(workflowadapter.TaskTOMLGen) {
					if entry.Prompt.ID != "standard-authoring.task-toml-generate.prompt" || entry.Prompt.Version != "1.9.0" || entry.Prompt.RelativePath != "prompts/task-toml-generate.json" {
						t.Fatalf("task TOML manifest prompt = %+v", entry.Prompt)
					}
					return
				}
			}
			t.Fatal("task TOML manifest entry is missing")
		})
	}
}

func TestStandardAuthoringRawFilePromptsRequireDirectFilePayloads(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	for _, test := range []struct {
		path    string
		version string
		file    string
	}{
		{path: "instruction-generate.json", version: "1.4.0", file: "instruction.md"},
		{path: "task-toml-generate.json", version: "1.9.0", file: "task.toml"},
		{path: "dockerfile-generate.json", version: "2.0.0", file: "environment/Dockerfile"},
		{path: "solve-generate.json", version: "1.5.0", file: "solution/solve.sh"},
		{path: "test-generate.json", version: "1.6.0", file: "tests/test.sh"},
	} {
		raw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "prompts", test.path))
		if err != nil {
			t.Fatal(err)
		}
		program, err := ParseStandardAuthoringCodexTurnProgramAsset(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", test.path, err)
		}
		joined := strings.Join(program.TurnPrompts, "\n")
		for _, required := range []string{"content_base64", test.file, "bytes themselves", "not a JSON object"} {
			if !strings.Contains(joined, required) {
				t.Fatalf("prompt %s omits direct payload rule %q", test.path, required)
			}
		}
		if program.Version != test.version {
			t.Fatalf("prompt %s version = %s, want %s", test.path, program.Version, test.version)
		}
	}
}

func TestStandardAuthoringFixedFilePromptsRequireWorkspaceWriteAndPassReceipt(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	deploymentRoot := filepath.Join(root, "deployments", "standard-authoring-1.8")
	for _, test := range []struct {
		path    string
		version string
		file    string
	}{
		{path: "solve-generate.json", version: "1.6.0", file: "task/solution/solve.sh"},
		{path: "test-generate.json", version: "1.7.0", file: "task/tests/test.sh"},
	} {
		raw, err := os.ReadFile(filepath.Join(deploymentRoot, "prompts", test.path))
		if err != nil {
			t.Fatal(err)
		}
		program, err := ParseStandardAuthoringCodexTurnProgramAsset(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", test.path, err)
		}
		if program.Version != test.version || len(program.TurnPrompts) != 1 {
			t.Fatalf("fixed-file prompt %s = version %s turns %d", test.path, program.Version, len(program.TurnPrompts))
		}
		joined := strings.ToLower(program.TurnPrompts[0])
		for _, required := range []string{
			strings.ToLower(test.file), "harbor_submit_stage_output", "sole verdict field set to pass",
			"model never submits artifact bytes", "final action", "do not encode the script as base64", "do not finish with prose alone",
		} {
			if !strings.Contains(joined, required) {
				t.Fatalf("fixed-file prompt %s omits %q", test.path, required)
			}
		}
		for _, forbidden := range []string{"sole artifact content_base64", "strictly decodable as standard base64", "bytes themselves"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("fixed-file prompt %s retains base64 output contract %q", test.path, forbidden)
			}
		}
	}
}

func TestStandardAuthoringPhase1PromptsMatchControlledRuntime(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	for _, test := range []struct {
		path     string
		version  string
		required []string
	}{
		{
			path:    "dockerfile-generate.json",
			version: "2.0.0",
			required: []string{
				"build context is exactly task/environment",
				"Never COPY or ADD tests/",
				"solution/",
				"exactly /workspace/source",
				"strictly decodable as standard Base64",
			},
		},
		{
			path:    "solve-generate.json",
			version: "1.5.0",
			required: []string{
				"currently bound validated_dockerfile",
				"sh ./solution/solve.sh",
				"working directory /oracle",
				"root filesystem is read-only",
				"/oracle bind mount",
				"/tmp tmpfs",
				"/workspace/source",
				"/oracle/worktree",
				"portable POSIX sh",
				"Bash-only syntax",
				"strictly decodable as standard Base64",
			},
		},
		{
			path:    "test-generate.json",
			version: "1.6.0",
			required: []string{
				"currently bound validated_dockerfile",
				"sh ./tests/test.sh",
				"again after sh ./solution/solve.sh",
				"working directory /oracle",
				"root filesystem is read-only",
				"/oracle bind mount",
				"/tmp tmpfs",
				"reuse /oracle/worktree",
				"fresh recursive copy of /workspace/source",
				"portable POSIX sh",
				"Bash-only syntax",
				"strictly decodable as standard Base64",
			},
		},
	} {
		raw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "prompts", test.path))
		if err != nil {
			t.Fatal(err)
		}
		program, err := ParseStandardAuthoringCodexTurnProgramAsset(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", test.path, err)
		}
		if program.Version != test.version {
			t.Fatalf("prompt %s version = %s, want %s", test.path, program.Version, test.version)
		}
		joined := strings.Join(program.TurnPrompts, "\n")
		for _, required := range test.required {
			if !strings.Contains(joined, required) {
				t.Fatalf("prompt %s omits controlled runtime rule %q", test.path, required)
			}
		}
	}
}

func TestStandardAuthoringAgentPromptsBindAttemptWorkspace(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	for _, name := range []string{
		"repo-analyze.json", "task-design.json", "generate-task-files.json",
		"instruction-generate.json", "task-toml-generate.json", "dockerfile-generate.json",
		"dockerfile-build-validate.json", "solve-generate.json", "test-generate.json",
		"authoring-harness.json", "tests-analysis.json",
	} {
		raw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "prompts", name))
		if err != nil {
			t.Fatal(err)
		}
		program, err := ParseStandardAuthoringCodexTurnProgramAsset(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		joined := strings.Join(program.TurnPrompts, "\n")
		for _, required := range []string{
			"current working directory is the disposable attempt `work` root",
			"under `source/`",
			"never edit",
		} {
			if !strings.Contains(joined, required) {
				t.Fatalf("prompt %s omits attempt-workspace rule %q", name, required)
			}
		}
	}
}

func TestStandardAuthoringHarnessPromptsRequireInTurnEditValidateLoop(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	for _, test := range []struct {
		path     string
		required []string
	}{
		{
			path: "dockerfile-build-validate.json",
			required: []string{
				"task/environment/dockerfile", "host-owned locked docker build",
				"stdout_tail", "do not run docker directly", "do not return base64",
			},
		},
		{
			path: "authoring-harness.json",
			required: []string{
				"task/environment/dockerfile", "task/solution/solve.sh", "task/tests/test.sh",
				"initial verification", "oracle verification", "validated_dockerfile_changed",
			},
		},
	} {
		raw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "prompts", test.path))
		if err != nil {
			t.Fatal(err)
		}
		program, err := ParseStandardAuthoringCodexTurnProgramAsset(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", test.path, err)
		}
		if program.Version != "1.0.0" || len(program.TurnPrompts) != 1 {
			t.Fatalf("harness prompt %s = version %s turns %d, want 1.0.0/1", test.path, program.Version, len(program.TurnPrompts))
		}
		joined := strings.ToLower(program.TurnPrompts[0])
		for _, required := range append(test.required,
			"harbor_submit_stage_output", "sole `verdict` field set to `pass`",
			"accepted=false", "remaining", "same turn", "accepted=true",
			"do not finish with prose",
		) {
			if !strings.Contains(joined, required) {
				t.Fatalf("harness prompt %s omits ReAct rule %q", test.path, required)
			}
		}
		if strings.Contains(joined, "sole artifact content_base64") {
			t.Fatalf("harness prompt %s still asks the model to submit artifact bytes", test.path)
		}
	}
}

func TestStandardAuthoringTestsAnalysisReevaluatesFeedbackAgainstCurrentScript(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "prompts", "tests-analysis.json"))
	if err != nil {
		t.Fatal(err)
	}
	program, err := ParseStandardAuthoringCodexTurnProgramAsset(raw)
	if err != nil {
		t.Fatal(err)
	}
	if program.Version != "1.6.0" {
		t.Fatalf("tests-analysis prompt version = %s, want 1.6.0", program.Version)
	}
	joined := strings.Join(program.TurnPrompts, "\n")
	for _, required := range []string{
		"currently bound validated_test_script",
		"exact current tests/test.sh bytes",
		"authoring_harness_report",
		"initial verification failed",
		"Oracle verification passed",
		"current validated_test_script has fixed is resolved",
		"verdict=pass",
		"Do not submit needs_repair, reject, or advisory",
		"downstream solution_review",
		"current validated_test_script defect",
		"normal tool or executor failure path",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("tests-analysis prompt omits current-script rule %q", required)
		}
	}
	if strings.Contains(joined, "do not discard an earlier unresolved requirement") {
		t.Fatal("tests-analysis prompt still treats historical feedback as automatically unresolved")
	}
}

func TestStandardAuthoringRepairPromptsConsumeOnlyFrozenFeedback(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	for _, test := range []struct {
		path     string
		version  string
		feedback []string
	}{
		{path: "repo-analyze.json", version: "1.5.0", feedback: []string{"task_review_decision"}},
		{path: "task-design.json", version: "1.6.0", feedback: []string{"task_review_decision"}},
		{path: "instruction-generate.json", version: "1.4.0", feedback: []string{"content_review_decision", "solution_review_decision", "codeedge_package_admission_report"}},
		{path: "task-toml-generate.json", version: "1.9.0", feedback: []string{"content_review_decision", "solution_review_decision", "codeedge_package_admission_report"}},
		{path: "dockerfile-generate.json", version: "2.0.0", feedback: []string{"content_review_decision", "solution_review_decision", "codeedge_package_admission_report"}},
		{path: "solve-generate.json", version: "1.5.0", feedback: []string{"solution_review_decision", "codeedge_package_admission_report"}},
		{path: "test-generate.json", version: "1.6.0", feedback: []string{"solution_review_decision", "codeedge_package_admission_report"}},
		{path: "tests-analysis.json", version: "1.6.0", feedback: []string{"solution_review_decision", "codeedge_package_admission_report"}},
	} {
		raw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "prompts", test.path))
		if err != nil {
			t.Fatal(err)
		}
		program, err := ParseStandardAuthoringCodexTurnProgramAsset(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", test.path, err)
		}
		joined := strings.Join(program.TurnPrompts, "\n")
		if program.Version != test.version {
			t.Fatalf("prompt %s version = %s, want %s", test.path, program.Version, test.version)
		}
		for _, required := range append(test.feedback, "content_base64", "request_changes", "untrusted task data", "output schema") {
			if !strings.Contains(joined, required) {
				t.Fatalf("repair prompt %s omits %q", test.path, required)
			}
		}
	}
}

func TestStandardAuthoringContractAssetManifestRejectsMissingOrUnknownStage(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseStandardAuthoringContractAssetManifestJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	missing := manifest.Clone()
	missing.Operations = missing.Operations[:len(missing.Operations)-1]
	if err := missing.Validate(); err == nil {
		t.Fatal("manifest with a missing Standard authoring stage was accepted")
	}
	unknown := manifest.Clone()
	unknown.Operations[0].StageKey = "unknown_stage"
	if err := unknown.Validate(); err == nil {
		t.Fatal("manifest with an unknown Standard authoring stage was accepted")
	}
}

func TestStandardAuthoringSSHKnownHostsAssetRequiresExplicitPreNetworkHosts(t *testing.T) {
	knownHosts := []byte("github.com ssh-ed25519 AQID\n[git.example]:2222 ssh-ed25519 BAUG\n")
	if err := ValidateStandardAuthoringSSHKnownHostsAsset(knownHosts); err != nil {
		t.Fatalf("validate explicit known_hosts allow-list: %v", err)
	}
	for _, test := range []struct {
		host string
		port string
		want bool
	}{
		{host: "github.com", want: true},
		{host: "github.com", port: "22", want: true},
		{host: "github.com", port: "2222", want: false},
		{host: "git.example", port: "2222", want: true},
		{host: "git.example", want: false},
		{host: "unlisted.example", want: false},
	} {
		got, err := StandardAuthoringSSHKnownHostsAllowsHost(knownHosts, test.host, test.port)
		if err != nil || got != test.want {
			t.Fatalf("allow host %q:%q = %t, %v; want %t", test.host, test.port, got, err, test.want)
		}
	}
	for _, raw := range [][]byte{
		[]byte("*.example ssh-ed25519 AQID\n"),
		[]byte("|1|salt|hash ssh-ed25519 AQID\n"),
		[]byte("@cert-authority example ssh-ed25519 AQID\n"),
		[]byte("github.com ssh-ed25519 not-base64\n"),
	} {
		if err := ValidateStandardAuthoringSSHKnownHostsAsset(raw); err == nil {
			t.Fatalf("non-explicit known_hosts input was accepted: %q", raw)
		}
	}
}

func TestLoadStandardAuthoringDeploymentAssetBundleStrictlyBindsGeneratedLockAndAssets(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	deploymentRoot := filepath.Join(t.TempDir(), "deployments", "standard-authoring")
	if err := os.MkdirAll(deploymentRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"operation-catalog.v1.json", "contract-assets.v1.json", filepath.Join("ssh", "known_hosts")} {
		contents, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", name))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(deploymentRoot, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifestRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Operations {
		for _, asset := range []StandardAuthoringContractAssetReference{entry.Prompt, entry.Schema} {
			source := filepath.Join(root, "deployments", "standard-authoring", filepath.FromSlash(asset.RelativePath))
			contents, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(deploymentRoot, filepath.FromSlash(asset.RelativePath))
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, contents, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	catalogRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "operation-catalog.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalogDocument, err := ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	lock := standardAuthoringDeploymentTestLock(t, catalog, manifest, deploymentRoot)
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(deploymentRoot, "operation-catalog.lock.json")
	if err := os.WriteFile(lockPath, canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := LoadStandardAuthoringDeploymentAssetBundle(filepath.Join(deploymentRoot, "operation-catalog.v1.json"), lockPath, deploymentRoot)
	if err != nil {
		t.Fatalf("load exact generated deployment bundle: %v", err)
	}
	expectedFingerprint, err := lock.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Verifier.LockIdentity().Fingerprint != expectedFingerprint || len(bundle.Lock.Operations) != len(manifest.Operations) {
		t.Fatalf("loaded deployment bundle did not retain the exact static lock")
	}
	mismatchedManifest := manifest.Clone()
	mismatchedManifest.Template = workflowadapter.StandardAuthoringBriefTemplateReference()
	mismatchedRaw, err := json.Marshal(mismatchedManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"), mismatchedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStandardAuthoringDeploymentAssetBundle(filepath.Join(deploymentRoot, "operation-catalog.v1.json"), lockPath, deploymentRoot); err == nil {
		t.Fatal("catalog/manifest template mismatch was accepted")
	}
	if err := os.WriteFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"), manifestRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	entry := manifest.Operations[0]
	promptPath := filepath.Join(deploymentRoot, filepath.FromSlash(entry.Prompt.RelativePath))
	if err := os.WriteFile(promptPath, []byte("asset drift"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStandardAuthoringDeploymentAssetBundle(filepath.Join(deploymentRoot, "operation-catalog.v1.json"), lockPath, deploymentRoot); err == nil {
		t.Fatal("asset-drifted generated deployment bundle was accepted")
	}
}

func TestLoadStandardAuthoringDeploymentAssetBundleFailsClosedWithoutGeneratedLock(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	deploymentRoot := filepath.Join(root, "deployments", "standard-authoring")
	missing := filepath.Join(t.TempDir(), "operation-catalog.lock.json")
	if _, err := LoadStandardAuthoringDeploymentAssetBundle(filepath.Join(deploymentRoot, "operation-catalog.v1.json"), missing, deploymentRoot); err == nil {
		t.Fatal("missing generated Standard authoring lock was accepted")
	}
}

func TestStandardAuthoringV18PromptsRequireRuntimeSourceAccess(t *testing.T) {
	root := filepath.Join(standardAuthoringDeploymentRepositoryRoot(t), "deployments", "standard-authoring-1.8", "prompts")
	for _, test := range []struct {
		path     string
		version  string
		required []string
	}{
		{
			path: "dockerfile-generate.json", version: "2.1.0",
			required: []string{"chmod -R a+rX /workspace/source", "cp -R /workspace/source/. /oracle/worktree/", "drops all capabilities"},
		},
		{
			path: "dockerfile-build-validate.json", version: "1.1.0",
			required: []string{"read-only root", "all capabilities dropped", "cp -R /workspace/source/. /oracle/worktree/", "writable worktree"},
		},
		{
			path: "authoring-harness.json", version: "1.1.0",
			required: []string{"cp -R /workspace/source/. /oracle/worktree/", "Never serialize, embed, or unpack a replacement source archive"},
		},
	} {
		t.Run(test.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, test.path))
			if err != nil {
				t.Fatal(err)
			}
			program, err := ParseStandardAuthoringCodexTurnProgramAsset(raw)
			if err != nil {
				t.Fatal(err)
			}
			if program.Version != test.version {
				t.Fatalf("prompt version = %q, want %q", program.Version, test.version)
			}
			joined := strings.Join(program.TurnPrompts, "\n")
			for _, required := range test.required {
				if !strings.Contains(joined, required) {
					t.Fatalf("prompt omits runtime source contract %q", required)
				}
			}
		})
	}
}

func standardAuthoringDeploymentTestLock(t *testing.T, catalog *DeploymentOperationCatalogResolver, manifest StandardAuthoringContractAssetManifest, root string) DeploymentOperationCatalogLock {
	t.Helper()
	assets := make(map[workflowkit.StageKey]StandardAuthoringContractAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		assets[entry.StageKey] = entry
	}
	operations := make([]DeploymentOperationCatalogLockRecord, 0, len(catalog.Catalog().Operations))
	for _, registration := range catalog.Catalog().Operations {
		entry, found := assets[registration.Stage.Key]
		if !found {
			t.Fatalf("missing asset entry for %q", registration.Stage.Key)
		}
		prompt, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Prompt.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		schema, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Schema.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		record := DeploymentOperationCatalogLockRecord{
			Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
			Checkout: registration.Checkout, Secrets: append([]workflowadapter.SecretReference{}, registration.Secrets...),
			PromptContentFingerprint: workflowkit.SHA256Fingerprint(prompt), SchemaContentFingerprint: workflowkit.SHA256Fingerprint(schema), ExecutionKind: registration.Operation.Payload.Kind(),
			StandardAuthoringContract: &StandardAuthoringContractLock{Format: StandardAuthoringContractLockFormat, Version: StandardAuthoringContractLockVersion, Prompt: entry.Prompt, Schema: entry.Schema},
		}
		switch payload := registration.Operation.Payload.(type) {
		case workflowadapter.LocalCommandOperationPayload:
			local := LocalExecutableLock{CommandID: payload.CommandID, AbsolutePath: "/opt/standard-authoring/git", Version: "2.47.3", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("locked git"))}
			record.LocalExecutable = &local
		case workflowadapter.AgentTurnOperationPayload:
			codex := CodexAppServerOperationLock{
				Format: CodexAppServerOperationLockFormat, Version: CodexAppServerOperationLockVersion,
				JavaScriptLauncher: LocalExecutableLock{CommandID: CodexAppServerJavaScriptLauncherCommandID, AbsolutePath: "/opt/standard-authoring/codex.js", Version: "0.133.0", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("locked launcher"))},
				NodeExecutable:     LocalExecutableLock{CommandID: CodexAppServerNodeExecutableCommandID, AbsolutePath: "/opt/standard-authoring/node", Version: "v26.2.0", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("locked node"))},
				CodexHomeDirectory: "/opt/standard-authoring/codex-home", CLIVersionOutput: "codex-cli 0.133.0",
				SandboxMode: CodexAppServerSandboxModeReadOnly, SandboxPolicy: CodexAppServerSandboxPolicyReadOnly,
			}
			record.AgentModel = &AgentModelLock{
				AgentID: payload.AgentID, AgentVersion: "0.133.0", ModelID: payload.ModelID,
				ModelVersion: "gpt-5.6-terra",
			}
			record.CodexAppServer = &codex
		case workflowadapter.DurableReviewOperationPayload:
			record.DurableReviewPolicy = &DurableReviewPolicyLock{PolicyID: payload.PolicyID, Version: "1.0.0"}
		case workflowadapter.HarborBuiltinOperationPayload:
			record.HarborFlowBuiltin = &HarborFlowBuiltinOperationLock{Format: HarborFlowBuiltinOperationLockFormat, Version: HarborFlowBuiltinOperationLockVersion, HandlerID: payload.HandlerID, HandlerVersion: "1.0.0"}
		default:
			t.Fatalf("unsupported payload %T", payload)
		}
		operations = append(operations, record)
	}
	knownHosts, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(StandardAuthoringSSHKnownHostsRelativePath)))
	if err != nil {
		t.Fatal(err)
	}
	return DeploymentOperationCatalogLock{
		Format: DeploymentOperationCatalogLockFormat, Version: DeploymentOperationCatalogLockVersion,
		LockID: "standard-authoring-deployment-assets-test", LockVersion: "test-v1", CatalogReceipt: catalog.Receipt(),
		HarborFlowBuild:                   HarborFlowBuildIdentity{Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("test build"))},
		StandardAuthoringExecutionProfile: &StandardAuthoringExecutionProfileLock{Profile: standardAuthoringTestExecutionProfileForTemplate(t, catalog.Template())},
		StandardAuthoringSSHTransport:     standardAuthoringSSHTransportTestLock(t, knownHosts),
		Operations:                        operations,
	}
}

const standardAuthoringSSHTransportTestKnownHosts = "github.com ssh-ed25519 AQID\n"

func standardAuthoringSSHTransportTestLock(t *testing.T, knownHosts []byte) *StandardAuthoringSSHTransportLock {
	t.Helper()
	sshContent := workflowkit.SHA256Fingerprint([]byte("locked ssh"))
	shellContent := workflowkit.SHA256Fingerprint([]byte("locked shell"))
	return &StandardAuthoringSSHTransportLock{
		Format:  StandardAuthoringSSHTransportLockFormat,
		Version: StandardAuthoringSSHTransportLockVersion,
		SSHExecutable: LocalExecutableLock{
			CommandID: StandardAuthoringSSHTransportCommandID, AbsolutePath: "/opt/standard-authoring/ssh", Version: "OpenSSH_10.0p2", ContentSHA256: sshContent,
		},
		WrapperShell: LocalExecutableLock{
			CommandID: StandardAuthoringSSHWrapperShellCommandID, AbsolutePath: "/opt/standard-authoring/dash", Version: string(shellContent), ContentSHA256: shellContent,
		},
		KnownHosts:                 StandardAuthoringSSHKnownHostsLock{Format: StandardAuthoringSSHKnownHostsLockFormat, Version: StandardAuthoringSSHKnownHostsLockVersion, RelativePath: StandardAuthoringSSHKnownHostsRelativePath, ContentSHA256: workflowkit.SHA256Fingerprint(knownHosts)},
		AgentSocketEnvironmentName: StandardAuthoringSSHAgentSocketEnvironment,
	}
}

func standardAuthoringDeploymentRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate deployment asset test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
