package jobs

import (
	"bytes"
	"context"
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
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
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

// newMirrorJobContext returns a context carrying everything the mirror job
// reads: config, database, store and backend.
func newMirrorJobContext(t *testing.T) (context.Context, *backend.Backend) {
	t.Helper()
	ctx := context.Background()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx = config.WithContext(ctx, cfg)

	// The server's own SQLite settings: the job's workers look up mirror
	// owners concurrently and rely on busy_timeout to take turns.
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = filepath.Join(cfg.DataPath, "soft-serve.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	st := database.New(ctx, dbx)
	be := backend.New(ctx, cfg, dbx, st)
	ctx = db.WithContext(ctx, dbx)
	ctx = store.WithContext(ctx, st)
	ctx = backend.WithContext(ctx, be)
	return ctx, be
}

// The mirror job fetches every mirror from its remote, prunes what the remote
// deleted, and leaves alone mirrors whose remote fails validation and
// repositories that are not mirrors.
func TestMirrorPullFunc(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh client is a shell script")
	}
	isolateGitConfig(t)

	// git resolves the ssh client the job configures through PATH.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSH), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, be := newMirrorJobContext(t)
	if reason := be.ImportsDisabledReason(); reason != "" {
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
		dir := filepath.Join(config.FromContext(ctx).DataPath, "repos", name+".git")
		runGit(t, dir, "config", "remote.origin.url", remote)
		runGit(t, dir, "config", "remote.origin.fetch", "+refs/*:refs/*")
		runGit(t, dir, "config", "remote.origin.mirror", "true")
		return dir
	}
	mirrorDir := addRepo("mirror", true, sshRemote)
	refusedDir := addRepo("refused", true, "file://"+filepath.ToSlash(upstream))
	plainDir := addRepo("plain", false, sshRemote)

	job := func() { SyncMirrors(ctx) }
	job()

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
	job()

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
	isolateGitConfig(t)
	ctx, be := newMirrorJobContext(t)
	if reason := be.ImportsDisabledReason(); reason != "" {
		t.Skipf("mirror syncs are disabled here: %s", reason)
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	ctx = log.WithContext(ctx, log.New(&logs))

	dirs := map[string]string{}
	for _, name := range []string{"panics", "fine"} {
		if _, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{Mirror: true}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(config.FromContext(ctx).DataPath, "repos", name+".git")
		runGit(t, dir, "config", "remote.origin.url", "ssh://upstream.example/"+name)
		dirs[name] = dir
	}

	var mu sync.Mutex
	var fetched []string
	orig := runMirrorGit
	runMirrorGit = func(_ context.Context, dir string, _ []string, _ ...string) error {
		if dir == dirs["panics"] {
			panic("mirror boom")
		}
		mu.Lock()
		defer mu.Unlock()
		fetched = append(fetched, dir)
		return nil
	}
	t.Cleanup(func() { runMirrorGit = orig })

	runMirrorJob(ctx)

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(fetched, []string{dirs["fine"]}) {
		t.Errorf("fetched %v, want only %s", fetched, dirs["fine"])
	}
	if !strings.Contains(logs.String(), "mirror boom") {
		t.Errorf("panic not logged: %q", logs.String())
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
	for _, endpoint := range []string{"ssh://TOKEN@upstream.example/m", "TOKEN@upstream.example:m"} {
		t.Run(endpoint, func(t *testing.T) {
			isolateGitConfig(t)
			ctx, be := newMirrorJobContext(t)
			if reason := be.ImportsDisabledReason(); reason != "" {
				t.Skipf("mirror syncs are disabled here: %s", reason)
			}
			config.FromContext(ctx).LFS.Enabled = true
			admin, err := be.User(ctx, "admin")
			if err != nil {
				t.Fatal(err)
			}
			var logs syncBuffer
			ctx = log.WithContext(ctx, log.New(&logs))

			if _, err := be.CreateRepository(ctx, "m", admin, proto.RepositoryOptions{Mirror: true}); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(config.FromContext(ctx).DataPath, "repos", "m.git")
			runGit(t, dir, "config", "remote.origin.url", "ssh://upstream.example/m")
			runGit(t, dir, "config", "lfs.url", endpoint)

			orig := runMirrorGit
			runMirrorGit = func(context.Context, string, []string, ...string) error { return nil }
			t.Cleanup(func() { runMirrorGit = orig })

			runMirrorJob(ctx)

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
// endpoint, enables LFS, and returns the context the job runs with, logging
// to logs.
func deadlineMirror(t *testing.T, logs *syncBuffer) context.Context {
	t.Helper()
	isolateGitConfig(t)
	ctx, be := newMirrorJobContext(t)
	if reason := be.ImportsDisabledReason(); reason != "" {
		t.Skipf("mirror syncs are disabled here: %s", reason)
	}
	config.FromContext(ctx).LFS.Enabled = true
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "m", admin, proto.RepositoryOptions{Mirror: true}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(config.FromContext(ctx).DataPath, "repos", "m.git")
	runGit(t, dir, "config", "remote.origin.url", "ssh://upstream.example/m")
	// A public address literal passes validation without a lookup; the
	// LFS step is stubbed, so nothing dials it.
	runGit(t, dir, "config", "lfs.url", "https://1.1.1.1/m.git/info/lfs")
	return log.WithContext(ctx, log.New(logs))
}

// One deadline, mirrorSyncTimeout from the start, bounds the whole sync of a
// mirror: its fetches and its LFS step, which has no limit of its own while
// the remote keeps sending, so one slow LFS endpoint cannot hold the job,
// and with it every later run, for ever.
func TestMirrorPullOneDeadlinePerMirror(t *testing.T) {
	var logs syncBuffer
	ctx := deadlineMirror(t, &logs)

	var fetchDeadline, lfsDeadline time.Time
	var fetchHasDeadline, lfsHasDeadline bool
	origGit := runMirrorGit
	runMirrorGit = func(ctx context.Context, _ string, _ []string, _ ...string) error {
		fetchDeadline, fetchHasDeadline = ctx.Deadline()
		return nil
	}
	t.Cleanup(func() { runMirrorGit = origGit })
	origLFS := syncRepoLFS
	syncRepoLFS = func(ctx context.Context, _ *backend.Backend, _ proto.Repository, _ backend.LFSClient) error {
		lfsDeadline, lfsHasDeadline = ctx.Deadline()
		return nil
	}
	t.Cleanup(func() { syncRepoLFS = origLFS })

	start := time.Now()
	runMirrorJob(ctx)
	end := time.Now()

	if !fetchHasDeadline || !lfsHasDeadline {
		t.Fatalf("deadlines: fetch %v, lfs %v; want both", fetchHasDeadline, lfsHasDeadline)
	}
	if !fetchDeadline.Equal(lfsDeadline) {
		t.Errorf("fetch deadline %v, lfs deadline %v; want one for the whole sync", fetchDeadline, lfsDeadline)
	}
	// Taken while the job ran, so between its start and end, plus the
	// timeout.
	if fetchDeadline.Before(start.Add(mirrorSyncTimeout)) || fetchDeadline.After(end.Add(mirrorSyncTimeout)) {
		t.Errorf("deadline %v is not mirrorSyncTimeout after the sync started (job ran %v to %v)", fetchDeadline, start, end)
	}
}

// A sync that runs out of time stops, and says so; one stopped by a
// shutdown does not.
func TestMirrorPullLogsDeadline(t *testing.T) {
	var logs syncBuffer
	ctx := deadlineMirror(t, &logs)

	origGit := runMirrorGit
	runMirrorGit = func(context.Context, string, []string, ...string) error { return nil }
	t.Cleanup(func() { runMirrorGit = origGit })
	lfsCalled := false
	origLFS := syncRepoLFS
	syncRepoLFS = func(context.Context, *backend.Backend, proto.Repository, backend.LFSClient) error {
		lfsCalled = true
		return nil
	}
	t.Cleanup(func() { syncRepoLFS = origLFS })
	// Out of time before the first step.
	origTimeout := mirrorSyncTimeout
	mirrorSyncTimeout = time.Nanosecond
	t.Cleanup(func() { mirrorSyncTimeout = origTimeout })

	runMirrorJob(ctx)

	if lfsCalled {
		t.Error("the lfs step ran after the deadline")
	}
	if !strings.Contains(logs.String(), "ran out of time") {
		t.Errorf("the stopped sync was not logged: %q", logs.String())
	}
}

// isolateGitConfig keeps the git commands the code under test runs from
// reading the host's system and global configuration.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func runMirrorJob(ctx context.Context) { SyncMirrors(ctx) }
