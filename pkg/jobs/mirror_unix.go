//go:build !windows

package jobs

import (
	"os/exec"
	"syscall"
)

// terminateOnCancel makes cancelling cmd's context send it SIGTERM rather
// than SIGKILL. git handles SIGTERM by removing its lock files and exiting;
// cmd.WaitDelay kills it if it has not exited by then.
func terminateOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		// os.ErrProcessDone, for a git that already exited, is what
		// os/exec expects from Cancel then.
		return cmd.Process.Signal(syscall.SIGTERM) //nolint:wrapcheck
	}
}
