package db

import (
	"context"
	"database/sql"
)

// Handler is a database handler: a [DB] or a [Tx].
//
// Errors returned by SelectContext, GetContext and ExecContext are already
// mapped with [WrapError], so callers can test them with errors.Is against
// [ErrRecordNotFound], [ErrDuplicateKey] and [ErrForeignKey] without wrapping
// them again.
type Handler interface {
	// DriverName returns the name of the database driver, "sqlite" or
	// "postgres".
	DriverName() string
	Rebind(string) string

	SelectContext(context.Context, interface{}, string, ...interface{}) error
	GetContext(context.Context, interface{}, string, ...interface{}) error
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

var (
	_ Handler = (*DB)(nil)
	_ Handler = (*Tx)(nil)
)
