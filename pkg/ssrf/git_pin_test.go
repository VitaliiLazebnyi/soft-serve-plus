package ssrf

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
)

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
