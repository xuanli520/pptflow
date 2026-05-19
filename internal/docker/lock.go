package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type MaintenanceLock struct {
	Path string
	file *os.File
}

func AcquireMaintenanceLock(scanPath, operation string) (MaintenanceLock, error) {
	lockDir := filepath.Join(scanPath, ".qa-control", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return MaintenanceLock{}, err
	}
	path := filepath.Join(lockDir, "docker-maintenance.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil && os.IsExist(err) && staleMaintenanceLock(path) {
		_ = os.Remove(path)
		file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	}
	if err != nil {
		return MaintenanceLock{}, err
	}
	_, _ = fmt.Fprintf(file, "pid=%d\ncreated_at=%s\noperation=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339), operation)
	return MaintenanceLock{Path: path, file: file}, nil
}

func (l MaintenanceLock) Release() {
	if l.file != nil {
		_ = l.file.Close()
	}
	if l.Path != "" {
		_ = os.Remove(l.Path)
	}
}

func staleMaintenanceLock(path string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	pid, err := strconv.Atoi(values["pid"])
	if err != nil || pid <= 0 {
		return false
	}
	return !processAlive(pid)
}
