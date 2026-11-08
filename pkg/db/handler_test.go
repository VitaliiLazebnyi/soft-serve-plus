package db

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
)

// TestHandlerMapsErrors checks that DB and Tx map driver errors with
// WrapError, so callers need not wrap them again.
func TestHandlerMapsErrors(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck
	if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err != nil {
		t.Fatal(err)
	}

	check := func(t *testing.T, h Handler) {
		t.Helper()
		if got := h.DriverName(); got != "sqlite" {
			t.Errorf("DriverName() = %q, want sqlite", got)
		}
		if _, err := h.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); !errors.Is(err, ErrDuplicateKey) {
			t.Errorf("ExecContext duplicate insert: err = %v, want ErrDuplicateKey", err)
		}
		var n int
		if err := h.GetContext(ctx, &n, "SELECT n FROM t WHERE n = 2"); !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("GetContext missing row: err = %v, want ErrRecordNotFound", err)
		}
		var ns []int
		if err := h.SelectContext(ctx, &ns, "SELECT n FROM t"); err != nil || len(ns) != 1 {
			t.Errorf("SelectContext = %v, %v; want [1], nil", ns, err)
		}
		func() {
			rows, err := h.QueryxContext(ctx, "SELECT n FROM missing")
			if err != nil {
				return
			}
			defer rows.Close() //nolint:errcheck
			t.Error("QueryxContext on a missing table: err = nil, want error")
		}()
	}

	t.Run("DB", func(t *testing.T) { check(t, d) })
	t.Run("Tx", func(t *testing.T) {
		// A failed statement does not abort a SQLite transaction, so all
		// checks can run in one.
		if err := d.TransactionContext(ctx, func(tx *Tx) error {
			check(t, tx)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// TestHandlerLogsConstraintDetail checks that a mapped constraint violation,
// whose message is ErrDuplicateKey's generic one, still has the driver error,
// naming the failed constraint, written to the debug log.
func TestHandlerLogsConstraintDetail(t *testing.T) {
	var buf bytes.Buffer
	l := log.New(&buf)
	l.SetLevel(log.DebugLevel)
	ctx := log.WithContext(context.Background(), l)
	d, err := Open(ctx, "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck
	if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	_, err = d.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)")
	if !errors.Is(err, ErrDuplicateKey) || err.Error() != ErrDuplicateKey.Error() {
		t.Fatalf("duplicate insert: err = %v, want ErrDuplicateKey's message", err)
	}
	if out := buf.String(); !strings.Contains(out, "constraint violation") || !strings.Contains(out, "t.n") {
		t.Errorf("debug log = %q, want the driver error naming t.n", out)
	}
}
