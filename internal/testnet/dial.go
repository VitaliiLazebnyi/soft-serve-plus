package testnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"

	gossh "golang.org/x/crypto/ssh"
)

// maxDialAttempts bounds how many times Dial tries a connect that timed out.
// On macOS a stalled loopback connect fails with ETIMEDOUT after about 7.8 s,
// so four attempts wait at most about half a minute.
const maxDialAttempts = 4

// dialContext makes one connect attempt; tests of Dial replace it.
var dialContext = (&net.Dialer{}).DialContext

// Dial connects to addr as net.Dialer.DialContext does, and tries again,
// up to maxDialAttempts in all, when the connect itself timed out.
//
// Why a retry is safe here: on macOS, many test binaries opening loopback
// connections at once make the kernel stall some TCP connects machine-wide
// for seconds and fail them with ETIMEDOUT (KNOWN_ISSUES item 309). A
// connect that timed out never completed the TCP handshake, so the server
// under test never accepted it: the next attempt is a fresh connection the
// server sees as the first one. Nothing else is retried: a refused
// connection, a reset, or the caller's context ending are returned at once,
// so a server that is down or slow still fails its test. Each retry is
// written to stderr, so the stall stays visible in the test output.
//
// Dial is for test clients only; the server's own dials never retry.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var err error
	for attempt := 1; ; attempt++ {
		var c net.Conn
		c, err = dialContext(ctx, network, addr)
		if err == nil || !ConnectTimedOut(err) || ctx.Err() != nil || attempt == maxDialAttempts {
			return c, err
		}
		fmt.Fprintf(os.Stderr, "testnet: connect to %s timed out (attempt %d of %d), trying again: %v\n",
			addr, attempt, maxDialAttempts, err)
	}
}

// ConnectTimedOut reports whether err is a TCP connect that timed out in
// the kernel, which is the error Dial tries again on. A deadline or the
// context ending is not one: those are the caller's own limits.
func ConnectTimedOut(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial" && errors.Is(err, syscall.ETIMEDOUT)
}

// DialSSH is gossh.Dial over Dial.
func DialSSH(network, addr string, config *gossh.ClientConfig) (*gossh.Client, error) {
	ctx := context.Background()
	if config.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, config.Timeout)
		defer cancel()
	}
	conn, err := Dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := gossh.NewClientConn(conn, addr, config)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return gossh.NewClient(c, chans, reqs), nil
}

// UseDialForDefaultTransport makes http.DefaultTransport connect with Dial,
// so http.DefaultClient and every client without a Transport of its own
// get its retry. Call it from a test package's TestMain, before any test
// runs.
func UseDialForDefaultTransport() {
	http.DefaultTransport.(*http.Transport).DialContext = Dial
}

// UseDial makes c's transport, an *http.Transport such as an
// httptest.Server's, connect with Dial, and returns c. Call it before c
// sends anything: the transport is changed in place, so c keeps reusing
// its connections.
func UseDial(c *http.Client) *http.Client {
	c.Transport.(*http.Transport).DialContext = Dial
	return c
}
