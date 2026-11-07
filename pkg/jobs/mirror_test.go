package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

// newRepoWithRemotes creates a bare repo with the given name=url remotes.
func newRepoWithRemotes(t *testing.T, remotes map[string]string) *git.Repository {
	t.Helper()

	path := filepath.Join(t.TempDir(), "repo.git")
	if _, err := git.Init(path, true); err != nil {
		t.Fatalf("init: %v", err)
	}

	for name, url := range remotes {
		cmd := exec.CommandContext(t.Context(), "git", "remote", "add", name, url)
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("adding remote %s: %v: %s", name, err, out)
		}
	}

	r, err := git.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return r
}

func TestValidateMirrorRemotes(t *testing.T) {
	allowed, err := ssrf.ParseAllowedHosts([]string{"git.internal.example"})
	if err != nil {
		t.Fatal(err)
	}
	allow := ssrf.GitRemotePolicy{AllowedSSHHosts: allowed}
	admin := ssrf.GitRemotePolicy{AllowAnySSHHost: true}

	tests := []struct {
		name    string
		remotes map[string]string
		policy  ssrf.GitRemotePolicy
		wantErr error
	}{
		{
			// With an allowlist, every ssh remote must be on it. A second
			// remote added out of band is fetched by `remote update` too.
			name: "ssh remote on allowlist",
			remotes: map[string]string{
				"origin": "ssh://git@git.internal.example/x.git",
			},
			policy: allow,
		},
		{
			name: "ssh remote off allowlist",
			remotes: map[string]string{
				"origin": "ssh://git@git.internal.example/x.git",
				"backup": "git@10.0.0.1:x.git",
			},
			policy:  allow,
			wantErr: ssrf.ErrSSHHostNotAllowed,
		},
		{
			name:    "public origin",
			remotes: map[string]string{"origin": "https://1.1.1.1/x.git"},
		},
		{
			// With no allowlist, an ssh remote syncs only for an admin owner.
			name:    "ssh origin, admin owner",
			remotes: map[string]string{"origin": "ssh://git@10.0.0.1/x.git"},
			policy:  admin,
		},
		{
			name:    "ssh origin, unprivileged owner",
			remotes: map[string]string{"origin": "ssh://git@10.0.0.1/x.git"},
			wantErr: ssrf.ErrSSHRequiresAdmin,
		},
		{
			name:    "private origin",
			remotes: map[string]string{"origin": "http://127.0.0.1:8080/x.git"},
			wantErr: ssrf.ErrPrivateIP,
		},
		{
			name:    "metadata origin",
			remotes: map[string]string{"origin": "http://169.254.169.254/x.git"},
			wantErr: ssrf.ErrPrivateIP,
		},
		{
			// `git remote update` fetches from every remote, not just
			// origin. A guard that only reads origin misses this entirely.
			name: "private non-origin remote",
			remotes: map[string]string{
				"origin": "https://1.1.1.1/x.git",
				"backup": "http://192.168.1.1/x.git",
			},
			wantErr: ssrf.ErrPrivateIP,
		},
		{
			// git:// is a raw TCP connect and is just as usable for SSRF.
			name:    "private git scheme",
			remotes: map[string]string{"origin": "git://10.0.0.1/x.git"},
			wantErr: ssrf.ErrPrivateIP,
		},
		{
			// Go and libcurl disagree on non-canonical IPv4 literals.
			name:    "octal loopback",
			remotes: map[string]string{"origin": "http://0177.0.0.1/x.git"},
			wantErr: ssrf.ErrAmbiguousHost,
		},
		{
			// A remote that cannot be parsed must not be fetched from.
			// Failing open here was how unvalidated remotes kept firing.
			name:    "unparseable remote",
			remotes: map[string]string{"origin": "http://[::1/x.git"},
			wantErr: ssrf.ErrInvalidURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepoWithRemotes(t, tt.remotes)
			remotes, err := validateMirrorRemotes(r, tt.policy)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("validateMirrorRemotes() error = %v, want %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("validateMirrorRemotes() unexpected error: %v", err)
			}
			env := ssrf.GitEnv(remotes...)
			if !slices.Contains(env, "GIT_CONFIG_KEY_0=http.followRedirects") ||
				!slices.Contains(env, "GIT_CONFIG_VALUE_0=false") {
				t.Errorf("sync env did not disable redirects: %v", env)
			}
		})
	}
}

// TestValidateMirrorRemotesNoRemote verifies a mirror with no remote URL is
// skipped rather than treated as valid.
func TestValidateMirrorRemotesNoRemote(t *testing.T) {
	r := newRepoWithRemotes(t, nil)
	if _, err := validateMirrorRemotes(r, ssrf.GitRemotePolicy{}); err == nil {
		t.Error("validateMirrorRemotes() accepted a repo with no remote")
	}
}

// TestRunMirrorGitStalledRemote covers a remote that answers and then stalls.
// git sets no read timeout of its own, so before the per-command timeout the
// sync never returned, and the scheduler skipped every later mirror run. The
// http helper git starts outlives the killed git and holds its stderr, so
// this also covers not waiting on it.
func TestRunMirrorGitStalledRemote(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			// Answer with headers, then send nothing more.
			go func() {
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n"+
					"Content-Type: application/x-git-upload-pack-advertisement\r\n"+
					"Content-Length: 1000\r\n\r\n")
			}()
		}
	}()

	r := newRepoWithRemotes(t, map[string]string{
		"origin": "http://" + ln.Addr().String() + "/x.git",
	})

	old := mirrorGitTimeout
	mirrorGitTimeout = time.Second
	t.Cleanup(func() { mirrorGitTimeout = old })

	done := make(chan error, 1)
	go func() {
		done <- runMirrorGit(t.Context(), r.Path, nil, "fetch", "--prune")
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("runMirrorGit = %v, want a deadline error", err)
		}
	case <-time.After(mirrorGitTimeout + mirrorGitWaitDelay + 30*time.Second):
		t.Fatal("runMirrorGit did not return for a stalled remote")
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := limitedBuffer{limit: 4}
	for _, s := range []string{"ab", "cdef", "gh"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := b.String(); got != "abcd" {
		t.Errorf("buffer = %q, want %q", got, "abcd")
	}
}

// os/exec copies git's stderr pipe into the buffer with io.Copy, which
// prefers the writer's ReadFrom, so the cap must hold on that path too.
func TestLimitedBufferCopyFromPipe(t *testing.T) {
	if _, ok := any(&limitedBuffer{}).(io.ReaderFrom); ok {
		t.Fatal("limitedBuffer implements io.ReaderFrom, which bypasses its limit")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	const total = 1 << 20
	go func() {
		defer w.Close()
		_, _ = w.Write(bytes.Repeat([]byte("x"), total))
	}()

	b := limitedBuffer{limit: 4096}
	n, err := io.Copy(&b, r)
	if err != nil {
		t.Fatal(err)
	}
	if n != total {
		t.Errorf("io.Copy copied %d bytes, want %d", n, total)
	}
	if got := len(b.String()); got != 4096 {
		t.Errorf("buffer holds %d bytes, want 4096", got)
	}
}

// git keeps the query of a remote URL in its messages, and the query can hold
// a token, so the error that gets logged must not carry it.
func TestRunMirrorGitRedactsRemoteURL(t *testing.T) {
	// A port nothing listens on, so the fetch fails at once.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	r := newRepoWithRemotes(t, map[string]string{
		"origin": "http://user:secret@" + addr + "/x.git?token=abc",
	})

	err = runMirrorGit(t.Context(), r.Path, nil, "fetch", "--prune")
	if err == nil {
		t.Fatal("runMirrorGit succeeded against a closed port")
	}
	msg := err.Error()
	if !strings.Contains(msg, addr) {
		t.Errorf("error %q does not include git's message about the remote", msg)
	}
	for _, secret := range []string{"secret", "token=abc"} {
		if strings.Contains(msg, secret) {
			t.Errorf("error %q leaks %q", msg, secret)
		}
	}
}

func TestRedactURLs(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ in, want string }{
		{
			in:   "fatal: unable to access 'http://127.0.0.1:9/x.git?token=abc/': Failed to connect",
			want: "fatal: unable to access 'http://127.0.0.1:9/x.git': Failed to connect",
		},
		{
			in:   "remote: see https://u:p@example.com/a?k=v and git://h/r.git",
			want: "remote: see https://example.com/a and git://h/r.git",
		},
		{
			in:   "fatal: could not read from remote repository",
			want: "fatal: could not read from remote repository",
		},
	} {
		if got := redactURLs(tt.in); got != tt.want {
			t.Errorf("redactURLs(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
