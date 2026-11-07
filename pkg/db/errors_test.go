package db

import (
	"database/sql"
	"errors"
	"fmt"
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

// A mapped constraint violation keeps the driver error in its chain, so code
// that needs the driver's own code can still find it.
func TestWrapErrorKeepsDriverError(t *testing.T) {
	drv := &pq.Error{Code: "23503"}
	err := WrapError(drv)
	if !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("WrapError(%v) = %v, want ErrDuplicateKey", drv, err)
	}
	if err.Error() != ErrDuplicateKey.Error() {
		t.Errorf("WrapError(%v).Error() = %q, want %q", drv, err.Error(), ErrDuplicateKey.Error())
	}
	var pgErr *pq.Error
	if !errors.As(err, &pgErr) || pgErr != drv {
		t.Errorf("errors.As(WrapError(%v)) = %v, want the driver error", drv, pgErr)
	}
	if again := WrapError(err); again != err {
		t.Errorf("WrapError of a mapped error = %v, want it unchanged", again)
	}
}
