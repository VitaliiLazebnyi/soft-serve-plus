package repo

import (
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/testregexp"
)

// TestMain keeps the user's and the system's git config out of the git
// commands the tests run, so that they behave the same on every machine.
func TestMain(m *testing.M) {
	_ = os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}

// These tests check rendered diffs, whose colours come from chroma and must
// not depend on its regex timeout.
func init() { testregexp.DisableTimeouts() }
