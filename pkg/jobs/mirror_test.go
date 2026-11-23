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
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

// newRepoWithRemotes creates a bare repo with the given name=url remotes.
func newRepoWithRemotes(t *testing.T, remotes map[string]string) *git.Repository {
	t.Helper()

	path := filepath.Join(t.TempDir(), "repo.git")
	if _, err := git.Init(t.Context(), path, true); err != nil {
		t.Fatalf("init: %v", err)
	}

	for name, url := range remotes {
		cmd := exec.CommandContext(t.Context(), "git", "remote", "add", name, url)
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("adding remote %s: %v: %s", name, err, out)
		}
	}

	r, err := git.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return r
}

func TestValidateMirrorRemotes(t *testing.T) {
	t.Parallel()
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
			// With an allowlist, an ssh remote must be on it.
			name: "ssh remote on allowlist",
			remotes: map[string]string{
				"origin": "ssh://git@git.internal.example/x.git",
			},
			policy: allow,
		},
		{
			name: "ssh remote off allowlist",
			remotes: map[string]string{
				"origin": "git@10.0.0.1:x.git",
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
			// The remote is validated whatever its name, not only when
			// it is origin.
			name: "private non-origin remote",
			remotes: map[string]string{
				"backup": "http://192.168.1.1/x.git",
			},
			wantErr: ssrf.ErrPrivateIP,
		},
		{
			// Each remote is fetched with --prune and +refs/*:refs/*, so
			// two would delete and overwrite each other's refs.
			name: "two remotes",
			remotes: map[string]string{
				"origin": "https://1.1.1.1/x.git",
				"backup": "https://1.0.0.1/x.git",
			},
			wantErr: errMultipleRemotes,
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
			t.Parallel()
			r := newRepoWithRemotes(t, tt.remotes)
			remotes, err := validateMirrorRemotes(t.Context(), r, tt.policy)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("validateMirrorRemotes() error = %v, want %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("validateMirrorRemotes() unexpected error: %v", err)
			}
			if len(remotes.names) != len(tt.remotes) {
				t.Errorf("validateMirrorRemotes() names = %v, want one per remote in %v", remotes.names, tt.remotes)
			}
			env := ssrf.GitEnv(remotes.validated...)
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
	t.Parallel()
	r := newRepoWithRemotes(t, nil)
	if _, err := validateMirrorRemotes(t.Context(), r, ssrf.GitRemotePolicy{}); err == nil {
		t.Error("validateMirrorRemotes() accepted a repo with no remote")
	}
}

// TestRunMirrorGitStalledRemote covers a remote that answers and then stalls.
// git sets no read timeout of its own, so without the sync's deadline the
// sync never returned, and the scheduler skipped every later mirror run. The
// http helper git starts outlives git and holds its stderr, so this also
// covers not waiting on it.
func TestRunMirrorGitStalledRemote(t *testing.T) {
	t.Parallel()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	closeRemote := func() {
		ln.Close() //nolint:errcheck
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close() //nolint:errcheck
		}
	}
	t.Cleanup(closeRemote)
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
	// The http helper outlives the killed git and, on Windows, keeps the
	// repository directory open until its connection drops. Cleanups run in
	// reverse order, so this one runs before t.TempDir removes the directory:
	// it closes the stalled connections so the helper exits, then waits for
	// the directory to become removable.
	t.Cleanup(func() {
		closeRemote()
		deadline := time.Now().Add(time.Minute)
		for {
			err := os.RemoveAll(r.Path)
			if err == nil || time.Now().After(deadline) {
				if err != nil {
					t.Errorf("removing %s: %v", r.Path, err)
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	// The sync's deadline stops the command, as it would a mirror whose
	// sync runs out of time.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second) // meant to run out: the command stalls
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- backend.RunRemoteGit(ctx, r.Path, nil, "fetch", "--prune")
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunRemoteGit = %v, want a deadline error", err)
		}
	case <-time.After(backend.RemoteGitWaitDelay + time.Minute):
		t.Fatal("RunRemoteGit did not return for a stalled remote")
	}
}

// git keeps the query of a remote URL in its messages, and the query can hold
// a token, so the error that gets logged must not carry it.
func TestRunMirrorGitRedactsRemoteURL(t *testing.T) {
	t.Parallel()
	// A port nothing listens on, so the fetch fails at once.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() //nolint:errcheck

	r := newRepoWithRemotes(t, map[string]string{
		"origin": "http://user:secret@" + addr + "/x.git?token=abc",
	})

	err = backend.RunRemoteGit(t.Context(), r.Path, nil, "fetch", "--prune")
	if err == nil {
		t.Fatal("RunRemoteGit succeeded against a closed port")
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

// TestValidateMirrorRemotesReadsLikeGit verifies remotes are read the way git
// fetch reads them, not the way a config parser does: the first of several
// urls, urls rewritten by insteadOf, and the legacy [remote.name] section all
// reach validation.
func TestValidateMirrorRemotesReadsLikeGit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config string
	}{
		{
			// git fetches from the first url; a parser that keeps the
			// last one validates a different remote.
			name: "first of several urls",
			config: `[remote "origin"]
	url = http://10.0.0.1/x.git
	url = https://1.1.1.1/x.git
`,
		},
		{
			// The rewrite applies when git fetches, after the stored url
			// was validated.
			name: "insteadOf rewrite",
			config: `[url "http://169.254.169.254/"]
	insteadOf = https://1.1.1.1/
[remote "origin"]
	url = https://1.1.1.1/x.git
`,
		},
		{
			// The legacy section syntax is a remote to git, but not a
			// subsection to a config parser.
			name: "legacy section syntax",
			config: `[remote.backup]
	url = http://127.0.0.1/x.git
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRepoWithRemotes(t, nil)
			appendConfig(t, r, tt.config)

			if _, err := validateMirrorRemotes(t.Context(), r, ssrf.GitRemotePolicy{}); !errors.Is(err, ssrf.ErrPrivateIP) {
				t.Fatalf("validateMirrorRemotes() error = %v, want %v", err, ssrf.ErrPrivateIP)
			}
		})
	}
}

// appendConfig appends raw text to the repository's config file.
func appendConfig(t *testing.T, r *git.Repository, config string) {
	t.Helper()

	f, err := os.OpenFile(filepath.Join(r.Path, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(config); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestValidateMirrorRemotesRefusesUnlistedFetchSources verifies a mirror is
// refused when git could be led to fetch from something validation does not
// see, and that a remote git would fetch from as a local path fails.
func TestValidateMirrorRemotesRefusesUnlistedFetchSources(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  string
		wantErr error
		// noOrigin leaves out the origin remote the repository otherwise
		// has.
		noOrigin bool
	}{
		{
			// `git remote update` fetches the remotes.default group, which
			// can name a legacy branches/ file remote `git remote` does not
			// list.
			name: "remotes.default group",
			config: `[remotes]
	default = foo
`,
			wantErr: errRemoteGroup,
		},
		{
			// A group named like a remote is fetched in its place even when
			// the remote is fetched by name.
			name: "group named like a remote",
			config: `[remotes]
	origin = foo bar
`,
			wantErr: errRemoteGroup,
		},
		{
			// git reports a remote without a url by its name and would
			// fetch from that name as a local path.
			name: "remote without url",
			config: `[remote "x"]
	fetch = +refs/heads/*:refs/remotes/x/*
`,
			wantErr:  ssrf.ErrUnsupportedRemoteScheme,
			noOrigin: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			remotes := map[string]string{"origin": "https://1.1.1.1/x.git"}
			if tt.noOrigin {
				remotes = nil
			}
			r := newRepoWithRemotes(t, remotes)
			appendConfig(t, r, tt.config)

			if _, err := validateMirrorRemotes(t.Context(), r, ssrf.GitRemotePolicy{}); !errors.Is(err, tt.wantErr) {
				t.Fatalf("validateMirrorRemotes() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestSyncMirrorFetchesOnlyNamedRemotes verifies the sync fetches the named
// remotes and nothing git would pick on its own: neither the url in
// branch.<HEAD>.remote, which a bare `git fetch` uses, nor the legacy file
// remote in the remotes.default group, which `git remote update` uses.
func TestSyncMirrorFetchesOnlyNamedRemotes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Each upstream holds one commit of its own on main, which its HEAD
	// points at, so a bare fetch of the upstream and the legacy branches/
	// refspec (refs/heads/main) both find something to fetch. Whether the
	// mirror then has that commit shows whether the upstream was fetched.
	upstream := func(name string) (path, commit string) {
		path = filepath.Join(dir, name+".git")
		runGit(t, dir, "init", "--bare", path)
		runGit(t, path, "symbolic-ref", "HEAD", "refs/heads/main")
		tree := runGit(t, path, "mktree")
		commit = runGit(t, path, "commit-tree", "-m", name, tree)
		runGit(t, path, "update-ref", "refs/heads/main", commit)
		return path, commit
	}
	origin, originCommit := upstream("origin")
	viaBranch, viaBranchCommit := upstream("viabranch")
	viaGroup, viaGroupCommit := upstream("viagroup")

	mirror := filepath.Join(dir, "mirror.git")
	runGit(t, dir, "init", "--bare", mirror)
	runGit(t, mirror, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, mirror, "remote", "add", "--mirror=fetch", "origin", origin)
	runGit(t, mirror, "config", "branch.main.remote", viaBranch)
	runGit(t, mirror, "config", "remotes.default", "foo")
	if err := os.MkdirAll(filepath.Join(mirror, "branches"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "branches", "foo"), []byte(viaGroup+"#main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &MirrorPull{runGit: backend.RunRemoteGit}
	if err := m.syncMirror(t.Context(), mirror, []string{"origin"}, nil); err != nil {
		t.Fatalf("syncMirror() error = %v", err)
	}

	hasObject := func(commit string) bool {
		cmd := exec.CommandContext(t.Context(), "git", "cat-file", "-e", commit+"^{commit}")
		cmd.Dir = mirror
		return cmd.Run() == nil
	}
	if !hasObject(originCommit) {
		t.Errorf("origin commit %s was not fetched", originCommit)
	}

	fetchHead, err := os.ReadFile(filepath.Join(mirror, "FETCH_HEAD"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, other := range []struct{ name, path, commit string }{
		{"viabranch", viaBranch, viaBranchCommit},
		{"viagroup", viaGroup, viaGroupCommit},
	} {
		if hasObject(other.commit) {
			t.Errorf("%s commit %s is in the mirror, though %s was not named", other.name, other.commit, other.name)
		}
		if strings.Contains(string(fetchHead), other.commit) || strings.Contains(string(fetchHead), filepath.Base(other.path)) {
			t.Errorf("FETCH_HEAD mentions %s, which was not named:\n%s", other.name, fetchHead)
		}
	}
}

// stubMirrorGit returns a mirror-pull job, logging to w, whose git commands
// run fn, and the commands it was asked to run.
func stubMirrorGit(w io.Writer, fn func(args []string) error) (*MirrorPull, *[][]string) {
	var calls [][]string
	m := &MirrorPull{
		logger: log.New(w),
		runGit: func(_ context.Context, _ string, _ []string, args ...string) error {
			calls = append(calls, args)
			return fn(args)
		},
	}
	return m, &calls
}

// mirrorTestRemotes are the remotes the fetchMirror tests sync.
var mirrorTestRemotes = []string{"origin", "backup"}

func TestFetchMirrorFetchesEveryRemote(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	m, calls := stubMirrorGit(&logs, func([]string) error { return nil })

	if err := m.fetchMirror(t.Context(), "repo", t.TempDir(), mirrorTestRemotes, nil); err != nil {
		t.Fatalf("fetchMirror stopped without being cancelled: %v", err)
	}
	want := [][]string{
		{"fetch", "--prune", "--", "origin", "+refs/*:refs/*"},
		{"fetch", "--prune", "--", "backup", "+refs/*:refs/*"},
	}
	if !slices.EqualFunc(*calls, want, slices.Equal[[]string]) {
		t.Fatalf("ran %v, want %v", *calls, want)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected logs: %s", logs.String())
	}
}

// A remote that fails to fetch is logged and does not stop the others,
// but the lfs sync is skipped until the next run.
func TestFetchMirrorSkipsLFSAfterFailure(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	m, calls := stubMirrorGit(&logs, func(args []string) error {
		if slices.Contains(args, "origin") {
			return errors.New("exit status 128")
		}
		return nil
	})

	if err := m.fetchMirror(t.Context(), "repo", t.TempDir(), mirrorTestRemotes, nil); err == nil {
		t.Fatal("fetchMirror went on to the lfs sync after a failed fetch")
	}
	if len(*calls) != len(mirrorTestRemotes) {
		t.Fatalf("ran %v, want a fetch of each of %v", *calls, mirrorTestRemotes)
	}
	if !strings.Contains(logs.String(), "exit status 128") {
		t.Fatalf("the failed fetch was not logged: %s", logs.String())
	}
}

// A shutdown during a fetch lets it finish, then starts nothing else: not
// the next remote's fetch and not the lfs sync. It logs nothing either way.
func TestFetchMirrorStopsAfterCancel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"fetch finished", nil},
		{"fetch failed", errors.New("exit status 128")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var logs bytes.Buffer
			m, calls := stubMirrorGit(&logs, func([]string) error {
				cancel()
				return tc.err
			})

			if err := m.fetchMirror(ctx, "repo", t.TempDir(), mirrorTestRemotes, nil); !errors.Is(err, context.Canceled) {
				t.Fatalf("fetchMirror = %v, want %v", err, context.Canceled)
			}
			if len(*calls) != 1 {
				t.Fatalf("ran %v, want only the first fetch", *calls)
			}
			if logs.Len() != 0 {
				t.Fatalf("a cancelled sync logged: %s", logs.String())
			}
		})
	}
}

func TestFetchMirrorCancelledStartsNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m, calls := stubMirrorGit(io.Discard, func([]string) error { return nil })

	if err := m.fetchMirror(ctx, "repo", t.TempDir(), mirrorTestRemotes, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("fetchMirror = %v, want %v", err, context.Canceled)
	}
	if len(*calls) != 0 {
		t.Fatalf("ran %v after cancellation", *calls)
	}
}

// Only a sync that failed once its deadline passed, while the server keeps
// running, ran out of time; one that succeeded just as it passed did not.
func TestOutOfTime(t *testing.T) {
	t.Parallel()
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	stopping, stop := context.WithCancel(t.Context())
	stop()
	stoppedExpired, cancel2 := context.WithDeadline(stopping, time.Now().Add(-time.Second))
	defer cancel2()
	failed := errors.New("signal: terminated")

	for _, tc := range []struct {
		name        string
		ctx, jobCtx context.Context
		err         error
		want        bool
	}{
		{"failed after the deadline", expired, t.Context(), failed, true},
		{"succeeded at the deadline", expired, t.Context(), nil, false},
		{"failed before the deadline", t.Context(), t.Context(), failed, false},
		{"server stopping", stoppedExpired, stopping, failed, false},
	} {
		if got := outOfTime(tc.ctx, tc.jobCtx, tc.err); got != tc.want {
			t.Errorf("%s: outOfTime = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fakeSSH is an ssh client that runs the remote command locally, so a test
// can mirror over ssh:// without a server. git calls it as
// "ssh [options] host command" and the command comes last.
const fakeSSH = `#!/bin/sh
for last; do :; done
exec sh -c "$last"
`

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// refs lists a repository's refs as "name oid" lines.
func refs(t *testing.T, dir string) string {
	t.Helper()
	return runGit(t, dir, "for-each-ref", "--format=%(refname) %(objectname)")
}

// newMirrorJobContext returns a context, and the config and backend of a
// server with its own data path and database.
func newMirrorJobContext(t *testing.T) (context.Context, *config.Config, *backend.Backend) {
	t.Helper()
	// On SQLite, the job's workers look up mirror owners concurrently and
	// rely on the busy timeout db.Open sets to take turns.
	e := backendtest.New(t, backendtest.WithLogger(log.New(io.Discard)))
	return t.Context(), e.Cfg, e.Backend
}

// newTestMirrorPull returns the mirror-pull job of cfg and be, logging to w.
func newTestMirrorPull(t *testing.T, cfg *config.Config, be *backend.Backend, w io.Writer) *MirrorPull {
	t.Helper()
	m, err := NewMirrorPull(cfg, be, log.New(w))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The mirror job fetches every mirror from its remote, prunes what the remote
// deleted, and leaves alone mirrors whose remote fails validation and
// repositories that are not mirrors.
func TestMirrorPullFunc(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh client is a shell script")
	}

	// git resolves the ssh client the job configures through PATH.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cfg, be := newMirrorJobContext(t)
	if reason := be.ImportsDisabledReason(context.Background()); reason != "" {
		t.Skipf("mirror syncs are disabled here: %s", reason)
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// The upstream: two branches.
	work := t.TempDir()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	runGit(t, work, "init", "-q", "-b", "main", ".")
	runGit(t, work, "commit", "-q", "--allow-empty", "-m", "first")
	runGit(t, work, "branch", "feature")
	runGit(t, work, "clone", "-q", "--bare", work, upstream)
	runGit(t, work, "remote", "add", "origin", upstream)

	// A mirror of it over ssh (allowed: its owner is an admin), a mirror
	// of it over file:// (refused by validation), and a plain repository
	// with the ssh remote (not a mirror).
	sshRemote := "ssh://upstream.example" + filepath.ToSlash(upstream)
	addRepo := func(name string, mirror bool, remote string) string {
		t.Helper()
		if _, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{Mirror: mirror}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(cfg.DataPath, "repos", name+".git")
		runGit(t, dir, "config", "remote.origin.url", remote)
		runGit(t, dir, "config", "remote.origin.fetch", "+refs/*:refs/*")
		runGit(t, dir, "config", "remote.origin.mirror", "true")
		return dir
	}
	mirrorDir := addRepo("mirror", true, sshRemote)
	refusedDir := addRepo("refused", true, "file://"+filepath.ToSlash(upstream))
	plainDir := addRepo("plain", false, sshRemote)

	job := newTestMirrorPull(t, cfg, be, io.Discard)
	job.Run(ctx)

	if got, want := refs(t, mirrorDir), refs(t, upstream); got != want {
		t.Errorf("mirror refs after sync:\n%s\nwant:\n%s", got, want)
	}
	if got := refs(t, refusedDir); got != "" {
		t.Errorf("mirror with a refused remote was fetched; refs:\n%s", got)
	}
	if got := refs(t, plainDir); got != "" {
		t.Errorf("repository that is not a mirror was fetched; refs:\n%s", got)
	}

	// New commits arrive, deleted branches go.
	runGit(t, work, "commit", "-q", "--allow-empty", "-m", "second")
	runGit(t, work, "push", "-q", "origin", "main", ":feature")
	job.Run(ctx)

	got, want := refs(t, mirrorDir), refs(t, upstream)
	if got != want {
		t.Errorf("mirror refs after the second sync:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "refs/heads/feature") {
		t.Errorf("branch deleted upstream is still in the mirror:\n%s", got)
	}
}

// A mirror whose sync panics is logged and does not stop the job or the
// other mirrors.
func TestMirrorPullRecoversWorkerPanic(t *testing.T) {
	t.Parallel()
	ctx, cfg, be := newMirrorJobContext(t)
	if reason := be.ImportsDisabledReason(context.Background()); reason != "" {
		t.Skipf("mirror syncs are disabled here: %s", reason)
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	m := newTestMirrorPull(t, cfg, be, &logs)

	dirs := map[string]string{}
	for _, name := range []string{"panics", "fine"} {
		if _, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{Mirror: true}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(cfg.DataPath, "repos", name+".git")
		runGit(t, dir, "config", "remote.origin.url", "ssh://upstream.example/"+name)
		dirs[name] = dir
	}

	var mu sync.Mutex
	var fetched []string
	m.runGit = func(_ context.Context, dir string, _ []string, _ ...string) error {
		if dir == dirs["panics"] {
			panic("mirror boom")
		}
		mu.Lock()
		defer mu.Unlock()
		fetched = append(fetched, dir)
		return nil
	}

	m.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(fetched, []string{dirs["fine"]}) {
		t.Errorf("fetched %v, want only %s", fetched, dirs["fine"])
	}
	if !strings.Contains(logs.String(), "mirror boom") {
		t.Errorf("panic not logged: %q", logs.String())
	}
	// The panic's log line names the repository.
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "mirror boom") && !strings.Contains(line, "repo=panics") {
			t.Errorf("panic logged without its repository: %q", line)
		}
	}
}

// A worker whose slot comes free only once the server began to stop starts
// nothing: no fetch, and nothing logged or recorded. Run starts no
// worker once ctx is done, but one already waiting for a slot gets it after.
func TestSyncMirrorWorkerAfterStop(t *testing.T) {
	t.Parallel()
	ctx, cfg, be := newMirrorJobContext(t)
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "m", admin, proto.RepositoryOptions{Mirror: true}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.DataPath, "repos", "m.git")
	runGit(t, dir, "config", "remote.origin.url", "ssh://upstream.example/m")
	fetched := false
	var logs syncBuffer
	m := newTestMirrorPull(t, cfg, be, &logs)
	m.runGit = func(context.Context, string, []string, ...string) error {
		fetched = true
		return nil
	}

	stopped, stop := context.WithCancel(ctx)
	stop()
	repo, err := be.Repository(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	m.syncMirrorWorker(stopped, repo)

	if fetched {
		t.Error("a worker started after the stop fetched")
	}
	if logs.String() != "" {
		t.Errorf("a worker started after the stop logged: %q", logs.String())
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A mirror whose stored lfs.url is an ssh endpoint, which older imports
// accepted, is synced without its LFS objects, and the job says why without
// the endpoint's credentials.
func TestMirrorPullReportsSSHLFSEndpoint(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"ssh://TOKEN@upstream.example/m", "TOKEN@upstream.example:m"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			ctx, cfg, be := newMirrorJobContext(t)
			if reason := be.ImportsDisabledReason(context.Background()); reason != "" {
				t.Skipf("mirror syncs are disabled here: %s", reason)
			}
			cfg.LFS.Enabled = true
			admin, err := be.User(ctx, "admin")
			if err != nil {
				t.Fatal(err)
			}
			var logs syncBuffer
			m := newTestMirrorPull(t, cfg, be, &logs)

			if _, err := be.CreateRepository(ctx, "m", admin, proto.RepositoryOptions{Mirror: true}); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(cfg.DataPath, "repos", "m.git")
			runGit(t, dir, "config", "remote.origin.url", "ssh://upstream.example/m")
			runGit(t, dir, "config", "lfs.url", endpoint)

			m.runGit = func(context.Context, string, []string, ...string) error { return nil }

			m.Run(ctx)

			out := logs.String()
			if !strings.Contains(out, "skipping lfs sync") || !strings.Contains(out, lfs.ErrUnsupportedEndpoint.Error()) {
				t.Errorf("the ssh endpoint was not reported: %q", out)
			}
			if strings.Contains(out, "TOKEN") {
				t.Errorf("logs contain the endpoint's credentials: %q", out)
			}
		})
	}
}

// deadlineMirror creates a mirror with an ssh remote and an https LFS
// endpoint, enables LFS, and returns the context the job runs with and the
// job, logging to logs.
func deadlineMirror(t *testing.T, logs *syncBuffer) (context.Context, *MirrorPull) {
	t.Helper()
	ctx, cfg, be := newMirrorJobContext(t)
	if reason := be.ImportsDisabledReason(context.Background()); reason != "" {
		t.Skipf("mirror syncs are disabled here: %s", reason)
	}
	cfg.LFS.Enabled = true
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "m", admin, proto.RepositoryOptions{Mirror: true}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.DataPath, "repos", "m.git")
	runGit(t, dir, "config", "remote.origin.url", "ssh://upstream.example/m")
	// A public address literal passes validation without a lookup; the
	// LFS step is stubbed, so nothing dials it.
	runGit(t, dir, "config", "lfs.url", "https://1.1.1.1/m.git/info/lfs")
	return ctx, newTestMirrorPull(t, cfg, be, logs)
}

// One deadline, MirrorPull.timeout from the start, bounds the whole sync of a
// mirror: its fetches and its LFS step, which has no limit of its own while
// the remote keeps sending, so one slow LFS endpoint cannot hold the job,
// and with it every later run, for ever.
func TestMirrorPullOneDeadlinePerMirror(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	ctx, m := deadlineMirror(t, &logs)

	var fetchDeadline, lfsDeadline time.Time
	var fetchHasDeadline, lfsHasDeadline bool
	m.runGit = func(ctx context.Context, _ string, _ []string, _ ...string) error {
		fetchDeadline, fetchHasDeadline = ctx.Deadline()
		return nil
	}
	m.syncLFS = func(ctx context.Context, _ proto.Repository, _ backend.LFSClient) error {
		lfsDeadline, lfsHasDeadline = ctx.Deadline()
		return nil
	}

	start := time.Now()
	m.Run(ctx)
	end := time.Now()

	if !fetchHasDeadline || !lfsHasDeadline {
		t.Fatalf("deadlines: fetch %v, lfs %v; want both", fetchHasDeadline, lfsHasDeadline)
	}
	if !fetchDeadline.Equal(lfsDeadline) {
		t.Errorf("fetch deadline %v, lfs deadline %v; want one for the whole sync", fetchDeadline, lfsDeadline)
	}
	// Taken while the job ran, so between its start and end, plus the
	// timeout.
	if fetchDeadline.Before(start.Add(m.timeout)) || fetchDeadline.After(end.Add(m.timeout)) {
		t.Errorf("deadline %v is not the sync timeout after the sync started (job ran %v to %v)", fetchDeadline, start, end)
	}
}

// A sync that runs out of time stops, and says so; one stopped by a
// shutdown does not.
func TestMirrorPullLogsDeadline(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	ctx, m := deadlineMirror(t, &logs)

	m.runGit = func(context.Context, string, []string, ...string) error { return nil }
	lfsCalled := false
	m.syncLFS = func(context.Context, proto.Repository, backend.LFSClient) error {
		lfsCalled = true
		return nil
	}
	// Out of time before the first step.
	m.timeout = time.Nanosecond

	m.Run(ctx)

	if lfsCalled {
		t.Error("the lfs step ran after the deadline")
	}
	if !strings.Contains(logs.String(), "ran out of time") {
		t.Errorf("the stopped sync was not logged: %q", logs.String())
	}
}

// A repository imported without --mirror has a remote url and no fetch
// refspec, and turning mirroring on later adds none. Its syncs must still
// follow the remote, as a git clone --mirror does, rather than fetch only
// the remote's HEAD into FETCH_HEAD and leave every branch where it was.
func TestMirrorPullWithoutFetchRefspec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh client is a shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cfg, be := newMirrorJobContext(t)
	if reason := be.ImportsDisabledReason(context.Background()); reason != "" {
		t.Skipf("mirror syncs are disabled here: %s", reason)
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	runGit(t, work, "init", "-q", "-b", "main", ".")
	runGit(t, work, "commit", "-q", "--allow-empty", "-m", "first")
	runGit(t, work, "clone", "-q", "--bare", work, upstream)
	runGit(t, work, "remote", "add", "origin", upstream)

	// What git clone --bare leaves, plus the flag repo mirror NAME true
	// sets: a url, no fetch refspec.
	if _, err := be.CreateRepository(ctx, "m", admin, proto.RepositoryOptions{Mirror: true}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.DataPath, "repos", "m.git")
	runGit(t, dir, "config", "remote.origin.url", "ssh://upstream.example"+filepath.ToSlash(upstream))
	runGit(t, dir, "config", "remote.origin.mirror", "true")

	m := newTestMirrorPull(t, cfg, be, io.Discard)
	m.Run(ctx)
	if got, want := refs(t, dir), refs(t, upstream); got != want {
		t.Errorf("mirror refs after the first sync:\n%s\nwant:\n%s", got, want)
	}

	runGit(t, work, "commit", "-q", "--allow-empty", "-m", "second")
	runGit(t, work, "push", "-q", "origin", "main")
	m.Run(ctx)
	if got, want := refs(t, dir), refs(t, upstream); got != want {
		t.Errorf("mirror refs after the remote moved:\n%s\nwant:\n%s", got, want)
	}
}

// The job lists the repositories once, then works through them. A repository
// switched off while it waits its turn is not fetched: its fetch would
// prune every branch pushed to it since.
func TestMirrorPullRereadsMirrorFlag(t *testing.T) {
	t.Parallel()
	ctx, cfg, be := newMirrorJobContext(t)
	if reason := be.ImportsDisabledReason(context.Background()); reason != "" {
		t.Skipf("mirror syncs are disabled here: %s", reason)
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]string{}
	for _, name := range []string{"a", "b"} {
		if _, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{Mirror: true}); err != nil {
			t.Fatal(err)
		}
		dirs[name] = filepath.Join(cfg.DataPath, "repos", name+".git")
		runGit(t, dirs[name], "config", "remote.origin.url", "ssh://upstream.example/"+name)
	}

	// One worker, so the mirrors sync in list order, "a" first; "b" is
	// switched off while "a" syncs.
	m := newTestMirrorPull(t, cfg, be, io.Discard)
	m.workers = 1
	var mu sync.Mutex
	var fetched []string
	m.runGit = func(_ context.Context, dir string, _ []string, _ ...string) error {
		mu.Lock()
		fetched = append(fetched, dir)
		mu.Unlock()
		if dir == dirs["a"] {
			if err := be.SetMirror(ctx, "b", false); err != nil {
				t.Errorf("SetMirror(b, false): %v", err)
			}
		}
		return nil
	}

	m.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(fetched, []string{dirs["a"]}) {
		t.Errorf("fetched %v, want only %s", fetched, dirs["a"])
	}
}

// Each sync's outcome is recorded: the error of one that failed, and the
// time of one that succeeded, which also clears the error.
func TestMirrorPullRecordsOutcome(t *testing.T) {
	t.Parallel()
	ctx, cfg, be := newMirrorJobContext(t)
	if reason := be.ImportsDisabledReason(context.Background()); reason != "" {
		t.Skipf("mirror syncs are disabled here: %s", reason)
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "m", admin, proto.RepositoryOptions{Mirror: true}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.DataPath, "repos", "m.git")
	runGit(t, dir, "config", "remote.origin.url", "ssh://upstream.example/m")

	fail := true
	m := newTestMirrorPull(t, cfg, be, io.Discard)
	m.runGit = func(context.Context, string, []string, ...string) error {
		if fail {
			return errors.New("exit status 128: fatal: remote went away")
		}
		return nil
	}

	m.Run(ctx)
	st, err := be.MirrorSyncStatus(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !st.SyncedAt.IsZero() || !strings.Contains(st.Error, "remote went away") {
		t.Errorf("after a failed sync: %+v, want no sync time and the error", st)
	}

	fail = false
	m.Run(ctx)
	if st, err = be.MirrorSyncStatus(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	if st.SyncedAt.IsZero() || st.Error != "" {
		t.Errorf("after a sync that succeeded: %+v, want a sync time and no error", st)
	}
}

// The job constructors refuse a missing config or backend rather than build
// a job that fails at its first run.
func TestNewJobsNilDependencies(t *testing.T) {
	t.Parallel()
	be := new(backend.Backend)
	for _, tc := range []struct {
		name string
		new  func() (any, error)
		want error
	}{
		{"mirror-pull nil config", func() (any, error) { return NewMirrorPull(nil, be, nil) }, config.ErrNilConfig},
		{"mirror-pull nil backend", func() (any, error) { return NewMirrorPull(config.DefaultConfig(), nil, nil) }, errNilBackend},
		{"lfs-sweep nil config", func() (any, error) { return NewLFSSweep(nil, nil) }, config.ErrNilConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.new(); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A mirror-pull job with no worker count syncs as many mirrors at once as
// GOMAXPROCS allows when it runs, not when it was made: GOMAXPROCS follows
// the CPU limit of the server's cgroup.
func TestMirrorPullWorkerLimit(t *testing.T) {
	t.Parallel()
	m, err := NewMirrorPull(config.DefaultConfig(), new(backend.Backend), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.workers != 0 {
		t.Fatalf("workers = %d, want 0, read at run time", m.workers)
	}
	if got, want := m.workerLimit(), runtime.GOMAXPROCS(0); got != want {
		t.Errorf("workerLimit = %d, want GOMAXPROCS %d", got, want)
	}
	m.workers = 3
	if got := m.workerLimit(); got != 3 {
		t.Errorf("workerLimit with 3 workers = %d", got)
	}
}

// Run syncs up to its worker limit of mirrors at once: two workers, or
// GOMAXPROCS when the job has no worker count. Each fetch waits until the
// other mirror's fetch has started too, for at most a minute.
func TestMirrorPullSyncsConcurrently(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		workers int
	}{{"two workers", 2}, {"GOMAXPROCS", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.workers == 0 && runtime.GOMAXPROCS(0) < 2 {
				t.Skip("GOMAXPROCS is 1: one mirror syncs at a time")
			}
			ctx, cfg, be := newMirrorJobContext(t)
			if reason := be.ImportsDisabledReason(context.Background()); reason != "" {
				t.Skipf("mirror syncs are disabled here: %s", reason)
			}
			admin, err := be.User(ctx, "admin")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b"} {
				if _, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{Mirror: true}); err != nil {
					t.Fatal(err)
				}
				runGit(t, filepath.Join(cfg.DataPath, "repos", name+".git"), "config", "remote.origin.url", "ssh://upstream.example/"+name)
			}

			m := newTestMirrorPull(t, cfg, be, io.Discard)
			m.workers = tc.workers
			var inside atomic.Int32
			both := make(chan struct{})
			m.runGit = func(context.Context, string, []string, ...string) error {
				// Counted only while inside, so the two fetches must
				// overlap: one after the other never reaches two.
				defer inside.Add(-1)
				if inside.Add(1) == 2 {
					close(both)
				}
				select {
				case <-both:
					return nil
				case <-time.After(time.Minute):
					return errors.New("the other mirror's fetch did not start")
				}
			}
			m.Run(ctx)

			select {
			case <-both:
			default:
				t.Fatal("the two mirrors never synced at once")
			}
		})
	}
}
