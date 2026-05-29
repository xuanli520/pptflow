//go:build !windows

package executor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const commandTerminationGrace = 10 * time.Second

func prepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateCommand(cmd *exec.Cmd, done <-chan struct{}) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	err := syscall.Kill(-pid, syscall.SIGTERM)
	if err == nil {
		go killCommandGroupAfterGrace(cmd, pid, done)
		return nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if signalErr := cmd.Process.Signal(syscall.SIGTERM); signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
		return signalErr
	}
	go killCommandGroupAfterGrace(cmd, pid, done)
	return nil
}

func killCommandGroupAfterGrace(cmd *exec.Cmd, pid int, done <-chan struct{}) {
	if done != nil {
		select {
		case <-done:
			return
		case <-time.After(commandTerminationGrace):
		}
	} else {
		time.Sleep(commandTerminationGrace)
	}
	if done != nil {
		select {
		case <-done:
			return
		default:
		}
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		_ = killErr
	}
}
