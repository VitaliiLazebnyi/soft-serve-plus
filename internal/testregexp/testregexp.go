// Package testregexp keeps the results of this module's highlighting tests
// from depending on the clock. chroma gives each match of a lexer's rule a
// wall-clock timeout and takes a match that timed out for no match, without
// an error: in a process that was stalled, the same text can get other
// tokens. Nothing outside tests uses this package.
package testregexp

import (
	"testing"
	"time"

	"github.com/dlclark/regexp2/v2"
)

// never is longer than any test binary runs, and short enough to add to a
// timeout without overflowing a time.Duration.
const never = 100 * 365 * 24 * time.Hour

// DisableTimeouts keeps every regexp2 match of the test binary from timing
// out, those of chroma's lexers and of the lexers they embed included.
// regexp2 checks deadlines against a clock it updates once every check
// period, and adds that period to each deadline, so a period of never leaves
// no deadline to reach. A match's other limit, on its backtracking stack,
// stays.
//
// The period is process-wide and read without a lock: call this from a test
// file's init, before anything matches.
func DisableTimeouts() {
	if !testing.Testing() {
		panic("testregexp: DisableTimeouts called outside a test")
	}
	regexp2.SetTimeoutCheckPeriod(never)
}
