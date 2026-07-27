package app

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// standardAuthoringSourceArchiveRoot is deliberately code-owned rather
	// than derived from a caller's repository URL. It keeps both archive and
	// run-scoped workspace paths stable while keeping user-controlled text out
	// of filesystem layout decisions.
	standardAuthoringSourceArchiveRoot     = "source/"
	standardAuthoringSourceArchiveMaxBytes = 128 * 1024 * 1024
	// StandardAuthoringSourceCaptureTimeout bounds one non-interactive Git
	// capture. The TUI renders this same deployment-independent bound while a
	// launch is in flight, so operators are not given a misleading cancel hint.
	StandardAuthoringSourceCaptureTimeout = 10 * time.Minute
	// StandardAuthoringSourceFetchTimeout bounds only the remote-facing Git
	// fetch. A separate host context is required because Git's HTTP low-speed
	// controls do not cover a stalled connection establishment phase.
	StandardAuthoringSourceFetchTimeout     = 2 * time.Minute
	standardAuthoringGitCommandOutputMax    = 64 * 1024
	standardAuthoringGitPAXGlobalHeaderName = "pax_global_header"
)

var (
	errStandardAuthoringSourceArchiveTooLarge = errors.New("Standard authoring source archive exceeds the fixed size limit")
	errStandardAuthoringGitOutputTooLarge     = errors.New("Standard authoring Git command output exceeds the fixed size limit")
)

// StandardAuthoringSourceCoordinate identifies the one immutable Git object
// that a Standard authoring launch is permitted to capture. It intentionally
// accepts only credential-free HTTPS and SSH URI forms; references, local
// paths, query/fragment selectors, and inline credentials are not a source
// identity.
type StandardAuthoringSourceCoordinate struct {
	RepositoryURL string
	CommitSHA     string
}

// Canonical returns the durable representation of a source coordinate. The
// caller must retain this exact result in the AuthoringSource, Task, session,
// and Run so the identity used to fetch the object is also the identity later
// audited and materialized.
func (coordinate StandardAuthoringSourceCoordinate) Canonical() (StandardAuthoringSourceCoordinate, error) {
	repositoryURL, err := store.NormalizeAuthoringRepositoryURL(coordinate.RepositoryURL)
	if err != nil {
		return StandardAuthoringSourceCoordinate{}, err
	}
	commitSHA, err := store.NormalizeAuthoringCommitSHA(coordinate.CommitSHA)
	if err != nil {
		return StandardAuthoringSourceCoordinate{}, err
	}
	return StandardAuthoringSourceCoordinate{RepositoryURL: repositoryURL, CommitSHA: commitSHA}, nil
}

// Validate reports whether the coordinate has a safe, immutable source
// identity. Canonicalization is intentionally available separately because
// callers need to persist one stable spelling of an accepted URL.
func (coordinate StandardAuthoringSourceCoordinate) Validate() error {
	_, err := coordinate.Canonical()
	return err
}

// StandardAuthoringGitArchiveSourceCapturer captures a caller-selected,
// validated immutable commit as a deterministic Git tar archive. The
// executable must be an explicit absolute non-symlink regular file; there is
// intentionally no PATH discovery fallback. The deployment composition should
// pass the same locked Git executable that its Standard authoring provider
// attests at stage time.
type StandardAuthoringGitArchiveSourceCapturer struct {
	gitExecutable string
	lockedGit     *stageprovider.LocalExecutableLock
	sshTransport  *standardAuthoringSSHSourceCaptureTransport
}

// StandardAuthoringSSHSourceCaptureTransportConfig contains only deployment
// lock facts and the narrowly scoped environment lookup used for an optional
// SSH agent socket. The lookup is never called for HTTPS and its value is
// never made durable. Production composition must pass an explicit lookup;
// nil means no agent, never an ambient SSH_AUTH_SOCK fallback.
type StandardAuthoringSSHSourceCaptureTransportConfig struct {
	ContractRoot      string
	Transport         stageprovider.StandardAuthoringSSHTransportLock
	LookupEnvironment func(string) (string, bool)
}

type standardAuthoringSSHSourceCaptureTransport struct {
	contractRoot      string
	transport         stageprovider.StandardAuthoringSSHTransportLock
	lookupEnvironment func(string) (string, bool)
}

// NewStandardAuthoringGitArchiveSourceCapturer validates a deployment-owned
// Git executable. It does not invoke Git, inspect a remote, or read an
// environment variable; acquisition starts only through Capture.
func NewStandardAuthoringGitArchiveSourceCapturer(gitExecutable string) (*StandardAuthoringGitArchiveSourceCapturer, error) {
	gitExecutable, err := standardAuthoringRegularExecutable(gitExecutable)
	if err != nil {
		return nil, fmt.Errorf("Standard authoring Git executable: %w", err)
	}
	return &StandardAuthoringGitArchiveSourceCapturer{gitExecutable: gitExecutable}, nil
}

// NewLockedStandardAuthoringGitArchiveSourceCapturer additionally binds
// source capture to the exact local.command record selected by a generated
// Standard operation lock. Capture rechecks the executable's bytes and
// `git --version` immediately before it contacts the requested source remote.
// This is separate from (and complementary to) the per-stage runtime
// attestor because source capture occurs before an AuthoringSession exists.
func NewLockedStandardAuthoringGitArchiveSourceCapturer(locked stageprovider.LocalExecutableLock) (*StandardAuthoringGitArchiveSourceCapturer, error) {
	if locked.CommandID != stageprovider.StandardAuthoringGitSnapshotCommandID || strings.TrimSpace(locked.Version) == "" {
		return nil, fmt.Errorf("Standard authoring Git lock is invalid")
	}
	if err := locked.ContentSHA256.Validate(); err != nil {
		return nil, fmt.Errorf("Standard authoring Git lock content fingerprint: %w", err)
	}
	capturer, err := NewStandardAuthoringGitArchiveSourceCapturer(locked.AbsolutePath)
	if err != nil {
		return nil, err
	}
	copy := locked
	capturer.lockedGit = &copy
	return capturer, nil
}

// NewLockedStandardAuthoringGitArchiveSourceCapturerWithSSHTransport binds a
// source capturer to the lock-owned SSH acquisition contract. HTTPS capture
// remains credential-free and unchanged; SSH capture becomes available only
// through the pinned client, wrapper shell, packaged known_hosts allow-list,
// and optional explicitly named agent socket described by this configuration.
func NewLockedStandardAuthoringGitArchiveSourceCapturerWithSSHTransport(locked stageprovider.LocalExecutableLock, config StandardAuthoringSSHSourceCaptureTransportConfig) (*StandardAuthoringGitArchiveSourceCapturer, error) {
	capturer, err := NewLockedStandardAuthoringGitArchiveSourceCapturer(locked)
	if err != nil {
		return nil, err
	}
	if err := config.Transport.Validate(); err != nil {
		return nil, fmt.Errorf("Standard authoring SSH transport lock: %w", err)
	}
	contractRoot, err := filepath.Abs(strings.TrimSpace(config.ContractRoot))
	if err != nil || filepath.Clean(contractRoot) != contractRoot {
		return nil, fmt.Errorf("Standard authoring SSH contract root is invalid")
	}
	if _, err := stageprovider.ReadStandardAuthoringSSHKnownHostsAsset(contractRoot, config.Transport.KnownHosts); err != nil {
		return nil, fmt.Errorf("Standard authoring SSH known_hosts asset: %w", err)
	}
	transport := config.Transport.Clone()
	capturer.sshTransport = &standardAuthoringSSHSourceCaptureTransport{
		contractRoot: contractRoot, transport: transport, lookupEnvironment: config.LookupEnvironment,
	}
	return capturer, nil
}

// CaptureStandardAuthoringSource fetches one caller-selected but fully
// validated immutable commit into a private temporary bare repository,
// verifies the resolved commit object, and archives that exact object. No
// mutable ref, checkout path, credential, global Git config, or interactive
// prompt participates in this operation. The temporary repository is deleted
// before this method returns.
func (capturer *StandardAuthoringGitArchiveSourceCapturer) CaptureStandardAuthoringSource(ctx context.Context, source StandardAuthoringSourceCoordinate) (StandardAuthoringSourceSnapshot, error) {
	if capturer == nil || strings.TrimSpace(capturer.gitExecutable) == "" {
		return StandardAuthoringSourceSnapshot{}, ErrStandardAuthoringLaunchUnavailable
	}
	if ctx == nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("Standard authoring source capture context is required")
	}
	if err := ctx.Err(); err != nil {
		return StandardAuthoringSourceSnapshot{}, err
	}
	coordinate, err := source.Canonical()
	if err != nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("validate Standard authoring source coordinate: %w", err)
	}
	captureContext, cancel := context.WithTimeout(ctx, StandardAuthoringSourceCaptureTimeout)
	defer cancel()

	temporaryRoot, err := os.MkdirTemp("", "harbor-standard-authoring-source-")
	if err != nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("create Standard authoring source capture directory: %w", err)
	}
	defer os.RemoveAll(temporaryRoot)
	if err := capturer.verifyLockedGit(captureContext, temporaryRoot); err != nil {
		return StandardAuthoringSourceSnapshot{}, err
	}
	repository := filepath.Join(temporaryRoot, "source.git")

	if _, err := capturer.runGit(captureContext, temporaryRoot, "init", "--bare", repository); err != nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("initialize private Standard authoring Git repository: %w", err)
	}
	fetchEnvironment, err := capturer.sourceFetchEnvironment(captureContext, temporaryRoot, coordinate)
	if err != nil {
		return StandardAuthoringSourceSnapshot{}, err
	}
	fetchContext, fetchCancel := context.WithTimeout(captureContext, StandardAuthoringSourceFetchTimeout)
	defer fetchCancel()
	if _, err := capturer.runGitWithEnvironment(fetchContext, temporaryRoot, standardAuthoringGitCommandOutputMax, fetchEnvironment, "--git-dir", repository, "fetch", "--no-tags", "--depth=1", coordinate.RepositoryURL, coordinate.CommitSHA); err != nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("fetch requested Standard authoring Git commit: %w", err)
	}
	resolved, err := capturer.runGit(captureContext, temporaryRoot, "--git-dir", repository, "rev-parse", "--verify", coordinate.CommitSHA+"^{commit}")
	if err != nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("verify requested Standard authoring Git commit: %w", err)
	}
	if strings.TrimSpace(string(resolved)) != coordinate.CommitSHA {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("requested Standard authoring Git commit resolved to another object")
	}
	archive, err := capturer.runGitArchive(captureContext, temporaryRoot, repository, coordinate)
	if err != nil {
		return StandardAuthoringSourceSnapshot{}, err
	}
	if err := validateStandardAuthoringSourceArchive(archive, coordinate); err != nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("validate captured Standard authoring Git archive: %w", err)
	}
	return StandardAuthoringSourceSnapshot{
		RepositoryURL: coordinate.RepositoryURL,
		CommitSHA:     coordinate.CommitSHA,
		SchemaVersion: StandardAuthoringSourceSnapshotSchemaVersion,
		Content:       archive,
	}, nil
}

func (capturer *StandardAuthoringGitArchiveSourceCapturer) verifyLockedGit(ctx context.Context, temporaryRoot string) error {
	if capturer == nil || capturer.lockedGit == nil {
		return nil
	}
	if current, err := standardAuthoringRegularExecutable(capturer.gitExecutable); err != nil || current != capturer.lockedGit.AbsolutePath {
		return fmt.Errorf("locked Standard authoring Git executable path is no longer safe")
	}
	contents, err := os.ReadFile(capturer.gitExecutable)
	if err != nil || workflowkit.SHA256Fingerprint(contents) != capturer.lockedGit.ContentSHA256 {
		return fmt.Errorf("locked Standard authoring Git executable bytes do not match")
	}
	version, err := capturer.runGit(ctx, temporaryRoot, "--version")
	if err != nil || strings.TrimSpace(string(version)) != "git version "+capturer.lockedGit.Version {
		return fmt.Errorf("locked Standard authoring Git version does not match")
	}
	return nil
}

func (capturer *StandardAuthoringGitArchiveSourceCapturer) runGitArchive(ctx context.Context, workdir, repository string, coordinate StandardAuthoringSourceCoordinate) ([]byte, error) {
	archive, err := capturer.runGitWithLimit(ctx, workdir, int64(standardAuthoringSourceArchiveMaxBytes), "--git-dir", repository, "archive", "--format=tar", "--prefix="+standardAuthoringSourceArchiveRoot, coordinate.CommitSHA)
	if err != nil {
		if errors.Is(err, errStandardAuthoringGitOutputTooLarge) {
			return nil, errStandardAuthoringSourceArchiveTooLarge
		}
		return nil, fmt.Errorf("archive requested Standard authoring Git commit: %w", err)
	}
	return archive, nil
}

func (capturer *StandardAuthoringGitArchiveSourceCapturer) runGit(ctx context.Context, workdir string, arguments ...string) ([]byte, error) {
	return capturer.runGitWithLimit(ctx, workdir, standardAuthoringGitCommandOutputMax, arguments...)
}

func (capturer *StandardAuthoringGitArchiveSourceCapturer) runGitWithLimit(ctx context.Context, workdir string, limit int64, arguments ...string) ([]byte, error) {
	return capturer.runGitWithEnvironment(ctx, workdir, limit, standardAuthoringGitEnvironment(workdir), arguments...)
}

func (capturer *StandardAuthoringGitArchiveSourceCapturer) runGitWithEnvironment(ctx context.Context, workdir string, limit int64, environment []string, arguments ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output := newStandardAuthoringLimitedBuffer(limit)
	stderr := newStandardAuthoringLimitedBuffer(standardAuthoringGitCommandOutputMax)
	command := exec.CommandContext(ctx, capturer.gitExecutable, arguments...)
	standardAuthoringConfigureGitCommand(command)
	command.Dir = workdir
	command.Env = append([]string(nil), environment...)
	command.Stdout = output
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if output.exceeded || stderr.exceeded {
			return nil, errStandardAuthoringGitOutputTooLarge
		}
		// Do not include Git's remote output in a durable error. It can contain
		// repository-controlled content and has no value as a stable operator
		// contract; callers receive the fixed contextual error at the boundary.
		return nil, errors.New("controlled Git command failed")
	}
	if output.exceeded || stderr.exceeded {
		return nil, errStandardAuthoringGitOutputTooLarge
	}
	return append([]byte(nil), output.Bytes()...), nil
}

func standardAuthoringGitEnvironment(temporaryRoot string) []string {
	// Git's HTTPS helper discovers platform TLS material itself. We deliberately
	// clear only mutable configuration/prompt inputs and retain no credential or
	// provider environment value in a Run, source archive, or error message.
	return []string{
		"HOME=" + temporaryRoot,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LANG=C",
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		// Keep HTTPS capture on the broadly compatible HTTP/1.1 transport and
		// fail a stalled remote promptly enough for the TUI retry boundary. These
		// are invocation-scoped Git config values, not ambient user settings.
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=http.version",
		"GIT_CONFIG_VALUE_0=HTTP/1.1",
		"GIT_CONFIG_KEY_1=http.lowSpeedLimit",
		"GIT_CONFIG_VALUE_1=1",
		"GIT_CONFIG_KEY_2=http.lowSpeedTime",
		"GIT_CONFIG_VALUE_2=45",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https:ssh",
		"GIT_PROTOCOL_FROM_USER=0",
		"SSH_ASKPASS_REQUIRE=never",
	}
}

// sourceFetchEnvironment selects the one transport that may contact the
// caller-selected remote. HTTPS has no credential or SSH configuration. SSH
// is available only when production composition supplied the fully verified
// transport contract; this check occurs before Git receives the remote URL.
func (capturer *StandardAuthoringGitArchiveSourceCapturer) sourceFetchEnvironment(ctx context.Context, temporaryRoot string, source StandardAuthoringSourceCoordinate) ([]string, error) {
	parsed, err := url.Parse(source.RepositoryURL)
	if err != nil {
		return nil, fmt.Errorf("parse Standard authoring source transport")
	}
	switch parsed.Scheme {
	case "https":
		return standardAuthoringGitEnvironment(temporaryRoot), nil
	case "ssh":
		if capturer == nil || capturer.sshTransport == nil {
			return nil, fmt.Errorf("Standard authoring SSH source capture is not configured")
		}
		return capturer.sshTransport.gitEnvironment(ctx, temporaryRoot, parsed)
	default:
		return nil, fmt.Errorf("Standard authoring source transport is not permitted")
	}
}

func (transport *standardAuthoringSSHSourceCaptureTransport) gitEnvironment(ctx context.Context, temporaryRoot string, sourceURL *url.URL) ([]string, error) {
	if transport == nil || sourceURL == nil || sourceURL.Scheme != "ssh" {
		return nil, fmt.Errorf("Standard authoring SSH transport is unavailable")
	}
	if sourceURL.User == nil || !standardAuthoringSSHUsername(sourceURL.User.Username()) {
		return nil, fmt.Errorf("Standard authoring SSH source user is invalid")
	}
	hostname := strings.ToLower(strings.TrimSpace(sourceURL.Hostname()))
	port := strings.TrimSpace(sourceURL.Port())
	knownHosts, err := stageprovider.ReadStandardAuthoringSSHKnownHostsAsset(transport.contractRoot, transport.transport.KnownHosts)
	if err != nil {
		return nil, fmt.Errorf("verify Standard authoring SSH known_hosts asset: %w", err)
	}
	allowed, err := stageprovider.StandardAuthoringSSHKnownHostsAllowsHost(knownHosts, hostname, port)
	if err != nil {
		return nil, fmt.Errorf("validate Standard authoring SSH source host: %w", err)
	}
	if !allowed {
		return nil, fmt.Errorf("Standard authoring SSH source host is not in the packaged known_hosts allow-list")
	}
	if err := transport.verifyLockedExecutables(ctx, temporaryRoot); err != nil {
		return nil, err
	}
	agentSocket, err := transport.optionalAgentSocket()
	if err != nil {
		return nil, err
	}
	knownHostsPath, err := standardAuthoringWriteKnownHostsSnapshot(temporaryRoot, knownHosts)
	if err != nil {
		return nil, err
	}
	wrapper, err := transport.writeSSHWrapper(temporaryRoot, knownHostsPath, agentSocket)
	if err != nil {
		return nil, err
	}
	environment := standardAuthoringGitEnvironment(temporaryRoot)
	environment = append(environment, "GIT_SSH="+wrapper, "GIT_SSH_VARIANT=ssh")
	return environment, nil
}

func (transport *standardAuthoringSSHSourceCaptureTransport) verifyLockedExecutables(ctx context.Context, temporaryRoot string) error {
	if transport == nil {
		return fmt.Errorf("Standard authoring SSH transport is unavailable")
	}
	if err := standardAuthoringVerifyLockedExecutable(transport.transport.SSHExecutable); err != nil {
		return fmt.Errorf("verify Standard authoring locked SSH executable: %w", err)
	}
	if err := standardAuthoringVerifyLockedExecutable(transport.transport.WrapperShell); err != nil {
		return fmt.Errorf("verify Standard authoring locked SSH wrapper shell: %w", err)
	}
	version, err := standardAuthoringSSHVersion(ctx, temporaryRoot, transport.transport.SSHExecutable.AbsolutePath)
	if err != nil || version != transport.transport.SSHExecutable.Version {
		return fmt.Errorf("verify Standard authoring locked SSH version")
	}
	return nil
}

func standardAuthoringVerifyLockedExecutable(locked stageprovider.LocalExecutableLock) error {
	path, err := standardAuthoringRegularExecutable(locked.AbsolutePath)
	if err != nil || path != locked.AbsolutePath {
		return errors.New("locked executable path is unavailable")
	}
	contents, err := os.ReadFile(path)
	if err != nil || workflowkit.SHA256Fingerprint(contents) != locked.ContentSHA256 {
		return errors.New("locked executable bytes do not match")
	}
	return nil
}

func standardAuthoringSSHVersion(ctx context.Context, workdir, executable string) (string, error) {
	output := newStandardAuthoringLimitedBuffer(standardAuthoringGitCommandOutputMax)
	stderr := newStandardAuthoringLimitedBuffer(standardAuthoringGitCommandOutputMax)
	command := exec.CommandContext(ctx, executable, "-V")
	command.Dir = workdir
	command.Env = standardAuthoringGitEnvironment(workdir)
	command.Stdout = output
	command.Stderr = stderr
	if err := command.Run(); err != nil || output.exceeded || stderr.exceeded || ctx.Err() != nil {
		return "", errors.New("controlled SSH version command failed")
	}
	value := strings.TrimSpace(string(append(append([]byte(nil), output.Bytes()...), stderr.Bytes()...)))
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("SSH version output is invalid")
	}
	parts := strings.Fields(value)
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "OpenSSH_") {
		return "", errors.New("SSH version output is invalid")
	}
	return parts[0], nil
}

func (transport *standardAuthoringSSHSourceCaptureTransport) optionalAgentSocket() (string, error) {
	if transport == nil || transport.lookupEnvironment == nil {
		return "", nil
	}
	value, present := transport.lookupEnvironment(transport.transport.AgentSocketEnvironmentName)
	if !present || strings.TrimSpace(value) == "" {
		return "", nil
	}
	if value != strings.TrimSpace(value) {
		return "", fmt.Errorf("configured Standard authoring SSH agent socket is invalid")
	}
	if err := standardAuthoringUnixSocket(value); err != nil {
		return "", fmt.Errorf("configured Standard authoring SSH agent socket is unavailable")
	}
	return value, nil
}

func standardAuthoringUnixSocket(value string) error {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("socket path is invalid")
	}
	for current := value; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("socket path is unavailable")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	info, err := os.Lstat(value)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return errors.New("socket is unavailable")
	}
	return nil
}

// standardAuthoringWriteKnownHostsSnapshot closes the deployment-file TOCTOU
// window after its hash and contents have been verified. OpenSSH receives only
// this private, immutable-for-the-capture copy rather than reopening the
// package path later during Git's SSH transport.
func standardAuthoringWriteKnownHostsSnapshot(temporaryRoot string, contents []byte) (string, error) {
	if len(contents) == 0 || len(contents) > stageprovider.StandardAuthoringSSHKnownHostsMaxBytes {
		return "", errors.New("controlled SSH known_hosts snapshot is invalid")
	}
	path := filepath.Join(temporaryRoot, "ssh-known_hosts")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", errors.New("create controlled SSH known_hosts snapshot")
	}
	if _, writeErr := file.Write(contents); writeErr != nil || file.Sync() != nil || file.Close() != nil {
		file.Close()
		_ = os.Remove(path)
		return "", errors.New("write controlled SSH known_hosts snapshot")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != int64(len(contents)) {
		_ = os.Remove(path)
		return "", errors.New("verify controlled SSH known_hosts snapshot")
	}
	return path, nil
}

func (transport *standardAuthoringSSHSourceCaptureTransport) writeSSHWrapper(temporaryRoot, knownHostsPath, agentSocket string) (string, error) {
	if transport == nil {
		return "", errors.New("SSH transport is unavailable")
	}
	arguments := []string{
		transport.transport.SSHExecutable.AbsolutePath,
		"-F", "/dev/null",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "UpdateHostKeys=no",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "ChallengeResponseAuthentication=no",
		"-o", "HostbasedAuthentication=no",
		"-o", "GSSAPIAuthentication=no",
		"-o", "PubkeyAuthentication=yes",
		"-o", "NumberOfPasswordPrompts=0",
		"-o", "PreferredAuthentications=publickey",
		"-o", "ForwardAgent=no",
		"-o", "PermitLocalCommand=no",
		"-o", "ProxyCommand=none",
		"-o", "ProxyJump=none",
	}
	if agentSocket == "" {
		arguments = append(arguments, "-o", "IdentityAgent=none")
	} else {
		arguments = append(arguments, "-o", "IdentityAgent="+agentSocket)
	}
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		quoted = append(quoted, standardAuthoringPOSIXShellQuote(argument))
	}
	contents := "#!" + transport.transport.WrapperShell.AbsolutePath + "\nexec " + strings.Join(quoted, " ") + " \"$@\"\n"
	path := filepath.Join(temporaryRoot, "git-ssh-wrapper")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return "", errors.New("create controlled SSH wrapper")
	}
	if _, writeErr := io.WriteString(file, contents); writeErr != nil || file.Sync() != nil || file.Close() != nil {
		file.Close()
		_ = os.Remove(path)
		return "", errors.New("write controlled SSH wrapper")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.Remove(path)
		return "", errors.New("seal controlled SSH wrapper")
	}
	if _, err := standardAuthoringRegularExecutable(path); err != nil {
		_ = os.Remove(path)
		return "", errors.New("verify controlled SSH wrapper")
	}
	return path, nil
}

func standardAuthoringPOSIXShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func standardAuthoringSSHUsername(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') ||
			(index > 0 && (character == '.' || character == '_' || character == '-')) {
			continue
		}
		return false
	}
	return true
}

func standardAuthoringRegularExecutable(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !filepath.IsAbs(value) {
		return "", errors.New("must be an absolute path")
	}
	absolute, err := filepath.Abs(value)
	if err != nil || filepath.Clean(absolute) != absolute {
		return "", errors.New("must be a clean absolute path")
	}
	for current := absolute; ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return "", errors.New("cannot inspect path")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("must not contain a symbolic link")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", errors.New("must be an executable regular file")
	}
	return absolute, nil
}

// validateStandardAuthoringSourceArchive checks only the structural properties
// required to retain a Git-produced archive. The source was already captured
// from a verified commit by the controlled Git invocation, so archive metadata
// and entry types are deliberately opaque at this boundary. Workspace
// projection handles Git's regular files and links; unfamiliar tar entry types
// are retained without blocking task creation.
func validateStandardAuthoringSourceArchive(raw []byte, source StandardAuthoringSourceCoordinate) error {
	if _, err := source.Canonical(); err != nil {
		return fmt.Errorf("archive source coordinate is invalid: %w", err)
	}
	if len(raw) == 0 {
		return errors.New("archive is empty")
	}
	if len(raw) > standardAuthoringSourceArchiveMaxBytes {
		return errStandardAuthoringSourceArchiveTooLarge
	}
	reader := tar.NewReader(bytes.NewReader(raw))
	seen := make(map[string]struct{})
	regularFiles := 0
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("archive cannot be read")
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := header.Name
		if !standardAuthoringArchivePath(name) {
			return errors.New("archive contains an unsafe path")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("archive contains a duplicate path")
		}
		seen[name] = struct{}{}
		if header.Size < 0 {
			return errors.New("archive entry has an invalid size")
		}
		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > standardAuthoringSourceArchiveMaxBytes-total {
				return errStandardAuthoringSourceArchiveTooLarge
			}
			copied, copyErr := io.Copy(io.Discard, reader)
			if copyErr != nil || copied != header.Size {
				return errors.New("archive regular file cannot be read")
			}
			total += copied
			regularFiles++
		default:
			if _, err := io.Copy(io.Discard, reader); err != nil {
				return errors.New("archive entry cannot be read")
			}
		}
	}
	if regularFiles == 0 {
		return errors.New("archive has no regular files")
	}
	return nil
}

// standardAuthoringArchiveSymlinkTarget permits a relative link only when its
// lexical destination remains inside the archive root. The target may be
// dangling because that is a valid Git source-tree state.
func standardAuthoringArchiveSymlinkTarget(name, target string) bool {
	if target == "" || strings.Contains(target, "\\") || strings.HasPrefix(target, "/") || strings.ContainsRune(target, '\x00') {
		return false
	}
	return standardAuthoringArchivePath(path.Clean(path.Join(path.Dir(name), target)))
}

func standardAuthoringArchiveHardLinkTarget(target string) bool {
	return standardAuthoringArchivePath(target)
}

func standardAuthoringArchivePath(name string) bool {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || strings.ContainsRune(name, '\x00') {
		return false
	}
	if name != standardAuthoringSourceArchiveRoot && !strings.HasPrefix(name, standardAuthoringSourceArchiveRoot) {
		return false
	}
	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != strings.TrimSuffix(name, "/") {
		return false
	}
	return true
}

type standardAuthoringLimitedBuffer struct {
	limit    int64
	buffer   bytes.Buffer
	exceeded bool
}

func newStandardAuthoringLimitedBuffer(limit int64) *standardAuthoringLimitedBuffer {
	return &standardAuthoringLimitedBuffer{limit: limit}
}

func (buffer *standardAuthoringLimitedBuffer) Write(value []byte) (int, error) {
	if buffer == nil || buffer.limit < 0 || int64(buffer.buffer.Len())+int64(len(value)) > buffer.limit {
		if buffer != nil {
			buffer.exceeded = true
		}
		return 0, errStandardAuthoringGitOutputTooLarge
	}
	return buffer.buffer.Write(value)
}

func (buffer *standardAuthoringLimitedBuffer) Bytes() []byte {
	if buffer == nil {
		return nil
	}
	return buffer.buffer.Bytes()
}

var _ StandardAuthoringSourceCapturer = (*StandardAuthoringGitArchiveSourceCapturer)(nil)
