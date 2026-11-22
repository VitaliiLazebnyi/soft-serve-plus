package cmd

import "github.com/charmbracelet/soft-serve/internal/testregexp"

// The tests of blob --color and commit --color check what chroma's lexers
// make of their input, which must not depend on the clock.
func init() { testregexp.DisableTimeouts() }
