package testdial

import (
	"context"
	"net"
	"sync"
	"syscall"
)

// Network is an in-memory network for tests. Listen opens a listener at a
// loopback address that no socket uses, and Dial, set with Set, connects to
// it over a pipe. A pipe has no TCP connect for the kernel to stall: while
// many test binaries open and close loopback connections, macOS can hold a
// loopback connect for seconds, or fail it with ETIMEDOUT after about 7.8
// seconds. A dial to any other address is refused, so nothing a secure client
// sends leaves the process.
//
// The zero value is ready to use.
type Network struct {
	mu        sync.Mutex
	next      int
	listeners map[string]*listener
}

// firstPort is the port of a Network's first listener; later ones count up.
const firstPort = 20000

// Listen returns a listener at a fresh address on 127.0.0.1. Its address is
// an ordinary loopback address, so URLs and allowlists treat it as one, but
// only Dial reaches it.
func (n *Network) Listen() net.Listener {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.listeners == nil {
		n.listeners = map[string]*listener{}
	}
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: firstPort + n.next}
	n.next++
	l := &listener{
		net:   n,
		addr:  addr,
		conns: make(chan net.Conn),
		done:  make(chan struct{}),
	}
	n.listeners[addr.String()] = l
	return l
}

// Dial connects to the listener at address. An address with no listener,
// or one that has been closed, is refused as TCP would refuse it. It has the
// signature of a Func.
func (n *Network) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	l := n.listeners[address]
	n.mu.Unlock()
	refused := &net.OpError{Op: "dial", Net: network, Addr: addrString(address), Err: syscall.ECONNREFUSED}
	if l == nil {
		return nil, refused
	}
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		_ = client.Close()
		_ = server.Close()
		return nil, refused
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	}
}

// addrString is a net.Addr for an address that has no listener.
type addrString string

func (addrString) Network() string  { return "tcp" }
func (a addrString) String() string { return string(a) }

// listener is a net.Listener whose connections are the server ends of the
// pipes made by Network.Dial.
type listener struct {
	net   *Network
	addr  *net.TCPAddr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *listener) Close() error {
	l.once.Do(func() {
		l.net.mu.Lock()
		delete(l.net.listeners, l.addr.String())
		l.net.mu.Unlock()
		close(l.done)
	})
	return nil
}

func (l *listener) Addr() net.Addr { return l.addr }
