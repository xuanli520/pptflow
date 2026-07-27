package app

import (
	"archive/tar"
	"bytes"
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringSourceCoordinateCanonical(t *testing.T) {
	commit40 := strings.Repeat("a", 40)
	commit64 := strings.Repeat("b", 64)
	for _, test := range []struct {
		name string
		in   StandardAuthoringSourceCoordinate
		want StandardAuthoringSourceCoordinate
	}{
		{
			name: "HTTPS canonicalizes host and trailing slash",
			in:   StandardAuthoringSourceCoordinate{RepositoryURL: "https://GitHub.com/example/repository.git/", CommitSHA: commit40},
			want: StandardAuthoringSourceCoordinate{RepositoryURL: "https://github.com/example/repository.git", CommitSHA: commit40},
		},
		{
			name: "SSH URI preserves credential-free login user",
			in:   StandardAuthoringSourceCoordinate{RepositoryURL: "ssh://git@GitHub.com/example/repository.git", CommitSHA: commit40},
			want: StandardAuthoringSourceCoordinate{RepositoryURL: "ssh://git@github.com/example/repository.git", CommitSHA: commit40},
		},
		{
			name: "scp-like SSH canonicalizes to SSH URI",
			in:   StandardAuthoringSourceCoordinate{RepositoryURL: "git@GitHub.com:example/repository.git", CommitSHA: commit64},
			want: StandardAuthoringSourceCoordinate{RepositoryURL: "ssh://git@github.com/example/repository.git", CommitSHA: commit64},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.in.Canonical()
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("canonical coordinate = %+v, want %+v", got, test.want)
			}
			if err := test.in.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestStandardAuthoringSourceCoordinateRejectsMutableOrUnsafeInput(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, test := range []struct {
		name string
		in   StandardAuthoringSourceCoordinate
	}{
		{"local path", StandardAuthoringSourceCoordinate{RepositoryURL: "/tmp/repository", CommitSHA: commit}},
		{"file URI", StandardAuthoringSourceCoordinate{RepositoryURL: "file:///tmp/repository", CommitSHA: commit}},
		{"git protocol", StandardAuthoringSourceCoordinate{RepositoryURL: "git://github.com/example/repository.git", CommitSHA: commit}},
		{"HTTPS credential", StandardAuthoringSourceCoordinate{RepositoryURL: "https://token@github.com/example/repository.git", CommitSHA: commit}},
		{"SSH password", StandardAuthoringSourceCoordinate{RepositoryURL: "ssh://git:secret@github.com/example/repository.git", CommitSHA: commit}},
		{"query selector", StandardAuthoringSourceCoordinate{RepositoryURL: "https://github.com/example/repository.git?ref=main", CommitSHA: commit}},
		{"fragment selector", StandardAuthoringSourceCoordinate{RepositoryURL: "https://github.com/example/repository.git#main", CommitSHA: commit}},
		{"unsafe repository path", StandardAuthoringSourceCoordinate{RepositoryURL: "https://github.com/example/../repository.git", CommitSHA: commit}},
		{"branch instead of commit", StandardAuthoringSourceCoordinate{RepositoryURL: "https://github.com/example/repository.git", CommitSHA: "main"}},
		{"uppercase commit", StandardAuthoringSourceCoordinate{RepositoryURL: "https://github.com/example/repository.git", CommitSHA: strings.Repeat("A", 40)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.in.Validate(); err == nil {
				t.Fatalf("Validate() accepted %+v", test.in)
			}
		})
	}
}

func TestStandardAuthoringGitArchiveSourceCapturerExecutesControlledHTTPSCapture(t *testing.T) {
	coordinate := StandardAuthoringSourceCoordinate{
		RepositoryURL: "https://github.com/example/repository.git",
		CommitSHA:     strings.Repeat("a", 40),
	}
	fixtureRoot := t.TempDir()
	archivePath := filepath.Join(fixtureRoot, "archive.tar")
	if err := os.WriteFile(archivePath, standardAuthoringArchiveFixture(t, map[string]string{"comment": coordinate.CommitSHA}, map[string]string{
		"source/README.md": "captured source\n",
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	gitPath := filepath.Join(fixtureRoot, "git")
	script := "#!/bin/sh\n" +
		"set -eu\n" +
		"root=${0%/*}\n" +
		"printf '%s\\n' \"$*\" >> \"$root/invocations\"\n" +
		"case \" $* \" in\n" +
		"  *' fetch '*)\n" +
		"    {\n" +
		"      printf 'git_allow_protocol=%s\\n' \"${GIT_ALLOW_PROTOCOL-}\"\n" +
		"      printf 'git_protocol_from_user=%s\\n' \"${GIT_PROTOCOL_FROM_USER-}\"\n" +
		"      printf 'git_config_global=%s\\n' \"${GIT_CONFIG_GLOBAL-}\"\n" +
		"      printf 'git_config_count=%s\\n' \"${GIT_CONFIG_COUNT-}\"\n" +
		"      printf 'git_http_version=%s\\n' \"${GIT_CONFIG_VALUE_0-}\"\n" +
		"      printf 'git_http_low_speed_limit=%s\\n' \"${GIT_CONFIG_VALUE_1-}\"\n" +
		"      printf 'git_http_low_speed_time=%s\\n' \"${GIT_CONFIG_VALUE_2-}\"\n" +
		"      printf 'git_terminal_prompt=%s\\n' \"${GIT_TERMINAL_PROMPT-}\"\n" +
		"      if [ \"${SSH_AUTH_SOCK+x}\" = x ]; then printf 'ssh_auth_sock=set\\n'; else printf 'ssh_auth_sock=unset\\n'; fi\n" +
		"    } > \"$root/fetch-environment\"\n" +
		"    exit 0;;\n" +
		"  *' rev-parse '*) printf '%s\\n' '" + coordinate.CommitSHA + "'; exit 0;;\n" +
		"  *' archive '*) cat \"$root/archive.tar\"; exit 0;;\n" +
		"  *) exit 0;;\n" +
		"esac\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", "/ambient-agent-that-must-not-be-inherited")
	capturer, err := NewStandardAuthoringGitArchiveSourceCapturer(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := capturer.CaptureStandardAuthoringSource(context.Background(), coordinate)
	if err != nil {
		t.Fatalf("capture controlled HTTPS source: %v", err)
	}
	if snapshot.RepositoryURL != coordinate.RepositoryURL || snapshot.CommitSHA != coordinate.CommitSHA || snapshot.SchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		t.Fatalf("captured source identity = %+v", snapshot)
	}
	if err := validateStandardAuthoringSourceArchive(snapshot.Content, coordinate); err != nil {
		t.Fatalf("captured source archive: %v", err)
	}

	invocations, err := os.ReadFile(filepath.Join(fixtureRoot, "invocations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"init --bare", "fetch --no-tags --depth=1 " + coordinate.RepositoryURL + " " + coordinate.CommitSHA,
		"rev-parse --verify " + coordinate.CommitSHA + "^{commit}", "archive --format=tar --prefix=" + standardAuthoringSourceArchiveRoot + " " + coordinate.CommitSHA,
	} {
		if !strings.Contains(string(invocations), expected) {
			t.Fatalf("controlled Git capture omitted %q:\n%s", expected, invocations)
		}
	}
	environment, err := os.ReadFile(filepath.Join(fixtureRoot, "fetch-environment"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"git_allow_protocol=https:ssh", "git_protocol_from_user=0", "git_config_global=/dev/null", "git_config_count=3", "git_http_version=HTTP/1.1", "git_http_low_speed_limit=1", "git_http_low_speed_time=45", "git_terminal_prompt=0", "ssh_auth_sock=unset",
	} {
		if !strings.Contains(string(environment), expected) {
			t.Fatalf("controlled Git fetch environment omitted %q:\n%s", expected, environment)
		}
	}
}

func TestStandardAuthoringSSHSourceCaptureUsesOnlyPackagedHostKeysAndExplicitAgent(t *testing.T) {
	contractRoot := t.TempDir()
	knownHosts := []byte("github.com ssh-ed25519 AQID\n")
	knownHostsPath := filepath.Join(contractRoot, "ssh", "known_hosts")
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHostsPath, knownHosts, 0o600); err != nil {
		t.Fatal(err)
	}
	sshPath := filepath.Join(contractRoot, "locked-ssh")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then printf '%s\\n' OpenSSH_9.9p1 >&2; exit 0; fi\nexit 73\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	shellPath := filepath.Join(contractRoot, "locked-shell")
	if err := os.WriteFile(shellPath, []byte("test wrapper shell bytes\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sshBytes, err := os.ReadFile(sshPath)
	if err != nil {
		t.Fatal(err)
	}
	shellBytes, err := os.ReadFile(shellPath)
	if err != nil {
		t.Fatal(err)
	}
	transportLock := stageprovider.StandardAuthoringSSHTransportLock{
		Format:  stageprovider.StandardAuthoringSSHTransportLockFormat,
		Version: stageprovider.StandardAuthoringSSHTransportLockVersion,
		SSHExecutable: stageprovider.LocalExecutableLock{
			CommandID: stageprovider.StandardAuthoringSSHTransportCommandID, AbsolutePath: sshPath, Version: "OpenSSH_9.9p1", ContentSHA256: workflowkit.SHA256Fingerprint(sshBytes),
		},
		WrapperShell: stageprovider.LocalExecutableLock{
			CommandID: stageprovider.StandardAuthoringSSHWrapperShellCommandID, AbsolutePath: shellPath, Version: string(workflowkit.SHA256Fingerprint(shellBytes)), ContentSHA256: workflowkit.SHA256Fingerprint(shellBytes),
		},
		KnownHosts:                 stageprovider.StandardAuthoringSSHKnownHostsLock{Format: stageprovider.StandardAuthoringSSHKnownHostsLockFormat, Version: stageprovider.StandardAuthoringSSHKnownHostsLockVersion, RelativePath: stageprovider.StandardAuthoringSSHKnownHostsRelativePath, ContentSHA256: workflowkit.SHA256Fingerprint(knownHosts)},
		AgentSocketEnvironmentName: stageprovider.StandardAuthoringSSHAgentSocketEnvironment,
	}
	if err := transportLock.Validate(); err != nil {
		t.Fatal(err)
	}
	agentSocket := filepath.Join(contractRoot, "agent.sock")
	listener, err := net.Listen("unix", agentSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("SSH_AUTH_SOCK", "/ambient-agent-that-must-not-be-inherited")
	transport := &standardAuthoringSSHSourceCaptureTransport{
		contractRoot: contractRoot, transport: transportLock,
		lookupEnvironment: func(name string) (string, bool) {
			if name != stageprovider.StandardAuthoringSSHAgentSocketEnvironment {
				t.Fatalf("unexpected environment lookup %q", name)
			}
			return agentSocket, true
		},
	}
	parsed, err := url.Parse("ssh://git@github.com/example/repository.git")
	if err != nil {
		t.Fatal(err)
	}
	captureRoot := t.TempDir()
	environment, err := transport.gitEnvironment(context.Background(), captureRoot, parsed)
	if err != nil {
		t.Fatalf("construct controlled SSH environment: %v", err)
	}
	byName := make(map[string]string, len(environment))
	for _, value := range environment {
		name, value, found := strings.Cut(value, "=")
		if !found {
			t.Fatalf("environment item %q is malformed", value)
		}
		byName[name] = value
	}
	if byName["GIT_SSH_VARIANT"] != "ssh" || byName["GIT_SSH"] == "" {
		t.Fatalf("SSH environment is incomplete: %+v", byName)
	}
	if _, found := byName["SSH_AUTH_SOCK"]; found || strings.Contains(strings.Join(environment, "\n"), "ambient-agent") {
		t.Fatalf("ambient SSH agent leaked into controlled environment: %+v", byName)
	}
	wrapper, err := os.ReadFile(byName["GIT_SSH"])
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(captureRoot, "ssh-known_hosts")
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil || !bytes.Equal(snapshot, knownHosts) {
		t.Fatalf("controlled known_hosts snapshot = %q, %v; want %q", snapshot, err, knownHosts)
	}
	for _, fragment := range []string{"StrictHostKeyChecking=yes", "GlobalKnownHostsFile=/dev/null", "UserKnownHostsFile=" + snapshotPath, "IdentityAgent=" + agentSocket, "\"$@\""} {
		if !strings.Contains(string(wrapper), fragment) {
			t.Fatalf("controlled SSH wrapper lacks %q: %s", fragment, wrapper)
		}
	}
	if strings.Contains(string(wrapper), "ambient-agent") || strings.Contains(string(wrapper), "GIT_SSH_COMMAND") {
		t.Fatalf("controlled SSH wrapper inherited ambient input: %s", wrapper)
	}
	transport.lookupEnvironment = nil
	withoutAgent, err := transport.gitEnvironment(context.Background(), t.TempDir(), parsed)
	if err != nil {
		t.Fatalf("construct no-agent SSH environment: %v", err)
	}
	withoutAgentWrapper, err := os.ReadFile(standardAuthoringEnvironmentValue(t, withoutAgent, "GIT_SSH"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withoutAgentWrapper), "IdentityAgent=none") || strings.Contains(string(withoutAgentWrapper), "ambient-agent") {
		t.Fatalf("no-agent SSH wrapper inherited an ambient agent: %s", withoutAgentWrapper)
	}
}

func TestStandardAuthoringSSHSourceCaptureRejectsUnlistedHostBeforeSSHProbe(t *testing.T) {
	contractRoot := t.TempDir()
	knownHosts := []byte("github.com ssh-ed25519 AQID\n")
	knownHostsPath := filepath.Join(contractRoot, "ssh", "known_hosts")
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHostsPath, knownHosts, 0o600); err != nil {
		t.Fatal(err)
	}
	probeMarker := filepath.Join(contractRoot, "ssh-was-invoked")
	sshPath := filepath.Join(contractRoot, "locked-ssh")
	sshScript := "#!/bin/sh\nprintf invoked > " + standardAuthoringPOSIXShellQuote(probeMarker) + "\nprintf '%s\\n' OpenSSH_9.9p1 >&2\n"
	if err := os.WriteFile(sshPath, []byte(sshScript), 0o700); err != nil {
		t.Fatal(err)
	}
	shellPath := filepath.Join(contractRoot, "locked-shell")
	if err := os.WriteFile(shellPath, []byte("test wrapper shell bytes\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sshBytes, _ := os.ReadFile(sshPath)
	shellBytes, _ := os.ReadFile(shellPath)
	transport := &standardAuthoringSSHSourceCaptureTransport{
		contractRoot: contractRoot,
		transport: stageprovider.StandardAuthoringSSHTransportLock{
			Format: stageprovider.StandardAuthoringSSHTransportLockFormat, Version: stageprovider.StandardAuthoringSSHTransportLockVersion,
			SSHExecutable:              stageprovider.LocalExecutableLock{CommandID: stageprovider.StandardAuthoringSSHTransportCommandID, AbsolutePath: sshPath, Version: "OpenSSH_9.9p1", ContentSHA256: workflowkit.SHA256Fingerprint(sshBytes)},
			WrapperShell:               stageprovider.LocalExecutableLock{CommandID: stageprovider.StandardAuthoringSSHWrapperShellCommandID, AbsolutePath: shellPath, Version: string(workflowkit.SHA256Fingerprint(shellBytes)), ContentSHA256: workflowkit.SHA256Fingerprint(shellBytes)},
			KnownHosts:                 stageprovider.StandardAuthoringSSHKnownHostsLock{Format: stageprovider.StandardAuthoringSSHKnownHostsLockFormat, Version: stageprovider.StandardAuthoringSSHKnownHostsLockVersion, RelativePath: stageprovider.StandardAuthoringSSHKnownHostsRelativePath, ContentSHA256: workflowkit.SHA256Fingerprint(knownHosts)},
			AgentSocketEnvironmentName: stageprovider.StandardAuthoringSSHAgentSocketEnvironment,
		},
	}
	parsed, err := url.Parse("ssh://git@unlisted.example/repository.git")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.gitEnvironment(context.Background(), t.TempDir(), parsed); err == nil || !strings.Contains(err.Error(), "known_hosts allow-list") {
		t.Fatalf("unlisted SSH host error = %v, want pre-network allow-list rejection", err)
	}
	if _, err := os.Stat(probeMarker); !os.IsNotExist(err) {
		t.Fatalf("SSH version probe ran for an unlisted host: %v", err)
	}
}

func TestStandardAuthoringSourceCaptureKeepsHTTPSIndependentOfSSHTransport(t *testing.T) {
	capturer := &StandardAuthoringGitArchiveSourceCapturer{gitExecutable: "/locked/git"}
	environment, err := capturer.sourceFetchEnvironment(context.Background(), t.TempDir(), StandardAuthoringSourceCoordinate{
		RepositoryURL: "https://github.com/example/repository.git", CommitSHA: strings.Repeat("a", 40),
	})
	if err != nil {
		t.Fatalf("construct HTTPS fetch environment: %v", err)
	}
	for _, value := range environment {
		if strings.HasPrefix(value, "GIT_SSH=") || strings.HasPrefix(value, "GIT_SSH_COMMAND=") || strings.HasPrefix(value, "SSH_AUTH_SOCK=") {
			t.Fatalf("HTTPS fetch inherited SSH transport state: %q", value)
		}
	}
}

func TestStandardAuthoringSSHUsernameRejectsOptionLikeValues(t *testing.T) {
	for value, want := range map[string]bool{
		"git": true, "build-user_2": true, "-oProxyCommand": false, "git name": false, "git@host": false,
	} {
		if got := standardAuthoringSSHUsername(value); got != want {
			t.Fatalf("SSH username %q accepted=%t, want %t", value, got, want)
		}
	}
}

func standardAuthoringEnvironmentValue(t *testing.T, environment []string, name string) string {
	t.Helper()
	for _, item := range environment {
		key, value, found := strings.Cut(item, "=")
		if found && key == name {
			return value
		}
	}
	t.Fatalf("environment does not contain %s", name)
	return ""
}

func TestValidateStandardAuthoringSourceArchiveAcceptsAndProjectsRealGitPAXLongPath(t *testing.T) {
	repository := t.TempDir()
	standardAuthoringTestGitRun(t, repository, "init")
	longPath := strings.Join([]string{
		strings.Repeat("alpha", 16),
		strings.Repeat("bravo", 16),
		strings.Repeat("charlie", 16),
		strings.Repeat("delta", 16),
		"captured.rs",
	}, "/")
	content := []byte("captured source\n")
	fullPath := filepath.Join(repository, filepath.FromSlash(longPath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	standardAuthoringTestGitRun(t, repository, "add", longPath)
	standardAuthoringTestGitRun(t, repository, "-c", "user.name=Harbor Factory Test", "-c", "user.email=harbor-factory@example.invalid", "commit", "-m", "capture fixture")
	commit := strings.TrimSpace(string(standardAuthoringTestGitRun(t, repository, "rev-parse", "HEAD")))
	archive := standardAuthoringTestGitRun(t, repository, "archive", "--format=tar", "--prefix="+standardAuthoringSourceArchiveRoot, "HEAD")
	coordinate := StandardAuthoringSourceCoordinate{RepositoryURL: "https://github.com/example/repository.git", CommitSHA: commit}

	if err := validateStandardAuthoringSourceArchive(archive, coordinate); err != nil {
		t.Fatalf("validate real Git long-path archive: %v", err)
	}

	workspace := t.TempDir()
	if err := extractStandardAuthoringSourceSnapshot(context.Background(), archive, workspace, coordinate); err != nil {
		t.Fatalf("extract real Git long-path archive: %v", err)
	}
	projected, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(standardAuthoringSourceArchiveRoot+longPath)))
	if err != nil || !bytes.Equal(projected, content) {
		t.Fatalf("projected long Git archive path = %q, %v; want %q", projected, err, content)
	}
	sourceRoot := filepath.Join(workspace, filepath.FromSlash(standardAuthoringSourceArchiveRoot))
	t.Cleanup(func() {
		_ = filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	if err := markStandardAuthoringSourceReadOnly(sourceRoot); err != nil {
		t.Fatalf("seal real Git long-path workspace: %v", err)
	}
	if err := verifyStandardAuthoringExtractedSnapshot(context.Background(), archive, sourceRoot, coordinate); err != nil {
		t.Fatalf("verify real Git long-path archive: %v", err)
	}
}

func TestValidateStandardAuthoringSourceArchiveAcceptsAndProjectsRealGitUnicodePath(t *testing.T) {
	repository := t.TempDir()
	standardAuthoringTestGitRun(t, repository, "init")
	unicodePath := "你好世界.txt"
	content := []byte("unicode source\n")
	if err := os.WriteFile(filepath.Join(repository, unicodePath), content, 0o644); err != nil {
		t.Fatal(err)
	}
	standardAuthoringTestGitRun(t, repository, "add", unicodePath)
	standardAuthoringTestGitRun(t, repository, "-c", "user.name=Harbor Factory Test", "-c", "user.email=harbor-factory@example.invalid", "commit", "-m", "unicode capture fixture")
	commit := strings.TrimSpace(string(standardAuthoringTestGitRun(t, repository, "rev-parse", "HEAD")))
	archive := standardAuthoringTestGitRun(t, repository, "archive", "--format=tar", "--prefix="+standardAuthoringSourceArchiveRoot, "HEAD")
	coordinate := StandardAuthoringSourceCoordinate{RepositoryURL: "https://github.com/example/repository.git", CommitSHA: commit}

	if err := validateStandardAuthoringSourceArchive(archive, coordinate); err != nil {
		t.Fatalf("validate real Git Unicode archive: %v", err)
	}

	workspace := t.TempDir()
	if err := extractStandardAuthoringSourceSnapshot(context.Background(), archive, workspace, coordinate); err != nil {
		t.Fatalf("extract real Git Unicode archive: %v", err)
	}
	projected, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(standardAuthoringSourceArchiveRoot+unicodePath)))
	if err != nil || !bytes.Equal(projected, content) {
		t.Fatalf("projected Unicode Git archive path = %q, %v; want %q", projected, err, content)
	}
}

func TestValidateStandardAuthoringSourceArchiveAllowsExtendedMetadata(t *testing.T) {
	coordinate := StandardAuthoringSourceCoordinate{
		RepositoryURL: "https://github.com/example/repository.git", CommitSHA: strings.Repeat("a", 40),
	}
	for _, test := range []struct {
		name    string
		archive []byte
	}{
		{
			name: "missing Git PAX header",
			archive: standardAuthoringArchiveFixture(t, nil, map[string]string{
				"source/README.md": "source\n",
			}),
		},
		{
			name: "PAX comment names another commit",
			archive: standardAuthoringArchiveFixture(t, map[string]string{"comment": strings.Repeat("b", 40)}, map[string]string{
				"source/README.md": "source\n",
			}),
		},
		{
			name: "PAX global header has another record",
			archive: standardAuthoringArchiveFixture(t, map[string]string{"comment": coordinate.CommitSHA, "path": "source/README.md"}, map[string]string{
				"source/README.md": "source\n",
			}),
		},
		{
			name:    "per-file unknown PAX metadata",
			archive: standardAuthoringArchiveFixtureWithLocalPAX(t, coordinate.CommitSHA),
		},
		{
			name: "xattr metadata",
			archive: standardAuthoringArchiveFixtureWithEntries(t, coordinate.CommitSHA, []standardAuthoringArchiveFixtureEntry{{
				name: "source/README.md", content: "source\n", xattrs: map[string]string{"user.untrusted": "value"},
			}}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateStandardAuthoringSourceArchive(test.archive, coordinate); err != nil {
				t.Fatalf("validate archive with extended metadata: %v", err)
			}
			workspace := t.TempDir()
			if err := extractStandardAuthoringSourceSnapshot(context.Background(), test.archive, workspace, coordinate); err != nil {
				t.Fatalf("extract archive with extended metadata: %v", err)
			}
			sourceRoot := filepath.Join(workspace, filepath.FromSlash(standardAuthoringSourceArchiveRoot))
			t.Cleanup(func() {
				_ = filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
					if walkErr == nil && entry.IsDir() {
						_ = os.Chmod(path, 0o755)
					}
					return nil
				})
			})
			if err := markStandardAuthoringSourceReadOnly(sourceRoot); err != nil {
				t.Fatalf("seal archive with extended metadata: %v", err)
			}
			if err := verifyStandardAuthoringExtractedSnapshot(context.Background(), test.archive, sourceRoot, coordinate); err != nil {
				t.Fatalf("verify archive with extended metadata: %v", err)
			}
		})
	}
}

func TestValidateStandardAuthoringSourceArchiveRejectsUnsafeStructure(t *testing.T) {
	coordinate := StandardAuthoringSourceCoordinate{
		RepositoryURL: "https://github.com/example/repository.git", CommitSHA: strings.Repeat("a", 40),
	}
	for _, test := range []struct {
		name    string
		archive []byte
	}{
		{
			name: "archive path escapes source root",
			archive: standardAuthoringArchiveFixture(t, nil, map[string]string{
				"outside.txt": "source\n",
			}),
		},
		{
			name: "PAX path escapes source root",
			archive: standardAuthoringArchiveFixtureWithEntries(t, coordinate.CommitSHA, []standardAuthoringArchiveFixtureEntry{{
				name: "../" + strings.Repeat("outside", 48), content: "source\n",
			}}),
		},
		{
			name: "PAX path duplicates final path",
			archive: standardAuthoringArchiveFixtureWithEntries(t, coordinate.CommitSHA, []standardAuthoringArchiveFixtureEntry{
				{name: standardAuthoringLongArchiveFixturePath(), content: "first\n"},
				{name: standardAuthoringLongArchiveFixturePath(), content: "second\n"},
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateStandardAuthoringSourceArchive(test.archive, coordinate); err == nil {
				t.Fatal("archive validation unexpectedly succeeded")
			}
		})
	}
}

func TestStandardAuthoringSourceArchiveProjectsRepositoryLinks(t *testing.T) {
	coordinate := StandardAuthoringSourceCoordinate{
		RepositoryURL: "https://github.com/example/repository.git", CommitSHA: strings.Repeat("a", 40),
	}
	archive := standardAuthoringArchiveFixtureWithEntries(t, coordinate.CommitSHA, []standardAuthoringArchiveFixtureEntry{
		{name: "source/README.md", content: "source\n"},
		{name: "source/README-link", typeflag: tar.TypeSymlink, linkname: "README.md"},
		{name: "source/README-copy", typeflag: tar.TypeLink, linkname: "source/README.md"},
	})
	if err := validateStandardAuthoringSourceArchive(archive, coordinate); err != nil {
		t.Fatalf("validate archive with repository links: %v", err)
	}
	workspace := t.TempDir()
	if err := extractStandardAuthoringSourceSnapshot(context.Background(), archive, workspace, coordinate); err != nil {
		t.Fatalf("extract archive with repository links: %v", err)
	}
	sourceRoot := filepath.Join(workspace, filepath.FromSlash(standardAuthoringSourceArchiveRoot))
	t.Cleanup(func() {
		_ = filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	if target, err := os.Readlink(filepath.Join(sourceRoot, "README-link")); err != nil || target != "README.md" {
		t.Fatalf("repository symbolic link = %q, %v", target, err)
	}
	if err := markStandardAuthoringSourceReadOnly(sourceRoot); err != nil {
		t.Fatalf("seal archive with repository links: %v", err)
	}
	if err := verifyStandardAuthoringExtractedSnapshot(context.Background(), archive, sourceRoot, coordinate); err != nil {
		t.Fatalf("verify archive with repository links: %v", err)
	}
	unsafeArchive := standardAuthoringArchiveFixtureWithEntries(t, coordinate.CommitSHA, []standardAuthoringArchiveFixtureEntry{
		{name: "source/README.md", content: "source\n"},
		{name: "source/link", typeflag: tar.TypeSymlink, linkname: "../../outside"},
	})
	if err := extractStandardAuthoringSourceSnapshot(context.Background(), unsafeArchive, t.TempDir(), coordinate); err == nil {
		t.Fatal("source-root escaping symbolic link was projected")
	}
}

type standardAuthoringArchiveFixtureEntry struct {
	name     string
	content  string
	typeflag byte
	linkname string
	xattrs   map[string]string
}

func standardAuthoringArchiveFixtureWithEntries(t *testing.T, commitSHA string, entries []standardAuthoringArchiveFixtureEntry) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: standardAuthoringGitPAXGlobalHeaderName, Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": commitSHA}}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Typeflag: typeflag, Mode: 0o644, Linkname: entry.linkname, Xattrs: entry.xattrs}
		if typeflag == tar.TypeReg || typeflag == tar.TypeRegA {
			header.Size = int64(len(entry.content))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if typeflag == tar.TypeReg || typeflag == tar.TypeRegA {
			if _, err := writer.Write([]byte(entry.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func standardAuthoringLongArchiveFixturePath() string {
	return standardAuthoringSourceArchiveRoot + strings.Join([]string{
		strings.Repeat("one", 32),
		strings.Repeat("two", 32),
		strings.Repeat("three", 32),
		"fixture.txt",
	}, "/")
}

func standardAuthoringArchiveFixture(t *testing.T, globalPAX map[string]string, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if globalPAX != nil {
		if err := writer.WriteHeader(&tar.Header{
			Name: standardAuthoringGitPAXGlobalHeaderName, Typeflag: tar.TypeXGlobalHeader, PAXRecords: globalPAX,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.WriteHeader(&tar.Header{Name: standardAuthoringSourceArchiveRoot, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func standardAuthoringArchiveFixtureWithLocalPAX(t *testing.T, commitSHA string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, header := range []*tar.Header{
		{Name: standardAuthoringGitPAXGlobalHeaderName, Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": commitSHA}},
		{Name: standardAuthoringSourceArchiveRoot, Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "source/README.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len("source\n")), PAXRecords: map[string]string{"comment": "unexpected"}},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte("source\n")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func standardAuthoringTestGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git executable is unavailable")
	}
	return git
}

func standardAuthoringTestGitRun(t *testing.T, repository string, arguments ...string) []byte {
	t.Helper()
	git := standardAuthoringTestGit(t)
	command := exec.Command(git, append([]string{"-C", repository}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return output
}
