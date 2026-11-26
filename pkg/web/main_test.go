package web

import (
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/testnet"
)

// TestMain keeps git off the user's and the system's git config, so the
// tests, which run in parallel, see the same git whatever the host has.
// HTTP clients without a transport of their own retry a loopback connect
// that timed out (see testnet.Dial).
func TestMain(m *testing.M) {
	testnet.UseDialForDefaultTransport()
	_ = os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}
