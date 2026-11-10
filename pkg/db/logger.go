package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"charm.land/log/v2"
	"github.com/jmoiron/sqlx"
)

// trace logs a query at debug level. Only the number of arguments is logged,
// never their values: they include webhook secrets and URLs, password and
// token hashes, and delivery payloads.
func trace(l *log.Logger, query string, args ...interface{}) {
	if l != nil {
		// Collapse newlines, tabs and runs of spaces into single spaces.
		query = strings.Join(strings.Fields(query), " ")
		l.Debug("trace", "query", query, "args", len(args))
	}
}

// mapError maps err with WrapError. A mapped constraint violation reads as
// the generic message of the error it maps to, so the driver error, which
// names the constraint that failed, is logged here at debug level. l is the
// handler's error logger, which unlike the query trace is set outside verbose
// mode too; a nil l skips the log.
func mapError(l *log.Logger, err error) error {
	err = WrapError(err)
	var me *mappedError
	if l != nil && errors.As(err, &me) {
		l.Debug("constraint violation", "err", me.driver)
	}
	return err
}

// SelectContext wraps sqlx.SelectContext: it logs the query and
// arguments and maps the error with WrapError.
func (d *DB) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(d.logger, query, args...)
	return mapError(d.errLog, d.DB.SelectContext(ctx, dest, query, args...))
}

// GetContext wraps sqlx.GetContext: it logs the query and
// arguments and maps the error with WrapError.
func (d *DB) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(d.logger, query, args...)
	return mapError(d.errLog, d.DB.GetContext(ctx, dest, query, args...))
}

// QueryxContext wraps sqlx.QueryxContext: it logs the query and
// arguments and maps the error with WrapError.
func (d *DB) QueryxContext(ctx context.Context, query string, args ...interface{}) (*sqlx.Rows, error) {
	trace(d.logger, query, args...)
	rows, err := d.DB.QueryxContext(ctx, query, args...)
	return rows, mapError(d.errLog, err)
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
	return res, mapError(d.errLog, err)
}

// SelectContext wraps sqlx.SelectContext: it logs the query and
// arguments and maps the error with WrapError.
func (t *Tx) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(t.logger, query, args...)
	return mapError(t.errLog, t.Tx.SelectContext(ctx, dest, query, args...))
}

// GetContext wraps sqlx.GetContext: it logs the query and
// arguments and maps the error with WrapError.
func (t *Tx) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(t.logger, query, args...)
	return mapError(t.errLog, t.Tx.GetContext(ctx, dest, query, args...))
}

// QueryxContext wraps sqlx.QueryxContext: it logs the query and
// arguments and maps the error with WrapError.
func (t *Tx) QueryxContext(ctx context.Context, query string, args ...interface{}) (*sqlx.Rows, error) {
	trace(t.logger, query, args...)
	rows, err := t.Tx.QueryxContext(ctx, query, args...)
	return rows, mapError(t.errLog, err)
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
	return res, mapError(t.errLog, err)
}
