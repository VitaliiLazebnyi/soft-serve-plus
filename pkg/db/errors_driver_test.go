package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
)

// WrapError must map the constraint violations the stores rely on (unique,
// primary key, foreign key, check) to ErrDuplicateKey, from the driver that
// actually raised them, and leave other errors alone.
func TestWrapErrorConstraintViolations(t *testing.T) {
	ctx := context.Background()
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	// A single connection, so the SQLite foreign_keys pragma applies to
	// every statement below.
	conn, err := dbx.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	setup := []string{
		`CREATE TABLE parents (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE children (
			id INTEGER PRIMARY KEY,
			parent_id INTEGER NOT NULL REFERENCES parents (id),
			n INTEGER NOT NULL CHECK (n > 0)
		)`,
		`INSERT INTO parents (id, name) VALUES (1, 'one')`,
	}
	if dbx.DriverName() == "sqlite" {
		setup = append([]string{"PRAGMA foreign_keys = ON"}, setup...)
	}
	for _, q := range setup {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	for _, tc := range []struct {
		name  string
		query string
		want  error
	}{
		{"unique", `INSERT INTO parents (id, name) VALUES (2, 'one')`, db.ErrDuplicateKey},
		{"primary key", `INSERT INTO parents (id, name) VALUES (1, 'two')`, db.ErrDuplicateKey},
		{"foreign key", `INSERT INTO children (id, parent_id, n) VALUES (1, 42, 1)`, db.ErrDuplicateKey},
		// SQLite reports CHECK failures as SQLITE_CONSTRAINT_CHECK, which
		// WrapError does not map; Postgres maps 23514.
		{"check", `INSERT INTO children (id, parent_id, n) VALUES (1, 1, 0)`, nil},
		{"not null", `INSERT INTO parents (id) VALUES (3)`, nil},
		{"syntax", `INSERT INTO nowhere VALUES (1)`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := conn.ExecContext(ctx, tc.query)
			if err == nil {
				t.Fatalf("%s: nil error, want a constraint violation", tc.query)
			}
			want := tc.want
			if tc.name == "check" && dbx.DriverName() == "postgres" {
				want = db.ErrDuplicateKey
			}
			got := db.WrapError(err)
			if want != nil {
				if !errors.Is(got, want) {
					t.Errorf("WrapError(%v) = %v, want %v", err, got, want)
				}
				return
			}
			if got != err { //nolint:errorlint
				t.Errorf("WrapError(%v) = %v, want the error unchanged", err, got)
			}
		})
	}
}

// The Postgres codes WrapError maps, and one it does not.
func TestWrapErrorPostgresCodes(t *testing.T) {
	for code, want := range map[pqerror.Code]bool{
		pqerror.UniqueViolation:     true,
		pqerror.ForeignKeyViolation: true,
		pqerror.CheckViolation:      true,
		pqerror.NotNullViolation:    false,
		pqerror.UndefinedTable:      false,
	} {
		err := &pq.Error{Code: code}
		got := db.WrapError(err)
		if want && !errors.Is(got, db.ErrDuplicateKey) {
			t.Errorf("WrapError(pq code %s) = %v, want ErrDuplicateKey", code, got)
		}
		if !want && got != error(err) { //nolint:errorlint
			t.Errorf("WrapError(pq code %s) = %v, want the error unchanged", code, got)
		}
	}
}

func TestWrapErrorNil(t *testing.T) {
	if err := db.WrapError(nil); err != nil {
		t.Errorf("WrapError(nil) = %v, want nil", err)
	}
}
