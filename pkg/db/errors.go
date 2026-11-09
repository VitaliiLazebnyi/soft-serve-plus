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

// WrapError is a convenient function that unite various database driver
// errors to consistent errors. It finds driver errors in err's chain, so
// err may be a driver error wrapped with fmt.Errorf and %w.
func WrapError(err error) error {
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRecordNotFound
		}

		// Handle sqlite constraint error.
		var liteErr *sqlite.Error
		if errors.As(err, &liteErr) {
			switch liteErr.Code() {
			case sqlitelib.SQLITE_CONSTRAINT_PRIMARYKEY,
				sqlitelib.SQLITE_CONSTRAINT_UNIQUE:
				return ErrDuplicateKey
			case sqlitelib.SQLITE_CONSTRAINT_FOREIGNKEY:
				return ErrForeignKey
			case sqlitelib.SQLITE_CONSTRAINT_NOTNULL:
				return ErrNotNull
			case sqlitelib.SQLITE_CONSTRAINT_CHECK:
				return ErrCheck
			}
		}

		// Handle postgres constraint error.
		var pgErr *pq.Error
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // unique_violation
				return ErrDuplicateKey
			case "23503": // foreign_key_violation
				return ErrForeignKey
			case "23502": // not_null_violation
				return ErrNotNull
			case "23514": // check_violation
				return ErrCheck
			}
		}
	}
	return err
}
