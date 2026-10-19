//go:build linux

package benchlib

import (
	"os/exec"
	"syscall"
)

// prepareChild asks the kernel to SIGKILL the child when this process dies, so a server
// never outlives the tool that started it.
func prepareChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

func adoptChild(cmd *exec.Cmd) error { return nil }

// ProcessAlive reports whether a process with this pid is still running (zombies count
// as ended).
func ProcessAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	var ws syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	return !(err == nil && wpid == pid)
}
