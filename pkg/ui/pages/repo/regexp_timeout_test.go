package repo

import "github.com/charmbracelet/soft-serve/internal/testregexp"

// These tests compare rendered diffs, whose colours come from chroma and
// must not depend on the clock.
func init() { testregexp.DisableTimeouts() }
