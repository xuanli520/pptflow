package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	standardAuthoringDockerHarnessStateDirectory      = "standard-authoring-docker-harness"
	standardAuthoringDockerHarnessTailLimit           = 16 << 10
	standardAuthoringDockerHarnessSourceAccessProgram = "rm -rf /oracle/worktree && mkdir -p /oracle/worktree && cp -R /workspace/source/. /oracle/worktree/ && test -d /oracle/worktree && touch /oracle/worktree/.harbor-source-access && rm /oracle/worktree/.harbor-source-access"

	standardAuthoringDockerHarnessReceiptFormat  = "harbor.standard-authoring.docker-image.v1"
	standardAuthoringDockerHarnessReceiptVersion = "1"
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
	stateRoot              string
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
	stateRoot := filepath.Join(layout.root, standardAuthoringDockerHarnessStateDirectory)
	if err := ensureStandardAuthoringHarnessDirectory(stateRoot); err != nil {
		return nil, err
	}
	return &StandardAuthoringDockerHarness{
		authoringWorkspaceRoot: authoringWorkspaceRoot,
		stateRoot:              stateRoot,
		docker:                 docker,
	}, nil
}

func (harness *StandardAuthoringDockerHarness) Validate(ctx context.Context, request authoringharness.Request) (authoringharness.Result, error) {
	if harness == nil || harness.docker == nil {
		return authoringharness.Result{}, errors.New("Standard authoring Docker harness is not configured")
	}
	if ctx == nil {
		return authoringharness.Result{}, errors.New("Standard authoring Docker harness context is required")
	}
	if err := ctx.Err(); err != nil {
		return authoringharness.Result{}, err
	}
	if err := validateStandardAuthoringHarnessRequest(request); err != nil {
		return authoringharness.Result{}, err
	}
	workRoot, err := stageprovider.StandardAuthoringAttemptWorkspacePath(harness.authoringWorkspaceRoot, request.RunID, request.StageKey, request.StageAttemptID)
	if err != nil {
		return authoringharness.Result{}, fmt.Errorf("resolve Standard authoring harness attempt workspace: %w", err)
	}
	if err := stageprovider.ValidateStandardAuthoringAttemptWorkspacePath(harness.authoringWorkspaceRoot, request.RunID, request.StageKey, request.StageAttemptID, workRoot); err != nil {
		return authoringharness.Result{}, fmt.Errorf("validate Standard authoring harness attempt workspace: %w", err)
	}
	taskRoot := filepath.Join(workRoot, stageprovider.StandardAuthoringCodexAttemptTaskDirectory)
	candidate, err := authoringharness.ReadCandidate(taskRoot, request.Mode)
	if err != nil {
		return authoringharness.Result{}, err
	}

	// Docker tags and host receipts are shared by environment digest. Serialize
	// validation to keep tag replacement and receipt publication atomic from
	// the harness's point of view.
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.validateCandidate(ctx, request, candidate)
}

func validateStandardAuthoringHarnessRequest(request authoringharness.Request) error {
	if err := request.Mode.Validate(); err != nil {
		return err
	}
	if err := store.ValidateUUIDv7(request.RunID); err != nil {
		return fmt.Errorf("Standard authoring harness Run identity: %w", err)
	}
	if err := store.ValidateUUIDv7(request.StageAttemptID); err != nil {
		return fmt.Errorf("Standard authoring harness stage attempt identity: %w", err)
	}
	expectedStage := workflowkit.StageKey("dockerfile_build_validate")
	if request.Mode == authoringharness.ModeInitialOracle {
		expectedStage = workflowkit.StageKey("authoring_harness")
	}
	if request.StageKey != expectedStage {
		return fmt.Errorf("Standard authoring harness mode %q is not bound to stage %q", request.Mode, request.StageKey)
	}
	return nil
}

func (harness *StandardAuthoringDockerHarness) validateCandidate(ctx context.Context, request authoringharness.Request, candidate authoringharness.Candidate) (authoringharness.Result, error) {
	runState := filepath.Join(harness.stateRoot, request.RunID)
	if err := ensureStandardAuthoringHarnessDirectory(runState); err != nil {
		return authoringharness.Result{}, err
	}
	invocationRoot, err := os.MkdirTemp(runState, ".validate-")
	if err != nil {
		return authoringharness.Result{}, fmt.Errorf("create Standard authoring harness invocation: %w", err)
	}
	defer os.RemoveAll(invocationRoot)
	if err := os.Chmod(invocationRoot, 0o700); err != nil {
		return authoringharness.Result{}, err
	}
	snapshotRoot := filepath.Join(invocationRoot, "candidate")
	if err := writeStandardAuthoringHarnessSnapshot(snapshotRoot, candidate); err != nil {
		return authoringharness.Result{}, err
	}

	result := authoringharness.Result{
		Mode: request.Mode, RunID: request.RunID, StageKey: request.StageKey, StageAttemptID: request.StageAttemptID,
		CandidateDigest: candidate.CandidateDigest, EnvironmentDigest: candidate.EnvironmentDigest,
	}
	image, buildStep, reused, err := harness.ensureCandidateImage(ctx, request, candidate, invocationRoot, snapshotRoot)
	if err != nil {
		return authoringharness.Result{}, err
	}
	result.ImageID = image.ImageID
	result.ImageReused = reused
	result.Steps = append(result.Steps, buildStep)
	if !buildStep.Passed {
		return harness.finishResult(result)
	}
	if request.Mode == authoringharness.ModeDockerfileBuild {
		sourceAccessStep, err := harness.runSourceAccess(ctx, image, invocationRoot)
		if err != nil {
			return authoringharness.Result{}, err
		}
		result.Steps = append(result.Steps, sourceAccessStep)
		if !sourceAccessStep.Passed {
			return harness.finishResult(result)
		}
		result.Passed = true
		return harness.finishResult(result)
	}

	initialStep, err := harness.runInitial(ctx, request, image, invocationRoot, snapshotRoot)
	if err != nil {
		return authoringharness.Result{}, err
	}
	result.Steps = append(result.Steps, initialStep)
	if !initialStep.Passed {
		return harness.finishResult(result)
	}
	oracleStep, err := harness.runOracle(ctx, request, image, invocationRoot, snapshotRoot)
	if err != nil {
		return authoringharness.Result{}, err
	}
	result.Steps = append(result.Steps, oracleStep)
	result.Passed = oracleStep.Passed
	return harness.finishResult(result)
}

type standardAuthoringHarnessImageReceipt struct {
	Format            string                  `json:"format"`
	Version           string                  `json:"version"`
	RunID             string                  `json:"run_id"`
	EnvironmentDigest workflowkit.Fingerprint `json:"environment_digest"`
	ImageTag          string                  `json:"image_tag"`
	ImageID           string                  `json:"image_id"`
}

func (harness *StandardAuthoringDockerHarness) ensureCandidateImage(ctx context.Context, request authoringharness.Request, candidate authoringharness.Candidate, invocationRoot, snapshotRoot string) (standardAuthoringHarnessImageReceipt, authoringharness.StepResult, bool, error) {
	receiptPath := harness.imageReceiptPath(request.RunID, candidate.EnvironmentDigest)
	if receipt, found, err := readStandardAuthoringHarnessImageReceipt(receiptPath, request.RunID, candidate.EnvironmentDigest); err != nil {
		return standardAuthoringHarnessImageReceipt{}, authoringharness.StepResult{}, false, err
	} else if found {
		imageID, inspected, fingerprint, inspectErr := harness.docker.inspectImage(ctx, invocationRoot, stageprovider.CodeEdgePhase1DockerBuildCommandID, receipt.ImageTag)
		if inspectErr == nil && imageID == receipt.ImageID {
			return receipt, harness.commandStep("docker_build", inspected, fingerprint, true, nil), true, nil
		}
		if inspectErr != nil && isStandardAuthoringHarnessAttestationError(inspectErr) {
			return standardAuthoringHarnessImageReceipt{}, authoringharness.StepResult{}, false, inspectErr
		}
	}

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
		Format: standardAuthoringDockerHarnessReceiptFormat, Version: standardAuthoringDockerHarnessReceiptVersion,
		RunID: request.RunID, EnvironmentDigest: candidate.EnvironmentDigest, ImageTag: imageTag, ImageID: imageID,
	}
	if err := writeStandardAuthoringHarnessImageReceipt(receiptPath, receipt); err != nil {
		return standardAuthoringHarnessImageReceipt{}, authoringharness.StepResult{}, false, err
	}
	return receipt, step, false, nil
}

// runSourceAccess proves the Dockerfile exposes the frozen source to the
// exact restricted runtime that will execute generated solution scripts. A
// successful image build alone is insufficient because source archive modes
// can make /workspace/source unreadable after capabilities are dropped.
func (harness *StandardAuthoringDockerHarness) runSourceAccess(ctx context.Context, image standardAuthoringHarnessImageReceipt, invocationRoot string) (authoringharness.StepResult, error) {
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
		codeEdgePhase1DockerRunArgs(image.ImageTag, checkout, "authoring-source-access-"+containerID, standardAuthoringDockerHarnessSourceAccessProgram), invocationRoot)
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

func (harness *StandardAuthoringDockerHarness) runInitial(ctx context.Context, request authoringharness.Request, image standardAuthoringHarnessImageReceipt, invocationRoot, snapshotRoot string) (authoringharness.StepResult, error) {
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
		codeEdgePhase1DockerRunArgs(image.ImageTag, checkout, "authoring-initial-"+containerID, "sh ./tests/test.sh"), invocationRoot)
	if runErr != nil && isStandardAuthoringHarnessFatalCommandError(ctx, runErr) {
		return authoringharness.StepResult{}, runErr
	}
	findings := []string{}
	passed := true
	if runErr != nil {
		passed = false
		findings = append(findings, "controlled initial verification could not complete: "+standardAuthoringHarnessSafeError(runErr))
	} else if err := verifyStandardAuthoringHarnessScripts(checkout, expected); err != nil {
		passed = false
		findings = append(findings, "initial verifier modified its immutable test script")
	} else if result.ExitCode == 0 {
		passed = false
		findings = append(findings, "initial verifier passed before the Oracle repair, so the task does not expose the intended problem")
	}
	return harness.commandStep("initial_verify", result, fingerprint, passed, findings), nil
}

func (harness *StandardAuthoringDockerHarness) runOracle(ctx context.Context, request authoringharness.Request, image standardAuthoringHarnessImageReceipt, invocationRoot, snapshotRoot string) (authoringharness.StepResult, error) {
	if step, ok, err := harness.reattestImage(ctx, invocationRoot, stageprovider.CodeEdgePhase1OracleVerifyCommandID, image); err != nil || !ok {
		return step, err
	}
	checkout := filepath.Join(invocationRoot, "verification", "oracle")
	expected, err := copyStandardAuthoringHarnessScripts(checkout, snapshotRoot, []string{authoringharness.SolveScriptRelativePath, authoringharness.TestScriptRelativePath})
	if err != nil {
		return authoringharness.StepResult{}, err
	}
	containerID, err := store.NewUUIDv7()
	if err != nil {
		return authoringharness.StepResult{}, err
	}
	result, fingerprint, runErr := harness.docker.run(ctx, stageprovider.CodeEdgePhase1OracleVerifyCommandID,
		codeEdgePhase1DockerRunArgs(image.ImageTag, checkout, "authoring-oracle-"+containerID, "sh ./solution/solve.sh && sh ./tests/test.sh"), invocationRoot)
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
		findings = append(findings, "Oracle repair followed by verifier did not pass")
	}
	return harness.commandStep("oracle_verify", result, fingerprint, passed, findings), nil
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
	value = strings.ReplaceAll(value, harness.stateRoot, "<harness-state>")
	value = strings.ReplaceAll(value, harness.authoringWorkspaceRoot, "<authoring-workspace>")
	return standardAuthoringDockerHarnessTokenPattern.ReplaceAllString(value, "<redacted-token>")
}

func writeStandardAuthoringHarnessSnapshot(root string, candidate authoringharness.Candidate) error {
	files := map[string][]byte{authoringharness.DockerfileRelativePath: candidate.Dockerfile}
	if candidate.Mode == authoringharness.ModeInitialOracle {
		files[authoringharness.SolveScriptRelativePath] = candidate.SolveScript
		files[authoringharness.TestScriptRelativePath] = candidate.TestScript
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	for relative, content := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if relative != authoringharness.DockerfileRelativePath {
			mode = 0o700
		}
		if err := writeNewBytesWithMode(path, content, mode); err != nil {
			return err
		}
	}
	return nil
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

func ensureStandardAuthoringHarnessDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Standard authoring harness state directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("Standard authoring harness state directory is unavailable or unsafe")
	}
	return nil
}

func (harness *StandardAuthoringDockerHarness) imageReceiptPath(runID string, digest workflowkit.Fingerprint) string {
	name := strings.TrimPrefix(string(digest), "sha256:") + ".json"
	return filepath.Join(harness.stateRoot, runID, "images", name)
}

func readStandardAuthoringHarnessImageReceipt(path, runID string, digest workflowkit.Fingerprint) (standardAuthoringHarnessImageReceipt, bool, error) {
	var receipt standardAuthoringHarnessImageReceipt
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return receipt, false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<20 {
		return receipt, false, errors.New("Standard authoring harness image receipt is unavailable or unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return receipt, false, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, false, errors.New("Standard authoring harness image receipt is malformed")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return receipt, false, errors.New("Standard authoring harness image receipt has trailing data")
	}
	if receipt.Format != standardAuthoringDockerHarnessReceiptFormat || receipt.Version != standardAuthoringDockerHarnessReceiptVersion ||
		receipt.RunID != runID || receipt.EnvironmentDigest != digest || receipt.ImageTag != standardAuthoringHarnessImageTag(runID, digest) {
		return receipt, false, errors.New("Standard authoring harness image receipt does not match the candidate")
	}
	if _, err := codeEdgePhase1DockerImageID([]byte(receipt.ImageID)); err != nil {
		return receipt, false, err
	}
	return receipt, true, nil
}

func writeStandardAuthoringHarnessImageReceipt(path string, receipt standardAuthoringHarnessImageReceipt) error {
	if receipt.Format != standardAuthoringDockerHarnessReceiptFormat || receipt.Version != standardAuthoringDockerHarnessReceiptVersion ||
		receipt.RunID == "" || receipt.EnvironmentDigest.Validate() != nil || receipt.ImageTag == "" {
		return errors.New("Standard authoring harness image receipt is invalid")
	}
	if _, err := codeEdgePhase1DockerImageID([]byte(receipt.ImageID)); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := ensureStandardAuthoringHarnessDirectory(filepath.Dir(directory)); err != nil {
		return err
	}
	if err := ensureStandardAuthoringHarnessDirectory(directory); err != nil {
		return err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".image-receipt-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
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

var _ authoringharness.Validator = (*StandardAuthoringDockerHarness)(nil)
