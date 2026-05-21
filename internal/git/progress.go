package git

import (
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
