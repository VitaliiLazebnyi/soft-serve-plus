package db

import (
	"database/sql"
	"errors"

	"github.com/lib/pq"
	sqlite "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

var (
	// ErrDuplicateKey is a constraint violation error.
	ErrDuplicateKey = errors.New("duplicate key value violates table constraint")

	// ErrRecordNotFound is returned when a record is not found.
	ErrRecordNotFound = sql.ErrNoRows
)

// WrapError maps database driver errors to the errors of this package. A
// constraint violation becomes an error that errors.Is reports as
// ErrDuplicateKey and whose message is ErrDuplicateKey's; the driver error
// stays in its chain, so errors.As still finds it. sql.ErrNoRows becomes
// ErrRecordNotFound. Other errors are returned unchanged.
//
// The methods of DB and Tx already apply WrapError, so errors from a Handler
// need no further mapping.
func WrapError(err error) error {
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRecordNotFound
		}

		// Handle sqlite constraint error.
		if liteErr, ok := err.(*sqlite.Error); ok {
			code := liteErr.Code()
			if code == sqlitelib.SQLITE_CONSTRAINT_PRIMARYKEY ||
				code == sqlitelib.SQLITE_CONSTRAINT_FOREIGNKEY ||
				code == sqlitelib.SQLITE_CONSTRAINT_UNIQUE {
				return &mappedError{ErrDuplicateKey, err}
			}
		}

		// Handle postgres constraint error.
		if pgErr, ok := err.(*pq.Error); ok {
			if pgErr.Code == "23505" ||
				pgErr.Code == "23503" ||
				pgErr.Code == "23514" {
				return &mappedError{ErrDuplicateKey, err}
			}
		}
	}
	return err
}

// mappedError is a driver error mapped to one of this package's errors.
type mappedError struct {
	mapped error
	driver error
}

func (e *mappedError) Error() string { return e.mapped.Error() }

func (e *mappedError) Unwrap() []error { return []error{e.mapped, e.driver} }
