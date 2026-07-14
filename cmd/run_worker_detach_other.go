//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package cmd

import "os/exec"

func configureDetachedRunWorkerProcess(_ *exec.Cmd) {}
