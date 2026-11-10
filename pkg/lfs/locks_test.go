package lfs

import (
	"math"
	"strconv"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

func TestParseLocksCursor(t *testing.T) {
	for cursor, want := range map[string]int64{
		"":                     0,
		"1":                    1,
		"01":                   1,
		"9223372036854775807":  math.MaxInt64,
		"0":                    -1,
		"-1":                   -1,
		"abc":                  -1,
		"1x":                   -1,
		"99999999999999999999": -1,
	} {
		id, err := ParseLocksCursor(cursor)
		if want < 0 {
			if err != ErrInvalidLocksCursor {
				t.Errorf("ParseLocksCursor(%q) = %d, %v; want %v", cursor, id, err, ErrInvalidLocksCursor)
			}
			continue
		}
		if err != nil || id != want {
			t.Errorf("ParseLocksCursor(%q) = %d, %v; want %d", cursor, id, err, want)
		}
	}
}

func TestClampLocksLimit(t *testing.T) {
	for limit, want := range map[int]int{
		-1:                DefaultLocksLimit,
		0:                 DefaultLocksLimit,
		1:                 1,
		MaxLocksLimit:     MaxLocksLimit,
		MaxLocksLimit + 1: MaxLocksLimit,
	} {
		if got := ClampLocksLimit(limit); got != want {
			t.Errorf("ClampLocksLimit(%d) = %d, want %d", limit, got, want)
		}
	}
}

// TestNextLocksCursor checks that a full page hands out the ID of its last
// lock, which ParseLocksCursor accepts, and a short page hands out none.
func TestNextLocksCursor(t *testing.T) {
	page := func(ids ...int64) []models.LFSLock {
		locks := make([]models.LFSLock, len(ids))
		for i, id := range ids {
			locks[i] = models.LFSLock{ID: id}
		}
		return locks
	}
	for _, tc := range []struct {
		limit int
		locks []models.LFSLock
		want  string
	}{
		{2, page(3, 7), "7"},
		{2, page(3), ""},
		{2, page(), ""},
		{0, page(), ""},
		{1, page(math.MaxInt64), strconv.FormatInt(math.MaxInt64, 10)},
	} {
		got := NextLocksCursor(tc.limit, tc.locks)
		if got != tc.want {
			t.Errorf("NextLocksCursor(%d, %v) = %q, want %q", tc.limit, tc.locks, got, tc.want)
		}
		if got != "" {
			if _, err := ParseLocksCursor(got); err != nil {
				t.Errorf("NextLocksCursor(%d, %v) = %q, which ParseLocksCursor refuses: %v", tc.limit, tc.locks, got, err)
			}
		}
	}
}
