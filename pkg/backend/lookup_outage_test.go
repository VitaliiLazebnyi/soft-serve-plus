package backend

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	charmlog "charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
)

// newLookupBackend is newTestBackendWithLog on the test database dbtest
// opens: Postgres when SOFT_SERVE_DB_DRIVER is "postgres", SQLite otherwise.
// The lookup tests break its tables.
func newLookupBackend(t *testing.T, logs io.Writer) *Backend {
	t.Helper()
	ctx := context.Background()
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = dbx.DriverName()
	ctx = config.WithContext(ctx, cfg)
	if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	var logger *charmlog.Logger
	if logs != nil {
		logger = charmlog.New(logs)
		logger.SetLevel(charmlog.DebugLevel)
	}
	return New(cfg, dbx, database.New(), logger)
}

// During an outage a missing name gets the same answer as an existing
// private one: an error, for anonymous and registered callers alike. An
// answer of "no access" for the missing name only would tell anyone which
// private names exist.
func TestOutageAnswersMissingAndExistingAlike(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"repos", "settings"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			be := newLookupBackend(t, nil)
			ctx := context.Background()
			_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
			is.NoErr(err)
			_, err = be.CreateRepository(ctx, "public", defaultAdmin(t, be), proto.RepositoryOptions{})
			is.NoErr(err)
			stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
			is.NoErr(err)
			collab, err := be.CreateUser(ctx, "collab", proto.UserOptions{})
			is.NoErr(err)
			is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
				return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "private", access.ReadOnlyAccess)
			}))

			breakTable(t, be, table)
			for _, user := range []proto.User{nil, stranger, collab} {
				for _, name := range []string{"private", "public", "ghost"} {
					r, level, err := be.Readable(ctx, name, user)
					is.True(err != nil)
					is.True(r == nil)
					is.Equal(level, access.NoAccess)
					_, _, err = be.ReadableForUser(ctx, name, user)
					is.True(err != nil)
				}
			}
		})
	}
}

// A settings lookup that fails is returned by the access checks rather than
// read as "no anonymous access": that would refuse an anonymous reader of a
// public repository, cap a registered user at read-only where anon-access
// gives read-write, and list nothing for an anonymous caller, all with no
// error. The wrappers other callers use still fail closed.
func TestSettingsLookupErrorsReturned(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs syncBuffer
	be := newLookupBackend(t, &logs)
	ctx := context.Background()
	_, err := be.CreateRepository(ctx, "public", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(be.SetAnonAccess(ctx, access.ReadWriteAccess))
	is.NoErr(be.SetAllowKeyless(ctx, true))
	is.Equal(levelOf(t, be, ctx, "public", alice), access.ReadWriteAccess)

	breakTable(t, be, "settings")
	for _, user := range []proto.User{nil, alice} {
		_, _, err := be.Readable(ctx, "public", user)
		is.True(err != nil)
	}
	_, err = be.ReadableRepositories(ctx, nil)
	is.True(err != nil)
	_, err = be.AllowKeylessSetting(ctx)
	is.True(err != nil)

	is.Equal(be.AnonAccess(ctx), access.NoAccess)
	is.True(!be.AllowKeyless(ctx))
	is.True(strings.Contains(logs.String(), "ERRO"))
}

// The fail-closed settings wrappers log a lookup the client canceled at
// Debug, not as a server error.
func TestSettingsLookupCanceledLoggedAtDebug(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs syncBuffer
	be := newLookupBackend(t, &logs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	is.Equal(be.AnonAccess(ctx), access.NoAccess)
	is.True(!be.AllowKeyless(ctx))
	is.True(strings.Contains(logs.String(), "anon-access"))
	is.True(strings.Contains(logs.String(), "allow-keyless"))
	is.True(!strings.Contains(logs.String(), "ERRO"))
}

// A failed owner lookup keeps the about repository's readme hidden, as
// before, and is now logged.
func TestAboutRepoOwnerLookupFailureLogged(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs syncBuffer
	be := newLookupBackend(t, &logs)
	ctx := context.Background()
	r, err := be.CreateRepository(ctx, AboutRepo, defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	is.True(be.AboutRepoOwnedByAdmin(ctx, r))

	breakTable(t, be, "users")
	is.True(!be.AboutRepoOwnedByAdmin(ctx, r))
	is.True(strings.Contains(logs.String(), "about repository's owner"))
}

// When the creation rule cannot be worked out because the anon-access
// setting cannot be read, create, import and rename onto a new name fail
// with the lookup error, marked as proto.ErrLookupFailed, not with a
// refusal, and nothing is created.
func TestCheckCanCreateReturnsLookupError(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be := newLookupBackend(t, nil)
	ctx := context.Background()
	_, err := be.CreateRepository(ctx, "a", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	is.NoErr(err)
	strangerCtx := proto.WithUserContext(ctx, stranger)

	breakTable(t, be, "settings")
	_, cerr := be.CreateRepository(strangerCtx, "new", stranger, proto.RepositoryOptions{})
	_, ierr := be.ImportRepository(strangerCtx, "new", stranger, offlineRemote, proto.RepositoryOptions{})
	rerr := be.RenameRepository(strangerCtx, "a", "new")
	for _, err := range []error{cerr, ierr, rerr} {
		is.True(err != nil)
		is.True(!errors.Is(err, proto.ErrUnauthorized))
		is.True(errors.Is(err, proto.ErrLookupFailed))
		is.True(strings.Contains(err.Error(), "anon-access")) // the cause is kept
	}
	is.True(!be.repoPathExists("new"))
}
