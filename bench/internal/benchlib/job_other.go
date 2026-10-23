//go:build !windows && !linux

package benchlib

import (
	"os/exec"
	"syscall"
)

// On other systems children are only stopped by StopAll (Fatal and signal handling).
func prepareChild(cmd *exec.Cmd) {}

func adoptChild(cmd *exec.Cmd) error { return nil }

// ProcessAlive reports whether a process with this pid is still running.
func ProcessAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
