package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"charm.land/log/v2"
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

// SelectContext wraps sqlx.SelectContext: it logs the query and the
// number of its arguments, and maps the error with WrapError.
func (d *DB) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(d.logger, query, args...)
	return mapError(d.errLog, d.DB.SelectContext(ctx, dest, query, args...))
}

// GetContext wraps sqlx.GetContext: it logs the query and the
// number of its arguments, and maps the error with WrapError.
func (d *DB) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(d.logger, query, args...)
	return mapError(d.errLog, d.DB.GetContext(ctx, dest, query, args...))
}

// ExecContext wraps sqlx.ExecContext: it logs the query and the
// number of its arguments, and maps the error with WrapError.
func (d *DB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	trace(d.logger, query, args...)
	res, err := d.DB.ExecContext(ctx, query, args...)
	return res, mapError(d.errLog, err)
}

// SelectContext wraps sqlx.SelectContext: it logs the query and the
// number of its arguments, and maps the error with WrapError.
func (t *Tx) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(t.logger, query, args...)
	return mapError(t.errLog, t.Tx.SelectContext(ctx, dest, query, args...))
}

// GetContext wraps sqlx.GetContext: it logs the query and the
// number of its arguments, and maps the error with WrapError.
func (t *Tx) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	trace(t.logger, query, args...)
	return mapError(t.errLog, t.Tx.GetContext(ctx, dest, query, args...))
}

// ExecContext wraps sqlx.ExecContext: it logs the query and the
// number of its arguments, and maps the error with WrapError.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	trace(t.logger, query, args...)
	res, err := t.Tx.ExecContext(ctx, query, args...)
	return res, mapError(t.errLog, err)
}
