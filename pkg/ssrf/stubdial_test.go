package ssrf

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// errStubRefused is what a stubbed connection attempt fails with.
var errStubRefused = errors.New("stub: connection refused")

// dialRecorder records every address the secure client gets as far as
// connecting to, that is every address its checks let through, with the
// deadline of the attempt. It refuses every attempt except to the addresses
// in connect, which are dialed for real and must be local listeners, so a
// test never leaves the machine.
type dialRecorder struct {
	mu        sync.Mutex
	connect   map[string]bool
	failFirst int // refuse this many attempts before honouring connect
	dialed    []string
	deadlines []time.Time
}

func (r *dialRecorder) addrs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dialed...)
}

// attempt records one attempt and reports whether it may connect.
func (r *dialRecorder) attempt(ctx context.Context, addr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dialed = append(r.dialed, addr)
	d, _ := ctx.Deadline()
	r.deadlines = append(r.deadlines, d)
	if r.failFirst > 0 {
		r.failFirst--
		return errStubRefused
	}
	if !r.connect[addr] {
		return errStubRefused
	}
	return nil
}

// stubConnect replaces the secure client's connection attempts for the rest
// of the test; see dialRecorder. It swaps package state, so tests using it
// must not run in parallel.
func stubConnect(t *testing.T, connect ...string) *dialRecorder {
	t.Helper()
	r := &dialRecorder{connect: map[string]bool{}}
	for _, a := range connect {
		r.connect[a] = true
	}
	hook := dialHookFunc(func(ctx context.Context, _, address string) error {
		return r.attempt(ctx, address)
	})
	orig := dialHook.Swap(&hook)
	t.Cleanup(func() { dialHook.Store(orig) })
	return r
}

// fakeResolver points the package's resolver (the secure client's dials and
// every validation lookup not given a resolver of its own) at a DNS server
// that answers from table for the rest of the test, and reports every other
// name as not found. It swaps package state, so tests using it must not run
// in parallel.
func fakeResolver(t *testing.T, table map[string][]string) {
	t.Helper()
	orig := testResolver.Swap(stubResolver(table))
	t.Cleanup(func() { testResolver.Store(orig) })
}

// stubResolver returns a resolver that sends every query to an in-memory
// DNS server answering from table. The server is reached over net.Pipe, so
// nothing touches the host's DNS or the network; the Go resolver still reads
// the hosts file first, which is why tests use names under .test and
// .internal.
func stubResolver(table map[string][]string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go serveStubDNS(server, table)
			return client, nil
		},
	}
}

// serveStubDNS answers DNS queries on conn, which carries the TCP framing
// (a two-byte length before each message) since a net.Pipe is not a
// PacketConn. Only the question is read; A and AAAA records come from table.
func serveStubDNS(conn net.Conn, table map[string][]string) {
	defer conn.Close() //nolint:errcheck
	for {
		var n uint16
		if err := binary.Read(conn, binary.BigEndian, &n); err != nil {
			return
		}
		q := make([]byte, n)
		if _, err := io.ReadFull(conn, q); err != nil {
			return
		}
		resp := stubDNSAnswer(q, table)
		if resp == nil {
			return
		}
		out := binary.BigEndian.AppendUint16(nil, uint16(len(resp))) //nolint:gosec // a DNS message is short
		if _, err := conn.Write(append(out, resp...)); err != nil {
			return
		}
	}
}

// stubDNSAnswer builds the response to query q, or nil if q is malformed.
func stubDNSAnswer(q []byte, table map[string][]string) []byte {
	const headerLen = 12
	if len(q) < headerLen {
		return nil
	}
	// The question's name: length-prefixed labels up to a zero byte.
	var labels []string
	i := headerLen
	for {
		if i >= len(q) {
			return nil
		}
		l := int(q[i])
		i++
		if l == 0 {
			break
		}
		if i+l > len(q) {
			return nil
		}
		labels = append(labels, string(q[i:i+l]))
		i += l
	}
	if i+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i:])
	question := q[headerLen : i+4]

	const typeA, typeAAAA = 1, 28
	answers, known := table[strings.ToLower(strings.Join(labels, "."))]
	var rdata [][]byte
	for _, a := range answers {
		addr := netip.MustParseAddr(a)
		switch {
		case qtype == typeA && addr.Is4():
			rdata = append(rdata, addr.AsSlice())
		case qtype == typeAAAA && addr.Is6():
			rdata = append(rdata, addr.AsSlice())
		}
	}

	// QR, AA, RD and RA; NXDOMAIN for a name not in the table.
	flags := uint16(0x8580)
	if !known {
		flags |= 3
	}
	resp := append([]byte(nil), q[0:2]...) // ID
	resp = binary.BigEndian.AppendUint16(resp, flags)
	resp = binary.BigEndian.AppendUint16(resp, 1)                  // QDCOUNT
	resp = binary.BigEndian.AppendUint16(resp, uint16(len(rdata))) //nolint:gosec // a short table
	resp = binary.BigEndian.AppendUint16(resp, 0)                  // NSCOUNT
	resp = binary.BigEndian.AppendUint16(resp, 0)                  // ARCOUNT
	resp = append(resp, question...)
	for _, rd := range rdata {
		resp = append(resp, 0xc0, headerLen) // the name, as a pointer to the question's
		resp = binary.BigEndian.AppendUint16(resp, qtype)
		resp = binary.BigEndian.AppendUint16(resp, 1)               // class IN
		resp = binary.BigEndian.AppendUint32(resp, 60)              // TTL
		resp = binary.BigEndian.AppendUint16(resp, uint16(len(rd))) //nolint:gosec // 4 or 16
		resp = append(resp, rd...)
	}
	return resp
}
