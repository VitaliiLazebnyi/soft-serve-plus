//go:build windows

package backend

import "os/exec"

// killTreeOnCancel leaves cmd to the default cancellation, which kills git
// only. A transport helper it started exits on its own once its connection
// times out or closes; cloneWaitDelay keeps the import from waiting on it.
func killTreeOnCancel(*exec.Cmd) {}
