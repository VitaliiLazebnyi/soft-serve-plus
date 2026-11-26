package ssh

import (
	"os"
	"testing"

	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/soft-serve/internal/testnet"
	gossh "golang.org/x/crypto/ssh"
)

// TestMain keeps git off the user's and the system's git config, so the
// tests, which run in parallel, see the same git whatever the host has.
func TestMain(m *testing.M) {
	_ = os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}

// newTestSession is testsession.New, connecting with testnet.DialSSH so a
// loopback connect that timed out is tried again.
func newTestSession(tb testing.TB, srv *ssh.Server, cfg *gossh.ClientConfig) *gossh.Session {
	tb.Helper()
	sess, err := newClientSession(tb, testsession.Listen(tb, srv), cfg)
	if err != nil {
		tb.Fatal(err)
	}
	return sess
}

// newClientSession is testsession.NewClientSession, connecting with
// testnet.DialSSH.
func newClientSession(tb testing.TB, addr string, config *gossh.ClientConfig) (*gossh.Session, error) {
	tb.Helper()
	if config == nil {
		config = &gossh.ClientConfig{
			User: "testuser",
			Auth: []gossh.AuthMethod{gossh.Password("testpass")},
		}
	}
	if config.HostKeyCallback == nil {
		config.HostKeyCallback = gossh.InsecureIgnoreHostKey()
	}
	client, err := testnet.DialSSH("tcp", addr, config)
	if err != nil {
		return nil, err
	}
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	tb.Cleanup(func() {
		_ = session.Close()
		_ = client.Close()
	})
	return session, nil
}
