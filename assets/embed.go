package assets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed config.yaml scripts/* prompt_profiles/*
var FS embed.FS

type ReleasedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func Release(controlDir string) ([]ReleasedFile, error) {
	var released []ReleasedFile
	for _, root := range []string{"scripts", "prompt_profiles"} {
		err := fs.WalkDir(FS, root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			content, err := FS.ReadFile(path)
			if err != nil {
				return err
			}
			target := filepath.Join(controlDir, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if root == "prompt_profiles" {
				existing, err := os.ReadFile(target)
				if err == nil {
					released = append(released, releasedFileForContent(target, existing))
					return nil
				}
				if err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			if err := os.WriteFile(target, content, 0o644); err != nil {
				return err
			}
			released = append(released, releasedFileForContent(target, content))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return released, nil
}

func releasedFileForContent(path string, content []byte) ReleasedFile {
	sum := sha256.Sum256(content)
	return ReleasedFile{
		Path:   path,
		SHA256: hex.EncodeToString(sum[:]),
	}
}
