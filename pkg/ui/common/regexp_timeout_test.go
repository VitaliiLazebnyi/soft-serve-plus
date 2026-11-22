package common

import "github.com/charmbracelet/soft-serve/internal/testregexp"

// These tests check chroma's tokens, which must not depend on the clock.
func init() { testregexp.DisableTimeouts() }
