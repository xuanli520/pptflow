package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type ArtifactWarning = model.ArtifactWarning

type ArtifactWriter struct {
	Root string
}

func NewArtifactWriter(root string) ArtifactWriter {
	return ArtifactWriter{Root: filepath.Clean(root)}
}

func (w ArtifactWriter) RequiredJSON(path string, value any) error {
	if err := writeJSON(w.resolve(path), value); err != nil {
		return fmt.Errorf("write required artifact %s: %w", w.displayPath(path), err)
	}
	return nil
}

func (w ArtifactWriter) RequiredText(path, content string) error {
	if err := writeText(w.resolve(path), content); err != nil {
		return fmt.Errorf("write required artifact %s: %w", w.displayPath(path), err)
	}
	return nil
}

func (w ArtifactWriter) BestEffortJSON(path string, value any) ArtifactWarning {
	if err := writeJSON(w.resolve(path), value); err != nil {
		return newArtifactWarning(path, "write_json", false, err)
	}
	return ArtifactWarning{}
}

func (w ArtifactWriter) BestEffortText(path, content string) ArtifactWarning {
	if err := writeText(w.resolve(path), content); err != nil {
		return newArtifactWarning(path, "write_text", false, err)
	}
	return ArtifactWarning{}
}

func (w ArtifactWriter) BestEffortAppend(path, content string) ArtifactWarning {
	if err := appendText(w.resolve(path), content); err != nil {
		return newArtifactWarning(path, "append_text", false, err)
	}
	return ArtifactWarning{}
}

func (w ArtifactWriter) Path(path string) string {
	return w.resolve(path)
}

func (w ArtifactWriter) RelativePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) || w.Root == "" {
		return path
	}
	rel, err := filepath.Rel(filepath.Clean(w.Root), filepath.Clean(path))
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return filepath.Clean(path)
	}
	return rel
}

func (w ArtifactWriter) resolve(path string) string {
	path = strings.TrimSpace(path)
	if filepath.IsAbs(path) || w.Root == "" {
		return filepath.Clean(path)
	}
	return filepath.Join(w.Root, path)
}

func (w ArtifactWriter) displayPath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return strings.TrimSpace(path)
}

func newArtifactWarning(path, op string, required bool, err error) ArtifactWarning {
	if err == nil {
		return ArtifactWarning{}
	}
	return ArtifactWarning{
		Path:       filepath.Clean(path),
		Op:         op,
		Error:      err.Error(),
		Required:   required,
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func requiredStageJSON(record model.StageRecord, writer ArtifactWriter, path string, value any) model.StageRecord {
	if err := writer.RequiredJSON(path, value); err != nil {
		return recordArtifactWriteError(record, err, writer.Path(path))
	}
	return record
}

func requiredStageText(record model.StageRecord, writer ArtifactWriter, path, content string) model.StageRecord {
	if err := writer.RequiredText(path, content); err != nil {
		return recordArtifactWriteError(record, err, writer.Path(path))
	}
	return record
}

func bestEffortStageJSON(record *model.StageRecord, writer ArtifactWriter, path string, value any) {
	recordArtifactWarning(record, writer.BestEffortJSON(path, value))
}

func bestEffortStageText(record *model.StageRecord, writer ArtifactWriter, path, content string) {
	recordArtifactWarning(record, writer.BestEffortText(path, content))
}

func bestEffortStageAppend(record *model.StageRecord, writer ArtifactWriter, path, content string) {
	recordArtifactWarning(record, writer.BestEffortAppend(path, content))
}

func recordArtifactWarning(record *model.StageRecord, warning ArtifactWarning) {
	if record == nil || warning.OK() {
		return
	}
	record.ArtifactWarnings = append(record.ArtifactWarnings, warning)
}

func recordArtifactWarnings(record *model.StageRecord, writer ArtifactWriter, warnings []ArtifactWarning) {
	for _, warning := range warnings {
		if warning.Path != "" {
			warning.Path = writer.RelativePath(warning.Path)
		}
		recordArtifactWarning(record, warning)
	}
}

func copyPackageSnapshot(source, dest string) error {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	skipTopLevel := map[string]bool{
		".git":        true,
		".qa-control": true,
		"qa":          true,
		"result":      true,
		"task-docs":   true,
	}
	skipDirNames := map[string]bool{
		".next":         true,
		".pytest_cache": true,
		".venv":         true,
		"__pycache__":   true,
		"build":         true,
		"coverage":      true,
		"dist":          true,
		"node_modules":  true,
		"venv":          true,
	}
	return filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == "." {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) > 0 && skipTopLevel[parts[0]] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && skipDirNames[d.Name()] {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(source, dest string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(content, '\n'), 0o644)
}

func writeText(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func appendText(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(content)
	return err
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
