package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

func openPoolTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// holdConns checks out n connections at once and then returns them all to
// the pool.
func holdConns(t *testing.T, d *DB, n int) {
	t.Helper()
	conns := make([]*sql.Conn, 0, n)
	for range n {
		c, err := d.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

func TestSetPoolLimitsZeroKeepsDefaults(t *testing.T) {
	d := openPoolTestDB(t)
	d.SetPoolLimits(config.DBConfig{})
	if got := d.Stats().MaxOpenConnections; got != 0 {
		t.Errorf("MaxOpenConnections = %d, want 0 (no limit)", got)
	}
	// database/sql keeps two idle connections by default. Calling
	// SetMaxIdleConns(0) would keep none.
	holdConns(t, d, 4)
	if got := d.Stats().Idle; got != 2 {
		t.Errorf("Idle = %d, want the database/sql default of 2", got)
	}
}

func TestSetPoolLimits(t *testing.T) {
	d := openPoolTestDB(t)
	d.SetPoolLimits(config.DBConfig{MaxOpenConns: 5})
	if got := d.Stats().MaxOpenConnections; got != 5 {
		t.Errorf("MaxOpenConnections = %d, want 5", got)
	}
	holdConns(t, d, 5)
	if got := d.Stats().Idle; got != 2 {
		t.Errorf("Idle = %d, want the database/sql default of 2", got)
	}
}
