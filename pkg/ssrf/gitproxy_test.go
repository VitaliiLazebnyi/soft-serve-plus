package ssrf

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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

// runGit runs git with env and ignores the outcome: the remotes here are
// not real repositories, so only where git connected matters.
func runGit(t *testing.T, env []string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = t.TempDir()
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

	t.Run("http", func(t *testing.T) {
		t.Parallel()
		var originHits atomic.Int32
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			originHits.Add(1)
			http.NotFound(w, nil)
		}))
		t.Cleanup(origin.Close)
		_, port, _ := net.SplitHostPort(origin.Listener.Addr().String())

		proxyAddr, proxyHits := countingListener(t)
		proxyURL := "http://" + proxyAddr

		env := serverEnv(t, "http_proxy="+proxyURL, "ALL_PROXY="+proxyURL)
		// http.proxy in the global gitconfig is the other way an operator
		// sets one.
		if err := os.WriteFile(gitConfigGlobal(env), []byte("[http]\n\tproxy = "+proxyURL+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		pinned := ValidatedGitRemote{
			Transport: GitTransportHTTP,
			Config: []GitConfigEntry{{
				Key:   "http.curloptResolve",
				Value: "pinned.invalid:" + port + ":127.0.0.1",
			}},
		}
		env = append(env, GitEnv(pinned)...)

		runGit(t, env, "ls-remote", "http://pinned.invalid:"+port+"/repo.git")

		if n := proxyHits.Load(); n != 0 {
			t.Errorf("git connected to the proxy %d times, want none", n)
		}
		if originHits.Load() == 0 {
			t.Error("git never reached the pinned origin")
		}
	})

	t.Run("git", func(t *testing.T) {
		t.Parallel()
		daemonAddr, daemonHits := countingListener(t)

		// A proxy command that cannot run: if git uses it, it never reaches
		// the daemon.
		env := serverEnv(t, "GIT_PROXY_COMMAND=soft-serve-no-such-proxy-command")
		env = append(env, GitEnv(ValidatedGitRemote{Transport: GitTransportGit})...)

		runGit(t, env, "ls-remote", "git://"+daemonAddr+"/repo.git")

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
