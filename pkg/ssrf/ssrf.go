package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/soft-serve/internal/testdial"
)

var (
	// ErrPrivateIP is returned when a connection to a private or internal IP is blocked.
	ErrPrivateIP = errors.New("connection to private or internal IP address is not allowed")
	// ErrInvalidScheme is returned when a URL scheme is not http or https.
	ErrInvalidScheme = errors.New("URL must use http or https scheme")
	// ErrInvalidURL is returned when a URL is invalid.
	ErrInvalidURL = errors.New("invalid URL")
)

// ClientTimeout caps each request made by the client from NewSecureClient,
// including reading the response body.
const ClientTimeout = 30 * time.Second

// ResponseHeaderTimeout bounds the wait for a response's headers once the
// request has been written, on every client from NewSecureClient. It is set
// on the transport, so it still holds for a caller that drops the client's
// whole-request timeout to let long transfers run, as the LFS client does.
const ResponseHeaderTimeout = ClientTimeout

// MaxResponseHeaderBytes caps the response headers the client from
// NewSecureClient reads, status line included. The destinations it talks to
// are chosen by users, so the net/http default of 10 MiB would let one
// response make Soft Serve read and keep megabytes of headers. A response
// with more fails with an error instead.
const MaxResponseHeaderBytes = 64 << 10

// dialTimeout bounds one dial made by the secure client, from DNS resolution
// through the last connection attempt. It is a fixed budget: net/http dials
// with a context detached from the request (context.WithoutCancel), so the
// request deadline and ClientTimeout never reach the dialer. A shorter
// deadline already on the dial context still applies. net.Dialer splits the
// budget between the addresses a name resolves to.
const dialTimeout = 10 * time.Second

// testResolver, when set, resolves every hostname this package looks up:
// the secure client's dials and the lookups made during validation that are
// not given a resolver of their own. Unset means the resolver set with
// internal/testdial, or net.DefaultResolver. Tests point it at a stub DNS
// server. It is atomic because a dial's losing attempt or a background
// lookup may still read it after a test is done.
var testResolver atomic.Pointer[net.Resolver]

// dialHookFunc is the type of dialHook.
type dialHookFunc func(ctx context.Context, network, address string) error

// dialHook, when set, runs for every address the checks let through, just
// before the connection attempt, and an error stops that attempt. Tests use
// it to keep connections on the machine. It is atomic because the losing
// attempt of net.Dialer's IPv4/IPv6 race can run after DialContext returns,
// and so after a test has restored it.
var dialHook atomic.Pointer[dialHookFunc]

// resolver returns the resolver for hostname lookups: testResolver when set,
// then the one set with internal/testdial, otherwise net.DefaultResolver.
func resolver() *net.Resolver {
	if r := testResolver.Load(); r != nil {
		return r
	}
	if r := testdial.LoadResolver(); r != nil {
		return r
	}
	return net.DefaultResolver
}

// NewSecureClient returns an HTTP client with SSRF protection.
// It validates resolved IPs at dial time to block connections to private
// and internal networks; see secureDialer. Redirects are disabled to match
// the webhook client convention and prevent redirect-based SSRF.
func NewSecureClient() *http.Client {
	return NewSecureClientAllowing(nil)
}

// NewSecureClientAllowing is NewSecureClient with the private and internal
// destinations that allowed permits (see AllowedHosts.permitsHTTP) let
// through. With an empty allowed it is NewSecureClient. Redirects stay
// disabled, so every request reaches only the host it names and the
// allowlist is checked against that host alone.
func NewSecureClientAllowing(allowed AllowedHosts) *http.Client {
	return &http.Client{
		Timeout: ClientTimeout,
		Transport: &http.Transport{
			DialContext:            secureDialer(allowed),
			MaxIdleConns:           100,
			IdleConnTimeout:        90 * time.Second,
			TLSHandshakeTimeout:    10 * time.Second,
			ExpectContinueTimeout:  1 * time.Second,
			MaxResponseHeaderBytes: MaxResponseHeaderBytes,
			ResponseHeaderTimeout:  ResponseHeaderTimeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// secureDialer returns a dial function that checks every address it is
// about to connect to, after resolution and before connect(2), and refuses
// any that allowed does not permit for the host and port being dialed. The
// check runs on the socket's own address (net.Dialer.ControlContext), so
// the address checked is always the address connected to; resolution,
// address-family selection and failover come from net.Dialer.
//
// A name that answers with both public and private addresses is not
// refused as a whole here, but its private addresses are never dialed.
// ValidateURL and ValidateGitRemote refuse such a name when it is saved.
//
// TLS still verifies against the original hostname, which net/http takes
// from the request URL rather than from the dialed address.
func secureDialer(allowed AllowedHosts) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err //nolint:wrapcheck
		}
		ctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()

		// redirect is the first attempt the checks let through while
		// internal/testdial has a Func set; see errRedirected.
		var redirect atomic.Pointer[dialTarget]
		d := &net.Dialer{
			KeepAlive: 30 * time.Second,
			Resolver:  resolver(),
			ControlContext: func(ctx context.Context, network, address string, _ syscall.RawConn) error {
				// address is the IP and port about to be connected to. One
				// that does not parse, such as ":80" for an empty host,
				// which net.Dialer sends to the local system, is refused.
				ap, err := netip.ParseAddrPort(address)
				if err != nil || !allowed.permitsHTTP(host, port, ap.Addr().AsSlice()) {
					return ErrPrivateIP
				}
				if hook := dialHook.Load(); hook != nil {
					if err := (*hook)(ctx, network, address); err != nil {
						return err
					}
				}
				if testdial.Load() != nil {
					redirect.CompareAndSwap(nil, &dialTarget{network: network, address: address})
					return errRedirected
				}
				return nil
			},
		}
		conn, err := d.DialContext(ctx, network, addr)
		// Every attempt was stopped, so err is set, but it is the first
		// attempt's error, which can be a refusal of another address of a
		// name that also has a permitted one.
		if t := redirect.Load(); t != nil {
			if fn := testdial.Load(); fn != nil {
				return fn(ctx, t.network, t.address)
			}
		}
		if err != nil {
			return nil, dialError(ctx, host, err)
		}
		return conn, nil
	}
}

// errRedirected stops every connection attempt of a secure client while
// internal/testdial has a Func set, which only tests do. The first attempt
// the checks let through is then made by that Func instead, as the first
// permitted address is the one a real dial would connect to.
var errRedirected = errors.New("connection handed to the test dialer")

// dialTarget is one connection attempt: its network and address.
type dialTarget struct {
	network, address string
}

// dialError turns a failed dial into the error the client returns. The
// message is stored with every webhook delivery, where the repo admin reads
// it, so a refusal is the bare ErrPrivateIP, without the address refused,
// and a failed lookup names the host only, not the server's DNS resolver.
func dialError(ctx context.Context, host string, err error) error {
	if errors.Is(err, ErrPrivateIP) {
		return ErrPrivateIP
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Cancelled or out of time; say so rather than blame DNS.
			return fmt.Errorf("resolve %s: %w", host, ctxErr)
		}
		return fmt.Errorf("DNS resolution failed for host %s", host)
	}
	return err
}

// blockedPrefixes lists every network an outbound request must not reach.
//
// Prefixes are used rather than byte comparisons so the ranges stay readable
// and auditable against the RFCs that define them. This is also how the
// standard library models the same address space internally.
//
// The IPv6 transition ranges need explanation. Each embeds an arbitrary IPv4
// address inside an IPv6 one, so an address that looks public to the IPv6
// checks can name a loopback, RFC1918, or cloud metadata host once a relay
// decodes it. They are blocked in full rather than decoded and re-checked,
// because nothing here has a legitimate reason to reach a host through one of
// these encodings. Blocking the range removes the whole class of bypass
// instead of depending on getting each decoding exactly right.
var blockedPrefixes = []netip.Prefix{
	// IPv4.
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),      // RFC1918 private
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC6598 shared address space (CGNAT)
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local, includes cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),   // RFC1918 private
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("192.88.99.0/24"),  // RFC7526 6to4 relay anycast
	netip.MustParsePrefix("192.168.0.0/16"),  // RFC1918 private
	netip.MustParsePrefix("198.18.0.0/15"),   // RFC2544 benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, includes broadcast

	// IPv6.
	netip.MustParsePrefix("::1/128"),        // loopback
	netip.MustParsePrefix("64:ff9b::/96"),   // RFC6052 NAT64 well-known prefix
	netip.MustParsePrefix("64:ff9b:1::/48"), // RFC8215 NAT64 local-use prefix
	netip.MustParsePrefix("100::/64"),       // RFC6666 discard-only
	netip.MustParsePrefix("2001::/32"),      // RFC4380 Teredo
	netip.MustParsePrefix("2001:2::/48"),    // RFC5180 benchmarking
	netip.MustParsePrefix("2001:10::/28"),   // RFC4843 ORCHID (deprecated)
	netip.MustParsePrefix("2001:20::/28"),   // RFC7343 ORCHIDv2
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("2002::/16"),      // RFC3056 6to4
	netip.MustParsePrefix("3fff::/20"),      // RFC9637 documentation
	netip.MustParsePrefix("5f00::/16"),      // RFC9602 SRv6 SIDs
	netip.MustParsePrefix("fc00::/7"),       // unique local
	netip.MustParsePrefix("fe80::/10"),      // link-local
	netip.MustParsePrefix("ff00::/8"),       // multicast

	// IPv4-compatible IPv6 (::x.x.x.x), deprecated by RFC4291 but still
	// parsed. Unmap leaves this form alone, so the IPv4 prefixes above do not
	// apply to it. Covers the unspecified address too.
	netip.MustParsePrefix("::/96"),

	// IPv4-translated IPv6 (::ffff:0:x.x.x.x), RFC2765 SIIT. Like the
	// IPv4-compatible form, Unmap does not turn it into the IPv4 address it
	// carries, so it is blocked in full with the other transition ranges.
	netip.MustParsePrefix("::ffff:0:0:0/96"),
}

// isPrivateOrInternal reports whether an IP address is private, internal, or
// otherwise unsafe to send an outbound request to.
func isPrivateOrInternal(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		// Not an address we can reason about, so refuse rather than allow.
		return true
	}

	// Treat an IPv4 address written in IPv6 form (::ffff:127.0.0.1) as the
	// IPv4 address it names, so the IPv4 prefixes apply to it.
	addr = addr.Unmap()

	return slices.ContainsFunc(blockedPrefixes, func(p netip.Prefix) bool {
		return p.Contains(addr)
	})
}

// ValidateURL validates that a URL is safe to make requests to.
// It checks that the scheme is http/https, the hostname is not localhost,
// and all resolved IPs are public.
func ValidateURL(rawURL string) error {
	return ValidateURLContext(context.Background(), rawURL)
}

// ValidateURLContext is ValidateURL with the hostname lookup bound to ctx, so
// a caller that goes away stops waiting for the resolver.
func ValidateURLContext(ctx context.Context, rawURL string) error {
	return validateURL(ctx, rawURL, nil, nil)
}

// ValidateURLAllowing is ValidateURL with the private and internal
// destinations that allowed permits let through (see
// AllowedHosts.permitsHTTP). With an empty allowed it is ValidateURL. Every
// address the hostname resolves to must be public or permitted.
func ValidateURLAllowing(rawURL string, allowed AllowedHosts) error {
	return ValidateURLAllowingContext(context.Background(), rawURL, allowed)
}

// ValidateURLAllowingContext is ValidateURLAllowing with the hostname lookup
// bound to ctx, so a caller that goes away stops waiting for the resolver.
func ValidateURLAllowingContext(ctx context.Context, rawURL string, allowed AllowedHosts) error {
	return validateURL(ctx, rawURL, allowed, nil)
}

// validateURL is ValidateURLAllowingContext with the resolver used for
// hostnames; nil means the package resolver (net.DefaultResolver outside
// tests).
func validateURL(ctx context.Context, rawURL string, allowed AllowedHosts, lookup lookupFunc) error {
	if rawURL == "" {
		return ErrInvalidURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: cannot parse URL", ErrInvalidURL)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidScheme
	}

	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("%w: missing hostname", ErrInvalidURL)
	}

	port := u.Port()
	if port == "" {
		port = defaultHTTPPort(u.Scheme)
	}

	// A localhost name is refused before it is resolved, since the system
	// resolver may answer it without asking DNS. Only a hostname entry for
	// the name itself lets it through, and then its addresses are still
	// checked below.
	if isLocalhost(hostname) && !allowed.namesHost(hostname, port) {
		return ErrPrivateIP
	}

	if ip := net.ParseIP(hostname); ip != nil {
		if !allowed.permitsHTTP(hostname, port, ip) {
			return ErrPrivateIP
		}
		return nil
	}

	ips, err := lookup.resolve(ctx, hostname)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Cancelled or out of time; say so rather than blame DNS.
			return fmt.Errorf("resolve hostname: %w", ctxErr)
		}
		// See validateRemoteHost: the resolver error names the resolver.
		return fmt.Errorf("%w: cannot resolve hostname", ErrInvalidURL)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: no addresses for hostname", ErrInvalidURL)
	}

	if slices.ContainsFunc(ips, func(addr net.IPAddr) bool {
		return !allowed.permitsHTTP(hostname, port, addr.IP)
	}) {
		return ErrPrivateIP
	}

	return nil
}

// isLocalhost checks if the hostname is localhost or similar. A trailing dot
// (the fully qualified form, "localhost.") names the same host.
func isLocalhost(hostname string) bool {
	hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
	return hostname == "localhost" ||
		hostname == "localhost.localdomain" ||
		strings.HasSuffix(hostname, ".localhost")
}
