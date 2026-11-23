package lfs

import (
	"os"
	"testing"
)

// TestMain keeps the git commands the tests and the scanner run from reading
// the host's system and global configuration. It is set once here, rather
// than with t.Setenv in each test, so the tests can run in parallel.
func TestMain(m *testing.M) {
	_ = os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}
