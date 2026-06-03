package git

import (
	"errors"
	"fmt"
	"strings"
)

type SyncProgress struct {
	Phase   string
	Percent int
	Message string
}

type SyncResult struct {
	Operation string
	Commit    string
	RepoPath  string
	ClonePath string
	Error     error
}

type SyncCallback func(SyncProgress)

type DeliveryPackageError struct {
	RepoPath string
	Missing  []string
	Err      error
}

func (e *DeliveryPackageError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return fmt.Sprintf("verify delivery package %s: %v", e.RepoPath, e.Err)
	}
	return fmt.Sprintf("verify delivery package %s: missing %s", e.RepoPath, strings.Join(e.Missing, ", "))
}

func (e *DeliveryPackageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func IsTerminalSyncError(err error) bool {
	var deliveryErr *DeliveryPackageError
	if errors.As(err, &deliveryErr) {
		return true
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	return commandErr.remoteRepositoryMissing()
}

type CommandError struct {
	Dir    string
	Args   []string
	Stdout string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	if e == nil {
		return ""
	}
	message := strings.TrimSpace(e.Stderr)
	if message == "" {
		message = strings.TrimSpace(e.Stdout)
	}
	if message == "" && e.Err != nil {
		message = e.Err.Error()
	}
	if e.Err != nil {
		return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, message)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), message)
}

func (e *CommandError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *CommandError) remoteRepositoryMissing() bool {
	if e == nil || len(e.Args) == 0 {
		return false
	}
	switch e.Args[0] {
	case "clone", "fetch":
	default:
		return false
	}
	text := strings.ToLower(strings.Join([]string{e.Stdout, e.Stderr, e.Error()}, "\n"))
	for _, marker := range []string{
		"project you were looking for could not be found",
		"not found or you don't have permission",
		"repository not found",
		"repository '",
	} {
		if strings.Contains(text, marker) {
			if marker != "repository '" || strings.Contains(text, " not found") {
				return true
			}
		}
	}
	return false
}
