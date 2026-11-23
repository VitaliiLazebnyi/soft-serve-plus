package ui

import (
	"os"
	"testing"
)

// TestMain keeps git off the user's and the system's git config, so the
// tests, which run in parallel, see the same git whatever the host has.
func TestMain(m *testing.M) {
	_ = os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}
