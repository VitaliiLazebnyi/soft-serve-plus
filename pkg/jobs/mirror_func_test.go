package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

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
