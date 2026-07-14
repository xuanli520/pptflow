//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cmd

import (
	"os/exec"
	"syscall"
)

func configureDetachedRunWorkerProcess(command *exec.Cmd) {
	if command != nil {
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
}
