package database

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

func TestCollabStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	if err := s.CreateUser(ctx, dbx, "dev", false, nil); err != nil {
		t.Fatal(err)
	}

	if err := s.AddCollabByUsernameAndRepo(ctx, dbx, "DEV", "/repo7.git", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCollabByUsernameAndRepo(ctx, dbx, "dev", "repo7")
	if err != nil {
		t.Fatal(err)
	}
	if c.RepoID != 7 || c.AccessLevel != access.ReadWriteAccess {
		t.Errorf("GetCollabByUsernameAndRepo() = %+v, want read-write on repo 7", c)
	}
	if byID, err := s.GetCollabByUserIDAndRepoID(ctx, dbx, c.UserID, 7); err != nil || byID.ID != c.ID || byID.AccessLevel != c.AccessLevel {
		t.Errorf("GetCollabByUserIDAndRepoID() = %+v, %v, want %+v", byID, err, c)
	}
	if _, err := s.GetCollabByUserIDAndRepoID(ctx, dbx, c.UserID, 8); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetCollabByUserIDAndRepoID(repo 8) error = %v, want ErrRecordNotFound", err)
	}
	if err := s.AddCollabByUsernameAndRepo(ctx, dbx, "dev", "repo7", access.ReadOnlyAccess); !errors.Is(err, db.ErrDuplicateKey) {
		t.Errorf("AddCollabByUsernameAndRepo(again) error = %v, want ErrDuplicateKey", err)
	}

	cs, err := s.ListCollabsByRepoID(ctx, dbx, 7)
	if err != nil || len(cs) != 1 {
		t.Errorf("ListCollabsByRepoID(7) = %d, %v, want 1", len(cs), err)
	}
	us, err := s.ListCollabsByRepoIDAsUsers(ctx, dbx, 7)
	if err != nil || len(us) != 1 || us[0].Username != "dev" {
		t.Errorf("ListCollabsByRepoIDAsUsers(7) = %+v, %v, want [dev]", us, err)
	}
	if cs, err := s.ListCollabsByRepoID(ctx, dbx, 8); err != nil || len(cs) != 0 {
		t.Errorf("ListCollabsByRepoID(8) = %d, %v, want 0", len(cs), err)
	}

	if err := s.RemoveCollabByUsernameAndRepo(ctx, dbx, "dev", "repo7"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCollabByUsernameAndRepo(ctx, dbx, "dev", "repo7"); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetCollabByUsernameAndRepo() after remove: error = %v, want ErrRecordNotFound", err)
	}
}
