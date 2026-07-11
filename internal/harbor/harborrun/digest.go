package harborrun

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var digestRoots = []string{
	"instruction.md",
	"task.toml",
	"tests_analysis.md",
	"environment",
	"solution",
	"tests",
}

func ComputeTaskDigest(taskDir string) (string, error) {
	taskDir = strings.TrimSpace(taskDir)
	if taskDir == "" {
		return "", fmt.Errorf("task directory is required")
	}
	info, err := os.Stat(taskDir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("task path is not a directory: %s", taskDir)
	}
	var files []string
	for _, root := range digestRoots {
		path := filepath.Join(taskDir, root)
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			files = append(files, root)
			continue
		}
		err = filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeType != 0 {
				return nil
			}
			rel, err := filepath.Rel(taskDir, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(files)
	hash := sha256.New()
	for _, rel := range files {
		path := filepath.Join(taskDir, filepath.FromSlash(rel))
		if _, err := io.WriteString(hash, rel+"\n"); err != nil {
			return "", err
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(hash, file); err != nil {
			_ = file.Close()
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
		if _, err := io.WriteString(hash, "\n"); err != nil {
			return "", err
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
