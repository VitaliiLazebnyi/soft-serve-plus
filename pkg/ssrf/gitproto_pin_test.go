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

// gitDaemonStub accepts git:// connections on loopback and records the
// first pkt-line of each, the request naming the repository and the host
// git dialed by name (host=...). It answers nothing, so git fails after
// sending it, which is all these tests need.
type gitDaemonStub struct {
	port     string
	mu       sync.Mutex
	requests []string
}

func newGitDaemonStub(t *testing.T) *gitDaemonStub {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
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
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
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
// remotes with the configuration ValidateGitRemote gives them, and checks
// where git connected. With the pin, git connects to the pinned address
// without resolving the name: a name under .invalid (RFC 6761), which has no
// address, still reaches the listener, and the request names the address,
// so git used the rewritten URL. Without it, git dials the name it resolves
// itself, which is the rebinding hole; "localhost" stands in for that second
// answer, resolved from the hosts file rather than DNS.
func TestGitEnvPinHonouredForGitProtocol(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	loopback := netip.MustParseAddr("127.0.0.1")

	run := func(t *testing.T, remote string, r ValidatedGitRemote) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "ls-remote", remote)
		cmd.Env = append(isolatedGitEnv(t), GitEnv(r)...)
		out, _ := cmd.CombinedOutput()
		t.Logf("git ls-remote %s: %s", remote, out)
	}
	pinned := func(t *testing.T, remote string) ValidatedGitRemote {
		t.Helper()
		u, err := url.Parse(remote)
		if err != nil {
			t.Fatal(err)
		}
		pin, err := gitProtocolPin(remote, u, loopback)
		if err != nil || len(pin) != 1 {
			t.Fatalf("gitProtocolPin(%q) = %v, %v; want one entry", remote, pin, err)
		}
		return ValidatedGitRemote{Transport: GitTransportGit, Config: pin}
	}

	t.Run("pinned name that does not resolve", func(t *testing.T) {
		t.Parallel()
		s := newGitDaemonStub(t)
		remote := "git://pinned.invalid:" + s.port + "/x.git"
		run(t, remote, pinned(t, remote))
		if got, want := s.hosts(), []string{"127.0.0.1:" + s.port}; !slices.Equal(got, want) {
			t.Fatalf("requests named %v, want %v", got, want)
		}
	})

	t.Run("pinned name that resolves", func(t *testing.T) {
		t.Parallel()
		s := newGitDaemonStub(t)
		remote := "git://localhost:" + s.port + "/x.git"
		run(t, remote, pinned(t, remote))
		if got, want := s.hosts(), []string{"127.0.0.1:" + s.port}; !slices.Equal(got, want) {
			t.Fatalf("requests named %v, want %v: git dialed the name, not the pin", got, want)
		}
	})

	// The control: without the pin git resolves the name itself, so the
	// checks above tell the two apart.
	t.Run("unpinned", func(t *testing.T) {
		t.Parallel()
		s := newGitDaemonStub(t)
		remote := "git://localhost:" + s.port + "/x.git"
		run(t, remote, ValidatedGitRemote{Transport: GitTransportGit})
		if got, want := s.hosts(), []string{"localhost:" + s.port}; !slices.Equal(got, want) {
			t.Fatalf("unpinned requests named %v, want %v", got, want)
		}
	})
}
