//go:build windows

package executor

import (
	"errors"
	"os"
	"os/exec"
)

func prepareCommand(cmd *exec.Cmd) {}

func terminateCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
