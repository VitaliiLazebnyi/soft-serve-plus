package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// Migration versions must strictly increase in the order they run, and each
// name must be unique, since both are recorded and checked on every start.
func TestMigrationsOrdered(t *testing.T) {
	names := map[string]int64{}
	for i, m := range migrations {
		if m.Version <= 0 {
			t.Errorf("migration %q has version %d, want > 0", m.Name, m.Version)
		}
		if i > 0 && m.Version <= migrations[i-1].Version {
			t.Errorf("migration %d %q follows %d %q: versions must strictly increase", m.Version, m.Name, migrations[i-1].Version, migrations[i-1].Name)
		}
		if m.Name == "" {
			t.Errorf("migration %d has no name", m.Version)
		}
		if v, ok := names[m.Name]; ok {
			t.Errorf("migrations %d and %d share the name %q", v, m.Version, m.Name)
		}
		names[m.Name] = m.Version
		if m.Migrate == nil || m.Rollback == nil {
			t.Errorf("migration %d %q lacks a Migrate or Rollback function", m.Version, m.Name)
		}
	}
}

var sqlFileName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)_(sqlite|postgres)\.(up|down)\.sql$`)

// Every migration that is written in SQL has an up and a down file for both
// SQLite and Postgres, and every SQL file belongs to a known migration.
func TestMigrationSQLFilesComplete(t *testing.T) {
	files, err := fs.Glob(sqls, "*.sql")
	if err != nil {
		t.Fatal(err)
	}

	known := map[string]Migration{}
	for _, m := range migrations {
		known[fmt.Sprintf("%04d_%s", m.Version, toSnakeCase(m.Name))] = m
	}

	have := map[string]map[string]bool{}
	for _, f := range files {
		parts := sqlFileName.FindStringSubmatch(f)
		if parts == nil {
			t.Errorf("%s: name does not match NNNN_name_{sqlite,postgres}.{up,down}.sql", f)
			continue
		}
		prefix := parts[1] + "_" + parts[2]
		if _, ok := known[prefix]; !ok {
			v, _ := strconv.Atoi(parts[1])
			t.Errorf("%s: no migration %d named %q", f, v, parts[2])
			continue
		}
		if have[prefix] == nil {
			have[prefix] = map[string]bool{}
		}
		have[prefix][parts[3]+"."+parts[4]] = true
	}

	if len(have) == 0 {
		t.Fatal("found no SQL migrations")
	}
	for prefix, got := range have {
		for _, want := range []string{"sqlite.up", "sqlite.down", "postgres.up", "postgres.down"} {
			if !got[want] {
				t.Errorf("migration %s has SQL files but no %s_%s.sql", prefix, prefix, want)
			}
		}
	}
}

func migratedDB(t *testing.T) (context.Context, *db.DB) {
	t.Helper()
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}
	return ctx, dbx
}

// A database whose recorded migration has another name under the same
// version, such as one migrated by a build whose numbering collides with this
// one's, is refused by Migrate and Rollback, and left alone.
func TestMigrateRefusesRenamedMigration(t *testing.T) {
	ctx, dbx := migratedDB(t)
	if len(migrations) < 2 {
		t.Skip("needs two migrations")
	}
	m := migrations[len(migrations)-2]
	if _, err := dbx.ExecContext(ctx, dbx.Rebind("UPDATE migrations SET name = ? WHERE version = ?"), "someone_elses_migration", m.Version); err != nil {
		t.Fatal(err)
	}

	for name, run := range map[string]func(context.Context, *db.DB) error{"Migrate": Migrate, "Rollback": Rollback} {
		err := run(ctx, dbx)
		if err == nil || !strings.Contains(err.Error(), `"someone_elses_migration"`) || !strings.Contains(err.Error(), fmt.Sprintf("%q", m.Name)) {
			t.Errorf("%s() => %v, want an error naming both migrations", name, err)
		}
	}
	assertAppliedCount(t, ctx, dbx, len(migrations))
}

// A recorded migration this binary does not know, below its latest version,
// is refused.
func TestMigrateRefusesUnknownMigration(t *testing.T) {
	ctx, dbx := migratedDB(t)
	if len(migrations) < 3 {
		t.Skip("needs three migrations")
	}

	// Simulate a binary that lacks one of the applied migrations.
	all := migrations
	t.Cleanup(func() { migrations = all })
	gone := all[len(all)-2]
	migrations = append(append([]Migration{}, all[:len(all)-2]...), all[len(all)-1])

	for name, run := range map[string]func(context.Context, *db.DB) error{"Migrate": Migrate, "Rollback": Rollback} {
		err := run(ctx, dbx)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("migration %d %q, which is unknown", gone.Version, gone.Name)) {
			t.Errorf("%s() => %v, want an unknown migration error", name, err)
		}
	}
	assertAppliedCount(t, ctx, dbx, len(all))
}

// A database that lacks a migration older than its latest one is refused:
// Migrate would otherwise never run it.
func TestMigrateRefusesMissingMigration(t *testing.T) {
	ctx, dbx := migratedDB(t)
	if len(migrations) < 2 {
		t.Skip("needs two migrations")
	}
	m := migrations[len(migrations)-2]
	if _, err := dbx.ExecContext(ctx, dbx.Rebind("DELETE FROM migrations WHERE version = ?"), m.Version); err != nil {
		t.Fatal(err)
	}

	err := Migrate(ctx, dbx)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("missing migration %d %q", m.Version, m.Name)) {
		t.Errorf("Migrate() => %v, want a missing migration error", err)
	}
	assertAppliedCount(t, ctx, dbx, len(migrations)-1)
}

// A database migrated in full by this binary is accepted again, and a rollback
// followed by a migrate brings it back.
func TestMigrateAcceptsOwnHistory(t *testing.T) {
	ctx, dbx := migratedDB(t)
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("second Migrate() => %v, want nil error", err)
	}
	if err := Rollback(ctx, dbx); err != nil {
		t.Fatalf("Rollback() => %v, want nil error", err)
	}
	assertAppliedCount(t, ctx, dbx, len(migrations)-1)
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() after Rollback() => %v, want nil error", err)
	}
	assertAppliedCount(t, ctx, dbx, len(migrations))
}

func assertAppliedCount(t *testing.T, ctx context.Context, dbx *db.DB, want int) {
	t.Helper()
	var n int
	if err := dbx.GetContext(ctx, &n, "SELECT COUNT(*) FROM migrations"); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Errorf("migrations recorded = %d, want %d", n, want)
	}
}
