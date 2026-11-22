package cmd

import "github.com/charmbracelet/soft-serve/internal/testregexp"

// blob --color's tests check what chroma's lexers match, which must not
// depend on the clock.
func init() { testregexp.DisableTimeouts() }
