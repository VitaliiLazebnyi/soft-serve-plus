package ssrf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
)

// anySSH is the policy of a server admin with no allowlist configured; it
// keeps the address-validation table independent of the ssh policy. Its
// resolver knows no names, so a hostname slipping into a table fails instead
// of reaching real DNS.
var anySSH = GitRemotePolicy{AllowAnySSHHost: true, lookup: fakeLookup(nil)}

func TestValidateGitRemote(t *testing.T) {
	tests := []struct {
		name      string
		remote    string
		transport GitRemoteTransport
		wantErr   error
	}{
		// Allowed network remotes. IP literals only, so the suite does not
		// depend on DNS. Hostname resolution is covered separately.
		{"public http", "http://1.1.1.1/x.git", GitTransportHTTP, nil},
		{"public https", "https://1.1.1.1/x.git", GitTransportHTTP, nil},
		{"public https with port", "https://1.1.1.1:8443/x.git", GitTransportHTTP, nil},
		{"public ipv6", "https://[2606:4700::1111]/x.git", GitTransportHTTP, nil},

		// SSH is reachability-governed by the operator's client key, and
		// here by a policy that allows any host.
		{"ssh url", "ssh://git@10.0.0.1/x.git", GitTransportSSH, nil},
		{"bare ssh", "git@github.com:charmbracelet/soft-serve.git", GitTransportSSH, nil},
		{"git+ssh", "git+ssh://git@example.com/x.git", GitTransportSSH, nil},
		{"ssh+git", "ssh+git://git@example.com/x.git", GitTransportSSH, nil},

		// Loopback and private ranges over http.
		{"loopback", "http://127.0.0.1/x.git", 0, ErrPrivateIP},
		{"localhost", "http://localhost:8080/x.git", 0, ErrPrivateIP},
		{"private 10.x", "http://10.0.0.1/x.git", 0, ErrPrivateIP},
		{"private 192.168.x", "http://192.168.1.1/x.git", 0, ErrPrivateIP},
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/", 0, ErrPrivateIP},
		{"ipv6 loopback", "http://[::1]/x.git", 0, ErrPrivateIP},
		{"ipv4-mapped loopback", "http://[::ffff:127.0.0.1]/x.git", 0, ErrPrivateIP},

		// git:// is a raw TCP connect and must be validated the same way,
		// on import and on mirror sync.
		{"git scheme private", "git://10.0.0.1/x.git", 0, ErrPrivateIP},
		{"git scheme metadata", "git://169.254.169.254/x.git", 0, ErrPrivateIP},
		{"git scheme public", "git://1.1.1.1/x.git", GitTransportGit, nil},

		// Non-canonical IPv4 literals parse differently in Go and libcurl:
		// net.ParseIP reads 0177.0.0.1 as public 177.0.0.1 while libcurl
		// reads it as loopback.
		{"octal loopback", "http://0177.0.0.1/x.git", 0, ErrAmbiguousHost},
		{"hex loopback", "http://0x7f.1/x.git", 0, ErrAmbiguousHost},
		{"short-form loopback", "http://127.1/x.git", 0, ErrAmbiguousHost},
		{"decimal loopback", "http://2130706433/x.git", 0, ErrAmbiguousHost},
		{"octal private", "http://010.0.0.1/x.git", 0, ErrAmbiguousHost},

		// Non-network schemes are not remotes at all.
		{"file scheme", "file:///etc/passwd", 0, ErrUnsupportedRemoteScheme},
		{"ext scheme", "ext::sh -c whoami", 0, ErrUnsupportedRemoteScheme},
		{"local path", "/data/repos/secret.git", 0, ErrUnsupportedRemoteScheme},

		{"empty", "", 0, ErrInvalidURL},
		{"no host", "https:///x.git", 0, ErrInvalidURL},
		{"ssh no host", "ssh:///x.git", 0, ErrInvalidURL},

		// URLs that Go refuses to parse but libcurl accepts. They must not
		// fall through as scp-style SSH remotes and skip validation:
		// http://a{b@127.0.0.1:8080/ would reach loopback.
		{"unparseable http userinfo", "http://a{b@127.0.0.1:8080/", 0, ErrInvalidURL},
		{"unparseable git userinfo", "git://a{b@127.0.0.1:9418/x", 0, ErrInvalidURL},
		{"unparseable http port", "http://a@127.0.0.1:80x/", 0, ErrInvalidURL},
		{"unparseable http space", "http://a b@127.0.0.1/", 0, ErrInvalidURL},

		// scp-style remotes that are really SSH.
		{"bare ssh no user", "github.com:charmbracelet/soft-serve.git", GitTransportSSH, nil},
		{"bare ssh ipv6", "git@[::1]:x.git", GitTransportSSH, nil},
		{"bare ssh bracketed host port", "[git@example.com:2222]:x.git", GitTransportSSH, nil},
		{"bare ssh underscore in user", "a_b@127.0.0.1:x", GitTransportSSH, nil},
		// Recognized as ssh rather than failing url.Parse, then refused for
		// the user (see TestValidateGitRemoteSSHUserAndZone).
		{"bare ssh brace in user", "a{b@127.0.0.1:x", 0, ErrInvalidURL},
		{"bare ssh scheme-looking host", "http:foo", GitTransportSSH, nil},

		// Things that look a bit like scp syntax but are not SSH remotes.
		{"bare ssh option host", "-oProxyCommand=id:x", 0, ErrInvalidURL},
		{"bare ssh option user", "-oProxyCommand=id@host:x", 0, ErrInvalidURL},
		{"bare ssh option host after user", "user@-host:x", 0, ErrInvalidURL},
		{"ssh url option user", "ssh://-user@example.com/x", 0, ErrInvalidURL},
		{"bare ssh slash before colon", "dir/host:x", 0, ErrUnsupportedRemoteScheme},
		{"bare ssh slash inside brackets", "[a/b@c]:x", 0, ErrUnsupportedRemoteScheme},
		{"bare ssh no colon", "host", 0, ErrUnsupportedRemoteScheme},
		{"bare ssh user no colon", "user@host", 0, ErrUnsupportedRemoteScheme},
		{"bare ssh unterminated bracket", "[host:22]path", 0, ErrInvalidURL},
		{"bare ssh empty host", ":path", 0, ErrInvalidURL},
		{"remote helper syntax", "host::path", 0, ErrUnsupportedRemoteScheme},
		// git for Windows reads a drive prefix as a local path, not as the
		// ssh host "C", so these must not validate as ssh remotes, which
		// would let an import open a local repository on a Windows server.
		{"drive path forward slash", "C:/Users/x/repo", 0, ErrUnsupportedRemoteScheme},
		{"drive path backslash", `C:\Users\x\repo`, 0, ErrUnsupportedRemoteScheme},
		{"drive relative path", "c:repo", 0, ErrUnsupportedRemoteScheme},
		{"two-letter ssh host", "cc:repo", GitTransportSSH, nil},
		// An ssh host must be a plain DNS name or IP literal, so that the
		// name the policy matched is the name ssh connects to.
		{"ssh url non-canonical ip", "ssh://git@0177.0.0.1/x.git", 0, ErrAmbiguousHost},
		{"ssh url unicode host", "ssh://git@gît.example/x.git", 0, ErrAmbiguousHost},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateGitRemote(t.Context(), tt.remote, anySSH)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ValidateGitRemote(%q) error = %v, want %v", tt.remote, err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("ValidateGitRemote(%q) unexpected error: %v", tt.remote, err)
			}
			if got.Transport != tt.transport {
				t.Errorf("ValidateGitRemote(%q) transport = %v, want %v", tt.remote, got.Transport, tt.transport)
			}
		})
	}
}

// TestValidateGitRemoteResolvesHostnames covers the DNS path, which the main
// table deliberately avoids. The resolver is faked, so it runs offline.
func TestValidateGitRemoteResolvesHostnames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		remote  string
		ips     []string
		wantErr error
		// wantMsg, when set, must appear in the error; it tells apart the
		// branches that share ErrInvalidURL.
		wantMsg string
	}{
		{"public https", "https://git.example/x.git", []string{"140.82.112.3"}, nil, ""},
		{"public git", "git://git.example/x.git", []string{"140.82.112.3"}, nil, ""},
		{"private https", "https://git.example/x.git", []string{"10.0.0.1"}, ErrPrivateIP, ""},
		{"metadata git", "git://git.example/x.git", []string{"169.254.169.254"}, ErrPrivateIP, ""},
		{"loopback v6", "https://git.example/x.git", []string{"::1"}, ErrPrivateIP, ""},
		// A name answering with both is a rebinding attempt.
		{"public and private", "https://git.example/x.git", []string{"140.82.112.3", "10.0.0.1"}, ErrPrivateIP, ""},
		{"does not resolve", "https://git.example/x.git", nil, ErrInvalidURL, "cannot resolve hostname"},
		// A resolver may answer with no addresses and no error.
		{"empty answer", "https://git.example/x.git", []string{}, ErrInvalidURL, "no addresses for hostname"},
		{"empty answer git", "git://git.example/x.git", []string{}, ErrInvalidURL, "no addresses for hostname"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			table := map[string][]string{}
			if tt.ips != nil {
				table["git.example"] = tt.ips
			}
			var lookups atomic.Int32
			fake := fakeLookup(table)
			policy := GitRemotePolicy{AllowAnySSHHost: true, lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
				lookups.Add(1)
				return fake(ctx, host)
			}}

			v, err := ValidateGitRemote(t.Context(), tt.remote, policy)
			// The injected resolver, not the system one, answered.
			if lookups.Load() != 1 {
				t.Fatalf("fake resolver called %d times, want 1", lookups.Load())
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ValidateGitRemote(%q) error = %v, want %v", tt.remote, err, tt.wantErr)
				}
				if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
					t.Fatalf("ValidateGitRemote(%q) error = %q, want it to mention %q", tt.remote, err, tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("public hostname rejected: %v", err)
			}
			// A resolved http hostname must be pinned, or git re-resolves it.
			if v.Transport == GitTransportHTTP && !slices.ContainsFunc(v.Config, func(e GitConfigEntry) bool {
				return e.Key == "http.curloptResolve" && strings.HasSuffix(e.Value, ":"+tt.ips[0])
			}) {
				t.Errorf("resolved hostname was not pinned to %s: %v", tt.ips[0], v.Config)
			}
		})
	}
}

// TestGitEnvDisablesRedirects covers the bypass where a validated public URL
// 302s to an internal address. git follows the first redirect by default, so
// validation is meaningless without this.
func TestGitEnvDisablesRedirects(t *testing.T) {
	for _, name := range []string{"no remotes", "one remote"} {
		t.Run(name, func(t *testing.T) {
			var env []string
			if name == "no remotes" {
				env = GitEnv()
			} else {
				env = GitEnv(ValidatedGitRemote{Transport: GitTransportHTTP})
			}
			if !hasGitConfig(env, "http.followRedirects", "false") {
				t.Errorf("GitEnv did not disable redirects: %v", env)
			}
		})
	}
}

// TestGitEnvDisablesProxy checks the settings that keep git from handing the
// connection to a proxy, which would resolve the hostname past the pin. See
// TestGitEnvBypassesProxy for the same against a real git.
func TestGitEnvDisablesProxy(t *testing.T) {
	env := GitEnv()
	if !hasGitConfig(env, "http.proxy", "") {
		t.Errorf("GitEnv did not clear http.proxy: %v", env)
	}
	if !slices.Contains(env, "GIT_PROXY_COMMAND=") {
		t.Errorf("GitEnv did not clear GIT_PROXY_COMMAND: %v", env)
	}
}

// TestURLProxyOverride checks the key that clears a proxy set for the
// remote's own URL: the full URL with the user name, never the password.
func TestURLProxyOverride(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"https://example.com/org/x.git", "http.https://example.com/org/x.git.proxy"},
		{"http://example.com:8080/x.git", "http.http://example.com:8080/x.git.proxy"},
		{"https://alice@example.com/x.git", "http.https://alice@example.com/x.git.proxy"},
		{"https://alice:secret@example.com/x.git", "http.https://alice@example.com/x.git.proxy"},
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatalf("bad test url: %v", err)
		}
		got := urlProxyOverride(u)
		if got.Key != tc.want || got.Value != "" {
			t.Errorf("urlProxyOverride(%q) = %+v, want key %q with empty value", tc.url, got, tc.want)
		}
		if u.String() != tc.url {
			t.Errorf("urlProxyOverride modified its URL: %q", u)
		}
	}
}

// TestGitEnvSetsLowSpeedLimit covers a remote that stops sending, or
// trickles, after the transfer starts: without a low-speed limit libcurl
// waits on it forever.
func TestGitEnvSetsLowSpeedLimit(t *testing.T) {
	for _, env := range [][]string{GitEnv(), GitEnv(ValidatedGitRemote{Transport: GitTransportHTTP})} {
		if !hasGitConfig(env, "http.lowSpeedLimit", strconv.Itoa(GitLowSpeedLimit)) ||
			!hasGitConfig(env, "http.lowSpeedTime", strconv.Itoa(GitLowSpeedTime)) {
			t.Errorf("GitEnv set no low-speed limit: %v", env)
		}
	}
}

// TestValidatedRemoteProxyOverrides checks that an http remote clears a
// proxy set for its URL, and Named and ForClone one set for its name.
func TestValidatedRemoteProxyOverrides(t *testing.T) {
	u, _ := url.Parse("https://example.com/x.git")
	v := validatedHTTPRemote(u, netip.MustParseAddr("1.1.1.1"))
	env := GitEnv(v)
	if !hasGitConfig(env, "http.https://example.com/x.git.proxy", "") {
		t.Errorf("http remote did not clear its URL's proxy: %v", env)
	}

	if env := GitEnv(v.Named("up")); !hasGitConfig(env, "remote.up.proxy", "") {
		t.Errorf("Named did not clear remote.up.proxy: %v", env)
	}
	env = GitEnv(v.ForClone())
	if !hasGitConfig(env, "remote.origin.proxy", "") || !hasGitConfig(env, "clone.defaultRemoteName", "origin") {
		t.Errorf("ForClone did not fix the remote name and clear its proxy: %v", env)
	}
	if len(v.Config) != 2 {
		t.Errorf("Named or ForClone modified the remote: %v", v.Config)
	}

	// Other transports never go through an http proxy.
	ssh := ValidatedGitRemote{Transport: GitTransportSSH}
	if c := ssh.Named("up").ForClone().Config; len(c) != 0 {
		t.Errorf("ssh remote got proxy overrides: %v", c)
	}
}

// TestCurlResolvePin covers DNS rebinding: git re-resolves the hostname
// itself, so the validated address has to be pinned for the check to survive
// into the subprocess.
func TestCurlResolvePin(t *testing.T) {
	tests := []struct {
		name string
		url  string
		addr string
		want string
	}{
		{"https default port", "https://example.com/x.git", "1.1.1.1", "example.com:443:1.1.1.1"},
		{"http default port", "http://example.com/x.git", "1.1.1.1", "example.com:80:1.1.1.1"},
		{"explicit port", "https://example.com:8443/x.git", "1.1.1.1", "example.com:8443:1.1.1.1"},
		{"ipv6 address", "https://example.com/x.git", "2606:4700::1111", "example.com:443:2606:4700::1111"},
		// An IP literal is pinned by construction; nothing to re-resolve.
		{"ip literal needs no pin", "https://1.1.1.1/x.git", "1.1.1.1", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.url)
			if err != nil {
				t.Fatalf("bad test url: %v", err)
			}

			got := curlResolvePin(u, netip.MustParseAddr(tt.addr))

			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("curlResolvePin(%q) = %v, want none", tt.url, got)
				}
				return
			}

			if len(got) != 1 {
				t.Fatalf("curlResolvePin(%q) = %v, want 1 entry", tt.url, got)
			}
			if got[0].Key != "http.curloptResolve" {
				t.Errorf("key = %q, want http.curloptResolve", got[0].Key)
			}
			if got[0].Value != tt.want {
				t.Errorf("value = %q, want %q", got[0].Value, tt.want)
			}
		})
	}
}

// TestGitEnvCountMatchesEntries guards the encoding itself. git reads exactly
// GIT_CONFIG_COUNT entries, so a miscount silently drops the security settings
// rather than failing loudly.
func TestGitEnvCountMatchesEntries(t *testing.T) {
	pinned := ValidatedGitRemote{
		Transport: GitTransportHTTP,
		Config: []GitConfigEntry{{
			Key:   "http.curloptResolve",
			Value: "example.com:443:1.1.1.1",
		}},
	}

	for _, tc := range []struct {
		name    string
		remotes []ValidatedGitRemote
	}{
		{"none", nil},
		{"unpinned", []ValidatedGitRemote{{Transport: GitTransportSSH}}},
		{"one pinned", []ValidatedGitRemote{pinned}},
		{"multiple pinned", []ValidatedGitRemote{pinned, pinned}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := GitEnv(tc.remotes...)

			var count string
			keys := 0
			for _, e := range env {
				switch {
				case strings.HasPrefix(e, "GIT_CONFIG_COUNT="):
					count = strings.TrimPrefix(e, "GIT_CONFIG_COUNT=")
				case strings.HasPrefix(e, "GIT_CONFIG_KEY_"):
					keys++
				}
			}

			if want := strconv.Itoa(keys); count != want {
				t.Errorf("GIT_CONFIG_COUNT = %q, want %q (env: %v)", count, want, env)
			}
			// Every key must have a matching value at the same index.
			for i := range keys {
				if !slices.ContainsFunc(env, func(e string) bool {
					return strings.HasPrefix(e, "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"=")
				}) {
					t.Errorf("GIT_CONFIG_KEY_%d has no matching value: %v", i, env)
				}
			}
		})
	}
}

func TestIsDNSName(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"sub.example.com", true},
		{"example.com.", true},
		{"my-host", true},

		// All-numeric final label means an IP was intended. netip already
		// rejected these as non-canonical, so they must not be resolved.
		{"0177.0.0.1", false},
		{"127.1", false},
		{"2130706433", false},
		{"0x7f.1", false},

		{"", false},
		{"-leading.com", false},
		{"trailing-.com", false},
		{"has space.com", false},
		{"double..dot", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := isDNSName(tt.host); got != tt.want {
				t.Errorf("isDNSName(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func hasGitConfig(env []string, key, value string) bool {
	for _, e := range env {
		name, v, ok := strings.Cut(e, "=")
		if !ok || !strings.HasPrefix(name, "GIT_CONFIG_KEY_") || v != key {
			continue
		}
		idx := strings.TrimPrefix(name, "GIT_CONFIG_KEY_")
		if slices.Contains(env, "GIT_CONFIG_VALUE_"+idx+"="+value) {
			return true
		}
	}
	return false
}

// TestGitEnvCarriesServerConfig checks that the environment for mirror and
// import fetches, which replaces the server's own GIT_CONFIG_*, still keeps
// automatic maintenance in the foreground: the server's settings are merged
// into the one GIT_CONFIG_COUNT, not set by a second one that would replace
// the first.
func TestGitEnvCarriesServerConfig(t *testing.T) {
	t.Parallel()

	v := ValidatedGitRemote{
		Transport: GitTransportHTTP,
		Config:    []GitConfigEntry{{Key: "http.curloptResolve", Value: "h:443:192.0.2.1"}},
	}
	for _, env := range [][]string{GitEnv(), GitEnv(v)} {
		for _, e := range git.ServerConfig() {
			if !hasGitConfig(env, e.Key, e.Value) {
				t.Errorf("GitEnv() lacks %s=%s: %v", e.Key, e.Value, env)
			}
		}
		if !hasGitConfig(env, "http.followRedirects", "false") {
			t.Errorf("GitEnv() lost the redirect guard: %v", env)
		}
		counts := 0
		for _, e := range env {
			if strings.HasPrefix(e, "GIT_CONFIG_COUNT=") {
				counts++
			}
		}
		if counts != 1 {
			t.Errorf("GitEnv() sets GIT_CONFIG_COUNT %d times, want once: %v", counts, env)
		}
	}
}

// TestValidateGitRemoteErrorsHideResolver verifies that the messages handed
// back to an importing user do not include the resolver's own error, which
// names the server's DNS server ("lookup x on 10.0.0.53:53: no such host").
// The fake resolver fails the way the system one does, so this runs offline.
func TestValidateGitRemoteErrorsHideResolver(t *testing.T) {
	t.Parallel()

	var lookups atomic.Int32
	failing := GitRemotePolicy{AllowAnySSHHost: true, lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
		lookups.Add(1)
		return nil, &net.DNSError{Err: "server misbehaving", Name: host, Server: "10.0.0.53:53"}
	}}
	for _, remote := range []string{
		"https://does-not-exist.invalid/x.git",
		"git://does-not-exist.invalid/x.git",
	} {
		before := lookups.Load()
		_, err := ValidateGitRemote(t.Context(), remote, failing)
		// Without this, a check rejecting the name before lookup would let
		// the test pass without exercising the resolver-error path.
		if got := lookups.Load() - before; got != 1 {
			t.Fatalf("ValidateGitRemote(%q) called the resolver %d times, want 1", remote, got)
		}
		if !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("ValidateGitRemote(%q) error = %v, want ErrInvalidURL", remote, err)
		}
		for _, leak := range []string{"lookup", " on ", ":53", "no such host", "server misbehaving"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("ValidateGitRemote(%q) error %q leaks resolver detail %q", remote, err, leak)
			}
		}
	}

	// A URL Go cannot parse must produce exactly the typed error and its
	// fixed reason, never the parser's own message.
	_, err := ValidateGitRemote(t.Context(), "http://a{b@127.0.0.1:8080/", anySSH)
	if !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("unparseable remote error = %v, want ErrInvalidURL", err)
	}
	if got, want := err.Error(), "invalid URL: cannot parse remote"; got != want {
		t.Errorf("unparseable remote error = %q, want %q", got, want)
	}
}

// TestValidateGitRemoteRefusesMixedAnswers covers a name answering with both
// public and private addresses. Validation refuses it as a whole; the secure
// client's dialer only skips the private answers (TestSecureClientMixedAnswers).
func TestValidateGitRemoteRefusesMixedAnswers(t *testing.T) {
	t.Parallel()
	policy := GitRemotePolicy{AllowAnySSHHost: true, lookup: fakeLookup(map[string][]string{
		"mixed.example": {"140.82.112.3", "10.0.0.1"},
	})}
	for _, remote := range []string{"https://mixed.example/x.git", "git://mixed.example/x.git"} {
		if _, err := ValidateGitRemote(t.Context(), remote, policy); !errors.Is(err, ErrPrivateIP) {
			t.Errorf("ValidateGitRemote(%q) error = %v, want ErrPrivateIP", remote, err)
		}
	}
}

// TestValidateGitRemoteRefusesTrailingDot covers hosts written in the fully
// qualified form. The pin would carry the dot, which older libcurl may not
// match against the request, so http, https and git remotes are refused even
// though the name resolves to a public address. ssh has no pin and keeps
// accepting the form.
func TestValidateGitRemoteRefusesTrailingDot(t *testing.T) {
	t.Parallel()
	policy := GitRemotePolicy{AllowAnySSHHost: true, lookup: fakeLookup(map[string][]string{
		"git.example":  {"140.82.112.3"},
		"git.example.": {"140.82.112.3"},
		"GIT.example.": {"140.82.112.3"},
	})}
	for _, remote := range []string{
		"https://git.example./x.git",
		"http://git.example.:8080/x.git",
		"https://GIT.example./x.git",
		"https://user@git.example./x.git",
		"git://git.example./x.git",
		"git://git.example.:9418/x.git",
	} {
		if _, err := ValidateGitRemote(t.Context(), remote, policy); !errors.Is(err, ErrAmbiguousHost) {
			t.Errorf("ValidateGitRemote(%q) error = %v, want ErrAmbiguousHost", remote, err)
		}
	}
	for _, remote := range []string{
		"https://git.example/x.git",
		"git://git.example/x.git",
		"ssh://git@git.example./x.git",
		"git@git.example.:x.git",
	} {
		if _, err := ValidateGitRemote(t.Context(), remote, policy); err != nil {
			t.Errorf("ValidateGitRemote(%q): %v", remote, err)
		}
	}
}

// TestValidateGitRemoteSSHUserAndZone covers what reaches ssh as the user
// and host, which an ssh config may expand into a shell command (%r, %h;
// CVE-2023-51385). The user must be a plain account name and the host may
// not carry an IPv6 zone, in both the URL and the scp-like syntax. Zones are
// refused for the other transports too.
func TestValidateGitRemoteSSHUserAndZone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		remote string
		err    error // nil: accepted
	}{
		// Users.
		{"ssh://git@git.example/x.git", nil},
		{"ssh://deploy.bot_1-a@git.example/x.git", nil},
		{"ssh://git.example/x.git", nil},
		{"git@git.example:x.git", nil},
		{"Deploy.Bot_1-a@git.example:x.git", nil},
		{"git.example:x.git", nil},
		{"ssh://%24%28id%29@git.example/x.git", ErrInvalidURL},
		{"ssh://a%3Bb@git.example/x.git", ErrInvalidURL},
		{"ssh://a%20b@git.example/x.git", ErrInvalidURL},
		{"ssh://a%25r@git.example/x.git", ErrInvalidURL},
		{"git+ssh://a&b@git.example/x.git", ErrInvalidURL},
		{"ssh+git://a$b@git.example/x.git", ErrInvalidURL},
		{"$(id)@git.example:x.git", ErrInvalidURL},
		{"a;touch x@git.example:x.git", ErrInvalidURL},
		{"a b@git.example:x.git", ErrInvalidURL},
		{"a`id`@git.example:x.git", ErrInvalidURL},
		{"a|b@git.example:x.git", ErrInvalidURL},
		{"a'b@git.example:x.git", ErrInvalidURL},
		{"a@b@git.example:x.git", ErrInvalidURL},
		{"[a$b@git.example:22]:x.git", ErrInvalidURL},
		{"-oProxyCommand=x@git.example:x.git", ErrInvalidURL},
		// A password: git hands "user:password@host" to ssh, so it would
		// reach %r with the user.
		{"ssh://a:$(id)@git.example/x.git", ErrInvalidURL},
		{"ssh://git:x;id@git.example/x.git", ErrInvalidURL},
		{"ssh://git:%24%28touch%20%2Ftmp%2Fp%29@git.example/x.git", ErrInvalidURL},
		{"git+ssh://git:secret@git.example/x.git", ErrInvalidURL},
		{"ssh+git://git:@git.example/x.git", ErrInvalidURL},
		// Zones.
		{"ssh://git@[2606:4700::1]/x.git", nil},
		{"git@[2606:4700::1]:x.git", nil},
		{"ssh://git@[fe80::1%25eth0]/x.git", ErrAmbiguousHost},
		{"ssh://[2606:4700::1%25$(id)]:22/x.git", ErrAmbiguousHost},
		{"git@[fe80::1%eth0]:x.git", ErrAmbiguousHost},
		{"[2606:4700::1%$(id)]:x.git", ErrAmbiguousHost},
		{"[git@2606:4700::1%x]:x.git", ErrAmbiguousHost},
		{"https://[2606:4700::1%25x]/x.git", ErrAmbiguousHost},
		{"git://[2606:4700::1%25x]/x.git", ErrAmbiguousHost},
	}
	for _, tt := range tests {
		_, err := ValidateGitRemote(t.Context(), tt.remote, anySSH)
		if tt.err == nil {
			if err != nil {
				t.Errorf("ValidateGitRemote(%q): %v", tt.remote, err)
			}
			continue
		}
		if !errors.Is(err, tt.err) {
			t.Errorf("ValidateGitRemote(%q) error = %v, want %v", tt.remote, err, tt.err)
		}
	}
}

// minPinGitVersion is the first git with http.curloptResolve. The import gate
// in pkg/backend (minImportGitVersion) must not admit anything older.
var minPinGitVersion = git.Version{Major: 2, Minor: 37}

// TestGitEnvPinHonouredByGit runs the installed git against a name under
// .invalid, which never resolves (RFC 6761), with GitEnv pinning it to a local
// server. Only a git that honours http.curloptResolve can reach the server; an
// older one ignores the key silently and fails the lookup, which is the DNS
// rebinding hole the import gate exists to close.
func TestGitEnvPinHonouredByGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	v, err := git.BinVersion(t.Context())
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	if v.Less(minPinGitVersion) {
		t.Skipf("git %s predates http.curloptResolve; imports are gated off for it", v)
	}

	tests := []struct {
		name    string
		pin     bool
		reached bool
	}{
		{"pinned", true, true},
		// The control: without the pin the name must not resolve, or the
		// pinned case would prove nothing.
		{"unpinned", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			var host atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				host.Store(r.Host)
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)

			_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			remote := "http://pinned.invalid:" + port + "/x.git"
			u, err := url.Parse(remote)
			if err != nil {
				t.Fatal(err)
			}

			var r ValidatedGitRemote
			if tt.pin {
				r = ValidatedGitRemote{
					Transport: GitTransportHTTP,
					Config:    curlResolvePin(u, netip.MustParseAddr("127.0.0.1")),
				}
			}

			env := append(isolatedGitEnv(t), GitEnv(r)...)
			run := func() []byte {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				cmd := exec.CommandContext(ctx, "git", "ls-remote", remote)
				cmd.Env = env
				out, _ := cmd.CombinedOutput()
				return out
			}

			if tt.reached {
				runUntilReached(t, func() bool { return hits.Load() > 0 }, run)
			} else if out := run(); hits.Load() > 0 {
				t.Fatalf("server reached without the pin; git output:\n%s", out)
			}
			if tt.reached {
				if got, want := host.Load(), "pinned.invalid:"+port; got != want {
					t.Errorf("Host = %v, want %q", got, want)
				}
			}
		})
	}
}

// gitReachDeadline bounds how long runUntilReached runs git again.
const gitReachDeadline = time.Minute

// gitRetryPause is the pause between two runs of runUntilReached.
const gitRetryPause = 250 * time.Millisecond

// runUntilReached calls run, which runs git against a listener of the test,
// until reached reports that the listener got git's request, and fails the
// test if that has not happened within gitReachDeadline.
//
// The listeners are on loopback TCP. While many test processes open and close
// loopback connections, macOS can stall a loopback connect for seconds, fail
// it with ETIMEDOUT after about 7.8 seconds, or leave git with a socket whose
// first write fails ("Socket is not connected"). git has to run as a
// subprocess, so these tests cannot use an in-memory network. A run that
// never got its request to the listener says nothing about the pin, so it is
// run again after a short pause.
//
// A run that did reach the listener is never repeated: the caller then checks
// what the listener got, so a request naming the wrong host still fails. A
// pin that git ignores never reaches the listener either (the remote's name
// does not resolve), so it fails here once the deadline passes, with git's
// last output.
func runUntilReached(t *testing.T, reached func() bool, run func() []byte) {
	t.Helper()
	deadline := time.Now().Add(gitReachDeadline)
	for try := 1; ; try++ {
		out := run()
		if reached() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("git did not reach the listener in %d tries over %v; last git output:\n%s", try, gitReachDeadline, out)
		}
		t.Logf("try %d: git did not reach the listener, trying again; git output:\n%s", try, out)
		time.Sleep(gitRetryPause)
	}
}

// isolatedGitEnv returns an environment that keeps the developer's git
// configuration and any proxy out of the test, so git talks to the test
// server directly.
func isolatedGitEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + global,
		"GIT_TERMINAL_PROMPT=0",
		"NO_PROXY=*",
		"no_proxy=*",
	}
	// PATH and, on Windows, the system variables git needs to start.
	for _, k := range []string{"PATH", "SYSTEMROOT", "TEMP", "TMP"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// TestGitSSHDestinationMatchesValidation runs the installed git with a fake
// ssh that records its arguments, for ssh remotes ValidateGitRemote accepts,
// and checks that the destination git hands to ssh is the user and host that
// were validated. It runs nothing over the network.
func TestGitSSHDestinationMatchesValidation(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// The fake ssh is a shell script, which git runs directly.
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh is a shell script")
	}
	dir := t.TempDir()
	fakeSSH := filepath.Join(dir, "ssh")
	// One argument per line; then fail, so git stops.
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"$FAKE_SSH_LOG\"\nexit 1\n"
	if err := os.WriteFile(fakeSSH, []byte(script), 0o700); err != nil { // an executable script
		t.Fatal(err)
	}

	tests := []struct {
		remote string
		dest   string // the destination argument git must pass
		port   string // the -p value, or ""
	}{
		{"ssh://git@git.example/x.git", "git@git.example", ""},
		{"ssh://git@git.example:2222/x.git?a#b", "git@git.example", "2222"},
		{"ssh://g%69t@git.example/x.git", "git@git.example", ""},
		{"git+ssh://git.example/x.git", "git.example", ""},
		{"git@git.example:x.git", "git@git.example", ""},
		{"[git@git.example:2222]:x.git", "git@git.example", "2222"},
		{"git@git.example:x.git?y#z", "git@git.example", ""},
	}
	policy := GitRemotePolicy{AllowAnySSHHost: true, lookup: fakeLookup(nil)}
	for _, tt := range tests {
		if _, err := ValidateGitRemote(t.Context(), tt.remote, policy); err != nil {
			t.Errorf("ValidateGitRemote(%q): %v", tt.remote, err)
			continue
		}
		log := filepath.Join(dir, "log")
		_ = os.Remove(log)
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		cmd := exec.CommandContext(ctx, "git", "ls-remote", tt.remote)
		cmd.Env = append(isolatedGitEnv(t), "GIT_SSH="+fakeSSH, "GIT_SSH_VARIANT=ssh", "FAKE_SSH_LOG="+log)
		_ = cmd.Run()
		cancel()
		out, err := os.ReadFile(log)
		if err != nil {
			t.Errorf("%s: fake ssh not run: %v", tt.remote, err)
			continue
		}
		args := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		// The destination comes right before the remote command.
		if len(args) < 2 || args[len(args)-2] != tt.dest {
			t.Errorf("%s: ssh got %q, want destination %q", tt.remote, args, tt.dest)
		}
		port := ""
		for i, a := range args {
			if a == "-p" && i+1 < len(args) {
				port = args[i+1]
			}
		}
		if port != tt.port {
			t.Errorf("%s: ssh got %q, want port %q", tt.remote, args, tt.port)
		}
	}
}

// TestGitCurlHostMatchesPin runs the installed git, through libcurl, against
// http remotes of unusual but accepted shapes, with the host pinned to a
// local server. A name under .invalid never resolves, so the server is
// reached only if curl's host is the one the pin was written for, which is
// the host Go validated.
func TestGitCurlHostMatchesPin(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	v, err := git.BinVersion(t.Context())
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	if v.Less(minPinGitVersion) {
		t.Skipf("git %s predates http.curloptResolve", v)
	}
	// A git built without curl has no http transport helper.
	if !hasGitHTTPHelper(t) {
		t.Skip("git has no http helper (git-remote-http)")
	}

	var mu sync.Mutex
	var hosts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.Host)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	for _, remote := range []string{
		"http://pinned.invalid:" + port + "/x.git",
		"http://user@pinned.invalid:" + port + "/x.git",
		"http://u%40x@pinned.invalid:" + port + "/x.git",
		"http://pinned.invalid:" + port + "/x.git?a#b",
		"http://PINNED.invalid:" + port + "/x.git",
	} {
		u, err := url.Parse(remote)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkAuthority(remote, u); err != nil {
			t.Errorf("%s: %v", remote, err)
			continue
		}
		mu.Lock()
		hosts = nil
		mu.Unlock()
		r := ValidatedGitRemote{Transport: GitTransportHTTP, Config: curlResolvePin(u, netip.MustParseAddr("127.0.0.1"))}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		cmd := exec.CommandContext(ctx, "git", "ls-remote", remote)
		cmd.Env = append(isolatedGitEnv(t), GitEnv(r)...)
		out, _ := cmd.CombinedOutput()
		cancel()
		mu.Lock()
		got := append([]string(nil), hosts...)
		mu.Unlock()
		// Only a missing helper skips; one that runs and fails (such as
		// "remote helper 'http' aborted session") fails the test.
		if len(got) == 0 && strings.Contains(string(out), "Unable to find remote helper") {
			t.Skipf("git cannot run its http helper: %s", out)
		}
		if len(got) == 0 || !strings.EqualFold(got[0], u.Host) {
			t.Errorf("%s: server saw hosts %q, want %q; git output:\n%s", remote, got, u.Host, out)
		}
	}
}

// hasGitHTTPHelper reports whether git's exec path holds git-remote-http,
// which git runs for http remotes and which a git built without curl lacks.
func hasGitHTTPHelper(t *testing.T) bool {
	t.Helper()
	// The same environment the test runs git with, so the probe sees the
	// same GIT_EXEC_PATH.
	cmd := exec.CommandContext(t.Context(), "git", "--exec-path")
	cmd.Env = isolatedGitEnv(t)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	helper := filepath.Join(strings.TrimSpace(string(out)), "git-remote-http")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	_, err = os.Stat(helper)
	return err == nil
}

// TestValidateGitRemoteAuthorityAsGitReadsIt covers remotes whose host part
// Go's URL parser and git read differently. url.Parse ends the authority at
// '?' or '#'; git takes everything up to the first '/' and hands it to ssh
// (percent-decoded) as the destination, or dials it. What Go validated must
// be exactly what git uses, or the remote is refused.
func TestValidateGitRemoteAuthorityAsGitReadsIt(t *testing.T) {
	t.Parallel()
	lookup := fakeLookup(map[string][]string{
		"git.example": {"140.82.112.3"},
	})
	anyHost := GitRemotePolicy{AllowAnySSHHost: true, lookup: lookup}
	// ssh splits its destination at the last '@', so the text after '?'
	// or '#' would pick the host and get past an allowlist naming the
	// first one.
	listed := GitRemotePolicy{AllowedSSHHosts: mustAllowedHosts(t, "git.example"), lookup: lookup}

	tests := []struct {
		remote string
		policy GitRemotePolicy
		err    error // nil: accepted
	}{
		// git passes the part after '?' or '#' on to ssh.
		{"ssh://git@git.example#$(id)/x.git", anyHost, ErrInvalidURL},
		{"ssh://git.example?;id/x.git", anyHost, ErrInvalidURL},
		{"git+ssh://git@git.example#`id`/x.git", anyHost, ErrInvalidURL},
		{"ssh+git://git@git.example?;touch${IFS}pwn@evil.example/x.git", anyHost, ErrInvalidURL},
		{"ssh://git@git.example#@evil.example:2222/x.git", anyHost, ErrInvalidURL},
		{"ssh://git@git.example?@evil.example/x.git", listed, ErrInvalidURL},
		{"ssh://git@git.example#@evil.example/x.git", listed, ErrInvalidURL},
		// The same split for http and git://: git reads the host up to the
		// first '/'.
		{"https://git.example?@evil.example/x.git", anyHost, ErrInvalidURL},
		{"http://git.example#@evil.example/x.git", anyHost, ErrInvalidURL},
		// Defence in depth: these two git:// rows were already refused by
		// gitProtocolPin, whose path check sees Go's empty path; the host
		// part check now refuses them first.
		{"git://140.82.112.3?@10.0.0.1/x.git", anyHost, ErrInvalidURL},
		{"git://git.example#@10.0.0.1/x.git", anyHost, ErrInvalidURL},
		// url.Parse lowercases the scheme; git does not, and would look for
		// a git-remote-SSH helper rather than run ssh.
		{"SSH://git@git.example/x.git", anyHost, ErrInvalidURL},
		{"Git+SSH://git@git.example/x.git", anyHost, ErrInvalidURL},
		{"ssh+GIT://git@git.example/x.git", anyHost, ErrInvalidURL},
		{"HTTPS://git.example/x.git", anyHost, ErrInvalidURL},
		{"Git://git.example/x.git", anyHost, ErrInvalidURL},
		// An empty user reaches ssh as "@host".
		{"ssh://@git.example/x.git", anyHost, ErrInvalidURL},
		{"@git.example:x.git", anyHost, ErrInvalidURL},
		// An empty port reaches ssh as part of the host ("git.example:").
		{"[git@git.example:]:x.git", anyHost, ErrInvalidURL},

		// '?' and '#' after the first '/' are part of the path, for git as
		// for Go, and percent-encoding reads the same to both.
		{"ssh://git@git.example/x.git?a#b", anyHost, nil},
		{"ssh://g%69t@git.example/x.git", anyHost, nil},
		{"ssh://git@git.example:2222/x.git", anyHost, nil},
		{"ssh://git@[2606:4700::1]:22/x.git", anyHost, nil},
		{"ssh://git@git.example/x.git", listed, nil},
		{"git@git.example:x.git", listed, nil},
		{"[git@git.example:2222]:x.git", anyHost, nil},
		{"https://git.example/x.git?a#b", anyHost, nil},
		{"https://user@git.example:8443/x.git", anyHost, nil},
		{"git://git.example:9418/x.git", anyHost, nil},
	}
	for _, tt := range tests {
		_, err := ValidateGitRemote(t.Context(), tt.remote, tt.policy)
		if tt.err == nil {
			if err != nil {
				t.Errorf("ValidateGitRemote(%q): %v", tt.remote, err)
			}
			continue
		}
		if !errors.Is(err, tt.err) {
			t.Errorf("ValidateGitRemote(%q) error = %v, want %v", tt.remote, err, tt.err)
		}
	}
}

// TestValidateGitRemotePinsGitProtocol checks the configuration a git://
// remote gets: its host as written is rewritten to the validated address.
func TestValidateGitRemotePinsGitProtocol(t *testing.T) {
	t.Parallel()
	policy := GitRemotePolicy{AllowAnySSHHost: true, lookup: fakeLookup(map[string][]string{
		"git.example": {"140.82.112.3"},
		"GIT.Example": {"140.82.112.3"},
		"v6.example":  {"2606:4700::1111"},
	})}
	tests := []struct {
		remote string
		want   []GitConfigEntry // nil: no configuration
		err    error
	}{
		{"git://git.example/x.git", []GitConfigEntry{{Key: "url.git://140.82.112.3/.insteadOf", Value: "git://git.example/"}}, nil},
		{"git://git.example:9418/x.git", []GitConfigEntry{{Key: "url.git://140.82.112.3:9418/.insteadOf", Value: "git://git.example:9418/"}}, nil},
		{"git://GIT.Example:1234/a/b", []GitConfigEntry{{Key: "url.git://140.82.112.3:1234/.insteadOf", Value: "git://GIT.Example:1234/"}}, nil},
		{"git://v6.example/x.git", []GitConfigEntry{{Key: "url.git://[2606:4700::1111]/.insteadOf", Value: "git://v6.example/"}}, nil},
		// An IP literal is dialed as is.
		{"git://140.82.112.3/x.git", nil, nil},
		{"git://[2606:4700::1111]:9418/x.git", nil, nil},
		// Shapes whose prefix could miss are refused, not left unpinned.
		{"git://user@git.example/x.git", nil, ErrInvalidURL},
		{"git://git.example", nil, ErrInvalidURL},
		{"git://git.example?q/x.git", nil, ErrInvalidURL},
		{"git://user@140.82.112.3/x.git", nil, ErrInvalidURL},
	}
	for _, tt := range tests {
		v, err := ValidateGitRemote(t.Context(), tt.remote, policy)
		if tt.err != nil {
			if !errors.Is(err, tt.err) {
				t.Errorf("ValidateGitRemote(%q) error = %v, want %v", tt.remote, err, tt.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ValidateGitRemote(%q): %v", tt.remote, err)
			continue
		}
		if v.Transport != GitTransportGit || !slices.Equal(v.Config, tt.want) {
			t.Errorf("ValidateGitRemote(%q) = %v %v, want %v", tt.remote, v.Transport, v.Config, tt.want)
		}
	}
}

// gitDaemonStub accepts git:// connections on one loopback address and
// records the first pkt-line of each: the request naming the repository and
// the host git put in its URL (host=...). It answers nothing, so git fails
// after sending it, which is all these tests need.
type gitDaemonStub struct {
	port     string
	mu       sync.Mutex
	requests []string
}

// newGitDaemonStub listens on ip (a loopback literal) at a free port. When
// skipUnavailable is set, a failure to listen skips the test rather than
// failing it, for an address family the machine may not have.
func newGitDaemonStub(t *testing.T, ip string, skipUnavailable bool) *gitDaemonStub {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		if skipUnavailable {
			t.Skipf("cannot listen on %s: %v", ip, err)
		}
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	s := &gitDaemonStub{port: port}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *gitDaemonStub) serve(c net.Conn) {
	defer c.Close() //nolint:errcheck
	_ = c.SetDeadline(time.Now().Add(time.Minute))
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil || n < 4 {
		return
	}
	line := make([]byte, n-4)
	if _, err := io.ReadFull(c, line); err != nil {
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, string(line))
	s.mu.Unlock()
}

// reached reports whether the stub has recorded a request.
func (s *gitDaemonStub) reached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests) > 0
}

func (s *gitDaemonStub) hosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.requests {
		for _, f := range strings.Split(r, "\x00") {
			if h, ok := strings.CutPrefix(f, "host="); ok {
				out = append(out, h)
			}
		}
	}
	return out
}

// TestGitEnvPinHonouredForGitProtocol runs the installed git against git://
// remotes with the configuration gitProtocolPin gives them, and checks where
// git connected. The pin rewrites the remote to an IP literal before git
// connects, so git resolves nothing: the listener must be reached, and the
// request must name the pinned address rather than the remote's name.
//
// The tests are hermetic when the pin works: no name is ever looked up, so
// neither DNS nor the hosts file is involved. If the pin were ignored, git
// would look the name up itself and either fail (.invalid, RFC 6761) or send
// host=<name>, and the test fails either way.
func TestGitEnvPinHonouredForGitProtocol(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	run := func(t *testing.T, remote string, r ValidatedGitRemote) func() []byte {
		t.Helper()
		env := append(isolatedGitEnv(t), GitEnv(r)...)
		return func() []byte {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "git", "ls-remote", remote)
			cmd.Env = env
			out, _ := cmd.CombinedOutput()
			return out
		}
	}
	pinned := func(t *testing.T, remote string, addr netip.Addr) ValidatedGitRemote {
		t.Helper()
		u, err := url.Parse(remote)
		if err != nil {
			t.Fatal(err)
		}
		pin, err := gitProtocolPin(remote, u, addr)
		if err != nil || len(pin) != 1 {
			t.Fatalf("gitProtocolPin(%q) = %v, %v; want one entry", remote, pin, err)
		}
		return ValidatedGitRemote{Transport: GitTransportGit, Config: pin}
	}

	tests := []struct {
		name string
		ip   string // the listener's and the pin's address
		host string // the remote's host, before the port
		want string // host= expected, before the port
		v6   bool
	}{
		// A name with no address at all.
		{"name that does not resolve", "127.0.0.1", "pinned.invalid", "127.0.0.1", false},
		// A name that would resolve to something of its own: the pin
		// outranks whatever that answer is.
		{"name that resolves", "127.0.0.1", "localhost", "127.0.0.1", false},
		// An IPv6 pin is bracketed in the rewritten URL.
		{"IPv6 address", "::1", "pinned.invalid", "[::1]", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newGitDaemonStub(t, tt.ip, tt.v6)
			remote := "git://" + tt.host + ":" + s.port + "/x.git"
			runUntilReached(t, s.reached, run(t, remote, pinned(t, remote, netip.MustParseAddr(tt.ip))))
			if got, want := s.hosts(), []string{tt.want + ":" + s.port}; !slices.Equal(got, want) {
				t.Fatalf("requests named %v, want %v", got, want)
			}
		})
	}
}

// proxyEnvNames are the variables through which the server's environment
// could route a git subprocess through a proxy.
var proxyEnvNames = []string{
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"GIT_PROXY_COMMAND", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "HOME",
}

// serverEnv returns the test process's environment without any proxy
// settings or git configuration of its own, plus extra, standing in for the
// environment a server runs git with.
func serverEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	var env []string
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		drop := false
		for _, p := range proxyEnvNames {
			if strings.EqualFold(name, p) {
				drop = true
				break
			}
		}
		if !drop {
			env = append(env, e)
		}
	}
	home := t.TempDir()
	return append(env, append([]string{
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	}, extra...)...)
}

// countingListener accepts and immediately closes connections, counting them.
func countingListener(t *testing.T) (addr string, count *atomic.Int32) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	count = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), count
}

// runGit runs git with env in dir (a new temporary directory if empty) and
// returns its output, ignoring the outcome: the remotes here are not real
// repositories, so only where git connected matters.
func runGit(t *testing.T, env []string, dir string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if dir == "" {
		dir = t.TempDir()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	t.Logf("git %s: %s", strings.Join(args, " "), out)
	return out
}

// TestGitEnvBypassesProxy covers a proxy in the server's environment or git
// configuration. A proxy resolves the remote's hostname itself, so the
// validated address pin never applies and the fetch reaches whatever the
// proxy can; GitEnv must make git connect directly.
func TestGitEnvBypassesProxy(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	// Each case sets a proxy one way git can be told to use one, and runs
	// git against http://pinned.invalid:<port>/org/repo.git, which the
	// remote pins to the origin.
	for _, tc := range []struct {
		name string
		// gitconfig is the global git configuration, with %[1]s for the
		// proxy URL and %[2]d for the origin's port.
		gitconfig string
		// env is extra server environment, with %s for the proxy URL.
		env []string
		// user is the user name in the remote URL, if any.
		user string
		// run is how git reaches the remote: "ls-remote" (the default),
		// "clone" as an import does, or "fetch" from a remote named
		// upstream as a mirror sync does. The last two read
		// remote.<name>.proxy.
		run string
	}{
		{name: "environment", env: []string{"http_proxy=%s", "ALL_PROXY=%s"}},
		{name: "http.proxy", gitconfig: "[http]\n\tproxy = %[1]s\n"},
		// git picks among URL-scoped keys by specificity, not order, so
		// these outrank a plain empty http.proxy.
		{name: "url-scoped http.proxy", gitconfig: "[http \"http://pinned.invalid:%[2]d/org/repo.git\"]\n\tproxy = %[1]s\n"},
		{name: "wildcard http.proxy", gitconfig: "[http \"http://*.invalid:%[2]d\"]\n\tproxy = %[1]s\n"},
		{
			name:      "user-scoped http.proxy",
			gitconfig: "[http \"http://alice@pinned.invalid:%[2]d/org/repo.git\"]\n\tproxy = %[1]s\n",
			user:      "alice:secret",
		},
		{
			name:      "remote.<name>.proxy on clone",
			gitconfig: "[clone]\n\tdefaultRemoteName = upstream\n[remote \"upstream\"]\n\tproxy = %[1]s\n",
			run:       "clone",
		},
		{
			name:      "remote.<name>.proxy on fetch",
			gitconfig: "[remote \"upstream\"]\n\tproxy = %[1]s\n",
			run:       "fetch",
		},
	} {
		t.Run("http/"+tc.name, func(t *testing.T) {
			t.Parallel()
			var originHits atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				originHits.Add(1)
				http.NotFound(w, nil)
			}))
			t.Cleanup(origin.Close)
			port := origin.Listener.Addr().(*net.TCPAddr).Port

			proxyAddr, proxyHits := countingListener(t)
			proxyURL := "http://" + proxyAddr

			var extra []string
			for _, e := range tc.env {
				extra = append(extra, fmt.Sprintf(e, proxyURL))
			}
			env := serverEnv(t, extra...)
			if tc.gitconfig != "" {
				cfg := fmt.Sprintf(tc.gitconfig, proxyURL, port)
				if err := os.WriteFile(gitConfigGlobal(env), []byte(cfg), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			remote := &url.URL{Scheme: "http", Host: fmt.Sprintf("pinned.invalid:%d", port), Path: "/org/repo.git"}
			if user, pass, ok := strings.Cut(tc.user, ":"); ok {
				remote.User = url.UserPassword(user, pass)
			}
			v := validatedHTTPRemote(remote, netip.MustParseAddr("127.0.0.1"))
			var run func() []byte
			switch tc.run {
			case "clone":
				env = append(env, GitEnv(v.ForClone())...)
				run = func() []byte {
					return runGit(t, env, "", "clone", "--mirror", remote.String(), filepath.Join(t.TempDir(), "repo.git"))
				}
			case "fetch":
				env = append(env, GitEnv(v.Named("upstream"))...)
				dir := t.TempDir()
				runGit(t, env, dir, "init", "--bare")
				runGit(t, env, dir, "remote", "add", "upstream", remote.String())
				run = func() []byte { return runGit(t, env, dir, "fetch", "upstream") }
			default:
				env = append(env, GitEnv(v)...)
				run = func() []byte { return runGit(t, env, "", "ls-remote", remote.String()) }
			}
			// A run that went through the proxy counts as reached, so it
			// is not repeated and fails below.
			runUntilReached(t, func() bool { return originHits.Load() > 0 || proxyHits.Load() > 0 }, run)

			if n := proxyHits.Load(); n != 0 {
				t.Errorf("git connected to the proxy %d times, want none", n)
			}
			if originHits.Load() == 0 {
				t.Error("git never reached the pinned origin")
			}
		})
	}

	t.Run("git", func(t *testing.T) {
		t.Parallel()
		daemonAddr, daemonHits := countingListener(t)

		// A proxy command that cannot run: if git uses it, it never reaches
		// the daemon.
		env := serverEnv(t, "GIT_PROXY_COMMAND=soft-serve-no-such-proxy-command")
		env = append(env, GitEnv(ValidatedGitRemote{Transport: GitTransportGit})...)

		runUntilReached(t, func() bool { return daemonHits.Load() > 0 }, func() []byte {
			return runGit(t, env, "", "ls-remote", "git://"+daemonAddr+"/repo.git")
		})

		if daemonHits.Load() == 0 {
			t.Error("git did not connect to the remote directly")
		}
	})
}

// gitConfigGlobal returns the GIT_CONFIG_GLOBAL path set in env.
func gitConfigGlobal(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GIT_CONFIG_GLOBAL="); ok {
			return v
		}
	}
	return ""
}
