package ssrf

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
// ignores the outcome: the remotes here are not real repositories, so only
// where git connected matters.
func runGit(t *testing.T, env []string, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if dir == "" {
		dir = t.TempDir()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	t.Logf("git %s: %s", strings.Join(args, " "), out)
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
			switch tc.run {
			case "clone":
				env = append(env, GitEnv(v.ForClone())...)
				runGit(t, env, "", "clone", "--mirror", remote.String(), filepath.Join(t.TempDir(), "repo.git"))
			case "fetch":
				env = append(env, GitEnv(v.Named("upstream"))...)
				dir := t.TempDir()
				runGit(t, env, dir, "init", "--bare")
				runGit(t, env, dir, "remote", "add", "upstream", remote.String())
				runGit(t, env, dir, "fetch", "upstream")
			default:
				env = append(env, GitEnv(v)...)
				runGit(t, env, "", "ls-remote", remote.String())
			}

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

		runGit(t, env, "", "ls-remote", "git://"+daemonAddr+"/repo.git")

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
