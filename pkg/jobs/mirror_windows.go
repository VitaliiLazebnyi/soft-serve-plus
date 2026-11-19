//go:build windows

package jobs

import "os/exec"

// terminateOnCancel leaves cmd to the default cancellation, which kills git:
// Windows has no SIGTERM to ask it to stop, and git does not handle the
// console events a process could be sent instead.
//
// A git killed while it updates refs leaves its *.lock files, packed-refs.lock
// among them, and later fetches into the mirror fail on "File exists" until
// an operator removes them. Removing them automatically is not safe: nothing
// tells a lock a killed git left from one a running git holds, such as a
// push an admin is doing after turning mirroring off. On Windows, stopping
// the server while a mirror fetches, or a fetch running out of time, can
// therefore need that cleanup, as the hourly kill always could.
func terminateOnCancel(*exec.Cmd) {}
