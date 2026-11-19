package database

import (
	"testing"
)

// A lock is stored for the path it was asked for. Validation
// (lfs.ValidateLockPath) accepts a path that ends in a space; the store used
// to trim it, so "a.bin " locked "a.bin" and a lock on "a.bin" then failed
// as a duplicate.
func TestLFSLockPathStoredAsGiven(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const repo, owner = 7, 100 // see newWebhookTestDB

	for _, p := range []string{"a.bin ", "a.bin", " a.bin"} {
		if err := s.CreateLFSLockForUser(ctx, dbx, repo, owner, p, ""); err != nil {
			t.Fatalf("CreateLFSLockForUser(%q) => %v", p, err)
		}
		l, err := s.GetLFSLockForPath(ctx, dbx, repo, p)
		if err != nil || l.Path != p {
			t.Errorf("GetLFSLockForPath(%q) = %q, %v", p, l.Path, err)
		}
		l, err = s.GetLFSLockForUserPath(ctx, dbx, repo, owner, p)
		if err != nil || l.Path != p {
			t.Errorf("GetLFSLockForUserPath(%q) = %q, %v", p, l.Path, err)
		}
	}
}
