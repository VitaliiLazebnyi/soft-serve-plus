//go:build !windows

package backend

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// terminateOnCancel starts cmd in a process group of its own and makes
// cancelling its context send the whole group SIGTERM rather than SIGKILL to
// git alone: git handles SIGTERM by removing its lock files and exiting, and
// the transport helpers it started (ssh, git-remote-https) go with it rather
// than hold its output open. cmd.WaitDelay kills git if it has not exited by
// then.
func terminateOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Through os.Process first: once git has been waited for, its pid
		// and so its group id can belong to another process. os/exec can
		// call Cancel after it reaped git, when git exited just as ctx was
		// done; the helpers git left running then get no SIGTERM, and
		// cmd.WaitDelay still bounds the wait for the output they hold.
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			// The group is gone: git and its helpers have exited.
			return os.ErrProcessDone
		}
		return err
	}
}
