package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// While the database is down, credentials cannot be checked. A request that
// carries them is answered 503 with Retry-After and no credentials
// challenge, not as an anonymous request (401 with a challenge, after which
// git discards the stored credential) nor as a failed login. The fault is
// logged at error level.
// Like TestAuthDoesNotLogCredentials, this swaps the global default logger
// and must not run in parallel.
func TestAuthDatabaseErrorIsUnavailable(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)
	prev := log.Default()
	log.SetDefault(logger)
	t.Cleanup(func() { log.SetDefault(prev) })

	f := newOracleFixture(t)
	stranger, err := f.be.User(f.ctx, "stranger")
	is.NoErr(err)
	_, err = f.be.CreateRepository(f.ctx, "mine", stranger, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	bearer := "Bearer " + mintJWT(f.ctx, t, stranger, "mine")
	basic := func(user, pass string) string {
		r := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}

	is.NoErr(db.FromContext(f.ctx).Close())

	creds := map[string]string{
		"token":             "Token " + f.token,
		"bearer":            bearer,
		"basic token":       basic("stranger", f.token),
		"basic token as id": basic(f.token, "x-oauth-basic"),
	}
	for name, auth := range creds {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{
				"/mine.git/info/refs?service=git-upload-pack",
				"/mine.git/info/refs?service=git-receive-pack",
				"/mine?go-get=1",
			} {
				logs.Reset()
				w := f.do(http.MethodGet, path, auth)
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s: got %d, want 503", path, w.Code)
				}
				if w.Header().Get("Retry-After") == "" {
					t.Errorf("%s: no Retry-After", path)
				}
				if h := w.Header().Get("WWW-Authenticate"); h != "" {
					t.Errorf("%s: credentials challenge %q", path, h)
				}
				if !strings.Contains(logs.String(), "ERRO") {
					t.Errorf("%s: database error not logged as an error:\n%s", path, logs.String())
				}
			}

			// LFS gets the same answer as JSON.
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mine.git/info/lfs/objects/batch",
				strings.NewReader(`{"operation":"download","objects":[]}`))
			req.Header.Set("Authorization", auth)
			req.Header.Set("Accept", lfs.MediaType)
			req.Header.Set("Content-Type", lfs.MediaType)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("lfs: got %d, want 503", w.Code)
			}
			is := is.New(t)
			is.True(w.Header().Get("Retry-After") != "")
			is.Equal(w.Header().Get("LFS-Authenticate"), "")
			is.Equal(w.Header().Get("WWW-Authenticate"), "")
			is.Equal(w.Header().Get("Content-Type"), lfs.MediaType)
			var body lfs.ErrorResponse
			is.NoErr(json.Unmarshal(w.Body.Bytes(), &body))
			is.True(body.Message != "")
		})
	}
}

// Only a failed lookup is a server fault: a missing user or an unknown or
// expired token is a failed login.
func TestLookupFailed(t *testing.T) {
	is := is.New(t)
	is.True(!lookupFailed(nil))
	is.True(!lookupFailed(proto.ErrUserNotFound))
	is.True(!lookupFailed(proto.ErrTokenExpired))
	is.True(lookupFailed(errors.New("sql: database is closed")))
	is.True(lookupFailed(context.DeadlineExceeded))
}

// A lookup the client canceled by going away is not logged as a server
// error, though it is still answered as unavailable.
func TestAuthUnavailableCanceled(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	cancel()
	err := authUnavailable(ctx, logger, context.Canceled)
	is.True(errors.Is(err, errAuthUnavailable))
	is.True(errors.Is(err, context.Canceled))
	is.True(!strings.Contains(logs.String(), "ERRO"))

	logs.Reset()
	err = authUnavailable(context.Background(), logger, errors.New("sql: database is closed"))
	is.True(errors.Is(err, errAuthUnavailable))
	is.True(strings.Contains(logs.String(), "ERRO"))
}
