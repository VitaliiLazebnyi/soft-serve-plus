package web

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
)

// anonFailsAfterStore answers the first ok reads of the anon-access setting
// since reads was last reset, and fails every one after, as a database that
// goes down between the request's access check and the create would.
type anonFailsAfterStore struct {
	store.Store
	ok, reads *atomic.Int32
}

func (s anonFailsAfterStore) GetAnonAccess(ctx context.Context, h db.Handler) (access.AccessLevel, error) {
	if s.reads.Add(1) > s.ok.Load() {
		return access.NoAccess, errors.New("SQL logic error: no such table: settings")
	}
	return s.Store.GetAnonAccess(ctx, h)
}

// A push-to-create whose creation check cannot read the settings is
// answered 503 with Retry-After and no credentials challenge, as for any
// failed lookup, not 500, and nothing is created.
func TestPushCreateLookupFailureUnavailable(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource, db.Options{})
	is.NoErr(err)
	t.Cleanup(func() { _ = dbx.Close() })
	is.NoErr(migrate.Migrate(ctx, dbx, cfg, nil))
	ok, reads := &atomic.Int32{}, &atomic.Int32{}
	ok.Store(math.MaxInt32)
	st := anonFailsAfterStore{Store: database.New(), ok: ok, reads: reads}
	ctx = store.WithContext(db.WithContext(ctx, dbx), st)
	be := backend.New(cfg, dbx, st, log.FromContext(ctx))
	ctx = backend.WithContext(ctx, be)

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	token, err := be.CreateAccessToken(ctx, alice, "t", time.Now().Add(time.Hour))
	is.NoErr(err)
	router := testRouter(t, ctx)

	// The request's access check reads the setting once; the create's
	// check is the read that fails.
	reads.Store(0)
	ok.Store(1)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/new.git/info/refs?service=git-receive-pack", nil)
	req.Header.Set("Authorization", "Token "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	is.Equal(w.Code, http.StatusServiceUnavailable)
	is.Equal(w.Header().Get("Retry-After"), authRetryAfter)
	is.Equal(w.Header().Get("WWW-Authenticate"), "")

	ok.Store(math.MaxInt32)
	_, err = be.Repository(ctx, "new")
	is.True(errors.Is(err, proto.ErrRepoNotFound))
}
