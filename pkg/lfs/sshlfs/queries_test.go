package sshlfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// lookupCountingStore counts the per-item lookups of the SSH transfer.
type lookupCountingStore struct {
	store.Store
	objectLookups atomic.Int32
	userLookups   atomic.Int32
}

func (s *lookupCountingStore) GetLFSObjectByOid(ctx context.Context, h db.Handler, repoID int64, oid string) (models.LFSObject, error) {
	s.objectLookups.Add(1)
	return s.Store.GetLFSObjectByOid(ctx, h, repoID, oid)
}

func (s *lookupCountingStore) GetUserByID(ctx context.Context, h db.Handler, id int64) (models.User, error) {
	s.userLookups.Add(1)
	return s.Store.GetUserByID(ctx, h, id)
}

// A batch over SSH looks its objects up in one query, and lock responses
// name their owners from the lock query, not one lookup per object or lock.
func TestLFSTransferLooksUpTogether(t *testing.T) {
	t.Parallel()
	e := backendtest.New(t)
	ctx, cfg, dbx, base := e.Ctx, e.Cfg, e.DB, e.Store
	dp := cfg.DataPath
	admin, err := base.FindUserByUsername(ctx, dbx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.CreateRepo(ctx, dbx, models.Repo{Name: "repo", UserID: admin.ID}); err != nil {
		t.Fatal(err)
	}
	r, err := base.GetRepoByName(ctx, dbx, "repo")
	if err != nil {
		t.Fatal(err)
	}

	strg := storage.NewLocal(filepath.Join(dp, "lfs", "1"))
	var pointers []transfer.BatchItem
	for _, content := range []string{"one", "two", "three"} {
		sum := sha256.Sum256([]byte(content))
		oid := hex.EncodeToString(sum[:])
		p := transfer.Pointer{Oid: oid, Size: int64(len(content))}
		if _, err := strg.Put(path.Join("objects", p.RelativePath()), strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		if err := base.CreateLFSObject(ctx, dbx, r.ID, oid, p.Size); err != nil {
			t.Fatal(err)
		}
		pointers = append(pointers, transfer.BatchItem{Pointer: p})
	}
	for _, lp := range []string{"a.bin", "b.bin"} {
		if _, err := base.CreateLFSLockForUser(ctx, dbx, r.ID, admin.ID, lp, ""); err != nil {
			t.Fatal(err)
		}
	}

	cs := &lookupCountingStore{Store: base}
	tr := &lfsTransfer{
		ctx:     ctx,
		dbx:     dbx,
		store:   cs,
		storage: strg,
		logger:  log.New(io.Discard),
		repo:    lfsIDRepo{id: r.ID},
		op:      lfs.OperationDownload,
	}
	items, err := tr.Batch("", pointers, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range items {
		if !it.Present {
			t.Errorf("object %d not present", i)
		}
	}

	locks := &lfsLockBackend{lfsTransfer: tr, user: lfsIDUser{id: admin.ID}}
	var owners []string
	if _, err := locks.Range("", 10, func(l transfer.Lock) error {
		owners = append(owners, l.OwnerName())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, get := range []func() (transfer.Lock, error){
		func() (transfer.Lock, error) { return locks.FromPath("a.bin") },
	} {
		l, err := get()
		if err != nil {
			t.Fatal(err)
		}
		owners = append(owners, l.OwnerName())
	}
	for _, o := range owners {
		if o != "admin" {
			t.Errorf("owners = %q, want admin for each lock", owners)
			break
		}
	}

	if n := cs.objectLookups.Load(); n != 0 {
		t.Errorf("per-object lookups = %d, want 0", n)
	}
	if n := cs.userLookups.Load(); n != 0 {
		t.Errorf("owner lookups = %d, want 0", n)
	}
}
