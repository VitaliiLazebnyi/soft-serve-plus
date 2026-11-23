package cmd

import (
	"bytes"
	"context"
	"io"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
)

// commandsKey is the context key of the Commands a test's commands are
// built with. The server passes its Commands explicitly; only these tests
// carry them on the context, beside the session's user, so that each
// context a test derives runs its commands with the same server.
type commandsKey struct{}

// withCommands returns ctx carrying c (see commandsKey).
func withCommands(ctx context.Context, c *Commands) context.Context {
	return context.WithValue(ctx, commandsKey{}, c)
}

// cmdsOf returns the Commands ctx carries. A test context always carries
// them.
func cmdsOf(ctx context.Context) *Commands {
	return ctx.Value(commandsKey{}).(*Commands)
}

// withTestBackend returns ctx whose commands use be.
func withTestBackend(ctx context.Context, be *backend.Backend) context.Context {
	c := *cmdsOf(ctx)
	c.be = be
	return withCommands(ctx, &c)
}

// withTestStore returns ctx whose commands use st, which they hand to
// git-lfs-transfer.
func withTestStore(ctx context.Context, st store.Store) context.Context {
	c := *cmdsOf(ctx)
	c.store = st
	return withCommands(ctx, &c)
}

// withTestLogger returns ctx whose commands log to logger.
func withTestLogger(ctx context.Context, logger *log.Logger) context.Context {
	c := *cmdsOf(ctx)
	c.logger = logger
	return withCommands(ctx, &c)
}

// newAuthTestContext returns a context carrying the Commands of a server
// with a config and a real migrated backend on the test database dbtest
// opens (Postgres when SOFT_SERVE_DB_DRIVER is "postgres", SQLite
// otherwise), plus the backend itself so tests can set up users and
// repositories.
//
// No user is attached to the returned context; use withUser to authenticate
// as somebody. AdminOnlyCreate is left at its default (off); use
// newAuthTestContextWith to turn it on.
func newAuthTestContext(t *testing.T) (context.Context, *backend.Backend) {
	t.Helper()
	return newAuthTestContextWith(t, nil)
}

// newAuthTestContextWith is newAuthTestContext with a hook to adjust the
// config before the backend is built, for example to set a flag.
func newAuthTestContextWith(t *testing.T, tweak func(*config.Config)) (context.Context, *backend.Backend) {
	t.Helper()
	return newTestCommands(t, tweak, nil, nil)
}

// newTestCommands is newAuthTestContextWith with the backend's store
// wrapped by wrap and the commands logging to logger, each when not nil.
// The Commands have no token signer; a test of jwt sets one.
func newTestCommands(t *testing.T, tweak func(*config.Config), wrap func(store.Store) store.Store, logger *log.Logger) (context.Context, *backend.Backend) {
	t.Helper()
	is := is.New(t)
	ctx := context.Background()

	dbx, err := dbtest.Open(ctx, t)
	is.NoErr(err)

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = dbx.DriverName()
	if tweak != nil {
		tweak(cfg)
	}
	// Validate parses the admin keys a tweak may list.
	is.NoErr(cfg.Validate())

	is.NoErr(migrate.Migrate(ctx, dbx, cfg, nil))
	var st store.Store = database.New(nil)
	if wrap != nil {
		st = wrap(st)
	}
	if logger == nil {
		logger = log.New(io.Discard)
	}
	be := backend.New(cfg, dbx, st, logger)
	cmds, err := New(Deps{Config: cfg, Backend: be, DB: dbx, Store: st, Logger: logger})
	is.NoErr(err)

	return withCommands(ctx, cmds), be
}

// levelOf is the access level Backend.Readable gives user's account on repo.
func levelOf(t *testing.T, be *backend.Backend, ctx context.Context, repo string, user proto.User) access.AccessLevel {
	t.Helper()
	_, level, err := be.Readable(ctx, repo, user)
	if err != nil {
		t.Fatal(err)
	}
	return level
}

// collabOf is Backend.IsCollaborator for the repository and user with these
// names.
func collabOf(t *testing.T, be *backend.Backend, ctx context.Context, repo, username string) (access.AccessLevel, bool, error) {
	t.Helper()
	u, err := be.User(ctx, username)
	if err != nil {
		t.Fatal(err)
	}
	r, err := be.Repository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	return be.IsCollaborator(ctx, r, u)
}

// withUser creates a user and returns a context authenticated as them.
func withUser(t *testing.T, ctx context.Context, be *backend.Backend, username string, admin bool) context.Context {
	t.Helper()
	is := is.New(t)
	user, err := be.CreateUser(ctx, username, proto.UserOptions{Admin: admin})
	is.NoErr(err)
	return proto.WithUserContext(ctx, user)
}

// runRepo runs a `repo` subcommand and returns only its error.
func runRepo(t *testing.T, ctx context.Context, args ...string) error {
	t.Helper()
	_, _, err := runRepoOutput(t, ctx, args...)
	return err
}

// runRepoOutput runs a `repo` subcommand and returns its stdout and stderr
// alongside the error, so tests can assert that sensitive values were not
// printed even when a command fails.
func runRepoOutput(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	c := cmdsOf(ctx).RepoCommand()
	// The SSH root command silences usage, so errors are the only output.
	c.SilenceUsage = true
	var outBuf, errBuf bytes.Buffer
	c.SetOut(&outBuf)
	c.SetErr(&errBuf)
	c.SetArgs(args)
	err = c.ExecuteContext(ctx)
	return outBuf.String(), errBuf.String(), err
}

// runUser runs a `user` subcommand and returns only its error.
func runUser(t *testing.T, ctx context.Context, args ...string) error {
	t.Helper()
	c := cmdsOf(ctx).UserCommand()
	var outBuf, errBuf bytes.Buffer
	c.SetOut(&outBuf)
	c.SetErr(&errBuf)
	c.SetArgs(args)
	return c.ExecuteContext(ctx)
}
