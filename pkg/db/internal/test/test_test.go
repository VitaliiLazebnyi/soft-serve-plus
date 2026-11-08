package test

import (
	"context"
	"testing"

	"github.com/lib/pq"
)

func TestWithDatabase(t *testing.T) {
	for _, dsn := range []string{
		"postgres://postgres@localhost:5432/postgres?sslmode=disable",
		"postgresql://postgres:secret@localhost/other",
		"POSTGRES://postgres@localhost/postgres",
		"PostgreSQL://postgres@localhost/postgres?sslmode=disable",
		"postgres://postgres@localhost/postgres?dbname=zzz&sslmode=disable",
		"postgres://postgres@localhost?dbname=zzz",
		"host=localhost user=postgres dbname=postgres sslmode=disable",
		"host=localhost user=postgres sslmode=disable ",
	} {
		got, err := withDatabase(dsn, "softserve_test_1")
		if err != nil {
			t.Fatalf("withDatabase(%q) => %v", dsn, err)
		}
		cfg, err := pq.NewConfig(got)
		if err != nil {
			t.Fatalf("withDatabase(%q) = %q: %v", dsn, got, err)
		}
		if cfg.Database != "softserve_test_1" || cfg.Host != "localhost" || cfg.User != "postgres" {
			t.Errorf("withDatabase(%q) = %q: database %q, host %q, user %q", dsn, got, cfg.Database, cfg.Host, cfg.User)
		}
	}
}

// The test database enforces foreign keys and waits on a busy database, as
// the server's does, so migrations and queries are tested as they run.
func TestOpenSqlitePragmas(t *testing.T) {
	ctx := context.Background()
	dbx, err := OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pragma string
		want   int
	}{
		{"foreign_keys", 1},
		{"busy_timeout", 5000},
	} {
		var got int
		if err := dbx.GetContext(ctx, &got, "PRAGMA "+tc.pragma); err != nil {
			t.Fatalf("PRAGMA %s: %v", tc.pragma, err)
		}
		if got != tc.want {
			t.Errorf("PRAGMA %s = %d, want %d", tc.pragma, got, tc.want)
		}
	}
}
