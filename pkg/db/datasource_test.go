package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// TestOpenResolvedSQLiteDataSource opens the data sources Validate resolves
// and checks that each database ends up where it should: a relative path or
// a relative "file:" URI in the data directory, and an in-memory database
// nowhere on disk.
func TestOpenResolvedSQLiteDataSource(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		dsn  string
		file string // expected database file in the data directory, or ""
	}{
		{"plain.db?_pragma=foreign_keys(1)", "plain.db"},
		{"file:uri.db?_pragma=foreign_keys(1)", "uri.db"},
		{":memory:?_pragma=foreign_keys(1)", ""},
		{"file::memory:?_pragma=foreign_keys(1)", ""},
	} {
		t.Run(c.dsn, func(t *testing.T) {
			// A space and a '#' in the data path must survive a URI.
			dp := filepath.Join(t.TempDir(), "data dir#1")
			if err := os.MkdirAll(dp, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := config.DefaultConfig()
			cfg.DataPath = dp
			cfg.DB.DataSource = c.dsn
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}

			d, err := Open(ctx, "sqlite", cfg.DB.DataSource)
			if err != nil {
				t.Fatalf("open %q: %v", cfg.DB.DataSource, err)
			}
			defer d.Close() //nolint:errcheck
			if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER)"); err != nil {
				t.Fatal(err)
			}

			entries, err := os.ReadDir(dp)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if c.file == "" {
				if len(names) != 0 {
					t.Errorf("data directory holds %q, want nothing for an in-memory database", names)
				}
				return
			}
			if _, err := os.Stat(filepath.Join(dp, c.file)); err != nil {
				t.Errorf("database not at %s (data directory holds %q): %v", c.file, names, err)
			}
		})
	}
}
