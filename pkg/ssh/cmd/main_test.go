package cmd

import (
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/testdial"
)

// testNet is the in-memory network every connection an SSRF-protected
// client opens in this package's tests goes to (see newMemServer).
var testNet testdial.Network

// TestMain keeps pkg/ssrf off the network in this package's tests. Every
// connection an SSRF-protected client would open goes to testNet, where only
// a test's own receiver listens (newMemServer): the webhooks the tests create
// have public test addresses, and a send to one fails at once. A test that
// needs a recorded failure points the webhook at loopback, which the SSRF
// checks refuse before any connection unless webhook.allowed_hosts covers it
// (see whPointAtLoopback); a test that receives the webhooks allows
// 127.0.0.0/8 and listens on testNet. A hostname pkg/ssrf looks up, such as
// an import remote's, resolves from the hosts file only and otherwise fails
// at once.
func TestMain(m *testing.M) {
	restoreDial := testdial.Set(testNet.Dial)
	restoreResolver := testdial.SetResolver(testdial.NoDNS())
	code := m.Run()
	restoreResolver()
	restoreDial()
	os.Exit(code)
}
