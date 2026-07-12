package commandlog

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/redact"
)

func RedactText(text string) string {
	return redact.Text(text)
}

func RedactEnv(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, item := range env {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			out = append(out, RedactText(item))
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if isSecretKey(key) {
			out = append(out, key+"=<redacted>")
		} else {
			out = append(out, key+"=<set>")
		}
	}
	return out
}

func RedactArgv(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		item := strings.TrimSpace(argv[i])
		if item == "" {
			continue
		}
		if strings.EqualFold(item, "--ae") && i+1 < len(argv) {
			out = append(out, item)
			i++
			out = append(out, redactAssignment(strings.TrimSpace(argv[i])))
			continue
		}
		if len(item) >= len("--ae=") && strings.EqualFold(item[:len("--ae=")], "--ae=") {
			out = append(out, "--ae="+redactAssignment(item[len("--ae="):]))
			continue
		}
		if isSecretKey(item) && !strings.Contains(item, "=") && i+1 < len(argv) && !strings.HasPrefix(strings.TrimSpace(argv[i+1]), "-") {
			out = append(out, item)
			i++
			out = append(out, "<redacted>")
			continue
		}
		out = append(out, redactAssignment(item))
	}
	return out
}

func EffectiveEnv(explicit []string) []string {
	if len(explicit) > 0 {
		return append([]string{}, explicit...)
	}
	return os.Environ()
}

func ResolveCWD(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		return wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

func ClassifyFailure(exitCode int, timeout bool, stdout, stderr string) string {
	if timeout {
		return "timeout"
	}
	if exitCode == 0 {
		return ""
	}
	text := strings.ToLower(stdout + "\n" + stderr)
	switch {
	case strings.Contains(text, "executable file not found") || strings.Contains(text, "command not found") || strings.Contains(text, "no such file or directory"):
		return "missing_tool_or_path"
	case strings.Contains(text, "permission denied") || strings.Contains(text, "authentication") || strings.Contains(text, "unauthorized") || strings.Contains(text, "forbidden"):
		return "permission_or_auth"
	case containsNetworkFailure(text):
		return "network_or_timeout"
	case strings.Contains(text, "docker") && (strings.Contains(text, "daemon") || strings.Contains(text, "cannot connect")):
		return "docker_daemon"
	case strings.Contains(text, "test") || strings.Contains(text, "assert") || strings.Contains(text, "failed"):
		return "test_or_verification_failure"
	default:
		return "command_failed"
	}
}

func containsNetworkFailure(text string) bool {
	for _, marker := range []string{
		"network is unreachable",
		"network error",
		"network timeout",
		"connection",
		"timeout",
		"temporary failure",
		"tls handshake",
		"gnutls recv error",
		"unexpected disconnect",
		"early eof",
		"failed to resolve source metadata",
		"could not resolve host",
		"rpc failed; curl",
		"failed to download",
		"failed to fetch",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func WriteOutputFiles(dir, stdout, stderr string) (string, string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", "", nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	stdoutPath := filepath.Join(dir, "stdout.txt")
	stderrPath := filepath.Join(dir, "stderr.txt")
	if err := os.WriteFile(stdoutPath, []byte(RedactText(stdout)), 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(stderrPath, []byte(RedactText(stderr)), 0o600); err != nil {
		return "", "", err
	}
	return stdoutPath, stderrPath, nil
}

func isSecretKey(key string) bool {
	return redact.SensitiveKey(key)
}

func redactAssignment(item string) string {
	if item == "" {
		return ""
	}
	key, _, ok := strings.Cut(item, "=")
	if !ok {
		return RedactText(item)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return RedactText(item)
	}
	if isSecretKey(key) {
		return key + "=<redacted>"
	}
	return RedactText(item)
}
