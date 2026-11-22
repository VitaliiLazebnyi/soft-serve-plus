// Package uitest holds what the tests of the TUI share.
package uitest

import (
	"sync/atomic"
	"testing"
	"time"
)

// HangGuard is how long a test waits for a command, or for the commands it
// started to settle, before it fails as hung. The commands the TUI returns
// load git data and finish however slow the machine is: a loaded Windows
// runner takes seconds per git command. So a test runs each command to
// completion, drops timers such as spinner ticks by their type, and never
// skips a command for being slow; the guard only turns a hang into a
// failure.
const HangGuard = time.Minute

// WaitFor reports whether cond holds within HangGuard. It counts polls
// rather than watch a deadline, which a pause of the process would use up.
func WaitFor(cond func() bool) bool {
	const poll = 10 * time.Millisecond
	for range HangGuard / poll {
		if cond() {
			return true
		}
		time.Sleep(poll)
	}
	return cond()
}

// Receive returns what c gives, or false when WaitFor gives up on it.
func Receive[T any](c <-chan T) (v T, ok bool) {
	ok = WaitFor(func() bool {
		select {
		case v = <-c:
			return true
		default:
			return false
		}
	})
	return v, ok
}

// Unbounded is a highlighting budget that never ends.
func Unbounded() <-chan time.Time { return nil }

// leaked is set once WaitIdle has failed.
var leaked atomic.Bool

// WaitIdle waits until running, package common's Tokenising, returns 0: a
// tokenisation holds its slot until it ends, also past its budget, and
// highlighting that finds no slot free shows plain text. A slot that is
// never freed would cost every later test a HangGuard, so once a wait has
// failed the following ones fail at once.
func WaitIdle(t testing.TB, running func() int) {
	t.Helper()
	idle := func() bool { return running() == 0 }
	if idle() || !leaked.Load() && WaitFor(idle) {
		return
	}
	leaked.Store(true)
	t.Fatal("tokenisations of earlier tests never ended")
}
