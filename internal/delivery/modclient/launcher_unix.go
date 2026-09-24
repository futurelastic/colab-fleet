//go:build unix

package modclient

import (
	"os/exec"
	"syscall"
)

// childSysProcAttr puts the module in its own process group so Kill can take
// down everything it spawned.
func childSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// killGroup sends SIGKILL to the module's process group, and to the process
// itself in case it is not (yet) a group leader.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
