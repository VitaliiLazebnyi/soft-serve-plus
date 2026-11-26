package cmd

import (
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/testdial"
	"github.com/charmbracelet/soft-serve/internal/testregexp"
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
//
// It also keeps git off the user's and the system's git config, so the
// tests, which run in parallel, see the same git whatever the host has.
func TestMain(m *testing.M) {
	_ = os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	restoreDial := testdial.Set(testNet.Dial)
	restoreResolver := testdial.SetResolver(testdial.NoDNS())
	code := m.Run()
	restoreResolver()
	restoreDial()
	os.Exit(code)
}

// The tests of blob --color and commit --color check what chroma's lexers
// make of their input, which must not depend on chroma's regex timeout.
func init() { testregexp.DisableTimeouts() }
