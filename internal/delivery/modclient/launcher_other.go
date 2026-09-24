//go:build !unix

package modclient

import (
	"os/exec"
	"syscall"
)

// childSysProcAttr is a no-op where process groups are not available.
func childSysProcAttr() *syscall.SysProcAttr { return nil }

// killGroup kills just the module process where groups are not available.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
