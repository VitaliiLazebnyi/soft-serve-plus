// Package uitest holds what the tests of the TUI share.
package uitest

import "time"

// HangGuard is how long a test waits for a command, or for the commands it
// started to settle, before it fails as hung. The commands the TUI returns
// load git data and finish however slow the machine is: a loaded Windows
// runner takes seconds per git command. So a test runs each command to
// completion, drops timers such as spinner ticks by their type, and never
// skips a command for being slow; the guard only turns a hang into a
// failure.
const HangGuard = time.Minute
