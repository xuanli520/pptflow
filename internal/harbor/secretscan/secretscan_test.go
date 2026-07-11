package secretscan

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanBytesFindsSecretsAndRedactsSnippets(t *testing.T) {
	apiStyleKey := "sk-" + "secretvalue"
	githubClassic := "ghp_" + "abcdefghijklmnopqrstuvwxyz"
	githubFineGrained := "github_pat_" + "abcdefghijklmnopqrstuvwxyz"
	awsAccessKey := "AKIA" + "ABCDEFGHIJKLMNOP"
	findings := ScanBytes("logs/run.txt", []byte(strings.Join([]string{
		"OPENAI_API_KEY : raw-api-value",
		"Authorization: Bearer abc.def.ghi",
		"https://user:pass@example.com/repo",
		apiStyleKey,
		githubClassic,
		githubFineGrained,
		awsAccessKey,
		"-----BEGIN OPENSSH PRIVATE KEY-----",
	}, "\n")))
	if len(findings) < 8 {
		t.Fatalf("findings = %+v", findings)
	}
	joined := findingsText(findings)
	for _, secret := range []string{"raw-api-value", "abc.def.ghi", "user:pass@example", apiStyleKey, githubClassic, githubFineGrained, awsAccessKey} {
		if strings.Contains(joined, secret) {
			t.Fatalf("secret %q leaked in findings: %s", secret, joined)
		}
	}
}

func TestScanBytesIgnoresPlaceholders(t *testing.T) {
	findings := ScanBytes("example.env", []byte(strings.Join([]string{
		"OPENAI_API_KEY=$OPENAI_API_KEY",
		"ANTHROPIC_AUTH_TOKEN=<redacted>",
		"PASSWORD=replace_me",
	}, "\n")))
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %+v", findings)
	}
}

func TestScanBytesFindsJSONSecretAssignments(t *testing.T) {
	secretValue := "raw-json-token"
	findings := ScanBytes("result.json", []byte(`{"model":"qwen","API_TOKEN":"`+secretValue+`"}`))
	if len(findings) != 1 || findings[0].Kind != "secret_assignment" {
		t.Fatalf("expected JSON secret assignment finding, got %+v", findings)
	}
	if strings.Contains(findings[0].Snippet, secretValue) {
		t.Fatalf("secret leaked in snippet: %+v", findings[0])
	}
}

func TestScanDirSkipsBinaryImages(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "instruction.md"), []byte("TOKEN=raw-token-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pass4.png"), []byte{0x89, 'P', 'N', 'G', 0, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := ScanDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Path != "instruction.md" {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestScanZipFindsTextSecrets(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "task.zip")
	out, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	w, err := zw.Create("task/instruction.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("AUTH_TOKEN=raw-token-value\n")); err != nil {
		t.Fatal(err)
	}
	img, err := zw.Create("task/pass4.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := img.Write([]byte{0x89, 'P', 'N', 'G', 0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	findings, err := ScanZip(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Path != "task/instruction.md" {
		t.Fatalf("findings = %+v", findings)
	}
}

func findingsText(findings []Finding) string {
	var b strings.Builder
	for _, finding := range findings {
		b.WriteString(finding.Kind)
		b.WriteByte(' ')
		b.WriteString(finding.Snippet)
		b.WriteByte('\n')
	}
	return b.String()
}
