package app

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringSourceArchiveRoot     = "tower-http/"
	standardAuthoringSourceArchiveMaxBytes = 128 * 1024 * 1024
	standardAuthoringSourceCaptureTimeout  = 10 * time.Minute
	standardAuthoringGitCommandOutputMax   = 64 * 1024
)

var (
	errStandardAuthoringSourceArchiveTooLarge = errors.New("Standard authoring source archive exceeds the fixed size limit")
	errStandardAuthoringGitOutputTooLarge     = errors.New("Standard authoring Git command output exceeds the fixed size limit")
)

// StandardAuthoringGitArchiveSourceCapturer captures only the approved Tower
// HTTP commit as a deterministic Git tar archive. The executable must be an
// explicit absolute non-symlink regular file; there is intentionally no PATH
// discovery fallback. The deployment composition should pass the same locked
// Git executable that its Standard authoring provider attests at stage time.
type StandardAuthoringGitArchiveSourceCapturer struct {
	gitExecutable string
	lockedGit     *stageprovider.LocalExecutableLock
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
// `git --version` immediately before it contacts the public source remote.
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

// CaptureStandardAuthoringSource fetches only the fixed immutable commit into
// a private temporary bare repository, verifies the resolved commit object,
// and archives that exact object. No mutable ref, user URL, checkout path,
// credential, global Git config, or interactive prompt participates in this
// operation. The temporary repository is deleted before this method returns.
func (capturer *StandardAuthoringGitArchiveSourceCapturer) CaptureStandardAuthoringSource(ctx context.Context) (StandardAuthoringSourceSnapshot, error) {
	if capturer == nil || strings.TrimSpace(capturer.gitExecutable) == "" {
		return StandardAuthoringSourceSnapshot{}, ErrStandardAuthoringLaunchUnavailable
	}
	if ctx == nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("Standard authoring source capture context is required")
	}
	if err := ctx.Err(); err != nil {
		return StandardAuthoringSourceSnapshot{}, err
	}
	captureContext, cancel := context.WithTimeout(ctx, standardAuthoringSourceCaptureTimeout)
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
	if _, err := capturer.runGit(captureContext, temporaryRoot, "--git-dir", repository, "fetch", "--no-tags", "--depth=1", StandardAuthoringSourceRepositoryURL, StandardAuthoringSourceCommit); err != nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("fetch approved Standard authoring Git commit: %w", err)
	}
	resolved, err := capturer.runGit(captureContext, temporaryRoot, "--git-dir", repository, "rev-parse", "--verify", StandardAuthoringSourceCommit+"^{commit}")
	if err != nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("verify approved Standard authoring Git commit: %w", err)
	}
	if strings.TrimSpace(string(resolved)) != StandardAuthoringSourceCommit {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("approved Standard authoring Git commit resolved to another object")
	}
	archive, err := capturer.runGitArchive(captureContext, temporaryRoot, repository)
	if err != nil {
		return StandardAuthoringSourceSnapshot{}, err
	}
	if err := validateStandardAuthoringSourceArchive(archive); err != nil {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("validate captured Standard authoring Git archive: %w", err)
	}
	return StandardAuthoringSourceSnapshot{
		RepositoryURL: StandardAuthoringSourceRepositoryURL,
		CommitSHA:     StandardAuthoringSourceCommit,
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

func (capturer *StandardAuthoringGitArchiveSourceCapturer) runGitArchive(ctx context.Context, workdir, repository string) ([]byte, error) {
	archive, err := capturer.runGitWithLimit(ctx, workdir, int64(standardAuthoringSourceArchiveMaxBytes), "--git-dir", repository, "archive", "--format=tar", "--prefix="+standardAuthoringSourceArchiveRoot, StandardAuthoringSourceCommit)
	if err != nil {
		if errors.Is(err, errStandardAuthoringGitOutputTooLarge) {
			return nil, errStandardAuthoringSourceArchiveTooLarge
		}
		return nil, fmt.Errorf("archive approved Standard authoring Git commit: %w", err)
	}
	return archive, nil
}

func (capturer *StandardAuthoringGitArchiveSourceCapturer) runGit(ctx context.Context, workdir string, arguments ...string) ([]byte, error) {
	return capturer.runGitWithLimit(ctx, workdir, standardAuthoringGitCommandOutputMax, arguments...)
}

func (capturer *StandardAuthoringGitArchiveSourceCapturer) runGitWithLimit(ctx context.Context, workdir string, limit int64, arguments ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output := newStandardAuthoringLimitedBuffer(limit)
	stderr := newStandardAuthoringLimitedBuffer(standardAuthoringGitCommandOutputMax)
	command := exec.CommandContext(ctx, capturer.gitExecutable, arguments...)
	command.Dir = workdir
	command.Env = standardAuthoringGitEnvironment(workdir)
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
		"GIT_TERMINAL_PROMPT=0",
	}
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

// validateStandardAuthoringSourceArchive proves the source object is a safe
// deterministic Git archive before it becomes durable evidence. It refuses
// paths outside the fixed archive root, duplicate names, links, devices, and
// other non-regular entries, so a later controlled checkout cannot interpret
// a source snapshot as a filesystem escape hatch.
func validateStandardAuthoringSourceArchive(raw []byte) error {
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
		name := header.Name
		if !standardAuthoringArchivePath(name) {
			return errors.New("archive contains an unsafe path")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("archive contains a duplicate path")
		}
		seen[name] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return errors.New("archive directory has content")
			}
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
			return errors.New("archive contains a link or unsupported entry")
		}
	}
	if regularFiles == 0 {
		return errors.New("archive has no regular files")
	}
	return nil
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
