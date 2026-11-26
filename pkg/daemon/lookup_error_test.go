package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// newLookupTestDaemon returns a daemon with its own database, which the
// lookup tests break: Postgres when SOFT_SERVE_DB_DRIVER is "postgres",
// SQLite otherwise. It holds one public repository, "public". The log is
// written to the returned buffer at Debug level.
func newLookupTestDaemon(t *testing.T) (context.Context, *Server, *db.DB, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := log.New(logs)
	logger.SetLevel(log.DebugLevel)
	e := backendtest.New(t, backendtest.WithLogger(logger))
	ctx, cfg, dbx, be := e.Ctx, e.Cfg, e.DB, e.Backend
	if _, err := be.CreateRepository(ctx, "public", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	d, err := NewServer(cfg, be, logger)
	if err != nil {
		t.Fatal(err)
	}
	patient(d)
	t.Cleanup(func() { _ = d.Close() })
	return ctx, d, dbx, logs
}

// A repository lookup that fails is a server fault: the daemon answers with
// its fixed malfunction message and logs the cause, rather than answering
// as for a missing or private repository. A missing name gets the same
// answer as an existing one, so an outage does not tell which names exist,
// and so does every name when the keyless setting cannot be read.
func TestAuthorizeLookupFailure(t *testing.T) {
	for _, table := range []string{"repos", "settings"} {
		t.Run(table, func(t *testing.T) {
			ctx, d, dbx, logs := newLookupTestDaemon(t)
			public := gitRequest{service: gitserve.UploadPackService, name: "public"}
			if err := d.authorize(ctx, public); err != nil {
				t.Fatalf("authorize = %v, want nil", err)
			}
			if err := d.authorize(ctx, gitRequest{service: gitserve.UploadPackService, name: "ghost"}); !errors.Is(err, gitserve.ErrNotAuthed) {
				t.Fatalf("authorize(ghost) = %v, want %v", err, gitserve.ErrNotAuthed)
			}

			if _, err := dbx.ExecContext(ctx, "ALTER TABLE "+table+" RENAME TO "+table+"_gone"); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"public", "ghost"} {
				if err := d.authorize(ctx, gitRequest{service: gitserve.UploadPackService, name: name}); !errors.Is(err, gitserve.ErrSystemMalfunction) {
					t.Fatalf("authorize(%s) with the lookup failing = %v, want %v", name, err, gitserve.ErrSystemMalfunction)
				}
			}
			if !strings.Contains(logs.String(), "failed to look up repository") {
				t.Errorf("logs = %q, want the failed lookup", logs.String())
			}
		})
	}
}

// A lookup canceled because the client went away is logged at Debug, not
// as a server error.
func TestAuthorizeLookupCanceled(t *testing.T) {
	ctx, d, _, logs := newLookupTestDaemon(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.authorize(cctx, gitRequest{service: gitserve.UploadPackService, name: "public"}); !errors.Is(err, gitserve.ErrSystemMalfunction) {
		t.Fatalf("authorize with the client gone = %v, want %v", err, gitserve.ErrSystemMalfunction)
	}
	out := logs.String()
	if !strings.Contains(out, "repository lookup canceled") || strings.Contains(out, "ERRO") {
		t.Errorf("logs = %q, want the canceled lookup at Debug only", out)
	}
}
