package cmd

import (
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/testdial"
)

// TestMain keeps pkg/ssrf off the network in this package's tests. Every
// connection an SSRF-protected client would open is refused: the webhooks
// the tests create have public test addresses, and a send to one fails at
// once. Tests that need a recorded failure point the webhook at loopback,
// which the SSRF checks refuse before any connection (see
// whPointAtLoopback). A hostname pkg/ssrf looks up, such as an import
// remote's, resolves from the hosts file only and otherwise fails at once.
func TestMain(m *testing.M) {
	restoreDial := testdial.Set(new(testdial.Network).Dial)
	restoreResolver := testdial.SetResolver(testdial.NoDNS())
	code := m.Run()
	restoreResolver()
	restoreDial()
	os.Exit(code)
}
