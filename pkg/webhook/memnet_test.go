package webhook

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
)

// The receivers of tests that use useHTTPClient listen on an in-memory
// network instead of loopback TCP, and the client useHTTPClient installs
// dials them there.
//
// Loopback TCP is not reliable enough for these tests. While several -race
// test binaries opened and closed loopback connections at once, macOS stalled
// every loopback connect on the machine for seconds at a time: connects to a
// fresh listener completed only after 0.5, 1, 2, 4 or 6 seconds (the steps of
// SYN retransmission), or failed with ETIMEDOUT after about 7.8 seconds, in
// the test binaries and in a separate process probing loopback at the same
// moment. CPU load alone did not do it. No client timeout covers a connect
// the kernel gives up on, and a test that expects a delivery then sees none.
// A pipe has no connect to stall, and these tests no longer add to the churn.
//
// Tests of the SSRF-protected client itself (allowed_hosts_test.go,
// ssrf_test.go) still use TCP, since its checks run on the socket.

// memNet maps each in-memory receiver's address to its listener.
var memNet = struct {
	sync.Mutex
	next      int
	listeners map[string]*memListener
}{listeners: map[string]*memListener{}}

// memAddr is the address of an in-memory receiver: a host name in the
// reserved .invalid domain, which never resolves, and port 80, so the
// receiver's URL is an ordinary http URL.
type memAddr string

func (memAddr) Network() string  { return "mem" }
func (a memAddr) String() string { return string(a) }

// memListener is a net.Listener whose connections are the server ends of
// pipes made by dialMem.
type memListener struct {
	addr  memAddr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newMemListener() *memListener {
	memNet.Lock()
	defer memNet.Unlock()
	memNet.next++
	l := &memListener{
		addr:  memAddr(fmt.Sprintf("receiver-%d.invalid:80", memNet.next)),
		conns: make(chan net.Conn),
		done:  make(chan struct{}),
	}
	memNet.listeners[string(l.addr)] = l
	return l
}

func (l *memListener) Accept() (net.Conn, error) {
	// A select with both cases ready picks one at random, so a closed
	// listener is checked on its own first and never hands out a conn.
	select {
	case <-l.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() {
		memNet.Lock()
		delete(memNet.listeners, string(l.addr))
		memNet.Unlock()
		close(l.done)
	})
	return nil
}

func (l *memListener) Addr() net.Addr { return l.addr }

// dialMem connects to the in-memory receiver at addr. An address with no
// receiver, or one that has been closed, is refused as TCP would refuse it.
func dialMem(ctx context.Context, _, addr string) (net.Conn, error) {
	memNet.Lock()
	l := memNet.listeners[addr]
	memNet.Unlock()
	refused := &net.OpError{Op: "dial", Net: "mem", Addr: memAddr(addr), Err: syscall.ECONNREFUSED}
	if l == nil {
		return nil, refused
	}
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		client.Close() //nolint:errcheck
		server.Close() //nolint:errcheck
		return nil, refused
	case <-ctx.Done():
		client.Close() //nolint:errcheck
		server.Close() //nolint:errcheck
		return nil, ctx.Err()
	}
}

// newTestServer starts an httptest server for h on the in-memory network
// and closes it when the test ends. Only the client useHTTPClient installs
// can reach it.
func newTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	// NewUnstartedServer has already opened a loopback listener; it is
	// replaced before anything can connect to it.
	srv.Listener.Close() //nolint:errcheck
	srv.Listener = newMemListener()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}
