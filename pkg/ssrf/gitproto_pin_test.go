package ssrf

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

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
		v, err := ValidateGitRemote(tt.remote, policy)
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
