package syntax

import "github.com/charmbracelet/soft-serve/internal/testregexp"

// These tests check chroma's tokens, which must not depend on its regex
// timeout.
func init() { testregexp.DisableTimeouts() }
