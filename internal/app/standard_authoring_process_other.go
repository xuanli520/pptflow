//go:build !unix

package app

import "os/exec"

func standardAuthoringConfigureGitCommand(_ *exec.Cmd) {}
