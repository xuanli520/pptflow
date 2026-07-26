//go:build unix

package app

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// standardAuthoringConfigureGitCommand keeps Git's transport helpers in the
// same process group so source-capture cancellation cannot leave a
// git-remote-https child holding the TUI request open.
func standardAuthoringConfigureGitCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	command.WaitDelay = time.Second
}
