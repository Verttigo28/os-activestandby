//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// ownGroup puts the command in a process group of its own, and makes the cancellation (timeout) kill the whole group.
func ownGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
