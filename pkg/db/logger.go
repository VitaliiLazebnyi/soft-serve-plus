package db

import (
	"context"
	"database/sql"
	"strings"

	"charm.land/log/v2"
	"github.com/jmoiron/sqlx"
)

func trace(l *log.Logger, query string, args ...interface{}) {
	if l != nil {
		// Remove newlines and tabs
		query = strings.ReplaceAll(query, "\t", "")
		query = strings.TrimSpace(query)
		l.Debug("trace", "query", query, "args", args)
	}
}

// SelectContext wraps sqlx.SelectContext: it logs the query and
// arguments and maps the error with WrapError.
func (d *DB) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(d.logger, query, args...)
	return WrapError(d.DB.SelectContext(ctx, dest, query, args...))
}

// GetContext wraps sqlx.GetContext: it logs the query and
// arguments and maps the error with WrapError.
func (d *DB) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(d.logger, query, args...)
	return WrapError(d.DB.GetContext(ctx, dest, query, args...))
}

// QueryxContext wraps sqlx.QueryxContext: it logs the query and
// arguments and maps the error with WrapError.
func (d *DB) QueryxContext(ctx context.Context, query string, args ...interface{}) (*sqlx.Rows, error) {
	trace(d.logger, query, args...)
	rows, err := d.DB.QueryxContext(ctx, query, args...)
	return rows, WrapError(err)
}

// QueryRowxContext is a wrapper around sqlx.QueryRowxContext that logs the
// query and arguments. The error from scanning the row is not mapped.
func (d *DB) QueryRowxContext(ctx context.Context, query string, args ...interface{}) *sqlx.Row {
	trace(d.logger, query, args...)
	return d.DB.QueryRowxContext(ctx, query, args...)
}

// ExecContext wraps sqlx.ExecContext: it logs the query and
// arguments and maps the error with WrapError.
func (d *DB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	trace(d.logger, query, args...)
	res, err := d.DB.ExecContext(ctx, query, args...)
	return res, WrapError(err)
}

// SelectContext wraps sqlx.SelectContext: it logs the query and
// arguments and maps the error with WrapError.
func (t *Tx) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(t.logger, query, args...)
	return WrapError(t.Tx.SelectContext(ctx, dest, query, args...))
}

// GetContext wraps sqlx.GetContext: it logs the query and
// arguments and maps the error with WrapError.
func (t *Tx) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(t.logger, query, args...)
	return WrapError(t.Tx.GetContext(ctx, dest, query, args...))
}

// QueryxContext wraps sqlx.QueryxContext: it logs the query and
// arguments and maps the error with WrapError.
func (t *Tx) QueryxContext(ctx context.Context, query string, args ...interface{}) (*sqlx.Rows, error) {
	trace(t.logger, query, args...)
	rows, err := t.Tx.QueryxContext(ctx, query, args...)
	return rows, WrapError(err)
}

// QueryRowxContext is a wrapper around sqlx.QueryRowxContext that logs the
// query and arguments. The error from scanning the row is not mapped.
func (t *Tx) QueryRowxContext(ctx context.Context, query string, args ...interface{}) *sqlx.Row {
	trace(t.logger, query, args...)
	return t.Tx.QueryRowxContext(ctx, query, args...)
}

// ExecContext wraps sqlx.ExecContext: it logs the query and
// arguments and maps the error with WrapError.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	trace(t.logger, query, args...)
	res, err := t.Tx.ExecContext(ctx, query, args...)
	return res, WrapError(err)
}
