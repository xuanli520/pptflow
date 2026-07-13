package gen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

type conversationRuntimeOptions struct {
	RepoPrepared domain.RepoPrepared
	Workspace    string
	// CheckpointRoot is the durable artifact root used for per-attempt turn
	// evidence. Empty keeps standalone generation self-contained in Workspace.
	CheckpointRoot  string
	RunID           string
	Attempt         int
	ForceFresh      bool
	Model           string
	ReasoningEffort string
	Agent           workflow.AgentRuntime
}

type generatedFileWrite struct {
	taskPath string
	content  string
	mode     os.FileMode
}

const structuredOutputTurns = 3

type conversationCheckpoint struct {
	SchemaVersion    string    `json:"schema_version"`
	RunID            string    `json:"run_id"`
	NodeID           string    `json:"node_id"`
	Attempt          int       `json:"attempt"`
	Turn             int       `json:"turn"`
	Substep          string    `json:"substep"`
	Status           string    `json:"status"`
	CandidatePath    string    `json:"candidate_path,omitempty"`
	InputFingerprint string    `json:"input_fingerprint"`
	ValidationError  string    `json:"validation_error,omitempty"`
	Error            string    `json:"error,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// runJSONConversation scopes one ephemeral thread to exactly one workflow
// node. It always performs a draft and a self-review turn, then permits one
// bounded correction turn. Only the final validated value may be persisted by
// the caller; raw output and thread history never cross node boundaries.
func runJSONConversation(ctx context.Context, opts conversationRuntimeOptions, timeout int, nodeID, prompt string, target any, normalizeAndValidate func() error) error {
	projectPath, err := isolatedAgentProject(opts, nodeID)
	if err != nil {
		return err
	}
	attemptDir, err := conversationAttemptDir(opts, nodeID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(attemptDir, 0o700); err != nil {
		return fmt.Errorf("create %s attempt checkpoint directory: %w", nodeID, err)
	}
	conversation, err := opts.Agent.OpenConversation(ctx, workflow.AgentConversationRequest{
		ProjectPath:     projectPath,
		Model:           opts.Model,
		ReasoningEffort: opts.ReasoningEffort,
		SandboxMode:     "read-only",
		SandboxPolicy:   "readOnly",
		NetworkAccess:   false,
		WorkspaceRoots:  []string{projectPath},
		TimeoutSeconds:  timeout,
		MaxOutputBytes:  2 << 20,
		LogPath:         filepath.Join(attemptDir, "session.log"),
	})
	if err != nil {
		return fmt.Errorf("%s open agent conversation: %w", nodeID, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = conversation.Close()
		}
	}()

	turnPrompt := prompt
	inputFingerprint := conversationInputFingerprint(opts, nodeID, prompt)
	startTurn := 1
	if resumed, resumeErr := restoreDraftCheckpoint(opts, nodeID, inputFingerprint, target, normalizeAndValidate); resumeErr != nil {
		return fmt.Errorf("restore %s draft checkpoint: %w", nodeID, resumeErr)
	} else if resumed {
		startTurn = 2
		turnPrompt = structuredRefinementPrompt(nodeID, 1, nil)
	}
	var validationErr error
	for turn := startTurn; turn <= structuredOutputTurns; turn++ {
		turnDir := filepath.Join(attemptDir, fmt.Sprintf("turn-%03d", turn))
		result, turnErr := conversation.Turn(ctx, workflow.AgentTurnRequest{
			Prompt:         turnPrompt,
			Model:          opts.Model,
			TimeoutSeconds: timeout,
			MaxOutputBytes: 2 << 20,
			LogPath:        filepath.Join(turnDir, "agent.log"),
		})
		if turnErr != nil {
			if checkpointErr := persistConversationCheckpoint(opts, nodeID, inputFingerprint, turn, nil, "", nil, turnErr); checkpointErr != nil {
				return fmt.Errorf("%s persist interrupted turn %d: %w", nodeID, turn, checkpointErr)
			}
			return fmt.Errorf("%s agent turn %d: %w", nodeID, turn, turnErr)
		}
		validationErr = decodeStructuredOutput(result.Text, target, normalizeAndValidate)
		if checkpointErr := persistConversationCheckpoint(opts, nodeID, inputFingerprint, turn, target, result.Text, validationErr, nil); checkpointErr != nil {
			return fmt.Errorf("%s persist turn %d checkpoint: %w", nodeID, turn, checkpointErr)
		}
		if validationErr == nil && turn >= 2 {
			closed = true
			if err := conversation.Close(); err != nil {
				return fmt.Errorf("%s close agent conversation: %w", nodeID, err)
			}
			return nil
		}
		turnPrompt = structuredRefinementPrompt(nodeID, turn, validationErr)
	}
	return fmt.Errorf("%s structured output remained invalid after %d turns: %w", nodeID, structuredOutputTurns, validationErr)
}

func conversationAttemptDir(opts conversationRuntimeOptions, nodeID string) (string, error) {
	root := strings.TrimSpace(opts.CheckpointRoot)
	if root == "" {
		root = nodes.DefaultWorkspace(opts.Workspace)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	runID := safeCheckpointComponent(opts.RunID, "standalone")
	attempt := opts.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	node := safeCheckpointComponent(nodeID, "node")
	return filepath.Join(absolute, "runs", runID, "stages", node, "nodes", node, fmt.Sprintf("attempt-%03d", attempt)), nil
}

func conversationNodeRoot(opts conversationRuntimeOptions, nodeID string) (string, error) {
	attemptDir, err := conversationAttemptDir(opts, nodeID)
	if err != nil {
		return "", err
	}
	return filepath.Dir(attemptDir), nil
}

func safeCheckpointComponent(value, fallback string) string {
	value = strings.TrimSpace(value)
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, value)
	value = strings.Trim(value, ".-")
	if value == "" {
		return fallback
	}
	return value
}

func persistConversationCheckpoint(opts conversationRuntimeOptions, nodeID, inputFingerprint string, turn int, target any, raw string, validationErr, runtimeErr error) error {
	attemptDir, err := conversationAttemptDir(opts, nodeID)
	if err != nil {
		return err
	}
	turnDir := filepath.Join(attemptDir, fmt.Sprintf("turn-%03d", turn))
	if err := os.MkdirAll(turnDir, 0o700); err != nil {
		return err
	}
	if raw != "" {
		if err := atomicWriteFile(filepath.Join(turnDir, "response.txt"), []byte(raw), 0o600); err != nil {
			return err
		}
	}
	checkpoint := conversationCheckpoint{
		SchemaVersion:    "harbor.turn_checkpoint.v1",
		RunID:            safeCheckpointComponent(opts.RunID, "standalone"),
		NodeID:           nodeID,
		InputFingerprint: inputFingerprint,
		Attempt:          opts.Attempt,
		Turn:             turn,
		Substep:          conversationSubstep(turn),
		CreatedAt:        time.Now().UTC(),
	}
	if checkpoint.Attempt <= 0 {
		checkpoint.Attempt = 1
	}
	if runtimeErr != nil {
		checkpoint.Status = "interrupted"
		checkpoint.Error = runtimeErr.Error()
	} else {
		checkpoint.Status = "completed"
		if validationErr == nil {
			candidate, marshalErr := json.MarshalIndent(target, "", "  ")
			if marshalErr != nil {
				return marshalErr
			}
			candidatePath := filepath.Join(turnDir, "candidate.json")
			if err := atomicWriteFile(candidatePath, append(candidate, '\n'), 0o600); err != nil {
				return err
			}
			checkpoint.CandidatePath = candidatePath
		}
	}
	if runtimeErr != nil {
		checkpoint.ValidationError = ""
	} else if validationErr != nil {
		checkpoint.ValidationError = validationErr.Error()
	} else if raw == "" {
		checkpoint.ValidationError = "agent response was empty"
	}
	if runtimeErr == nil {
		// Store the validation result separately so a restore never treats an
		// invalid candidate as a completed draft.
		validation := struct {
			Valid bool   `json:"valid"`
			Error string `json:"error,omitempty"`
		}{Valid: validationErr == nil, Error: checkpoint.ValidationError}
		if err := atomicWriteJSON(filepath.Join(turnDir, "validation.json"), validation, 0o600); err != nil {
			return err
		}
	}
	if err := atomicWriteJSON(filepath.Join(turnDir, "checkpoint.json"), checkpoint, 0o600); err != nil {
		return err
	}
	return atomicWriteJSON(filepath.Join(attemptDir, "checkpoint.json"), checkpoint, 0o600)
}

func conversationSubstep(turn int) string {
	switch turn {
	case 1:
		return "draft"
	case 2:
		return "self_review"
	default:
		return "correction"
	}
}

func restoreDraftCheckpoint(opts conversationRuntimeOptions, nodeID, inputFingerprint string, target any, normalizeAndValidate func() error) (bool, error) {
	if opts.ForceFresh {
		return false, nil
	}
	nodeRoot, err := conversationNodeRoot(opts, nodeID)
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(nodeRoot)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	attempts := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "attempt-") {
			attempts = append(attempts, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(attempts)))
	for _, attempt := range attempts {
		checkpointPath := filepath.Join(nodeRoot, attempt, "turn-001", "checkpoint.json")
		var checkpoint conversationCheckpoint
		raw, readErr := os.ReadFile(checkpointPath)
		if readErr != nil || json.Unmarshal(raw, &checkpoint) != nil || checkpoint.Substep != "draft" || checkpoint.Status != "completed" || checkpoint.CandidatePath == "" || checkpoint.InputFingerprint != inputFingerprint {
			continue
		}
		candidate, readErr := os.ReadFile(checkpoint.CandidatePath)
		if readErr != nil || json.Unmarshal(candidate, target) != nil {
			continue
		}
		if normalizeAndValidate != nil {
			if err := normalizeAndValidate(); err != nil {
				continue
			}
		}
		return true, nil
	}
	return false, nil
}

func conversationInputFingerprint(opts conversationRuntimeOptions, nodeID, prompt string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("harbor.turn-input.v1\x00"))
	_, _ = hash.Write([]byte(nodeID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(opts.Model))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(opts.ReasoningEffort))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(prompt))
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func atomicWriteJSON(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, append(data, '\n'), mode)
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func isolatedAgentProject(opts conversationRuntimeOptions, nodeID string) (string, error) {
	if nodeID == nodes.RepoAnalyze {
		return opts.RepoPrepared.SourcePath, nil
	}
	root := filepath.Join(nodes.DefaultWorkspace(opts.Workspace), ".agent-workspaces", nodeID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create isolated %s agent workspace: %w", nodeID, err)
	}
	return root, nil
}

func decodeStructuredOutput(text string, target any, normalizeAndValidate func() error) error {
	value := reflect.ValueOf(target)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return fmt.Errorf("structured output target must be a non-nil pointer")
	}
	value.Elem().Set(reflect.Zero(value.Elem().Type()))
	raw, err := extractJSONObject(text)
	if err != nil {
		return fmt.Errorf("parse JSON: %w", err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if normalizeAndValidate != nil {
		if err := normalizeAndValidate(); err != nil {
			return fmt.Errorf("validate JSON: %w", err)
		}
	}
	return nil
}

func structuredRefinementPrompt(nodeID string, completedTurn int, validationErr error) string {
	if validationErr != nil {
		return fmt.Sprintf("Your previous %s JSON draft failed local validation after turn %d: %s. Correct every listed issue. Return only one complete JSON object matching the original schema; do not include Markdown or explanation.", nodeID, completedTurn, validationErr)
	}
	return fmt.Sprintf("Critically self-review your previous %s draft for schema accuracy, engineering realism, CodeEdge instruction/test derivability, reproducibility, and missing boundary conditions. Return the improved final answer as exactly one complete JSON object with no Markdown or explanation.", nodeID)
}

func publishCanonicalTask(taskDir string, writes []generatedFileWrite) error {
	if err := validateGeneratedFileWrites(taskDir, writes); err != nil {
		return err
	}
	parent := filepath.Dir(taskDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stageDir, err := os.MkdirTemp(parent, "."+filepath.Base(taskDir)+".staging-")
	if err != nil {
		return err
	}
	defer func() {
		if stageDir != "" {
			_ = os.RemoveAll(stageDir)
		}
	}()
	for _, write := range writes {
		rel, err := filepath.Rel(taskDir, write.taskPath)
		if err != nil {
			return err
		}
		if err := writeGeneratedFile(filepath.Join(stageDir, rel), write.content, write.mode); err != nil {
			return err
		}
	}
	if err := validateMaterializedTaskDir(stageDir); err != nil {
		return err
	}
	if err := publishGeneratedTaskDir(stageDir, taskDir); err != nil {
		return err
	}
	stageDir = ""
	return nil
}

func validateGeneratedFileWrites(taskDir string, writes []generatedFileWrite) error {
	for _, write := range writes {
		rel, err := filepath.Rel(taskDir, write.taskPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !taskpolicy.IsAllowedFile(rel) {
			return fmt.Errorf("unexpected file in generated Harbor task directory: %s", rel)
		}
		if taskpolicy.ContainsLegacyDomain(rel) {
			return fmt.Errorf("legacy non-Harbor domain file is not allowed in generated task: %s", rel)
		}
		if taskpolicy.ContainsLegacyDomain(write.content) {
			return fmt.Errorf("legacy non-Harbor domain content is not allowed in generated task: %s", rel)
		}
	}
	return nil
}

func publishGeneratedTaskDir(stageDir, taskDir string) error {
	if _, err := os.Lstat(taskDir); os.IsNotExist(err) {
		return os.Rename(stageDir, taskDir)
	} else if err != nil {
		return err
	}

	backupDir := stageDir + ".previous"
	if err := os.Rename(taskDir, backupDir); err != nil {
		return fmt.Errorf("preserve previous generated task: %w", err)
	}
	if err := os.Rename(stageDir, taskDir); err != nil {
		restoreErr := os.Rename(backupDir, taskDir)
		if restoreErr != nil {
			return fmt.Errorf("publish generated task: %w; restore previous task: %v", err, restoreErr)
		}
		return fmt.Errorf("publish generated task: %w", err)
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("remove previous generated task backup: %w", err)
	}
	return nil
}

func validateMaterializedTaskDir(taskDir string) error {
	return filepath.WalkDir(taskDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(taskDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not allowed in generated Harbor task directory: %s", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file is not allowed in generated Harbor task directory: %s", rel)
		}
		if !taskpolicy.IsAllowedFile(rel) {
			return fmt.Errorf("unexpected file in generated Harbor task directory: %s", rel)
		}
		if taskpolicy.ContainsLegacyDomain(rel) {
			return fmt.Errorf("legacy non-Harbor domain file is not allowed in generated task: %s", rel)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if taskpolicy.ContainsLegacyDomain(string(raw)) {
			return fmt.Errorf("legacy non-Harbor domain content is not allowed in generated task: %s", rel)
		}
		return nil
	})
}

func writeGeneratedFile(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), mode)
}

func validateRepoAnalysis(analysis domain.RepoAnalysis) error {
	if strings.TrimSpace(analysis.RepoURL) == "" {
		return fmt.Errorf("repo_analysis repo_url is required")
	}
	if strings.TrimSpace(analysis.CommitSHA) == "" {
		return fmt.Errorf("repo_analysis commit_sha is required")
	}
	if strings.TrimSpace(analysis.Language) == "" {
		return fmt.Errorf("repo_analysis language is required")
	}
	if strings.TrimSpace(analysis.BuildSystem) == "" {
		return fmt.Errorf("repo_analysis build_system is required")
	}
	return nil
}

func validateTaskProposal(proposal domain.TaskProposal) error {
	if strings.TrimSpace(proposal.TaskName) == "" {
		return fmt.Errorf("task_proposal task_name is required")
	}
	if strings.TrimSpace(proposal.OneLineDescription) == "" {
		return fmt.Errorf("task_proposal one_line_description is required")
	}
	if strings.TrimSpace(proposal.CodeLang) == "" {
		return fmt.Errorf("task_proposal code_lang is required")
	}
	if strings.TrimSpace(proposal.TaskType) == "" {
		return fmt.Errorf("task_proposal task_type is required")
	}
	if strings.TrimSpace(proposal.Application) == "" {
		return fmt.Errorf("task_proposal application is required")
	}
	if !proposal.IsZeroToOne && strings.TrimSpace(proposal.GitHubLink) == "" {
		return fmt.Errorf("task_proposal github_link is required for non 0-1 task")
	}
	if !commitPattern.MatchString(proposal.CommitSHA) {
		return fmt.Errorf("task_proposal commit_sha must be a concrete 7-40 hex commit")
	}
	return nil
}

func validateTaskFiles(files domain.GeneratedTaskFiles) error {
	if strings.TrimSpace(files.InstructionMD) == "" {
		return fmt.Errorf("generated instruction_md is required")
	}
	if strings.TrimSpace(files.SolveSH) == "" {
		return fmt.Errorf("generated solve_sh is required")
	}
	if strings.TrimSpace(files.TestSH) == "" {
		return fmt.Errorf("generated test_sh is required")
	}
	if strings.TrimSpace(files.TestsAnalysis) == "" {
		return fmt.Errorf("generated tests_analysis_md is required")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "instruction_md", value: files.InstructionMD},
		{name: "solve_sh", value: files.SolveSH},
		{name: "test_sh", value: files.TestSH},
		{name: "tests_analysis_md", value: files.TestsAnalysis},
	} {
		if taskpolicy.ContainsLegacyDomain(field.value) {
			return fmt.Errorf("generated %s contains legacy non-Harbor domain content", field.name)
		}
	}
	return nil
}

func stampTaskFilesProvenance(files *domain.GeneratedTaskFiles, prepared domain.RepoPrepared, proposal domain.TaskProposal) {
	files.RepoURL = strings.TrimSpace(prepared.RepoURL)
	files.CommitSHA = strings.TrimSpace(prepared.ResolvedCommit)
	files.TaskProposalDigest = taskProposalDigest(proposal)
}

func taskProposalDigest(proposal domain.TaskProposal) string {
	data, _ := json.Marshal(proposal)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func applyProposalDefaults(proposal *domain.TaskProposal, prepared domain.RepoPrepared) {
	proposal.SchemaVersion = defaultString(proposal.SchemaVersion, "harbor.task_proposal.v1")
	proposal.TaskName = safeHarborTaskName(defaultString(proposal.TaskName, "codeedge/generated-task"))
	proposal.GitHubLink = defaultString(proposal.GitHubLink, prepared.RepoURL)
	proposal.CommitSHA = defaultString(proposal.CommitSHA, prepared.ResolvedCommit)
	if proposal.EstimatedAHTMinutes <= 0 {
		proposal.EstimatedAHTMinutes = 45
	}
	if strings.TrimSpace(proposal.CodeLang) == "" {
		proposal.CodeLang = "unknown"
	}
	if strings.TrimSpace(proposal.TaskType) == "" {
		proposal.TaskType = "bug-fix"
	}
	if strings.TrimSpace(proposal.Application) == "" {
		proposal.Application = "software-engineering"
	}
}

func renderTaskTOML(proposal domain.TaskProposal) string {
	keywords := []string{"codeedge", proposal.CodeLang, proposal.TaskType, proposal.Application}
	var b strings.Builder
	b.WriteString("schema_version = \"1.3\"\n\n")
	b.WriteString("[task]\n")
	b.WriteString("name = " + tomlString(proposal.TaskName) + "\n")
	b.WriteString("description = " + tomlString(proposal.OneLineDescription) + "\n")
	b.WriteString("keywords = " + tomlStringList(keywords) + "\n\n")
	b.WriteString("[metadata]\n")
	b.WriteString("code_lang = " + tomlString(proposal.CodeLang) + "\n")
	b.WriteString("task_type = " + tomlString(proposal.TaskType) + "\n")
	b.WriteString("application = " + tomlString(proposal.Application) + "\n")
	b.WriteString("is_0_to_1 = " + strconv.FormatBool(proposal.IsZeroToOne) + "\n")
	b.WriteString("github_url = " + tomlString(proposal.GitHubLink) + "\n")
	b.WriteString("commit_id = " + tomlString(proposal.CommitSHA) + "\n")
	b.WriteString("estimated_aht_minutes = " + strconv.Itoa(proposal.EstimatedAHTMinutes) + "\n")
	b.WriteString("difficulty_explanation = " + tomlString(proposal.DifficultyRationale) + "\n")
	b.WriteString("target_files = " + tomlStringList(proposal.TargetFiles) + "\n")
	b.WriteString("affected_modules = " + tomlStringList(proposal.AffectedModules) + "\n")
	b.WriteString("boundary_conditions = " + tomlStringList(proposal.BoundaryConditions) + "\n\n")
	b.WriteString("[verifier]\n")
	b.WriteString("timeout_sec = 600.0\n\n")
	b.WriteString("[agent]\n")
	b.WriteString("timeout_sec = 1800.0\n\n")
	b.WriteString("[environment]\n")
	b.WriteString("build_timeout_sec = 600.0\n")
	b.WriteString("network_mode = \"public\"\n")
	b.WriteString("os = \"linux\"\n")
	return b.String()
}

func renderDockerfile(prepared domain.RepoPrepared, proposal domain.TaskProposal) string {
	var b strings.Builder
	b.WriteString("FROM ")
	b.WriteString(baseImage(proposal.CodeLang))
	b.WriteString("\n\n")
	b.WriteString("ENV DEBIAN_FRONTEND=noninteractive\n")
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends ")
	b.WriteString(strings.Join(basePackages(proposal.CodeLang), " "))
	b.WriteString(" && rm -rf /var/lib/apt/lists/*\n\n")
	toolchainCommands := toolchainSetupCommands(proposal.CodeLang)
	for _, command := range toolchainCommands {
		b.WriteString("RUN ")
		b.WriteString(command)
		b.WriteString("\n")
	}
	if len(toolchainCommands) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("WORKDIR /app\n")
	b.WriteString("RUN git clone ")
	b.WriteString(shellQuote(prepared.RepoURL))
	b.WriteString(" /app/repo && cd /app/repo && git checkout ")
	b.WriteString(shellQuote(prepared.ResolvedCommit))
	b.WriteString("\n")
	for _, command := range normalizedSetupCommands(proposal.SetupCommands) {
		b.WriteString("RUN cd /app/repo && ")
		b.WriteString(command)
		b.WriteString("\n")
	}
	b.WriteString("\nWORKDIR /app/repo\n")
	return b.String()
}

func toolchainSetupCommands(language string) []string {
	if strings.Contains(strings.ToLower(language), "rust") {
		return []string{"rustup component add rustfmt clippy"}
	}
	return nil
}

const rustBaseImage = "rust:1.96.0-bookworm"

func baseImage(language string) string {
	if strings.Contains(strings.ToLower(language), "rust") {
		return rustBaseImage
	}
	return "ubuntu:24.04"
}

func basePackages(language string) []string {
	packages := []string{"ca-certificates", "git", "bash", "curl", "build-essential"}
	lower := strings.ToLower(language)
	switch {
	case strings.Contains(lower, "go"):
		packages = append(packages, "golang-go")
	case strings.Contains(lower, "python"):
		packages = append(packages, "python3", "python3-pip", "python3-pytest")
	case strings.Contains(lower, "javascript"), strings.Contains(lower, "typescript"), strings.Contains(lower, "node"):
		packages = append(packages, "nodejs", "npm")
	case strings.Contains(lower, "rust"):
		// The pinned Rust base image owns cargo and rustc. Ubuntu 24.04 ships
		// Cargo 1.75, which cannot resolve modern edition-2024 dependencies.
	case strings.Contains(lower, "java"), strings.Contains(lower, "kotlin"):
		packages = append(packages, "openjdk-17-jdk", "maven", "gradle")
	case strings.Contains(lower, "ruby"):
		packages = append(packages, "ruby", "ruby-dev")
	}
	return packages
}

func normalizeShellScript(content string) string {
	body := cleanScriptBody(content)
	return "#!/usr/bin/env bash\nset -euo pipefail\n\n" + ensureFinalNewline(body)
}

func normalizeTestScript(content string) string {
	body := cleanScriptBody(content)
	if strings.Contains(body, "/logs/verifier/reward") {
		return "#!/usr/bin/env bash\nset -euo pipefail\n\n" + ensureFinalNewline(body)
	}
	return strings.Join([]string{
		"#!/usr/bin/env bash",
		"set -euo pipefail",
		"mkdir -p /logs/verifier",
		"finish() {",
		"  status=$?",
		"  if [ \"$status\" -eq 0 ]; then",
		"    echo 1 > /logs/verifier/reward.txt",
		"  else",
		"    echo 0 > /logs/verifier/reward.txt",
		"  fi",
		"  exit \"$status\"",
		"}",
		"trap finish EXIT",
		"",
		"(",
		body,
		")",
		"",
	}, "\n")
}

func ensureTestsAnalysis(content string, proposal domain.TaskProposal) string {
	content = strings.TrimSpace(content)
	required := []string{
		"## 1. instruction 和 environment 已提供的信息",
		"## 2. 模型的理论通过路径",
		"## 3. 模型具备通过条件的依据",
	}
	ok := true
	for _, heading := range required {
		if !strings.Contains(content, heading) {
			ok = false
			break
		}
	}
	if ok && generatedTestsAnalysisSubstantive(content, required) {
		return ensureFinalNewline(content)
	}
	return fmt.Sprintf(`## 1. instruction 和 environment 已提供的信息
- instruction 描述的任务: %s
- environment 通过 Dockerfile 从 %s clone 固定 commit %s, 任务在 /app/repo 中执行。
- 关键约束: 不泄露标准答案, 不要求模型读取 tests 或 solution, 不依赖私有凭证。

---

## 2. 模型的理论通过路径
- 阅读 instruction 和仓库源码, 定位目标文件或相关模块: %s
- 按任务类型 %s 完成实现, 保持既有行为和边界条件。
- 运行项目测试或 instruction 建议命令, 修正失败并确认行为符合要求。

---

## 3. 模型具备通过条件的依据
- verifier 的核心检查点应来自 instruction、仓库现有行为和 environment 中可见的运行方式。
- 模型可根据任务描述、目标模块和建议验证方式判断交付边界。
- 不存在只能从 tests/test.sh 得知的隐藏业务要求。

原始生成备注:
%s
`, proposal.OneLineDescription, proposal.GitHubLink, proposal.CommitSHA, strings.Join(proposal.TargetFiles, ", "), proposal.TaskType, content)
}

func generatedTestsAnalysisSubstantive(content string, headings []string) bool {
	for i, heading := range headings {
		start := strings.Index(content, heading)
		if start < 0 {
			return false
		}
		start += len(heading)
		end := len(content)
		for _, next := range headings[i+1:] {
			if idx := strings.Index(content[start:], next); idx >= 0 {
				end = start + idx
				break
			}
		}
		if !generatedTestsAnalysisSectionSubstantive(content[start:end]) {
			return false
		}
	}
	return true
}

func generatedTestsAnalysisSectionSubstantive(section string) bool {
	bullets := 0
	chars := 0
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "---"))
		if line == "" {
			continue
		}
		trimmed := strings.TrimLeft(line, "-*0123456789.、) ")
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "*") || line[0] >= '0' && line[0] <= '9' {
			bullets++
		}
		chars += len([]rune(trimmed))
	}
	return bullets >= 1 && chars >= 40
}

func cleanScriptBody(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(content), "\n")
	for len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "#!") {
		lines = lines[1:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func extractJSONObject(text string) ([]byte, error) {
	data := []byte(text)
	for idx, b := range data {
		if b != '{' {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(data[idx:]))
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == nil && len(raw) > 0 {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("no JSON object found")
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return strings.TrimSpace(fallback)
	}
	return value
}

var (
	commitPattern        = regexp.MustCompile(`(?i)^[0-9a-f]{7,40}$`)
	taskNameChars        = regexp.MustCompile(`[^a-z0-9._/-]+`)
	forbiddenSetupToken  = regexp.MustCompile(`(?i)(\bcopy\b|\badd\b|tests/|solution/|/logs/verifier|reward\.txt|reward\.json)`)
	repositorySetupToken = regexp.MustCompile(`(?i)\bgit\s+(clone|checkout|switch|reset|init|worktree)\b`)
	testSetupToken       = regexp.MustCompile(`(?i)\b((go|cargo)\s+test\b|pytest\b|python\s+-m\s+pytest\b|npm\s+(run\s+)?test\b|pnpm\s+(run\s+)?test\b|yarn\s+test\b|mvn\s+test\b|gradle\s+test\b)`)
	standaloneCDCommand  = regexp.MustCompile(`(?i)^cd(?:\s+[^;&|]+)?$`)
)

func safeHarborTaskName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, " ", "-")
	name = taskNameChars.ReplaceAllString(name, "-")
	name = strings.Trim(name, "./-")
	if name == "" {
		return "codeedge/generated-task"
	}
	if !strings.Contains(name, "/") {
		name = "codeedge/" + name
	}
	return name
}

func safeSetupCommand(command string) bool {
	command = strings.TrimSpace(command)
	return command != "" &&
		!forbiddenSetupToken.MatchString(command) &&
		!repositorySetupToken.MatchString(command) &&
		!testSetupToken.MatchString(command) &&
		!standaloneCDCommand.MatchString(command)
}

func normalizedSetupCommands(commands []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(commands))
	for _, command := range commands {
		command = normalizeSetupCommand(command)
		key := strings.Join(strings.Fields(command), " ")
		if !safeSetupCommand(command) || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, command)
	}
	return out
}

func normalizeSetupCommand(command string) string {
	command = strings.TrimSpace(command)
	fields := strings.Fields(command)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "cargo") || !strings.EqualFold(fields[1], "fetch") || strings.ContainsAny(command, ";&|<>") {
		return command
	}
	fetchFields := make([]string, 0, len(fields))
	for _, field := range fields {
		switch strings.ToLower(field) {
		case "--locked", "--frozen", "--offline":
			continue
		default:
			fetchFields = append(fetchFields, field)
		}
	}
	base := strings.Join(fetchFields, " ")
	return "if [ -f Cargo.lock ]; then " + base + " --locked; else " + base + "; fi"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func tomlString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func tomlStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		quoted = append(quoted, tomlString(value))
	}
	if len(quoted) == 0 {
		return "[]"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func ensureFinalNewline(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "\n"
	}
	return content + "\n"
}
