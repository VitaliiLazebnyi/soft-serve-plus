package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/lib/pq"
)

func TestWrapErrorBadNoRows(t *testing.T) {
	for _, e := range []error{
		fmt.Errorf("foo"),
		errors.New("bar"),
	} {
		if err := WrapError(e); err != e {
			t.Errorf("WrapError(%v) => %v, want %v", e, err, e)
		}
	}
}

func TestWrapErrorGoodNoRows(t *testing.T) {
	if err := WrapError(sql.ErrNoRows); err != ErrRecordNotFound {
		t.Errorf("WrapError(sql.ErrNoRows) => %v, want %v", err, ErrRecordNotFound)
	}
}

// TestWrapErrorSqliteConstraints checks that each kind of SQLite constraint
// violation maps to its own error, also when the driver error is wrapped.
func TestWrapErrorSqliteConstraints(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=foreign_keys(1)"
	dbx, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := dbx.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, q := range []string{
		`CREATE TABLE parents (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE children (
			id INTEGER PRIMARY KEY,
			parent_id INTEGER NOT NULL REFERENCES parents (id),
			name TEXT UNIQUE,
			age INTEGER CHECK (age >= 0)
		)`,
		`INSERT INTO parents (id) VALUES (1)`,
		`INSERT INTO children (id, parent_id, name, age) VALUES (1, 1, 'a', 1)`,
	} {
		if _, err := dbx.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	for _, tc := range []struct {
		name  string
		query string
		want  error
	}{
		{"primary key", `INSERT INTO parents (id) VALUES (1)`, ErrDuplicateKey},
		{"unique", `INSERT INTO children (parent_id, name) VALUES (1, 'a')`, ErrDuplicateKey},
		{"foreign key", `INSERT INTO children (parent_id, name) VALUES (2, 'b')`, ErrForeignKey},
		{"foreign key delete", `DELETE FROM parents WHERE id = 1`, ErrForeignKey},
		{"not null", `INSERT INTO children (parent_id, name) VALUES (NULL, 'c')`, ErrNotNull},
		{"check", `INSERT INTO children (parent_id, name, age) VALUES (1, 'd', -1)`, ErrCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := dbx.ExecContext(ctx, tc.query)
			if err == nil {
				t.Fatal("no error")
			}
			if got := WrapError(err); got != tc.want {
				t.Errorf("WrapError(%v) => %v, want %v", err, got, tc.want)
			}
			wrapped := fmt.Errorf("insert: %w", err)
			if got := WrapError(wrapped); got != tc.want {
				t.Errorf("WrapError(%v) => %v, want %v", wrapped, got, tc.want)
			}
		})
	}
}

// TestWrapErrorPostgresConstraints checks the mapping of Postgres constraint
// violation codes, also when the driver error is wrapped.
func TestWrapErrorPostgresConstraints(t *testing.T) {
	for _, tc := range []struct {
		err  *pq.Error
		want error
	}{
		{&pq.Error{Code: "23505"}, ErrDuplicateKey},
		{&pq.Error{Code: "23503"}, ErrForeignKey},
		{&pq.Error{Code: "23502"}, ErrNotNull},
		{&pq.Error{Code: "23514"}, ErrCheck},
	} {
		if got := WrapError(tc.err); got != tc.want {
			t.Errorf("WrapError(%s) => %v, want %v", tc.err.Code, got, tc.want)
		}
		wrapped := fmt.Errorf("insert: %w", tc.err)
		if got := WrapError(wrapped); got != tc.want {
			t.Errorf("WrapError(wrapped %s) => %v, want %v", tc.err.Code, got, tc.want)
		}
	}

	// Other errors are returned as they are.
	err := &pq.Error{Code: "42P01"} // undefined_table
	if got := WrapError(err); got != err {
		t.Errorf("WrapError(42P01) => %v, want %v", got, err)
	}
}
