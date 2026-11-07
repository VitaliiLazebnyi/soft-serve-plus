package ssh

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// newTestUIContext returns a context holding a config, store and backend
// backed by a fresh database, for building a UI.
func newTestUIContext(t *testing.T) context.Context {
	t.Helper()
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

	ctx := config.WithContext(context.Background(), cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	return backend.WithContext(ctx, backend.New(ctx, cfg, dbx, dbstore))
}

// The subtests run one at a time: building the UI pages writes the shared
// style config.
func TestUIErrorScreenHidesInternalErrors(t *testing.T) {
	const leak = "/var/lib/soft-serve/repos/secret.git"
	cases := []struct {
		name    string
		err     error
		want    string
		notWant string
		logged  bool
	}{
		{
			name:    "internal error",
			err:     errors.New("open repository: detected dubious ownership in repository at '" + leak + "'"),
			want:    "something went wrong",
			notWant: leak,
			logged:  true,
		},
		{
			name: "user error",
			err:  common.ErrMissingRepo,
			want: "missing repo",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := log.WithContext(newTestUIContext(t), log.New(&buf))
			ui := NewUI(common.NewCommon(ctx, 120, 40), "")
			t.Cleanup(ui.common.Zone.Close)
			ui.Init()

			ui.Update(common.ErrorMsg(c.err))
			view := ui.View().Content
			if !strings.Contains(view, c.want) {
				t.Errorf("error screen does not show %q:\n%s", c.want, view)
			}
			if c.notWant != "" && strings.Contains(view, c.notWant) {
				t.Errorf("error screen shows %q:\n%s", c.notWant, view)
			}
			if got := strings.Contains(buf.String(), c.err.Error()); got != c.logged {
				t.Errorf("error logged = %v, want %v; log:\n%s", got, c.logged, buf.String())
			}
		})
	}
}
