package database

import (
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
)

func TestSettingsStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)

	// The defaults the first migration seeds.
	if v, err := s.GetAllowKeylessAccess(ctx, dbx); err != nil || !v {
		t.Errorf("GetAllowKeylessAccess() = %v, %v, want true", v, err)
	}
	if v, err := s.GetAnonAccess(ctx, dbx); err != nil || v != access.ReadOnlyAccess {
		t.Errorf("GetAnonAccess() = %v, %v, want read-only", v, err)
	}

	for _, allow := range []bool{false, true, false} {
		if err := s.SetAllowKeylessAccess(ctx, dbx, allow); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetAllowKeylessAccess(ctx, dbx); err != nil || v != allow {
			t.Errorf("GetAllowKeylessAccess() after setting %v = %v, %v", allow, v, err)
		}
	}
	for _, level := range []access.Level{access.NoAccess, access.ReadWriteAccess, access.AdminAccess, access.ReadOnlyAccess} {
		if err := s.SetAnonAccess(ctx, dbx, level); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetAnonAccess(ctx, dbx); err != nil || v != level {
			t.Errorf("GetAnonAccess() after setting %v = %v, %v", level, v, err)
		}
	}
}
