package repoprep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/repourl"
)

type Options struct {
	RepoURL            string
	Commit             string
	Workspace          string
	AllowLocal         bool
	MaxNetworkAttempts int
	RetryDelay         time.Duration
}

func Prepare(ctx context.Context, opts Options) (domain.RepoPrepared, error) {
	repoURL := strings.TrimSpace(opts.RepoURL)
	commit := strings.TrimSpace(opts.Commit)
	workspace := strings.TrimSpace(opts.Workspace)
	if repoURL == "" {
		return domain.RepoPrepared{}, fmt.Errorf("repo URL is required")
	}
	if err := repourl.RejectCredentials(repoURL); err != nil {
		return domain.RepoPrepared{}, err
	}
	publicURL, isGitHub := repourl.GitHubPublicHTTPSURL(repoURL)
	if !isGitHub && !opts.AllowLocal {
		return domain.RepoPrepared{}, fmt.Errorf("repo URL must be a public GitHub repository URL")
	}
	if commit == "" {
		return domain.RepoPrepared{}, fmt.Errorf("commit is required")
	}
	if workspace == "" {
		workspace = nodes.DefaultWorkspace("")
	}
	sourceDir := nodes.RepoSourcePath(workspace)
	if err := os.MkdirAll(filepath.Dir(sourceDir), 0o755); err != nil {
		return domain.RepoPrepared{}, err
	}
	if err := os.RemoveAll(sourceDir); err != nil {
		return domain.RepoPrepared{}, err
	}
	cloneURL := repoURL
	var gitEnv []string
	var commands []domain.CommandRun
	recordCommand := func(run *domain.CommandRun) {
		if run == nil {
			return
		}
		outputDir := nodes.RepoPrepareCommandLogDir(workspace, run.Name)
		if run.Attempt > 0 {
			outputDir = filepath.Join(outputDir, fmt.Sprintf("attempt-%d", run.Attempt))
		}
		_ = persistCommandOutput(run, outputDir)
		commands = append(commands, *run)
		_ = writeCommandLogs(workspace, commands)
	}
	if isGitHub {
		_, runs, err := runGitCommandWithRetry(ctx, "git_public_probe", publicGitEnv(), "", opts.MaxNetworkAttempts, opts.RetryDelay, nil, publicGitArgs("ls-remote", "--exit-code", publicURL, "HEAD")...)
		for i := range runs {
			recordCommand(&runs[i])
		}
		if err != nil {
			return domain.RepoPrepared{}, fmt.Errorf("repo must be publicly reachable without credentials: %w", err)
		}
		cloneURL = publicURL
		gitEnv = publicGitEnv()
	}
	cloneAttempts := 1
	if isGitHub {
		cloneAttempts = opts.MaxNetworkAttempts
	}
	_, cloneRuns, err := runGitCommandWithRetry(ctx, "git_clone", gitEnv, "", cloneAttempts, opts.RetryDelay, func(int) error {
		return os.RemoveAll(sourceDir)
	}, publicGitArgs("clone", "--no-checkout", cloneURL, sourceDir)...)
	for i := range cloneRuns {
		recordCommand(&cloneRuns[i])
	}
	if err != nil {
		return domain.RepoPrepared{}, err
	}
	_, run, err := runGitCommandWithEnv(ctx, "", gitEnv, sourceDir, publicGitArgs("checkout", commit)...)
	recordCommand(&run)
	if err != nil {
		return domain.RepoPrepared{}, err
	}
	resolved, run, err := runGitCommandWithEnv(ctx, "", gitEnv, sourceDir, publicGitArgs("rev-parse", "HEAD")...)
	recordCommand(&run)
	if err != nil {
		return domain.RepoPrepared{}, err
	}
	resolved = strings.TrimSpace(resolved)
	tree, run, err := runGitCommandWithEnv(ctx, "", gitEnv, sourceDir, publicGitArgs("rev-parse", "HEAD^{tree}")...)
	recordCommand(&run)
	if err != nil {
		return domain.RepoPrepared{}, err
	}
	tree = strings.TrimSpace(tree)
	prepared := domain.RepoPrepared{
		SchemaVersion:   "harbor.repo_prepared.v1",
		RepoURL:         cloneURL,
		RequestedCommit: commit,
		ResolvedCommit:  resolved,
		TreeHash:        tree,
		SourcePath:      sourceDir,
		CommandLogs:     commands,
		PreparedAt:      time.Now().UTC(),
	}
	data, err := json.MarshalIndent(prepared, "", "  ")
	if err != nil {
		return domain.RepoPrepared{}, err
	}
	if err := os.WriteFile(nodes.RepoPreparedPath(workspace), append(data, '\n'), 0o644); err != nil {
		return domain.RepoPrepared{}, err
	}
	if err := writeCommandLogs(workspace, commands); err != nil {
		return domain.RepoPrepared{}, err
	}
	return prepared, nil
}

func runGitCommand(ctx context.Context, dir string, args ...string) (string, domain.CommandRun, error) {
	return runGitCommandWithEnv(ctx, "", nil, dir, args...)
}

func runGitPublicProbe(ctx context.Context, publicURL string) (string, domain.CommandRun, error) {
	args := publicGitArgs("ls-remote", "--exit-code", publicURL, "HEAD")
	return runGitCommandWithEnv(ctx, "git_public_probe", publicGitEnv(), "", args...)
}

func runGitCommandWithRetry(ctx context.Context, name string, env []string, dir string, maxAttempts int, delay time.Duration, beforeAttempt func(int) error, args ...string) (string, []domain.CommandRun, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if delay <= 0 {
		delay = 500 * time.Millisecond
	}
	var runs []domain.CommandRun
	var lastOutput string
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if beforeAttempt != nil {
			if err := beforeAttempt(attempt); err != nil {
				return lastOutput, runs, err
			}
		}
		output, run, err := runGitCommandWithEnv(ctx, name, env, dir, args...)
		run.Attempt = attempt
		runs = append(runs, run)
		lastOutput, lastErr = output, err
		if err == nil {
			return output, runs, nil
		}
		if attempt == maxAttempts || !isTransientGitFailure(run, err) {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return lastOutput, runs, ctx.Err()
		case <-timer.C:
		}
	}
	return lastOutput, runs, lastErr
}

func isTransientGitFailure(run domain.CommandRun, err error) bool {
	text := strings.ToLower(run.Stdout + "\n" + run.Stderr)
	if err != nil {
		text += "\n" + strings.ToLower(err.Error())
	}
	for _, marker := range []string{
		"tls connection was non-properly terminated",
		"gnutls recv error",
		"connection reset",
		"connection timed out",
		"operation timed out",
		"temporary failure",
		"network is unreachable",
		"could not resolve host",
		"remote end hung up unexpectedly",
		"early eof",
		"http 500",
		"http 502",
		"http 503",
		"http 504",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func publicGitArgs(args ...string) []string {
	if len(args) == 0 {
		return nil
	}
	out := []string{
		"-c", "credential.helper=",
		"-c", "credential.useHttpPath=false",
	}
	return append(out, args...)
}

func runGitCommandWithEnv(ctx context.Context, name string, env []string, dir string, args ...string) (string, domain.CommandRun, error) {
	start := time.Now().UTC()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	finish := time.Now().UTC()
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	command := strings.Join(append([]string{"git"}, args...), " ")
	stdoutRaw := stdoutBuf.String()
	stderrRaw := stderrBuf.String()
	stdout := commandlog.RedactText(stdoutRaw)
	stderr := commandlog.RedactText(stderrRaw)
	if strings.TrimSpace(name) == "" {
		name = "git_" + safeCommandName(args)
	}
	run := domain.CommandRun{
		Name:         name,
		Command:      commandlog.RedactText(command),
		Argv:         commandlog.RedactArgv(append([]string{"git"}, args...)),
		Attempt:      1,
		Dir:          commandlog.ResolveCWD(dir),
		Env:          commandlog.RedactEnv(commandlog.EffectiveEnv(env)),
		ExitCode:     exitCode,
		Stdout:       stdout,
		Stderr:       stderr,
		FailureClass: commandlog.ClassifyFailure(exitCode, false, stdout, stderr),
		StartedAt:    start,
		FinishedAt:   finish,
		DurationMS:   finish.Sub(start).Milliseconds(),
		Passed:       err == nil,
	}
	if err != nil {
		detail := strings.TrimSpace(stderrRaw)
		if detail == "" {
			detail = strings.TrimSpace(stdoutRaw)
		}
		return stdoutRaw, run, fmt.Errorf("git %s: %w: %s", commandlog.RedactText(strings.Join(args, " ")), err, commandlog.RedactText(detail))
	}
	return stdoutRaw, run, nil
}

func publicGitEnv() []string {
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GCM_INTERACTIVE=Never",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	for _, key := range []string{"PATH", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func persistCommandOutput(run *domain.CommandRun, dir string) error {
	if run == nil {
		return nil
	}
	stdoutPath, stderrPath, err := commandlog.WriteOutputFiles(dir, run.Stdout, run.Stderr)
	if err != nil {
		return err
	}
	run.StdoutPath = stdoutPath
	run.StderrPath = stderrPath
	return nil
}

func writeCommandLogs(workspace string, commands []domain.CommandRun) error {
	commandLogPath := nodes.RepoPrepareCommandLogPath(workspace)
	if err := os.MkdirAll(filepath.Dir(commandLogPath), 0o755); err != nil {
		return err
	}
	logData, err := json.MarshalIndent(commands, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(commandLogPath, append(logData, '\n'), 0o644)
}

func safeCommandName(args []string) string {
	name := "unknown"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		name = arg
		break
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, name)
	return name
}
