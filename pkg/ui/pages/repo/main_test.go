package repo

import (
	"os"
	"testing"
)

// TestMain keeps the user's and the system's git config out of the git
// commands the tests run, so that they behave the same on every machine.
func TestMain(m *testing.M) {
	_ = os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}
