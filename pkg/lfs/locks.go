package lfs

import (
	"errors"
	"strconv"

	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

// MaxLocksLimit is the largest page of locks returned at once. A larger
// limit is lowered to it.
const MaxLocksLimit = 100

// ErrInvalidLocksCursor is returned by ParseLocksCursor for a cursor that is
// not a lock ID the server hands out.
var ErrInvalidLocksCursor = errors.New("invalid cursor")

// ParseLocksCursor parses the cursor of a lock list or verify request. The
// cursor is the ID of the last lock of the previous page, as handed out by
// NextLocksCursor, and the next page starts after it; empty means the first
// page and returns 0. Anything but a positive ID is refused with
// ErrInvalidLocksCursor.
//
// Paging by ID rather than by row offset keeps every page in a stable order:
// locks created or deleted between pages do not shift the pages after them,
// so no lock is skipped or listed twice.
func ParseLocksCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || id < 1 {
		return 0, ErrInvalidLocksCursor
	}
	return id, nil
}

// ClampLocksLimit returns the page size to use for a requested limit. Zero or
// less means DefaultLocksLimit, and a limit above MaxLocksLimit is lowered to
// it.
func ClampLocksLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultLocksLimit
	case limit > MaxLocksLimit:
		return MaxLocksLimit
	}
	return limit
}

// NextLocksCursor returns the cursor of the page after locks, a page fetched
// with the page size limit: the ID of its last lock. It is empty when there
// is no next page: the page was not full.
func NextLocksCursor(limit int, locks []models.LFSLock) string {
	if len(locks) == 0 || len(locks) < limit {
		return ""
	}
	return strconv.FormatInt(locks[len(locks)-1].ID, 10)
}
