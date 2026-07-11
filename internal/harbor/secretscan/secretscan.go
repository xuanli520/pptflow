package secretscan

import (
	"archive/zip"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
)

const maxScannedFileBytes int64 = 2 << 20

type Finding struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Kind    string `json:"kind"`
	Snippet string `json:"snippet,omitempty"`
}

var (
	skPattern                = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
	bearerPattern            = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	urlUserInfoPattern       = regexp.MustCompile(`(?i)(?:https?|ssh)://[^/\s"'<>@]+@`)
	githubClassicPattern     = regexp.MustCompile(`\bghp_[A-Za-z0-9_]{20,}\b`)
	githubFineGrainedPattern = regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`)
	awsAccessKeyPattern      = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	privateKeyPattern        = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	secretAssignmentPattern  = regexp.MustCompile(`(?i)["']?\b([A-Za-z0-9_.-]*(?:TOKEN|KEY|SECRET|PASSWORD|AUTH)[A-Za-z0-9_.-]*)\b["']?\s*[:=]\s*["']?([^"',}\s#]+)`)
)

var binaryExtensions = map[string]bool{
	".7z": true, ".avif": true, ".bin": true, ".bmp": true, ".class": true,
	".db": true, ".exe": true, ".gif": true, ".gz": true, ".ico": true,
	".jpeg": true, ".jpg": true, ".mov": true, ".mp4": true, ".pdf": true,
	".png": true, ".sqlite": true, ".tar": true, ".tgz": true, ".webp": true,
	".zip": true,
}

func ScanDir(root string) ([]Finding, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, nil
	}
	var findings []Finding
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root && shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipPath(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxScannedFileBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if isBinary(data) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		findings = append(findings, ScanBytes(filepath.ToSlash(rel), data)...)
		return nil
	})
	sortFindings(findings)
	return findings, err
}

func ScanZip(path string) ([]Finding, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var findings []Finding
	for _, file := range reader.File {
		name := strings.Trim(file.Name, "/")
		if name == "" || file.FileInfo().IsDir() || shouldSkipPath(name) || file.FileInfo().Size() > maxScannedFileBytes {
			continue
		}
		in, err := file.Open()
		if err != nil {
			return findings, err
		}
		data, readErr := io.ReadAll(io.LimitReader(in, maxScannedFileBytes+1))
		closeErr := in.Close()
		if readErr != nil {
			return findings, readErr
		}
		if closeErr != nil {
			return findings, closeErr
		}
		if int64(len(data)) > maxScannedFileBytes || isBinary(data) {
			continue
		}
		findings = append(findings, ScanBytes(name, data)...)
	}
	sortFindings(findings)
	return findings, nil
}

func ScanBytes(path string, data []byte) []Finding {
	if len(data) == 0 || isBinary(data) {
		return nil
	}
	var findings []Finding
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		findings = appendLineFindings(findings, path, lineNo, line)
	}
	return findings
}

func Summary(findings []Finding, limit int) string {
	if len(findings) == 0 {
		return "no secret-like values found"
	}
	if limit <= 0 || limit > len(findings) {
		limit = len(findings)
	}
	parts := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		f := findings[i]
		location := f.Path
		if f.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, f.Line)
		}
		parts = append(parts, location+" "+f.Kind)
	}
	if len(findings) > limit {
		parts = append(parts, fmt.Sprintf("+%d more", len(findings)-limit))
	}
	return strings.Join(parts, "; ")
}

func appendLineFindings(findings []Finding, path string, lineNo int, line string) []Finding {
	checks := []struct {
		kind    string
		pattern *regexp.Regexp
	}{
		{kind: "api_key_prefix", pattern: skPattern},
		{kind: "bearer_token", pattern: bearerPattern},
		{kind: "url_userinfo", pattern: urlUserInfoPattern},
		{kind: "github_token", pattern: githubClassicPattern},
		{kind: "github_token", pattern: githubFineGrainedPattern},
		{kind: "aws_access_key", pattern: awsAccessKeyPattern},
		{kind: "private_key", pattern: privateKeyPattern},
	}
	for _, check := range checks {
		if check.pattern.MatchString(line) {
			findings = append(findings, Finding{
				Path:    path,
				Line:    lineNo,
				Kind:    check.kind,
				Snippet: redactSnippet(line),
			})
		}
	}
	matches := secretAssignmentPattern.FindAllStringSubmatch(line, -1)
	for _, match := range matches {
		if len(match) < 3 || !isSecretKeyName(match[1]) || isPlaceholderValue(match[2]) {
			continue
		}
		findings = append(findings, Finding{
			Path:    path,
			Line:    lineNo,
			Kind:    "secret_assignment",
			Snippet: redactSnippet(line),
		})
	}
	return findings
}

func isPlaceholderValue(value string) bool {
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	if value == "" {
		return true
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(value, "$") || strings.HasPrefix(value, "${") || strings.HasPrefix(value, "<") {
		return true
	}
	switch lower {
	case "redacted", "<redacted>", "placeholder", "changeme", "change_me", "replace_me", "todo", "none", "null", "unset":
		return true
	}
	return false
}

func isSecretKeyName(key string) bool {
	key = strings.ToUpper(strings.TrimSpace(key))
	key = strings.TrimLeft(key, "-")
	switch key {
	case "KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "AUTH", "AUTHORIZATION":
		return true
	}
	for _, marker := range []string{"_TOKEN", "-TOKEN", ".TOKEN", "_SECRET", "-SECRET", ".SECRET", "_PASSWORD", "-PASSWORD", ".PASSWORD", "_AUTH", "-AUTH", ".AUTH", "_KEY", "-KEY", ".KEY"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return strings.HasSuffix(key, "APIKEY")
}

func redactSnippet(line string) string {
	line = commandlog.RedactText(line)
	line = secretAssignmentPattern.ReplaceAllString(line, "$1=<redacted>")
	line = githubClassicPattern.ReplaceAllString(line, "ghp_<redacted>")
	line = githubFineGrainedPattern.ReplaceAllString(line, "github_pat_<redacted>")
	line = awsAccessKeyPattern.ReplaceAllString(line, "AKIA<redacted>")
	line = strings.TrimSpace(line)
	if len(line) > 180 {
		line = line[:180] + "..."
	}
	return line
}

func shouldSkipPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if base == ".ds_store" {
		return true
	}
	return binaryExtensions[strings.ToLower(filepath.Ext(path))]
}

func shouldSkipDir(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ".git", ".harbor-factory", "node_modules", ".venv", "venv", "vendor", "dist", "build", "__pycache__":
		return true
	default:
		return false
	}
}

func isBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	limit := len(data)
	if limit > 8192 {
		limit = 8192
	}
	return bytes.IndexByte(data[:limit], 0) >= 0
}

func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Kind < findings[j].Kind
	})
}
