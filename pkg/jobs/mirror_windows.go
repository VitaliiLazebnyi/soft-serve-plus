//go:build windows

package jobs

import "os/exec"

// terminateOnCancel leaves cmd to the default cancellation, which kills git:
// Windows has no SIGTERM to ask it to stop, and git does not handle the
// console events a process could be sent instead.
func terminateOnCancel(*exec.Cmd) {}
