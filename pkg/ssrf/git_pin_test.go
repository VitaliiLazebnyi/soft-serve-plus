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
	v, err := git.BinVersion()
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

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "git", "ls-remote", remote)
			cmd.Env = append(isolatedGitEnv(t), GitEnv(r)...)
			out, _ := cmd.CombinedOutput()

			if got := hits.Load() > 0; got != tt.reached {
				t.Fatalf("server reached = %v, want %v; git output:\n%s", got, tt.reached, out)
			}
			if tt.reached {
				if got, want := host.Load(), "pinned.invalid:"+port; got != want {
					t.Errorf("Host = %v, want %q", got, want)
				}
			}
		})
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
