package gen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	harborgen "github.com/purplevoid/harbor-factory/internal/harbor/gen"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	RepoAnalyzeKind       = "harborfactory.repo_analyze"
	TaskDesignKind        = "harborfactory.task_design"
	GenerateTaskFilesKind = "harborfactory.generate_task_files"
	InstructionKind       = "harborfactory.instruction_generate"
	TaskTOMLKind          = "harborfactory.task_toml_generate"
	DockerfileKind        = "harborfactory.dockerfile_generate"
	SolveKind             = "harborfactory.solve_generate"
	TestKind              = "harborfactory.test_generate"
	TestsAnalysisKind     = "harborfactory.tests_analysis"
	MaterializeKind       = "harborfactory.materialize_task"
	PublishTaskKind       = "harborfactory.publish_task"
	RuntimeSelfCheckKind  = "harborfactory.runtime_self_check"
)

type RepoAnalyzePlugin struct{}
type TaskDesignPlugin struct{}
type GenerateTaskFilesPlugin struct{}
type InstructionPlugin struct{}
type TaskTOMLPlugin struct{}
type DockerfilePlugin struct{}
type SolvePlugin struct{}
type TestPlugin struct{}
type TestsAnalysisPlugin struct{}
type MaterializePlugin struct{}
type PublishTaskPlugin struct{}

type RuntimeSelfCheckFunc func(context.Context, harborgen.ConversationOptions, string, string) error
type RuntimeSelfCheckPlugin struct{ Check RuntimeSelfCheckFunc }

func (RepoAnalyzePlugin) Manifest() workflow.PluginManifest { return manifest(RepoAnalyzeKind) }
func (TaskDesignPlugin) Manifest() workflow.PluginManifest  { return manifest(TaskDesignKind) }
func (GenerateTaskFilesPlugin) Manifest() workflow.PluginManifest {
	return manifest(GenerateTaskFilesKind)
}
func (InstructionPlugin) Manifest() workflow.PluginManifest   { return manifest(InstructionKind) }
func (TaskTOMLPlugin) Manifest() workflow.PluginManifest      { return manifest(TaskTOMLKind) }
func (DockerfilePlugin) Manifest() workflow.PluginManifest    { return manifest(DockerfileKind) }
func (SolvePlugin) Manifest() workflow.PluginManifest         { return manifest(SolveKind) }
func (TestPlugin) Manifest() workflow.PluginManifest          { return manifest(TestKind) }
func (TestsAnalysisPlugin) Manifest() workflow.PluginManifest { return manifest(TestsAnalysisKind) }
func (MaterializePlugin) Manifest() workflow.PluginManifest   { return manifest(MaterializeKind) }
func (PublishTaskPlugin) Manifest() workflow.PluginManifest   { return manifest(PublishTaskKind) }
func (RuntimeSelfCheckPlugin) Manifest() workflow.PluginManifest {
	return manifest(RuntimeSelfCheckKind)
}

func manifest(kind string) workflow.PluginManifest {
	return workflow.PluginManifest{ID: kind, Version: "1.0.0", Kinds: []string{kind}}
}

func (RepoAnalyzePlugin) Validate(spec workflow.NodeSpec) error       { return validateAgentSpec(spec) }
func (TaskDesignPlugin) Validate(spec workflow.NodeSpec) error        { return validateAgentSpec(spec) }
func (GenerateTaskFilesPlugin) Validate(spec workflow.NodeSpec) error { return validateAgentSpec(spec) }
func (InstructionPlugin) Validate(spec workflow.NodeSpec) error       { return validateSpec(spec) }
func (TaskTOMLPlugin) Validate(spec workflow.NodeSpec) error          { return validateSpec(spec) }
func (DockerfilePlugin) Validate(spec workflow.NodeSpec) error        { return validateSpec(spec) }
func (SolvePlugin) Validate(spec workflow.NodeSpec) error             { return validateSpec(spec) }
func (TestPlugin) Validate(spec workflow.NodeSpec) error              { return validateSpec(spec) }
func (TestsAnalysisPlugin) Validate(spec workflow.NodeSpec) error     { return validateSpec(spec) }
func (MaterializePlugin) Validate(spec workflow.NodeSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	return pluginutil.RequiredString(spec, "task_dir")
}
func (PublishTaskPlugin) Validate(spec workflow.NodeSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	if err := pluginutil.RequiredString(spec, "task_dir"); err != nil {
		return err
	}
	return pluginutil.RequiredString(spec, "destination_dir")
}
func (RuntimeSelfCheckPlugin) Validate(spec workflow.NodeSpec) error {
	if err := validateAgentSpec(spec); err != nil {
		return err
	}
	return pluginutil.RequiredString(spec, "task_dir")
}

func validateSpec(spec workflow.NodeSpec) error {
	if strings.TrimSpace(spec.ID) == "" {
		return fmt.Errorf("generation node id is required")
	}
	return nil
}

func validateAgentSpec(spec workflow.NodeSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	if timeout, ok := spec.Config["timeout_seconds"]; ok {
		value := pluginutil.Int(workflow.NodeRequest{Spec: spec}, "timeout_seconds")
		if value <= 0 && timeout != nil {
			return fmt.Errorf("generation node %s timeout_seconds must be positive", spec.ID)
		}
	}
	return nil
}

func (RepoAnalyzePlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	prepared, err := readJSONArtifact[domain.RepoPrepared](ctx, req, "repo_prepared", "repo_prepared_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	analysis, err := harborgen.GenerateRepoAnalysis(ctx, conversationOptions(req), prepared)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return putJSON(ctx, req, "phase1/artifacts/repo_analyze/repo_analysis.json", "repo_analysis", analysis)
}

func (TaskDesignPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	prepared, err := readJSONArtifact[domain.RepoPrepared](ctx, req, "repo_prepared", "repo_prepared_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	analysis, err := readJSONArtifact[domain.RepoAnalysis](ctx, req, "repo_analysis", "repo_analysis_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	proposal, err := harborgen.GenerateTaskProposal(ctx, conversationOptions(req), prepared, analysis)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return putJSON(ctx, req, "phase1/artifacts/task_design/task_proposal.json", "task_proposal", proposal)
}

func (GenerateTaskFilesPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	prepared, err := readJSONArtifact[domain.RepoPrepared](ctx, req, "repo_prepared", "repo_prepared_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	analysis, err := readJSONArtifact[domain.RepoAnalysis](ctx, req, "repo_analysis", "repo_analysis_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	proposal, err := readJSONArtifact[domain.TaskProposal](ctx, req, "task_proposal", "task_proposal_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	files, err := harborgen.GenerateTaskFiles(ctx, conversationOptions(req), prepared, analysis, proposal)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return putJSON(ctx, req, "phase1/artifacts/generate_task_files/task_files.json", "generated_task_files", files)
}

func (InstructionPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	files, err := readJSONArtifact[domain.GeneratedTaskFiles](ctx, req, "generated_task_files", "task_files_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return putText(ctx, req, "phase1/artifacts/instruction_generate/instruction.md", "instruction", harborgen.Instruction(files))
}

func (TaskTOMLPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	proposal, err := readJSONArtifact[domain.TaskProposal](ctx, req, "task_proposal", "task_proposal_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return putText(ctx, req, "phase1/artifacts/task_toml_generate/task.toml", "task_toml", harborgen.TaskTOML(proposal))
}

func (DockerfilePlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	prepared, err := readJSONArtifact[domain.RepoPrepared](ctx, req, "repo_prepared", "repo_prepared_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	proposal, err := readJSONArtifact[domain.TaskProposal](ctx, req, "task_proposal", "task_proposal_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return putText(ctx, req, "phase1/artifacts/dockerfile_generate/Dockerfile", "dockerfile", harborgen.Dockerfile(prepared, proposal))
}

func (SolvePlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	files, err := readJSONArtifact[domain.GeneratedTaskFiles](ctx, req, "generated_task_files", "task_files_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return putText(ctx, req, "phase2/artifacts/solve_generate/solve.sh", "solve_script", harborgen.SolveScript(files))
}

func (TestPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	files, err := readJSONArtifact[domain.GeneratedTaskFiles](ctx, req, "generated_task_files", "task_files_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return putText(ctx, req, "phase2/artifacts/test_generate/test.sh", "test_script", harborgen.TestScript(files))
}

func (TestsAnalysisPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	files, err := readJSONArtifact[domain.GeneratedTaskFiles](ctx, req, "generated_task_files", "task_files_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	proposal, err := readJSONArtifact[domain.TaskProposal](ctx, req, "task_proposal", "task_proposal_artifact")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return putText(ctx, req, "phase3/artifacts/tests_analysis/tests_analysis.md", "tests_analysis", harborgen.TestsAnalysis(files, proposal))
}

func (MaterializePlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("materialize_task artifact store is required")
	}
	taskDir := pluginutil.String(req, "task_dir")
	if !withinRoot(taskDir, req.Store.Root()) {
		return workflow.NodeResult{}, fmt.Errorf("materialized task directory must remain within artifact store root: %s", taskDir)
	}
	contents := harborgen.CanonicalTask{}
	var err error
	if contents.Instruction, err = readTextArtifact(ctx, req, "instruction", "instruction_artifact"); err != nil {
		return workflow.NodeResult{}, err
	}
	if contents.TaskTOML, err = readTextArtifact(ctx, req, "task_toml", "task_toml_artifact"); err != nil {
		return workflow.NodeResult{}, err
	}
	if contents.Dockerfile, err = readTextArtifact(ctx, req, "dockerfile", "dockerfile_artifact"); err != nil {
		return workflow.NodeResult{}, err
	}
	if contents.SolveScript, err = readTextArtifact(ctx, req, "solve_script", "solve_artifact"); err != nil {
		return workflow.NodeResult{}, err
	}
	if contents.TestScript, err = readTextArtifact(ctx, req, "test_script", "test_artifact"); err != nil {
		return workflow.NodeResult{}, err
	}
	if contents.TestsAnalysis, err = readTextArtifact(ctx, req, "tests_analysis", "tests_analysis_artifact"); err != nil {
		return workflow.NodeResult{}, err
	}
	if err := harborgen.MaterializeCanonicalTask(taskDir, contents); err != nil {
		return workflow.NodeResult{}, err
	}
	files := []struct{ rel, artifactType string }{
		{"instruction.md", "materialized_instruction"}, {"task.toml", "materialized_task_toml"}, {"environment/Dockerfile", "materialized_dockerfile"},
		{"solution/solve.sh", "materialized_solve_script"}, {"tests/test.sh", "materialized_test_script"}, {"tests_analysis.md", "materialized_tests_analysis"},
	}
	result := workflow.NodeResult{}
	for _, file := range files {
		path := filepath.Join(taskDir, filepath.FromSlash(file.rel))
		name, err := filepath.Rel(req.Store.Root(), path)
		if err != nil {
			return workflow.NodeResult{}, err
		}
		ref, err := req.Store.Register(ctx, workflow.RegisterArtifactRequest{Name: filepath.ToSlash(name), Type: file.artifactType, Producer: req.Spec.ID, Path: path})
		if err != nil {
			return workflow.NodeResult{}, err
		}
		result.Artifacts = append(result.Artifacts, ref)
	}
	return result, nil
}

func (PublishTaskPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("publish_task artifact store is required")
	}
	source := filepath.Clean(pluginutil.String(req, "task_dir"))
	destination := filepath.Clean(pluginutil.String(req, "destination_dir"))
	resolvedSource, err := resolvePathWithExistingAncestor(source)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("resolve publish source: %w", err)
	}
	resolvedRoot, err := resolvePathWithExistingAncestor(req.Store.Root())
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("resolve artifact store root: %w", err)
	}
	if !withinRoot(resolvedSource, resolvedRoot) {
		return workflow.NodeResult{}, fmt.Errorf("publish source must remain within artifact store root: %s", source)
	}
	resolvedDestination, err := resolvePathWithExistingAncestor(destination)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("resolve publish destination: %w", err)
	}
	if withinRoot(resolvedDestination, resolvedRoot) || withinRoot(resolvedRoot, resolvedDestination) {
		return workflow.NodeResult{}, fmt.Errorf("publish destination must not overlap artifact store root: %s", destination)
	}
	sourceDigest, err := harborrun.ComputeTaskDigest(source)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("digest publish source: %w", err)
	}
	if err := harborgen.PublishCanonicalTask(source, destination); err != nil {
		return workflow.NodeResult{}, fmt.Errorf("publish canonical task: %w", err)
	}
	publishedDigest, err := harborrun.ComputeTaskDigest(destination)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("digest published task: %w", err)
	}
	if publishedDigest != sourceDigest {
		return workflow.NodeResult{}, fmt.Errorf("published task digest mismatch: source=%s destination=%s", sourceDigest, publishedDigest)
	}
	receipt := domain.TaskPublishReceipt{
		SchemaVersion: "harbor.task_publish_receipt.v1", SourceTaskDir: source, DestinationDir: destination,
		SourceDigest: sourceDigest, PublishedDigest: publishedDigest, CreatedAt: time.Now().UTC(), Passed: true,
	}
	return putJSON(ctx, req, "phase3/artifacts/task_publish/publish_receipt.json", "task_publish_receipt", receipt)
}

func (p RuntimeSelfCheckPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Runtimes.Agent == nil {
		return workflow.NodeResult{}, fmt.Errorf("runtime_self_check agent runtime is required")
	}
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("runtime_self_check artifact store is required")
	}
	check := p.Check
	if check == nil {
		check = harborgen.RuntimeSelfCheck
	}
	logName := pluginutil.ArtifactName(req, "phase2/artifacts/runtime_self_check/agent.log")
	logPath, err := req.Store.Path(logName)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	if err := check(ctx, conversationOptions(req), pluginutil.String(req, "task_dir"), logPath); err != nil {
		return workflow.NodeResult{}, err
	}
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		if _, err := req.Store.PutText(ctx, logName, "agent_log", req.Spec.ID, "runtime self-check completed\n"); err != nil {
			return workflow.NodeResult{}, err
		}
	}
	ref, err := req.Store.Register(ctx, workflow.RegisterArtifactRequest{Name: logName, Type: "agent_log", Producer: req.Spec.ID, Path: logPath})
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}

func conversationOptions(req workflow.NodeRequest) harborgen.ConversationOptions {
	return harborgen.ConversationOptions{Workspace: req.WorkspaceRoot, Model: pluginutil.String(req, "model"), ReasoningEffort: pluginutil.String(req, "reasoning_effort"), TimeoutSeconds: pluginutil.Int(req, "timeout_seconds"), Agent: req.Runtimes.Agent}
}

func putJSON(ctx context.Context, req workflow.NodeRequest, fallback, artifactType string, value any) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("%s artifact store is required", req.Spec.ID)
	}
	ref, err := req.Store.PutJSON(ctx, pluginutil.ArtifactName(req, fallback), artifactType, req.Spec.ID, value)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}

func putText(ctx context.Context, req workflow.NodeRequest, fallback, artifactType, value string) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("%s artifact store is required", req.Spec.ID)
	}
	ref, err := req.Store.PutText(ctx, pluginutil.ArtifactName(req, fallback), artifactType, req.Spec.ID, value)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}

func readJSONArtifact[T any](ctx context.Context, req workflow.NodeRequest, artifactType, configKey string) (T, error) {
	var target T
	if req.Store == nil {
		return target, fmt.Errorf("%s artifact store is required", req.Spec.ID)
	}
	if ref, ok := findInput(req.Inputs, artifactType); ok {
		reader, _, err := req.Store.Get(ctx, ref)
		if err != nil {
			return target, err
		}
		defer reader.Close()
		if err := json.NewDecoder(reader).Decode(&target); err != nil {
			return target, fmt.Errorf("decode canonical %s artifact: %w", artifactType, err)
		}
		return target, nil
	}
	name := pluginutil.String(req, configKey)
	if name == "" {
		return target, fmt.Errorf("%s node %s missing canonical %s input artifact", req.Spec.Kind, req.Spec.ID, artifactType)
	}
	_, err := req.Store.ReadJSON(ctx, name, &target)
	return target, err
}

func readTextArtifact(ctx context.Context, req workflow.NodeRequest, artifactType, configKey string) (string, error) {
	if req.Store == nil {
		return "", fmt.Errorf("%s artifact store is required", req.Spec.ID)
	}
	ref, ok := findInput(req.Inputs, artifactType)
	if !ok {
		name := pluginutil.String(req, configKey)
		if name == "" {
			return "", fmt.Errorf("%s node %s missing canonical %s input artifact", req.Spec.Kind, req.Spec.ID, artifactType)
		}
		refs, err := req.Store.List(ctx, name)
		if err != nil || len(refs) == 0 {
			return "", fmt.Errorf("canonical %s artifact %s not registered", artifactType, name)
		}
		ref = refs[0]
	}
	reader, _, err := req.Store.Get(ctx, ref)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func findInput(inputs []workflow.ArtifactRef, artifactType string) (workflow.ArtifactRef, bool) {
	for _, ref := range inputs {
		if ref.Type == artifactType {
			return ref, true
		}
	}
	return workflow.ArtifactRef{}, false
}

func withinRoot(path, root string) bool {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolvePathWithExistingAncestor(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(strings.TrimSpace(path)))
	if err != nil {
		return "", err
	}
	current := abs
	var suffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
