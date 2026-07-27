package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringDockerHarnessTailLimit           = 16 << 10
	standardAuthoringDockerHarnessSourceAccessProgram = "rm -rf /oracle/worktree && mkdir -p /oracle/worktree && cp -R /oracle/source/. /oracle/worktree/ && test -d /oracle/worktree && touch /oracle/worktree/.harbor-source-access && rm /oracle/worktree/.harbor-source-access"
)

var standardAuthoringDockerHarnessTokenPattern = regexp.MustCompile(`(?i)\b(?:sk|key|token)-[a-z0-9_-]{16,}\b`)

// StandardAuthoringDockerHarnessConfig binds the authoring verifier to the
// parent deployment lock. Runner and ExecutableAttestor are injectable only
// at the host composition boundary so deterministic tests need no Docker.
type StandardAuthoringDockerHarnessConfig struct {
	ManagedRoot        string
	LockedCommands     []stageprovider.LocalExecutableLock
	Runner             CodeEdgePhase1CommandRunner
	CommandTimeout     time.Duration
	ExecutableAttestor func(context.Context, stageprovider.LocalExecutableLock) error
}

// StandardAuthoringDockerHarness is the host-owned ReAct validation
// capability. It derives the candidate task root from frozen execution IDs,
// snapshots fixed files outside the model-writable root, and exposes no path,
// argv, image, environment or network control to the model.
type StandardAuthoringDockerHarness struct {
	authoringWorkspaceRoot string
	docker                 *lockedDockerRuntime
	mu                     sync.Mutex
}

func NewStandardAuthoringDockerHarness(config StandardAuthoringDockerHarnessConfig) (*StandardAuthoringDockerHarness, error) {
	layout, err := newManagedLayout(config.ManagedRoot)
	if err != nil {
		return nil, err
	}
	if err := layout.ensureRoot(); err != nil {
		return nil, err
	}
	authoringWorkspaceRoot, err := StandardAuthoringCodexWorkspaceRoot(config.ManagedRoot)
	if err != nil {
		return nil, err
	}
	if err := ensureStandardAuthoringWorkspaceRoot(authoringWorkspaceRoot); err != nil {
		return nil, err
	}
	commands, err := codeEdgePhase1LockedCommands(config.LockedCommands)
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring Docker command registry: %w", err)
	}
	attestor := config.ExecutableAttestor
	if attestor == nil {
		attestor = stageprovider.AttestLockedLocalExecutable
	}
	docker, err := newLockedDockerRuntime(commands, config.Runner, config.CommandTimeout, attestor)
	if err != nil {
		return nil, err
	}
	return &StandardAuthoringDockerHarness{
		authoringWorkspaceRoot: authoringWorkspaceRoot,
		docker:                 docker,
	}, nil
}

// ValidateV3Candidate runs the sealed 3.0 verifier over bytes already read by
// the host from frozen artifact bindings. The workspace used by an author is
// never consulted: the verifier writes a fresh private projection and mounts
// only the repo_prepare source snapshot into Docker.
func (harness *StandardAuthoringDockerHarness) ValidateV3Candidate(ctx context.Context, runID, stageAttemptID string, snapshot workflowkit.CandidateSnapshot, files map[string][]byte, verification StandardAuthoringVerificationContract) (authoringharness.Result, error) {
	if harness == nil || harness.docker == nil {
		return authoringharness.Result{}, errors.New("Standard authoring Docker harness is not configured")
	}
	if ctx == nil {
		return authoringharness.Result{}, errors.New("Standard authoring Docker harness context is required")
	}
	if err := ctx.Err(); err != nil {
		return authoringharness.Result{}, err
	}
	if err := store.ValidateUUIDv7(runID); err != nil {
		return authoringharness.Result{}, fmt.Errorf("Standard authoring candidate Run identity: %w", err)
	}
	if err := store.ValidateUUIDv7(stageAttemptID); err != nil {
		return authoringharness.Result{}, fmt.Errorf("Standard authoring candidate stage attempt identity: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return authoringharness.Result{}, fmt.Errorf("Standard authoring candidate snapshot: %w", err)
	}
	if err := validateStandardAuthoringV3CandidateFiles(snapshot, files); err != nil {
		return authoringharness.Result{}, err
	}
	if err := verification.Validate(); err != nil {
		return authoringharness.Result{}, fmt.Errorf("Standard authoring verification contract: %w", err)
	}
	candidate, err := authoringharness.CandidateFromBytes(authoringharness.ModeInitialOracle,
		files[standardAuthoringV3DockerfilePath], files[standardAuthoringV3SolveScriptPath], files[standardAuthoringV3TestScriptPath])
	if err != nil {
		return authoringharness.Result{}, fmt.Errorf("construct Standard authoring candidate: %w", err)
	}
	sourceRoot := filepath.Join(harness.authoringWorkspaceRoot, runID, stageprovider.StandardAuthoringCodexRunSourceDirectory)
	info, err := os.Stat(sourceRoot)
	if err != nil {
		return authoringharness.Result{}, fmt.Errorf("inspect Standard authoring frozen source: %w", err)
	}
	if !info.IsDir() {
		return authoringharness.Result{}, errors.New("Standard authoring frozen source is not a directory")
	}

	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.validateV3Candidate(ctx, runID, stageAttemptID, snapshot, candidate, files, sourceRoot, verification.Canonical())
}

const (
	standardAuthoringV3InstructionPath   = "instruction.md"
	standardAuthoringV3TaskTOMLPath      = "task.toml"
	standardAuthoringV3DockerfilePath    = authoringharness.DockerfileRelativePath
	standardAuthoringV3SolveScriptPath   = authoringharness.SolveScriptRelativePath
	standardAuthoringV3TestScriptPath    = authoringharness.TestScriptRelativePath
	standardAuthoringV3TestsAnalysisPath = "tests_analysis.json"
)

func validateStandardAuthoringV3CandidateFiles(snapshot workflowkit.CandidateSnapshot, files map[string][]byte) error {
	expected := map[string][]byte{
		standardAuthoringV3InstructionPath:   files[standardAuthoringV3InstructionPath],
		standardAuthoringV3TaskTOMLPath:      files[standardAuthoringV3TaskTOMLPath],
		standardAuthoringV3DockerfilePath:    files[standardAuthoringV3DockerfilePath],
		standardAuthoringV3SolveScriptPath:   files[standardAuthoringV3SolveScriptPath],
		standardAuthoringV3TestScriptPath:    files[standardAuthoringV3TestScriptPath],
		standardAuthoringV3TestsAnalysisPath: files[standardAuthoringV3TestsAnalysisPath],
	}
	if len(files) != len(expected) || len(snapshot.Files) != len(expected) {
		return errors.New("Standard authoring candidate snapshot does not contain the fixed 3.0 file set")
	}
	manifest := make(map[string]workflowkit.CandidateFile, len(snapshot.Files))
	for _, file := range snapshot.Files {
		manifest[file.Path] = file
	}
	for path, content := range expected {
		if len(content) == 0 {
			return fmt.Errorf("Standard authoring candidate file %q is empty", path)
		}
		file, found := manifest[path]
		if !found || file.SizeBytes != int64(len(content)) || file.ContentDigest != workflowkit.SHA256Fingerprint(content) {
			return fmt.Errorf("Standard authoring candidate snapshot does not bind %q", path)
		}
	}
	return nil
}

func (harness *StandardAuthoringDockerHarness) validateV3Candidate(ctx context.Context, runID, stageAttemptID string, snapshot workflowkit.CandidateSnapshot, candidate authoringharness.Candidate, files map[string][]byte, sourceRoot string, verification StandardAuthoringVerificationContract) (authoringharness.Result, error) {
	invocationRoot, err := os.MkdirTemp("", "harbor-authoring-v3-validate-")
	if err != nil {
		return authoringharness.Result{}, fmt.Errorf("create Standard authoring verifier invocation: %w", err)
	}
	defer os.RemoveAll(invocationRoot)
	if err := os.Chmod(invocationRoot, 0o700); err != nil {
		return authoringharness.Result{}, err
	}
	snapshotRoot := filepath.Join(invocationRoot, "candidate")
	if err := writeStandardAuthoringV3CandidateSnapshot(snapshotRoot, files); err != nil {
		return authoringharness.Result{}, err
	}
	result := authoringharness.Result{
		Mode: authoringharness.ModeInitialOracle, RunID: runID, StageKey: workflowkit.StageKey("host_candidate_verify"), StageAttemptID: stageAttemptID,
		CandidateDigest: candidate.CandidateDigest, EnvironmentDigest: candidate.EnvironmentDigest,
		Steps: []authoringharness.StepResult{{Step: "layout_probe", Passed: true, Findings: []string{}, OutputFingerprint: snapshot.Digest}},
	}
	image, buildStep, reused, err := harness.ensureCandidateImage(ctx, authoringharness.Request{Mode: authoringharness.ModeInitialOracle, RunID: runID, StageKey: workflowkit.StageKey("host_candidate_verify"), StageAttemptID: stageAttemptID}, candidate, invocationRoot, snapshotRoot)
	if err != nil {
		return authoringharness.Result{}, err
	}
	buildStep.Step = "environment_build"
	result.ImageID = image.ImageID
	result.ImageReused = reused
	result.Steps = append(result.Steps, buildStep)
	if !buildStep.Passed {
		return harness.finishResult(result)
	}
	sourceAccess, err := harness.runSourceAccess(ctx, image, invocationRoot, sourceRoot)
	if err != nil {
		return authoringharness.Result{}, err
	}
	result.Steps = append(result.Steps, sourceAccess)
	if !sourceAccess.Passed {
		return harness.finishResult(result)
	}
	baseline, err := harness.runV3Baseline(ctx, authoringharness.Request{Mode: authoringharness.ModeInitialOracle, RunID: runID, StageKey: workflowkit.StageKey("host_candidate_verify"), StageAttemptID: stageAttemptID}, image, invocationRoot, snapshotRoot, sourceRoot, verification)
	if err != nil {
		return authoringharness.Result{}, err
	}
	baseline.Step = "baseline_verify"
	result.Steps = append(result.Steps, baseline)
	if !baseline.Passed {
		return harness.finishResult(result)
	}
	oracle, err := harness.runV3Oracle(ctx, authoringharness.Request{Mode: authoringharness.ModeInitialOracle, RunID: runID, StageKey: workflowkit.StageKey("host_candidate_verify"), StageAttemptID: stageAttemptID}, image, invocationRoot, snapshotRoot, sourceRoot, verification, "oracle")
	if err != nil {
		return authoringharness.Result{}, err
	}
	result.Steps = append(result.Steps, oracle)
	if !oracle.Passed {
		return harness.finishResult(result)
	}
	coverage, err := harness.runV3Oracle(ctx, authoringharness.Request{Mode: authoringharness.ModeInitialOracle, RunID: runID, StageKey: workflowkit.StageKey("host_candidate_verify"), StageAttemptID: stageAttemptID}, image, invocationRoot, snapshotRoot, sourceRoot, verification, "coverage")
	if err != nil {
		return authoringharness.Result{}, err
	}
	coverage.Step = "coverage_verify"
	result.Steps = append(result.Steps, coverage)
	if !coverage.Passed {
		return harness.finishResult(result)
	}
	integrity := authoringharness.StepResult{Step: "integrity_verify", Passed: true, Findings: []string{}, OutputFingerprint: snapshot.Digest}
	if err := validateStandardAuthoringV3SolutionDiff(filepath.Join(invocationRoot, "verification", "oracle", "workspace"), sourceRoot, verification.AllowedSolutionPaths); err != nil {
		integrity.Passed = false
		integrity.Findings = []string{"Oracle modified a path outside the frozen verification contract"}
	}
	result.Steps = append(result.Steps, integrity)
	if !integrity.Passed {
		return harness.finishResult(result)
	}
	result.Passed = true
	return harness.finishResult(result)
}

func writeStandardAuthoringV3CandidateSnapshot(root string, files map[string][]byte) error {
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	for relative, content := range files {
		if err := workflowkit.ValidateCandidateFilePath(relative); err != nil {
			return err
		}
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if relative == standardAuthoringV3SolveScriptPath || relative == standardAuthoringV3TestScriptPath {
			mode = 0o700
		}
		if err := writeNewBytesWithMode(path, content, mode); err != nil {
			return err
		}
	}
	return nil
}

type standardAuthoringHarnessImageReceipt struct {
	ImageTag string `json:"image_tag"`
	ImageID  string `json:"image_id"`
}

func (harness *StandardAuthoringDockerHarness) ensureCandidateImage(ctx context.Context, request authoringharness.Request, candidate authoringharness.Candidate, invocationRoot, snapshotRoot string) (standardAuthoringHarnessImageReceipt, authoringharness.StepResult, bool, error) {
	imageTag := standardAuthoringHarnessImageTag(request.RunID, candidate.EnvironmentDigest)
	environmentRoot := filepath.Join(snapshotRoot, "environment")
	args := []string{
		"build", "--pull=false", "--network=default",
		"--label", "io.harbor-factory.authoring.run_id=" + request.RunID,
		"--label", "io.harbor-factory.authoring.environment_digest=" + string(candidate.EnvironmentDigest),
		"--tag", imageTag,
		"--file", filepath.Join(environmentRoot, "Dockerfile"),
		environmentRoot,
	}
	commandResult, fingerprint, runErr := harness.docker.run(ctx, stageprovider.CodeEdgePhase1DockerBuildCommandID, args, invocationRoot)
	if runErr != nil && isStandardAuthoringHarnessFatalCommandError(ctx, runErr) {
		return standardAuthoringHarnessImageReceipt{}, authoringharness.StepResult{}, false, runErr
	}
	findings := []string{}
	passed := runErr == nil && commandResult.ExitCode == 0
	if runErr != nil {
		findings = append(findings, "controlled Docker build could not complete: "+standardAuthoringHarnessSafeError(runErr))
	} else if commandResult.ExitCode != 0 {
		findings = append(findings, "controlled Docker build failed")
	}
	step := harness.commandStep("docker_build", commandResult, fingerprint, passed, findings)
	if !passed {
		return standardAuthoringHarnessImageReceipt{}, step, false, nil
	}
	imageID, inspected, inspectFingerprint, inspectErr := harness.docker.inspectImage(ctx, invocationRoot, stageprovider.CodeEdgePhase1DockerBuildCommandID, imageTag)
	if inspectErr != nil {
		if isStandardAuthoringHarnessFatalCommandError(ctx, inspectErr) {
			return standardAuthoringHarnessImageReceipt{}, authoringharness.StepResult{}, false, inspectErr
		}
		inspectStep := harness.commandStep("docker_image_inspect", inspected, inspectFingerprint, false, []string{"controlled Docker build image cannot be identified"})
		return standardAuthoringHarnessImageReceipt{}, inspectStep, false, nil
	}
	receipt := standardAuthoringHarnessImageReceipt{
		ImageTag: imageTag, ImageID: imageID,
	}
	return receipt, step, false, nil
}

// runSourceAccess proves the Dockerfile exposes the frozen source to the
// exact restricted runtime that will execute generated solution scripts. A
// successful image build alone is insufficient because source archive modes
// can make the read-only source mount unavailable after capabilities are dropped.
func (harness *StandardAuthoringDockerHarness) runSourceAccess(ctx context.Context, image standardAuthoringHarnessImageReceipt, invocationRoot, sourceRoot string) (authoringharness.StepResult, error) {
	if step, ok, err := harness.reattestImage(ctx, invocationRoot, stageprovider.CodeEdgePhase1DockerBuildCommandID, image); err != nil || !ok {
		return step, err
	}
	checkout := filepath.Join(invocationRoot, "verification", "source-access")
	if err := ensureStandardAuthoringHarnessDirectory(filepath.Dir(checkout)); err != nil {
		return authoringharness.StepResult{}, fmt.Errorf("prepare Standard authoring source-access verification parent: %w", err)
	}
	if err := codeEdgePhase1PrepareVerificationCheckoutRoot(checkout); err != nil {
		return authoringharness.StepResult{}, err
	}
	containerID, err := store.NewUUIDv7()
	if err != nil {
		return authoringharness.StepResult{}, err
	}
	result, fingerprint, runErr := harness.docker.run(ctx, stageprovider.CodeEdgePhase1DockerBuildCommandID,
		standardAuthoringDockerRunArgs(image.ImageTag, checkout, sourceRoot, "authoring-source-access-"+containerID, standardAuthoringDockerHarnessSourceAccessProgram), invocationRoot)
	if runErr != nil && isStandardAuthoringHarnessFatalCommandError(ctx, runErr) {
		return authoringharness.StepResult{}, runErr
	}
	findings := []string{}
	passed := true
	if runErr != nil {
		passed = false
		findings = append(findings, "controlled runtime source-access verification could not complete: "+standardAuthoringHarnessSafeError(runErr))
	} else if result.ExitCode != 0 {
		passed = false
		findings = append(findings, "runtime cannot materialize a writable Oracle worktree from /workspace/source")
	}
	return harness.commandStep("source_access", result, fingerprint, passed, findings), nil
}

// runV3Baseline executes the reviewed verification command against a fresh
// copy of the frozen source. It does not delegate command selection to the
// candidate's test script, although that script may be explicitly named by the
// frozen command as a test harness input.
func (harness *StandardAuthoringDockerHarness) runV3Baseline(ctx context.Context, request authoringharness.Request, image standardAuthoringHarnessImageReceipt, invocationRoot, snapshotRoot, sourceRoot string, verification StandardAuthoringVerificationContract) (authoringharness.StepResult, error) {
	if step, ok, err := harness.reattestImage(ctx, invocationRoot, stageprovider.CodeEdgePhase1InitialVerifyCommandID, image); err != nil || !ok {
		return step, err
	}
	checkout := filepath.Join(invocationRoot, "verification", "initial")
	expected, err := copyStandardAuthoringHarnessScripts(checkout, snapshotRoot, []string{authoringharness.TestScriptRelativePath})
	if err != nil {
		return authoringharness.StepResult{}, err
	}
	containerID, err := store.NewUUIDv7()
	if err != nil {
		return authoringharness.StepResult{}, err
	}
	result, fingerprint, runErr := harness.docker.run(ctx, stageprovider.CodeEdgePhase1InitialVerifyCommandID,
		standardAuthoringDockerRunArgs(image.ImageTag, checkout, sourceRoot, "authoring-v3-initial-"+containerID, standardAuthoringV3VerificationProgram(verification, false)), invocationRoot)
	if runErr != nil && isStandardAuthoringHarnessFatalCommandError(ctx, runErr) {
		return authoringharness.StepResult{}, runErr
	}
	findings := []string{}
	passed := true
	if runErr != nil {
		passed = false
		findings = append(findings, "controlled baseline verification could not complete: "+standardAuthoringHarnessSafeError(runErr))
	} else if err := verifyStandardAuthoringHarnessScripts(checkout, expected); err != nil {
		passed = false
		findings = append(findings, "baseline verifier modified its immutable test script")
	} else if result.ExitCode == 0 {
		passed = false
		findings = append(findings, "baseline command passed before the Oracle repair, so the task does not expose the intended problem")
	}
	return harness.commandStep("baseline_verify", result, fingerprint, passed, findings), nil
}

// runV3Oracle executes the same frozen command after the candidate solution
// has been applied. The checkout name is host-selected so coverage verification
// cannot reuse or observe the Oracle checkout.
func (harness *StandardAuthoringDockerHarness) runV3Oracle(ctx context.Context, request authoringharness.Request, image standardAuthoringHarnessImageReceipt, invocationRoot, snapshotRoot, sourceRoot string, verification StandardAuthoringVerificationContract, checkoutName string) (authoringharness.StepResult, error) {
	if step, ok, err := harness.reattestImage(ctx, invocationRoot, stageprovider.CodeEdgePhase1OracleVerifyCommandID, image); err != nil || !ok {
		return step, err
	}
	checkout := filepath.Join(invocationRoot, "verification", checkoutName)
	expected, err := copyStandardAuthoringHarnessScripts(checkout, snapshotRoot, []string{authoringharness.SolveScriptRelativePath, authoringharness.TestScriptRelativePath})
	if err != nil {
		return authoringharness.StepResult{}, err
	}
	containerID, err := store.NewUUIDv7()
	if err != nil {
		return authoringharness.StepResult{}, err
	}
	result, fingerprint, runErr := harness.docker.run(ctx, stageprovider.CodeEdgePhase1OracleVerifyCommandID,
		standardAuthoringDockerRunArgs(image.ImageTag, checkout, sourceRoot, "authoring-v3-"+checkoutName+"-"+containerID, standardAuthoringV3VerificationProgram(verification, true)), invocationRoot)
	if runErr != nil && isStandardAuthoringHarnessFatalCommandError(ctx, runErr) {
		return authoringharness.StepResult{}, runErr
	}
	findings := []string{}
	passed := true
	if runErr != nil {
		passed = false
		findings = append(findings, "controlled Oracle verification could not complete: "+standardAuthoringHarnessSafeError(runErr))
	} else if err := verifyStandardAuthoringHarnessScripts(checkout, expected); err != nil {
		passed = false
		findings = append(findings, "Oracle or verifier modified an immutable solution/test script")
	} else if result.ExitCode != 0 {
		passed = false
		findings = append(findings, "Oracle repair followed by the frozen verification command did not pass")
	}
	return harness.commandStep("oracle_verify", result, fingerprint, passed, findings), nil
}

func standardAuthoringV3VerificationProgram(verification StandardAuthoringVerificationContract, applySolution bool) string {
	command := standardAuthoringShellJoin(verification.Command)
	workdir := standardAuthoringShellQuote("/oracle/workspace/" + strings.TrimPrefix(verification.Workdir, "./"))
	prepare := "rm -rf /oracle/workspace && mkdir -p /oracle/workspace && cp -R /oracle/source/. /oracle/workspace/"
	if applySolution {
		return prepare + " && cd /oracle && sh ./solution/solve.sh && cd " + workdir + " && " + command
	}
	return prepare + " && cd " + workdir + " && " + command
}

func standardAuthoringShellJoin(arguments []string) string {
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		quoted = append(quoted, standardAuthoringShellQuote(argument))
	}
	return strings.Join(quoted, " ")
}

func standardAuthoringShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\\"'\\\"'") + "'"
}

func validateStandardAuthoringV3SolutionDiff(workspace, source string, allowed []string) error {
	workspaceFiles, err := standardAuthoringV3RegularFiles(workspace)
	if err != nil {
		return err
	}
	sourceFiles, err := standardAuthoringV3RegularFiles(source)
	if err != nil {
		return err
	}
	paths := make(map[string]struct{}, len(workspaceFiles)+len(sourceFiles))
	for path := range workspaceFiles {
		paths[path] = struct{}{}
	}
	for path := range sourceFiles {
		paths[path] = struct{}{}
	}
	for path := range paths {
		if workspaceFiles[path] == sourceFiles[path] {
			continue
		}
		permitted := false
		for _, root := range allowed {
			if path == root || strings.HasPrefix(path, root+"/") {
				permitted = true
				break
			}
		}
		if !permitted {
			return fmt.Errorf("modified workspace path %q is not allowed", path)
		}
	}
	return nil
}

func standardAuthoringV3RegularFiles(root string) (map[string]workflowkit.Fingerprint, error) {
	result := make(map[string]workflowkit.Fingerprint)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("verification worktree is unavailable or unsafe")
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("verification worktree contains an unsafe path")
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("verification worktree contains a non-regular file")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(relative)] = workflowkit.SHA256Fingerprint(content)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// standardAuthoringDockerRunArgs exposes the attempt's frozen source to the
// authoring verifier at the path directly beside its immutable scripts. The
// source mount is read-only; only the verifier checkout remains writable.
func standardAuthoringDockerRunArgs(imageTag, checkout, sourceRoot, name, shellProgram string) []string {
	args := codeEdgePhase1DockerRunArgs(imageTag, checkout, name, shellProgram)
	workdirIndex := -1
	for index, argument := range args {
		if argument == "--workdir" {
			workdirIndex = index
			break
		}
	}
	if workdirIndex < 0 {
		panic("CodeEdge Docker arguments are missing --workdir")
	}
	sourceMount := []string{"--mount", "type=bind,src=" + sourceRoot + ",dst=/oracle/source,readonly"}
	result := make([]string, 0, len(args)+len(sourceMount))
	result = append(result, args[:workdirIndex]...)
	result = append(result, sourceMount...)
	return append(result, args[workdirIndex:]...)
}

func (harness *StandardAuthoringDockerHarness) reattestImage(ctx context.Context, invocationRoot, commandID string, image standardAuthoringHarnessImageReceipt) (authoringharness.StepResult, bool, error) {
	actual, result, fingerprint, err := harness.docker.inspectImage(ctx, invocationRoot, commandID, image.ImageTag)
	if err != nil {
		if isStandardAuthoringHarnessFatalCommandError(ctx, err) {
			return authoringharness.StepResult{}, false, err
		}
		return harness.commandStep("docker_image_attest", result, fingerprint, false, []string{"controlled Docker build image cannot be re-attested"}), false, nil
	}
	if actual != image.ImageID {
		return harness.commandStep("docker_image_attest", result, fingerprint, false, []string{"controlled Docker image identity differs from the frozen build receipt"}), false, nil
	}
	return authoringharness.StepResult{}, true, nil
}

func (harness *StandardAuthoringDockerHarness) commandStep(name string, result CodeEdgePhase1CommandResult, fingerprint workflowkit.Fingerprint, passed bool, findings []string) authoringharness.StepResult {
	if findings == nil {
		findings = []string{}
	}
	exitCode := result.ExitCode
	return authoringharness.StepResult{
		Step: name, Passed: passed, ExitCode: exitCode, Findings: append([]string(nil), findings...),
		StdoutTail: harness.outputTail(result.Stdout), StderrTail: harness.outputTail(result.Stderr), OutputFingerprint: fingerprint,
	}
}

func (harness *StandardAuthoringDockerHarness) finishResult(result authoringharness.Result) (authoringharness.Result, error) {
	last := result.Steps[len(result.Steps)-1]
	result.Step = last.Step
	result.ExitCode = last.ExitCode
	result.Findings = append([]string(nil), last.Findings...)
	result.StdoutTail = last.StdoutTail
	result.StderrTail = last.StderrTail
	return authoringharness.Finalize(result)
}

func (harness *StandardAuthoringDockerHarness) outputTail(raw []byte) string {
	if len(raw) > standardAuthoringDockerHarnessTailLimit {
		raw = raw[len(raw)-standardAuthoringDockerHarnessTailLimit:]
	}
	value := strings.ToValidUTF8(string(raw), "\uFFFD")
	value = strings.ReplaceAll(value, harness.authoringWorkspaceRoot, "<authoring-workspace>")
	return standardAuthoringDockerHarnessTokenPattern.ReplaceAllString(value, "<redacted-token>")
}

func copyStandardAuthoringHarnessScripts(checkout, snapshotRoot string, relativeFiles []string) (map[string]workflowkit.Fingerprint, error) {
	// The checkout is nested below a per-invocation root. Keep its private
	// parent host-owned before exposing the sticky /oracle mount root.
	if err := ensureStandardAuthoringHarnessDirectory(filepath.Dir(checkout)); err != nil {
		return nil, fmt.Errorf("prepare Standard authoring verification parent: %w", err)
	}
	if err := codeEdgePhase1PrepareVerificationCheckoutRoot(checkout); err != nil {
		return nil, err
	}
	expected := make(map[string]workflowkit.Fingerprint, len(relativeFiles))
	for _, relative := range relativeFiles {
		content, digest, err := codeEdgePhase1ReadTaskScript(filepath.Join(snapshotRoot, filepath.FromSlash(relative)))
		if err != nil {
			return nil, err
		}
		destination := filepath.Join(checkout, filepath.FromSlash(relative))
		directory := filepath.Dir(destination)
		if err := codeEdgePhase1PrepareVerificationScriptDirectory(directory); err != nil {
			return nil, err
		}
		if err := writeNewBytesWithMode(destination, content, 0o444); err != nil {
			return nil, err
		}
		expected[relative] = digest
	}
	return expected, nil
}

func verifyStandardAuthoringHarnessScripts(checkout string, expected map[string]workflowkit.Fingerprint) error {
	return codeEdgePhase1VerifyCheckoutScripts(checkout, expected)
}

func ensureStandardAuthoringHarnessDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Standard authoring verifier directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("Standard authoring verifier directory is unavailable or unsafe")
	}
	return nil
}

func writeNewBytesWithMode(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	writeErr := func() error {
		if _, err := file.Write(content); err != nil {
			return err
		}
		return file.Sync()
	}()
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(path, mode)
}

func standardAuthoringHarnessImageTag(runID string, digest workflowkit.Fingerprint) string {
	hex := strings.TrimPrefix(string(digest), "sha256:")
	return "harbor-authoring:" + strings.ToLower(strings.ReplaceAll(runID, "-", "")) + "-" + hex
}

func isStandardAuthoringHarnessFatalCommandError(ctx context.Context, err error) bool {
	return ctx.Err() != nil || isStandardAuthoringHarnessAttestationError(err)
}

func isStandardAuthoringHarnessAttestationError(err error) bool {
	return errors.Is(err, stageprovider.ErrDeploymentOperationRuntimeAttestationFailed) || errors.Is(err, stageprovider.ErrDeploymentOperationRuntimeAttestationUnavailable)
}

func standardAuthoringHarnessSafeError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "command timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "command was canceled"
	}
	return "command process failed"
}
