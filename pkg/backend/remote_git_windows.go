//go:build windows

package backend

import "os/exec"

// terminateOnCancel leaves cmd to the default cancellation, which kills git
// only: Windows has no SIGTERM to ask it to stop. A transport helper it
// started exits on its own once its connection times out or closes;
// cmd.WaitDelay keeps RunRemoteGit from waiting on it.
//
// A git killed while it updates refs leaves its *.lock files, packed-refs.lock
// among them, and later fetches into a mirror fail on "File exists" until an
// operator removes them. Removing them automatically is not safe: nothing
// tells a lock a killed git left from one a running git holds, such as a
// push an admin is doing after turning mirroring off. On Windows, stopping
// the server while a mirror fetches, or a fetch running out of time, can
// therefore need that cleanup. An import's clone is removed when it is
// stopped, so it leaves nothing behind.
func terminateOnCancel(*exec.Cmd) {}
