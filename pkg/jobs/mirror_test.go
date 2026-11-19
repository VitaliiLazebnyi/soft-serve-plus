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

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/backend"
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
		deadline := time.Now().Add(30 * time.Second)
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
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runMirrorGit(ctx, r.Path, nil, "fetch", "--prune")
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("runMirrorGit = %v, want a deadline error", err)
		}
	case <-time.After(backend.RemoteGitWaitDelay + time.Minute):
		t.Fatal("runMirrorGit did not return for a stalled remote")
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
	ln.Close() //nolint:errcheck

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

// TestValidateMirrorRemotesReadsLikeGit verifies remotes are read the way git
// fetch reads them, not the way a config parser does: the first of several
// urls, urls rewritten by insteadOf, and the legacy [remote.name] section all
// reach validation.
func TestValidateMirrorRemotesReadsLikeGit(t *testing.T) {
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

	if err := syncMirror(t.Context(), mirror, []string{"origin"}, nil); err != nil {
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

// stubMirrorGit replaces runMirrorGit for the test and records the commands
// it was asked to run.
func stubMirrorGit(t *testing.T, fn func(args []string) error) *[][]string {
	t.Helper()
	var calls [][]string
	orig := runMirrorGit
	runMirrorGit = func(_ context.Context, _ string, _ []string, args ...string) error {
		calls = append(calls, args)
		return fn(args)
	}
	t.Cleanup(func() { runMirrorGit = orig })
	return &calls
}

// mirrorTestRemotes are the remotes the fetchMirror tests sync.
var mirrorTestRemotes = []string{"origin", "backup"}

func TestFetchMirrorFetchesEveryRemote(t *testing.T) {
	calls := stubMirrorGit(t, func([]string) error { return nil })
	var logs bytes.Buffer

	if err := fetchMirror(t.Context(), log.New(&logs), "repo", t.TempDir(), mirrorTestRemotes, nil); err != nil {
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
	calls := stubMirrorGit(t, func(args []string) error {
		if slices.Contains(args, "origin") {
			return errors.New("exit status 128")
		}
		return nil
	})
	var logs bytes.Buffer

	if err := fetchMirror(t.Context(), log.New(&logs), "repo", t.TempDir(), mirrorTestRemotes, nil); err == nil {
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
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"fetch finished", nil},
		{"fetch failed", errors.New("exit status 128")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := stubMirrorGit(t, func([]string) error {
				cancel()
				return tc.err
			})
			var logs bytes.Buffer

			if err := fetchMirror(ctx, log.New(&logs), "repo", t.TempDir(), mirrorTestRemotes, nil); !errors.Is(err, context.Canceled) {
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := stubMirrorGit(t, func([]string) error { return nil })

	if err := fetchMirror(ctx, log.New(io.Discard), "repo", t.TempDir(), mirrorTestRemotes, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("fetchMirror = %v, want %v", err, context.Canceled)
	}
	if len(*calls) != 0 {
		t.Fatalf("ran %v after cancellation", *calls)
	}
}

// Only a sync that failed once its deadline passed, while the server keeps
// running, ran out of time; one that succeeded just as it passed did not.
func TestOutOfTime(t *testing.T) {
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
