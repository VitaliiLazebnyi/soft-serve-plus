package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

var (
	// ErrUnsupportedRemoteScheme is returned when a git remote uses a scheme
	// that is neither a supported network transport nor safe to hand to the
	// git subprocess (e.g. file://, ext::, or a bare local path).
	ErrUnsupportedRemoteScheme = errors.New("remote scheme is not allowed")
	// ErrAmbiguousHost is returned when a remote host is neither a canonical
	// IP literal nor a valid DNS name. Non-canonical IPv4 literals such as
	// "0177.0.0.1" land here: Go reads them as one address and libcurl reads
	// them as another, so they can never be validated safely.
	ErrAmbiguousHost = errors.New("remote host is not a canonical IP or DNS name")
)

// GitRemoteTransport describes how a validated remote will be reached.
type GitRemoteTransport int

const (
	// GitTransportHTTP is an http/https remote. These are validated and
	// pinned to the resolved IP.
	GitTransportHTTP GitRemoteTransport = iota
	// GitTransportGit is a git:// remote. These are validated but cannot be
	// pinned, since the git protocol dials directly rather than via libcurl.
	GitTransportGit
	// GitTransportSSH is an ssh remote. Reachability is governed by the
	// operator's SSH client key, so these are not IP-validated.
	GitTransportSSH
)

// GitConfigEntry is a single git configuration key/value pair.
type GitConfigEntry struct {
	Key   string
	Value string
}

// ValidatedGitRemote is the result of validating a git remote URL.
type ValidatedGitRemote struct {
	// Transport is the transport the remote will use.
	Transport GitRemoteTransport
	// Config holds per-remote git configuration required for the validation
	// to hold, such as pinning a hostname to the address that was validated.
	// Render it with GitEnv, which also applies the settings that are needed
	// regardless of remote.
	Config []GitConfigEntry
}

// GitEnv renders the environment that must be passed to any git subprocess
// touching the given validated remotes.
//
// Validation alone is not sufficient for a subprocess: git resolves hostnames
// again itself, and follows the first HTTP redirect by default. Both walk
// straight through a validate-then-exec check, so this environment disables
// redirects, turns off any proxy, and pins each validated address. Callers
// MUST apply it to every git command that touches the remote, after the
// server's own environment so that it wins, or the validation means nothing.
//
// The environment needs git 2.37 or newer: GIT_CONFIG_COUNT arrived in 2.31
// and http.curloptResolve, which carries the pin, in 2.37. Older git ignores
// what it does not know without an error, so it would fetch unpinned. Callers
// must refuse to run git below that version rather than rely on this.
func GitEnv(remotes ...ValidatedGitRemote) []string {
	entries := []GitConfigEntry{
		// git follows the first HTTP redirect by default, which lets a public
		// URL hand off to an internal one after validation has already
		// passed. This costs imports of moved repositories, which is the
		// right trade for a user-supplied remote.
		{Key: "http.followRedirects", Value: "false"},
		// A proxy resolves the remote's hostname itself (curl sends CONNECT
		// host:443), so the pin below never applies, and a proxy usually
		// reaches the very internal networks validation keeps out. An empty
		// http.proxy makes git set CURLOPT_PROXY to "", which overrides
		// http.proxy from any gitconfig and the http_proxy, https_proxy and
		// all_proxy variables curl would otherwise read. A proxy set for one
		// URL or one named remote outranks this key; each validated remote
		// carries the overrides for those (see urlProxyOverride and Named).
		// The server's own Go clients (webhooks, LFS) never use a proxy
		// either.
		{Key: "http.proxy", Value: ""},
	}
	for _, r := range remotes {
		entries = append(entries, r.Config...)
	}

	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(entries))}
	for i, e := range entries {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, e.Key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, e.Value),
		)
	}
	// git:// remotes go through GIT_PROXY_COMMAND or core.gitProxy when one
	// is set, which likewise resolves and connects on its own. An empty
	// GIT_PROXY_COMMAND disables both, since git reads core.gitProxy only
	// when the variable is unset.
	return append(env, "GIT_PROXY_COMMAND=")
}

// ValidateGitRemote validates a git remote URL against private, internal, and
// loopback address ranges, and ssh remotes against the operator's policy.
//
// The result must be passed to GitEnv, and that environment applied to every
// git subprocess touching the remote. Validation on its own does not survive
// contact with git, which re-resolves hostnames and follows redirects.
//
// SSH remotes are not IP-validated: they authenticate with the operator's own
// client key, so which hosts they may reach is a policy question, answered by
// the GitRemotePolicy, not an address-range one.
//
// Limitation: git:// remotes are validated but cannot be pinned, since git
// dials them directly rather than through libcurl. git resolves the hostname
// again on every fetch, so a name that changes answers between validation
// and connect (DNS rebinding) is not caught for that transport.
func ValidateGitRemote(remote string, policy GitRemotePolicy) (ValidatedGitRemote, error) {
	return ValidateGitRemoteContext(context.Background(), remote, policy)
}

// ValidateGitRemoteContext is ValidateGitRemote with the hostname lookups it
// makes bounded by ctx, so a caller that must not stall on a slow resolver
// can set a deadline.
func ValidateGitRemoteContext(ctx context.Context, remote string, policy GitRemotePolicy) (ValidatedGitRemote, error) {
	if remote == "" {
		return ValidatedGitRemote{}, ErrInvalidURL
	}

	// scp-like syntax is recognized on git's own terms, never as "whatever
	// url.Parse rejects", since Go and libcurl disagree about what parses.
	if user, host, ok := parseBareSSHRemote(remote); ok {
		// The bracketed form [user@host:port]:path carries a port.
		host, port := splitHostPort(host)
		if err := validateSSHHost(user, host); err != nil {
			return ValidatedGitRemote{}, err
		}
		if err := policy.checkSSH(ctx, host, port); err != nil {
			return ValidatedGitRemote{}, err
		}
		return ValidatedGitRemote{Transport: GitTransportSSH}, nil
	}

	// The parse error is dropped on purpose: these messages go back to the
	// user, and only the typed error is needed to act on them.
	u, err := url.Parse(remote)
	if err != nil {
		return ValidatedGitRemote{}, fmt.Errorf("%w: cannot parse remote", ErrInvalidURL)
	}

	switch u.Scheme {
	case "ssh", "git+ssh", "ssh+git":
		if err := validateSSHHost(u.User.Username(), u.Hostname()); err != nil {
			return ValidatedGitRemote{}, err
		}
		if err := policy.checkSSH(ctx, u.Hostname(), u.Port()); err != nil {
			return ValidatedGitRemote{}, err
		}
		return ValidatedGitRemote{Transport: GitTransportSSH}, nil

	case "git":
		// git:// is a plain TCP connect to an arbitrary host:port, so it is
		// as much an SSRF primitive as http and gets the same validation.
		// It cannot be pinned, since libcurl is not involved.
		if _, err := validateRemoteHost(ctx, u.Hostname(), policy.lookup); err != nil {
			return ValidatedGitRemote{}, err
		}
		return ValidatedGitRemote{Transport: GitTransportGit}, nil

	case "http", "https":
		addr, err := validateRemoteHost(ctx, u.Hostname(), policy.lookup)
		if err != nil {
			return ValidatedGitRemote{}, err
		}
		return validatedHTTPRemote(u, addr), nil

	default:
		// file://, ext::, and bare local paths land here. None are network
		// remotes, and ext:: is arbitrary command execution.
		return ValidatedGitRemote{}, fmt.Errorf("%w: %q", ErrUnsupportedRemoteScheme, u.Scheme)
	}
}

// validateRemoteHost checks a remote host against private and internal ranges,
// returning the validated address so callers can pin it. ctx bounds the
// lookup; lookup resolves hostnames, nil meaning net.DefaultResolver.
func validateRemoteHost(ctx context.Context, host string, lookup lookupFunc) (netip.Addr, error) {
	if host == "" {
		return netip.Addr{}, fmt.Errorf("%w: missing hostname", ErrInvalidURL)
	}

	if isLocalhost(host) {
		return netip.Addr{}, ErrPrivateIP
	}

	// netip.ParseAddr accepts only canonical forms, so non-canonical IPv4
	// literals (octal, hex, short-form) fall through to the DNS name check
	// below and are rejected there rather than being resolved.
	if addr, err := netip.ParseAddr(host); err == nil {
		if isPrivateOrInternal(net.IP(addr.AsSlice())) {
			return netip.Addr{}, ErrPrivateIP
		}
		return addr, nil
	}

	if !isDNSName(host) {
		return netip.Addr{}, fmt.Errorf("%w: %q", ErrAmbiguousHost, host)
	}

	// The resolver error names the server's DNS resolver ("lookup x on
	// 10.0.0.53:53"), which the importing user has no business learning.
	// Telling "cannot resolve" apart from "private address" is kept on
	// purpose: it is one bit of recon over an authenticated session, and it
	// is what lets a user notice a typo in the hostname.
	ips, err := lookup.resolve(ctx, host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: cannot resolve hostname", ErrInvalidURL)
	}
	if len(ips) == 0 {
		return netip.Addr{}, fmt.Errorf("%w: no addresses for hostname", ErrInvalidURL)
	}

	// Every resolved address must be public. A hostname answering with both
	// a public and a private address is a rebinding attempt, not a remote.
	for _, ip := range ips {
		if isPrivateOrInternal(ip.IP) {
			return netip.Addr{}, ErrPrivateIP
		}
	}

	// Every answer was checked above, so pinning any one of them is safe.
	// Only the first is pinned: libcurl accepts a list in CURLOPT_RESOLVE
	// only from 7.59, and an older one drops an entry it cannot parse, which
	// would leave git to resolve the name itself. Losing failover between
	// records is the better trade than risking an unpinned fetch.
	addr, ok := netip.AddrFromSlice(ips[0].IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("%w: unusable address for hostname", ErrInvalidURL)
	}
	return addr.Unmap(), nil
}

// isDNSName reports whether a host is a valid DNS name. The final label must
// not be all-numeric (RFC 1123 section 2.1), which is what separates a real
// hostname from an IPv4 literal that netip rejected as non-canonical.
func isDNSName(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}

	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range label {
			c := label[i]
			isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			if !isAlnum && c != '-' && c != '_' {
				return false
			}
		}
	}

	// An all-numeric final label means this was meant as an IP address. Since
	// netip already rejected it as non-canonical, refuse rather than guess.
	last := labels[len(labels)-1]
	if _, err := strconv.ParseUint(last, 0, 64); err == nil {
		return false
	}

	return true
}

// validatedHTTPRemote returns an http/https remote whose host was validated
// as addr. The address is pinned so git's own resolution cannot return a
// different (private) one, and any proxy configured for the URL is cleared
// (see GitEnv).
func validatedHTTPRemote(u *url.URL, addr netip.Addr) ValidatedGitRemote {
	return ValidatedGitRemote{
		Transport: GitTransportHTTP,
		Config:    append(curlResolvePin(u, addr), urlProxyOverride(u)),
	}
}

// curlResolvePin returns git configuration pinning the URL's host to the
// validated address, or nothing if the URL needs no pinning.
func curlResolvePin(u *url.URL, addr netip.Addr) []GitConfigEntry {
	// An IP literal is already pinned by construction.
	if !addr.IsValid() || isIPLiteral(u.Hostname()) {
		return nil
	}

	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}

	return []GitConfigEntry{{
		Key:   "http.curloptResolve",
		Value: fmt.Sprintf("%s:%s:%s", u.Hostname(), port, addr.String()),
	}}
}

// urlProxyOverride returns git configuration clearing any proxy configured
// for the URL itself. git picks among http.<url>.* keys by how specifically
// they match, not by order, so the plain empty http.proxy GitEnv sets loses
// to an operator's [http "https://*.example.com"] proxy. The remote's own
// URL is the most specific match there is, and the later of two equally
// specific keys wins, which is GIT_CONFIG_* over any configuration file.
//
// The user name stays in the key, since a key naming the user outranks one
// that does not; the password is dropped, since git ignores it for matching
// and it has no business in the environment.
func urlProxyOverride(u *url.URL) GitConfigEntry {
	key := *u
	if key.User != nil {
		key.User = url.User(key.User.Username())
	}
	return GitConfigEntry{Key: "http." + key.String() + ".proxy", Value: ""}
}

// Named returns the remote with configuration clearing remote.<name>.proxy,
// which git reads for an http remote it knows by name (a clone's "origin",
// or each remote a mirror sync fetches) and which overrides every http.proxy
// key. Callers that run git against a named remote must use it.
func (v ValidatedGitRemote) Named(name string) ValidatedGitRemote {
	if v.Transport != GitTransportHTTP || name == "" {
		return v
	}
	v.Config = append(slices.Clip(v.Config), GitConfigEntry{Key: "remote." + name + ".proxy", Value: ""})
	return v
}

// cloneRemoteName is the name ForClone has git clone give the remote.
const cloneRemoteName = "origin"

// ForClone returns the remote ready for git clone, which knows it by the
// name in clone.defaultRemoteName: that name is fixed to "origin" so that
// the remote.origin.proxy Named clears is the one clone reads.
func (v ValidatedGitRemote) ForClone() ValidatedGitRemote {
	if v.Transport != GitTransportHTTP {
		return v
	}
	v = v.Named(cloneRemoteName)
	v.Config = append(v.Config, GitConfigEntry{Key: "clone.defaultRemoteName", Value: cloneRemoteName})
	return v
}

// isIPLiteral reports whether a host is already a canonical IP address.
func isIPLiteral(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// parseBareSSHRemote recognizes git's scp-like remote syntax
// (user@host:path, [user@host:port]:path, user@[v6:addr]:path) and returns
// the user and host. The rules mirror git's connect.c: "://" is a URL, a
// slash before the first colon is a local path, "::" selects a remote helper,
// and brackets count only at the start or right after "user@".
func parseBareSSHRemote(remote string) (user, host string, ok bool) {
	colon, slash := strings.IndexByte(remote, ':'), strings.IndexByte(remote, '/')
	if strings.Contains(remote, "://") || colon < 0 || (slash >= 0 && slash < colon) {
		return "", "", false
	}
	userHost, rest := remote[:colon], remote[colon:]
	if open := strings.IndexByte(remote, '['); open == 0 || (open > 0 && open < colon && remote[open-1] == '@') {
		end := strings.IndexByte(remote, ']')
		if end < 0 {
			return "", "", false
		}
		userHost, rest = remote[:open]+remote[open+1:end], remote[end+1:]
	}
	if !strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, "::") {
		return "", "", false
	}
	if at := strings.LastIndexByte(userHost, '@'); at >= 0 {
		return userHost[:at], userHost[at+1:], true
	}
	return "", userHost, true
}

// validateSSHHost rejects SSH remotes with no host, whose user or host would
// reach ssh as a command-line option, or whose host is not a plain DNS name
// or IP literal. The last part is what lets an allowlist match mean
// anything: ssh connects to exactly the name that was matched.
func validateSSHHost(user, host string) error {
	if host == "" {
		return fmt.Errorf("%w: missing hostname", ErrInvalidURL)
	}
	if strings.HasPrefix(host, "-") || strings.HasPrefix(user, "-") {
		return fmt.Errorf("%w: ssh user or host looks like a command-line option", ErrInvalidURL)
	}
	if !isDNSName(host) && !isIPLiteral(host) {
		return fmt.Errorf("%w: %q", ErrAmbiguousHost, host)
	}
	return nil
}
