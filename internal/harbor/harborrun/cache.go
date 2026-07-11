package harborrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
)

const (
	claudeCacheSchemaVersion = "harbor.claude_agent_cache.v1"
	claudeCacheVersion       = "2.1.207"
	claudeCacheMountTarget   = "/opt/harbor-factory/claude-code-cache"
)

type claudeCacheFile struct {
	Variant      string `json:"variant"`
	RelativePath string `json:"relative_path"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
}

type claudeCacheManifest struct {
	SchemaVersion string            `json:"schema_version"`
	Agent         string            `json:"agent"`
	Version       string            `json:"version"`
	Architecture  string            `json:"architecture"`
	Files         []claudeCacheFile `json:"files"`
	CreatedAt     time.Time         `json:"created_at"`
}

func prepareClaudeAgentCache(ctx context.Context, root string, exec executor.CommandRunner) (string, string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", "", nil
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve Harbor agent cache: %w", err)
	}
	if err := os.MkdirAll(absoluteRoot, 0o700); err != nil {
		return "", "", fmt.Errorf("create Harbor agent cache root: %w", err)
	}
	unlock, err := lockClaudeCache(ctx, filepath.Join(absoluteRoot, ".cache.lock"))
	if err != nil {
		return "", "", err
	}
	defer unlock()

	arch, npmArch, err := claudeCacheArchitecture()
	if err != nil {
		return "", "", err
	}
	cacheDir := filepath.Join(absoluteRoot, claudeCacheVersion, "linux-"+arch)
	manifestPath := filepath.Join(cacheDir, "manifest.json")
	if err := validateClaudeAgentCache(cacheDir, manifestPath, arch); err != nil {
		if exec == nil {
			exec = executor.New()
		}
		if _, lookErr := exec.LookPath("npm"); lookErr != nil {
			return "", "", fmt.Errorf("prepare Harbor agent cache: npm is required: %w", lookErr)
		}
		staging := fmt.Sprintf("%s.tmp-%d-%d", cacheDir, os.Getpid(), time.Now().UnixNano())
		_ = os.RemoveAll(staging)
		defer os.RemoveAll(staging)
		packages := []string{
			"@anthropic-ai/claude-code-linux-" + npmArch + "@" + claudeCacheVersion,
			"@anthropic-ai/claude-code-linux-" + npmArch + "-musl@" + claudeCacheVersion,
		}
		args := append([]string{"install", "--force", "--prefix", staging, "--no-audit", "--no-fund"}, packages...)
		result := exec.Run(ctx, 10*time.Minute, "", cacheProcessEnv(), "npm", args...)
		if result.Err != nil || result.ExitCode != 0 {
			message := strings.TrimSpace(commandlog.RedactText(result.Stderr))
			if message == "" {
				message = strings.TrimSpace(commandlog.RedactText(result.Stdout))
			}
			return "", "", fmt.Errorf("populate Harbor agent cache with npm: exit=%d: %s", result.ExitCode, message)
		}
		manifest, buildErr := buildClaudeCacheManifest(staging, arch, npmArch)
		if buildErr != nil {
			return "", "", buildErr
		}
		raw, marshalErr := json.MarshalIndent(manifest, "", "  ")
		if marshalErr != nil {
			return "", "", marshalErr
		}
		if writeErr := os.WriteFile(filepath.Join(staging, "manifest.json"), append(raw, '\n'), 0o600); writeErr != nil {
			return "", "", fmt.Errorf("write Harbor agent cache manifest: %w", writeErr)
		}
		if removeErr := os.RemoveAll(cacheDir); removeErr != nil {
			return "", "", fmt.Errorf("replace invalid Harbor agent cache: %w", removeErr)
		}
		if renameErr := os.Rename(staging, cacheDir); renameErr != nil {
			return "", "", fmt.Errorf("publish Harbor agent cache: %w", renameErr)
		}
		if err := validateClaudeAgentCache(cacheDir, manifestPath, arch); err != nil {
			return "", "", fmt.Errorf("validate published Harbor agent cache: %w", err)
		}
	}
	mountRaw, err := json.Marshal([]map[string]any{{
		"type":      "bind",
		"source":    cacheDir,
		"target":    claudeCacheMountTarget,
		"read_only": true,
	}})
	if err != nil {
		return "", "", err
	}
	return manifestPath, string(mountRaw), nil
}

func cacheProcessEnv() []string {
	allowed := []string{
		"PATH", "HOME", "TMPDIR", "XDG_CACHE_HOME",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
		"NPM_CONFIG_REGISTRY", "SSL_CERT_FILE", "SSL_CERT_DIR",
	}
	values := make([]string, 0, len(allowed))
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			values = append(values, key+"="+value)
		}
	}
	return values
}

func claudeCacheArchitecture() (string, string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64", "x64", nil
	case "arm64":
		return "arm64", "arm64", nil
	default:
		return "", "", fmt.Errorf("Harbor Claude agent cache does not support architecture %s", runtime.GOARCH)
	}
}

func buildClaudeCacheManifest(cacheDir, arch, npmArch string) (claudeCacheManifest, error) {
	files := []claudeCacheFile{
		{Variant: "glibc", RelativePath: filepath.ToSlash(filepath.Join("node_modules", "@anthropic-ai", "claude-code-linux-"+npmArch, "claude"))},
		{Variant: "musl", RelativePath: filepath.ToSlash(filepath.Join("node_modules", "@anthropic-ai", "claude-code-linux-"+npmArch+"-musl", "claude"))},
	}
	for i := range files {
		path := filepath.Join(cacheDir, filepath.FromSlash(files[i].RelativePath))
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return claudeCacheManifest{}, fmt.Errorf("Harbor agent cache binary missing: %s", files[i].RelativePath)
		}
		digest, err := sha256File(path)
		if err != nil {
			return claudeCacheManifest{}, err
		}
		files[i].SHA256 = "sha256:" + digest
		files[i].Size = info.Size()
	}
	return claudeCacheManifest{
		SchemaVersion: claudeCacheSchemaVersion,
		Agent:         "claude-code",
		Version:       claudeCacheVersion,
		Architecture:  arch,
		Files:         files,
		CreatedAt:     time.Now().UTC(),
	}, nil
}

func validateClaudeAgentCache(cacheDir, manifestPath, arch string) error {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest claudeCacheManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	if manifest.SchemaVersion != claudeCacheSchemaVersion || manifest.Agent != "claude-code" || manifest.Version != claudeCacheVersion || manifest.Architecture != arch || len(manifest.Files) != 2 {
		return fmt.Errorf("Harbor agent cache manifest identity mismatch")
	}
	seen := map[string]bool{}
	for _, file := range manifest.Files {
		if file.Variant != "glibc" && file.Variant != "musl" || seen[file.Variant] {
			return fmt.Errorf("invalid Harbor agent cache variant %q", file.Variant)
		}
		seen[file.Variant] = true
		rel := filepath.Clean(filepath.FromSlash(file.RelativePath))
		if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid Harbor agent cache path %q", file.RelativePath)
		}
		path := filepath.Join(cacheDir, rel)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size || file.Size <= 0 {
			return fmt.Errorf("Harbor agent cache file metadata mismatch: %s", file.RelativePath)
		}
		digest, err := sha256File(path)
		if err != nil || file.SHA256 != "sha256:"+digest {
			return fmt.Errorf("Harbor agent cache digest mismatch: %s", file.RelativePath)
		}
	}
	return nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func lockClaudeCache(ctx context.Context, path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Harbor agent cache lock: %w", err)
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock Harbor agent cache: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
