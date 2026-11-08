package git

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// TestLFSLockRangeRejectsInvalidCursor checks that listing locks over SSH
// refuses a cursor that is not a page number it hands out. A cursor that did
// not parse used to read as the first page, and a huge one overflowed the
// store's row offset, (cursor-1)*limit. The backend has no database on
// purpose: a refused cursor must not reach it.
func TestLFSLockRangeRejectsInvalidCursor(t *testing.T) {
	l := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{ctx: context.Background(), op: lfs.OperationUpload},
		user:        lfsTestUser{},
	}

	for _, cursor := range []string{
		"abc",
		"1x",
		"0",
		"-1",
		strconv.Itoa(lfs.MaxLocksCursor + 1),
		"9223372036854775807",
		"99999999999999999999",
	} {
		t.Run(cursor, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Range(%q) reached the database: %v", cursor, r)
				}
			}()
			_, err := l.Range(cursor, 10, func(transfer.Lock) error { return nil })
			if !errors.Is(err, transfer.ErrParseError) {
				t.Errorf("Range(%q) error = %v, want %v", cursor, err, transfer.ErrParseError)
			}
		})
	}
}

// fullPageLockStore returns a full page of locks for any page, standing in
// for a repository with more locks than the last page can reach.
type fullPageLockStore struct{ store.Store }

func (fullPageLockStore) GetLFSLocks(_ context.Context, _ db.Handler, repoID int64, page int, limit int) ([]models.LFSLock, error) {
	locks := make([]models.LFSLock, limit)
	for i := range locks {
		locks[i] = models.LFSLock{ID: int64(i + 1), RepoID: repoID, UserID: 1, Path: strconv.Itoa(i)}
	}
	return locks, nil
}

func (fullPageLockStore) GetUserByID(_ context.Context, _ db.Handler, id int64) (models.User, error) {
	return models.User{ID: id, Username: "owner"}, nil
}

// TestLFSLockRangeLastPage checks that a full page at the largest cursor the
// server accepts hands out no next cursor. It used to hand out
// lfs.MaxLocksCursor+1, which the next request was then refused for.
func TestLFSLockRangeLastPage(t *testing.T) {
	dbx, err := db.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	l := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{
			ctx:    context.Background(),
			dbx:    dbx,
			store:  fullPageLockStore{},
			logger: log.New(io.Discard),
			repo:   lfsTestRepo{},
			op:     lfs.OperationUpload,
		},
		user: lfsTestUser{},
	}

	for cursor, want := range map[string]string{
		strconv.Itoa(lfs.MaxLocksCursor - 1): strconv.Itoa(lfs.MaxLocksCursor),
		strconv.Itoa(lfs.MaxLocksCursor):     "",
	} {
		var n int
		next, err := l.Range(cursor, 1, func(transfer.Lock) error { n++; return nil })
		if err != nil {
			t.Fatalf("Range(%q): %v", cursor, err)
		}
		if n != 1 || next != want {
			t.Errorf("Range(%q) = %d locks, next cursor %q; want 1, %q", cursor, n, next, want)
		}
	}
}

// lfsLockTestUser is a proto.User with an ID and an admin flag.
type lfsLockTestUser struct {
	proto.User
	id    int64
	admin bool
}

func (u lfsLockTestUser) ID() int64     { return u.id }
func (u lfsLockTestUser) IsAdmin() bool { return u.admin }

// TestLFSLockOwnership checks that SSH lock commands see every user's locks
// and tell the caller's from other users', as the HTTP locks API does. Every
// lock used to be reported "ours", and lock lookups by id or path saw only
// the caller's own locks, so a conflicting lock went unreported and another
// user's lock could not be listed, refused or force-unlocked.
func TestLFSLockOwnership(t *testing.T) {
	tr, ctx := newLFSUploadTransfer(t)
	datastore := store.FromContext(ctx)
	dbx := db.FromContext(ctx)

	users := map[string]lfsLockTestUser{}
	for _, u := range []struct {
		name  string
		admin bool
	}{{"owner", false}, {"other", false}, {"boss", true}} {
		if u.name != "owner" {
			if err := datastore.CreateUser(ctx, dbx, u.name, u.admin, nil); err != nil {
				t.Fatal(err)
			}
		}
		m, err := datastore.FindUserByUsername(ctx, dbx, u.name)
		if err != nil {
			t.Fatal(err)
		}
		users[u.name] = lfsLockTestUser{id: m.ID, admin: u.admin}
	}

	// session runs one upload session of commands as user and returns the
	// response, without the capability advertisement and the final quit.
	session := func(user string, cmds ...string) string {
		t.Helper()
		var in strings.Builder
		for _, c := range cmds {
			in.WriteString(c)
		}
		in.WriteString(pkt("quit\n") + "0000")

		utr := *tr
		utr.ctx = proto.WithUserContext(ctx, users[user])
		var out bytes.Buffer
		p := transfer.NewProcessor(transfer.NewPktline(strings.NewReader(in.String()), &out, nil), &utr, nil)
		if err := p.ProcessCommands(lfs.OperationUpload); err != nil {
			t.Fatalf("ProcessCommands as %s: %v", user, err)
		}
		return strings.TrimSuffix(out.String(), pkt("status 200\n")+"0000")
	}
	lock := func(path string) string {
		return pkt("lock\n") + pkt("path="+path+"\n") + pkt("refname=refs/heads/main\n") + "0000"
	}
	listLocks := func(args ...string) string {
		s := pkt("list-lock\n")
		for _, a := range args {
			s += pkt(a + "\n")
		}
		return s + "0000"
	}
	unlock := func(id string, args ...string) string {
		s := pkt("unlock " + id + "\n")
		for _, a := range args {
			s += pkt(a + "\n")
		}
		return s + "0000"
	}
	expect := func(name, got string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: response lacks %q\nresponse: %q", name, w, got)
			}
		}
	}

	if out := session("owner", lock("a.bin")); !strings.Contains(out, "status 201") {
		t.Fatalf("owner lock: %q", out)
	}
	locks, err := datastore.GetLFSLocksForUser(ctx, dbx, 1, users["owner"].id)
	if err != nil || len(locks) != 1 {
		t.Fatalf("owner locks = %v, %v; want one", locks, err)
	}
	id := strconv.FormatInt(locks[0].ID, 10)

	expect("other lock", session("other", lock("a.bin")),
		pkt("status 409\n"), pkt("id="+id+"\n"), pkt("ownername=owner\n"))
	expect("owner list", session("owner", listLocks()),
		pkt("status 200\n"), pkt("owner "+id+" ours\n"))
	expect("other list", session("other", listLocks()),
		pkt("status 200\n"), pkt("owner "+id+" theirs\n"))
	expect("other list path", session("other", listLocks("path=a.bin")),
		pkt("status 200\n"), pkt("path "+id+" a.bin\n"), pkt("owner "+id+" theirs\n"))
	expect("other unlock", session("other", unlock(id)), pkt("status 403\n"))
	expect("other force unlock", session("other", unlock(id, "force=true")), pkt("status 403\n"))
	expect("admin unlock", session("boss", unlock(id)), pkt("status 403\n"))
	if _, err := datastore.GetLFSLockByID(ctx, dbx, 1, locks[0].ID); err != nil {
		t.Fatalf("lock is gone after refused unlocks: %v", err)
	}

	expect("admin force unlock", session("boss", unlock(id, "force=true")),
		pkt("status 200\n"), pkt("id="+id+"\n"))
	if _, err := datastore.GetLFSLockByID(ctx, dbx, 1, locks[0].ID); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("lock after admin force unlock: err = %v, want %v", err, db.ErrRecordNotFound)
	}
	expect("unlock missing", session("owner", unlock(id)), pkt("status 404\n"))

	// The owner still unlocks their own lock without force.
	session("other", lock("b.bin"))
	locks, err = datastore.GetLFSLocksForUser(ctx, dbx, 1, users["other"].id)
	if err != nil || len(locks) != 1 {
		t.Fatalf("other locks = %v, %v; want one", locks, err)
	}
	expect("other unlock own", session("other", unlock(strconv.FormatInt(locks[0].ID, 10))), pkt("status 200\n"))
}
