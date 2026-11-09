//go:build !windows

package backend

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// killTreeOnCancel starts cmd in a process group of its own and makes
// cancelling its context kill the whole group, so the transport helpers git
// started go with it.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			// The group is gone: git and its helpers have exited.
			return os.ErrProcessDone
		}
		return err
	}
}
