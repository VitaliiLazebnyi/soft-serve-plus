package db

import (
	"database/sql"
	"errors"

	"github.com/lib/pq"
	sqlite "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

var (
	// ErrDuplicateKey is a unique or primary key constraint violation error.
	ErrDuplicateKey = errors.New("duplicate key value violates table constraint")

	// ErrForeignKey is a foreign key constraint violation error: a row
	// refers to a row that does not exist, or a row that others refer to is
	// deleted.
	ErrForeignKey = errors.New("value violates foreign key constraint")

	// ErrNotNull is a not-null constraint violation error.
	ErrNotNull = errors.New("null value violates not-null constraint")

	// ErrCheck is a check constraint violation error.
	ErrCheck = errors.New("value violates check constraint")

	// ErrRecordNotFound is returned when a record is not found.
	ErrRecordNotFound = sql.ErrNoRows
)

// WrapError maps database driver errors to the errors of this package. It
// finds driver errors in err's chain, so err may be a driver error wrapped
// with fmt.Errorf and %w. A constraint violation becomes an error that
// errors.Is reports as ErrDuplicateKey, ErrForeignKey, ErrNotNull or ErrCheck
// and whose message is that error's; the driver error stays in its chain, so
// errors.As still finds it. sql.ErrNoRows becomes ErrRecordNotFound. An error
// that is already mapped, and any other error, is returned unchanged.
//
// The methods of DB and Tx already apply WrapError, so errors from a Handler
// need no further mapping.
func WrapError(err error) error {
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRecordNotFound
		}

		// An already mapped error still has the driver error in its chain;
		// mapping it again would only wrap it once more.
		var me *mappedError
		if errors.As(err, &me) {
			return err
		}

		// Handle sqlite constraint error.
		var liteErr *sqlite.Error
		if errors.As(err, &liteErr) {
			switch liteErr.Code() {
			case sqlitelib.SQLITE_CONSTRAINT_PRIMARYKEY,
				sqlitelib.SQLITE_CONSTRAINT_UNIQUE:
				return &mappedError{ErrDuplicateKey, err}
			case sqlitelib.SQLITE_CONSTRAINT_FOREIGNKEY:
				return &mappedError{ErrForeignKey, err}
			case sqlitelib.SQLITE_CONSTRAINT_NOTNULL:
				return &mappedError{ErrNotNull, err}
			case sqlitelib.SQLITE_CONSTRAINT_CHECK:
				return &mappedError{ErrCheck, err}
			}
		}

		// Handle postgres constraint error.
		var pgErr *pq.Error
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // unique_violation
				return &mappedError{ErrDuplicateKey, err}
			case "23503": // foreign_key_violation
				return &mappedError{ErrForeignKey, err}
			case "23502": // not_null_violation
				return &mappedError{ErrNotNull, err}
			case "23514": // check_violation
				return &mappedError{ErrCheck, err}
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
