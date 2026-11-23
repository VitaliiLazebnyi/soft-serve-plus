package cmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
)

// newLookupTestContext is newAuthTestContext on the test database dbtest
// opens (Postgres when SOFT_SERVE_DB_DRIVER is "postgres", SQLite
// otherwise), with the log written to the returned buffer at Debug level.
// The lookup tests break its tables.
func newLookupTestContext(t *testing.T) (context.Context, *backend.Backend, *syncLog) {
	t.Helper()
	return newLookupTestContextWith(t, nil)
}

// newLookupTestContextWith is newLookupTestContext with the backend's store
// wrapped by wrap, when it is not nil.
func newLookupTestContextWith(t *testing.T, wrap func(store.Store) store.Store) (context.Context, *backend.Backend, *syncLog) {
	t.Helper()
	logs := &syncLog{}
	logger := log.New(logs)
	logger.SetLevel(log.DebugLevel)
	ctx, be := newTestCommands(t, nil, wrap, logger)
	return ctx, be, logs
}

// syncLog is a log destination that is safe to read while git's goroutines
// may still write to it.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// reset empties the log.
func (l *syncLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

// count returns how many lines of the log hold level.
func (l *syncLog) count(level string) int {
	n := 0
	for line := range strings.Lines(l.String()) {
		if strings.Contains(line, level) {
			n++
		}
	}
	return n
}

// breakTable renames table so every query on it fails, as a database
// fault would.
func breakTable(t *testing.T, ctx context.Context, table string) {
	t.Helper()
	dbx := cmdsOf(ctx).db
	if _, err := dbx.ExecContext(ctx, "ALTER TABLE "+table+" RENAME TO "+table+"_gone"); err != nil {
		t.Fatal(err)
	}
}

// `repo list` lists the repositories of the session's user, as the session
// resolved it. A lookup of the session's key that fails no longer turns the
// list into what an anonymous caller sees.
func TestRepoListUsesSessionUser(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be, _ := newLookupTestContext(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	is.NoErr(err)
	signer, err := gossh.NewSignerFromKey(priv)
	is.NoErr(err)
	pk := signer.PublicKey()
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "mine", owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	sessCtx := context.WithValue(proto.WithUserContext(ctx, owner), ssh.ContextKeyPublicKey, pk)

	// Key lookups now fail; the session's user was resolved before.
	breakTable(t, ctx, "public_keys")

	out, _, err := runRepoOutput(t, sessCtx, "list")
	is.NoErr(err)
	is.True(strings.Contains(out, "mine"))
}

// A repository lookup that fails is answered as a server fault, not as a
// missing repository; the cause goes to the server log, not the client.
func TestRepoCommandLookupFailure(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be, logs := newLookupTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	strangerCtx := withUser(t, ctx, be, "stranger", false)

	is.True(errors.Is(runRepo(t, strangerCtx, "info", "repo"), proto.ErrRepoNotFound))

	// Without the collaborators table, the stranger's level cannot be
	// worked out.
	breakTable(t, ctx, "collabs")
	err = runRepo(t, strangerCtx, "info", "repo")
	is.True(errors.Is(err, errLookupFailed))
	is.True(!strings.Contains(err.Error(), "collabs"))
	is.True(strings.Contains(logs.String(), "lookup failed"))
	is.True(strings.Contains(logs.String(), "collabs"))

	// With the repository table gone, the lookup fails for everyone, and
	// so does the list.
	breakTable(t, ctx, "repos")
	is.True(errors.Is(runRepo(t, ownerCtx, "info", "repo"), errLookupFailed))
	is.True(errors.Is(runRepo(t, ownerCtx, "list"), errLookupFailed))
}

// During an outage a missing name and an existing private one get the same
// answer, for anonymous and registered callers: otherwise anyone could tell
// which private names exist.
func TestRepoCommandOutageSameAnswer(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"repos", "settings"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			ctx, be, _ := newLookupTestContext(t)
			ownerCtx := withUser(t, ctx, be, "owner", false)
			_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Private: true})
			is.NoErr(err)
			strangerCtx := withUser(t, ctx, be, "stranger", false)

			breakTable(t, ctx, table)
			for _, c := range []context.Context{ctx, strangerCtx} {
				for _, name := range []string{"repo", "ghost"} {
					is.True(errors.Is(runRepo(t, c, "info", name), errLookupFailed))
				}
			}
		})
	}
}

// runGit runs the git service command c for args as ctx's caller, with no
// input, and returns its error.
func runGit(ctx context.Context, c func(*Commands) *cobra.Command, args ...string) error {
	cmd := c(cmdsOf(ctx))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}

// A failed lookup in a git service over SSH, clone, push or
// git-lfs-authenticate, is answered with the fixed malfunction message and
// logged once at Error level.
func TestGitServiceLookupFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cmd  func(*Commands) *cobra.Command
		args []string
	}{
		{"upload-pack", (*Commands).GitUploadPackCommand, []string{"repo"}},
		{"receive-pack", (*Commands).GitReceivePackCommand, []string{"repo"}},
		{"lfs-authenticate", (*Commands).GitLFSAuthenticateCommand, []string{"repo", "download"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			ctx, be, logs := newLookupTestContext(t)
			ownerCtx := withUser(t, ctx, be, "owner", false)
			_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Private: true})
			is.NoErr(err)
			strangerCtx := withUser(t, ctx, be, "stranger", false)

			callerCtx := strangerCtx
			if tc.name == "lfs-authenticate" {
				// git-lfs-authenticate looks the repository up twice: as
				// the session, then for the account alone. An admin key
				// on a non-admin account passes the first without a
				// collaborator lookup, so the second is the one that fails.
				pk, ak := slashFreeKey(t)
				cfg := cmdsOf(ctx).cfg
				cfg.InitialAdminKeys = []string{ak}
				is.NoErr(cfg.Validate())
				callerCtx = context.WithValue(strangerCtx, ssh.ContextKeyPublicKey, pk)
			}

			breakTable(t, ctx, "collabs")
			err = runGit(callerCtx, tc.cmd, tc.args...)
			is.True(errors.Is(err, git.ErrSystemMalfunction))
			is.Equal(logs.count("ERRO"), 1)
			is.True(strings.Contains(logs.String(), "failed to look up repository"))
		})
	}
}

// During an outage a git service gives a missing name and an existing
// private one the same answer.
func TestGitServiceOutageSameAnswer(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be, _ := newLookupTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	strangerCtx := withUser(t, ctx, be, "stranger", false)

	breakTable(t, ctx, "repos")
	for _, c := range []context.Context{ctx, strangerCtx} {
		for _, name := range []string{"repo", "ghost"} {
			is.True(errors.Is(runGit(c, (*Commands).GitUploadPackCommand, name), git.ErrSystemMalfunction))
		}
	}
}

// A lookup the client canceled by going away is logged at Debug, not as a
// server error, and still answered as a failed lookup.
func TestLookupFailedCanceledLoggedAtDebug(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	is.True(errors.Is(lookupFailed(ctx, logger, "look up repository", context.Canceled), errLookupFailed))
	is.True(errors.Is(gitLookupFailed(ctx, logger, "repo", context.Canceled), git.ErrSystemMalfunction))
	is.True(strings.Contains(buf.String(), "DEBU"))
	is.True(!strings.Contains(buf.String(), "ERRO"))

	buf.Reset()
	is.True(errors.Is(lookupFailed(context.Background(), logger, "look up repository", errors.New("db down")), errLookupFailed))
	is.True(errors.Is(gitLookupFailed(context.Background(), logger, "repo", errors.New("db down")), git.ErrSystemMalfunction))
	is.Equal(strings.Count(buf.String(), "ERRO"), 2)
}

// While the settings cannot be read, the admin's settings getters say the
// lookup failed rather than print the fail-closed default as if it were the
// stored value.
func TestSettingsGettersLookupFailure(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be, _ := newLookupTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	breakTable(t, ctx, "settings")
	for _, setting := range []string{"allow-keyless", "anon-access"} {
		c := cmdsOf(adminCtx).SettingsCommand()
		c.SilenceUsage = true
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(io.Discard)
		c.SetArgs([]string{setting})
		is.True(errors.Is(c.ExecuteContext(adminCtx), errLookupFailed))
		is.Equal(out.String(), "")
	}
}
