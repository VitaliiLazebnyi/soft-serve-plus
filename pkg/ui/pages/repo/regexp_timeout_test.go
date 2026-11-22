package repo

import "github.com/charmbracelet/soft-serve/internal/testregexp"

// These tests check rendered diffs, whose colours come from chroma and must
// not depend on its regex timeout.
func init() { testregexp.DisableTimeouts() }
