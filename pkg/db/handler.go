package db

import (
	"context"
	"database/sql"

	"github.com/jmoiron/sqlx"
)

// Handler is a database handler: a [DB] or a [Tx].
//
// Errors returned by SelectContext, GetContext, QueryxContext and
// ExecContext are already mapped with [WrapError], so callers can test them
// with errors.Is against [ErrRecordNotFound] and [ErrDuplicateKey] without
// wrapping them again. Errors that surface later, from scanning the
// *sqlx.Row of QueryRowxContext or iterating the *sqlx.Rows of
// QueryxContext, are not mapped.
type Handler interface {
	// DriverName returns the name of the database driver, "sqlite" or
	// "postgres".
	DriverName() string
	Rebind(string) string

	SelectContext(context.Context, interface{}, string, ...interface{}) error
	GetContext(context.Context, interface{}, string, ...interface{}) error
	QueryxContext(context.Context, string, ...interface{}) (*sqlx.Rows, error)
	QueryRowxContext(context.Context, string, ...interface{}) *sqlx.Row
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

var (
	_ Handler = (*DB)(nil)
	_ Handler = (*Tx)(nil)
)
