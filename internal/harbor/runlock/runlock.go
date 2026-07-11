package runlock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const filename = ".runner.lock"

var ErrActive = errors.New("workspace run is already active")

type Metadata struct {
	PID       int       `json:"pid"`
	RunID     string    `json:"run_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

type Lock struct {
	file *os.File
}

func Acquire(workspace string, metadata Metadata) (*Lock, error) {
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace for run lock: %w", err)
	}
	path := filepath.Join(workspace, filename)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workspace run lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrActive
		}
		return nil, fmt.Errorf("acquire workspace run lock: %w", err)
	}
	if metadata.PID == 0 {
		metadata.PID = os.Getpid()
	}
	if metadata.StartedAt.IsZero() {
		metadata.StartedAt = time.Now().UTC()
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("truncate workspace run lock: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("seek workspace run lock: %w", err)
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("write workspace run lock: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("sync workspace run lock: %w", err)
	}
	return &Lock{file: file}, nil
}

func IsActive(workspace string) (bool, error) {
	path := filepath.Join(workspace, filename)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil
		}
		return false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return false, err
	}
	return false, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
