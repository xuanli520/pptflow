package docker

import (
	"errors"
	"os"
	"path/filepath"
)

type runtimeEnvFilePreparation struct {
	Generated []string
	Warnings  []string
}

func prepareRuntimeEnvFiles(workDir string) runtimeEnvFilePreparation {
	var result runtimeEnvFilePreparation
	if workDir == "" {
		return result
	}
	envPath := filepath.Join(workDir, ".env")
	if fileExists(envPath) {
		return result
	}
	examplePath := filepath.Join(workDir, ".env.example")
	content, err := os.ReadFile(examplePath)
	if os.IsNotExist(err) {
		return result
	}
	if err != nil {
		result.Warnings = append(result.Warnings, "runtime env preparation skipped: "+err.Error())
		return result
	}
	file, err := os.OpenFile(envPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return result
	}
	if err != nil {
		result.Warnings = append(result.Warnings, "runtime env preparation skipped: "+err.Error())
		return result
	}
	if _, err := file.Write(content); err != nil {
		result.Warnings = append(result.Warnings, "runtime env preparation incomplete: "+err.Error())
		_ = file.Close()
		return result
	}
	if err := file.Close(); err != nil {
		result.Warnings = append(result.Warnings, "runtime env preparation incomplete: "+err.Error())
		return result
	}
	result.Generated = append(result.Generated, envPath)
	return result
}
