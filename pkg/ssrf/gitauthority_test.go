package ssrf

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
)

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
	v, err := git.BinVersion()
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
