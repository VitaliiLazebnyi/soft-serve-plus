package git

import (
	"bytes"
	"context"
	"database/sql"
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
// refuses a cursor that is not a lock ID it hands out. A cursor that did not
// parse used to read as the first page. The backend has no database on
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

// pagedLockStore holds locks with the IDs 1 to n and returns them as the
// database store does: up to limit of them after afterID, in ID order.
type pagedLockStore struct {
	store.Store
	n int64
}

func (s pagedLockStore) GetLFSLocks(_ context.Context, _ db.Handler, repoID int64, afterID int64, limit int) ([]models.LFSLock, error) {
	var locks []models.LFSLock
	for id := afterID + 1; id <= s.n && len(locks) < limit; id++ {
		locks = append(locks, models.LFSLock{ID: id, RepoID: repoID, UserID: 1, Path: strconv.FormatInt(id, 10)})
	}
	return locks, nil
}

func (pagedLockStore) GetUserByID(_ context.Context, _ db.Handler, id int64) (models.User, error) {
	return models.User{ID: id, Username: "owner"}, nil
}

// TestLFSLockRangeCursor checks that listing locks over SSH hands out the ID
// of the last lock of a full page as the next cursor, and starts the next
// page after it, so paging lists every lock exactly once.
func TestLFSLockRangeCursor(t *testing.T) {
	dbx, err := db.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	l := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{
			ctx:    context.Background(),
			dbx:    dbx,
			store:  pagedLockStore{n: 7},
			logger: log.New(io.Discard),
			repo:   lfsTestRepo{},
			op:     lfs.OperationUpload,
		},
		user: lfsTestUser{},
	}

	var ids, cursors []string
	cursor := ""
	for range 10 {
		next, err := l.Range(cursor, 3, func(lock transfer.Lock) error {
			ids = append(ids, lock.ID())
			return nil
		})
		if err != nil {
			t.Fatalf("Range(%q): %v", cursor, err)
		}
		if next == "" {
			break
		}
		cursors = append(cursors, next)
		cursor = next
	}
	if got, want := strings.Join(ids, ","), "1,2,3,4,5,6,7"; got != want {
		t.Errorf("listed locks %s, want %s", got, want)
	}
	if got, want := strings.Join(cursors, ","), "3,6"; got != want {
		t.Errorf("next cursors %s, want %s", got, want)
	}
}

// passwordLockStore returns one lock owned by a user with a password hash.
type passwordLockStore struct{ pagedLockStore }

const testPasswordHash = "$2a$10$abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ012"

func (passwordLockStore) GetUserByID(_ context.Context, _ db.Handler, id int64) (models.User, error) {
	return models.User{
		ID:       id,
		Username: "owner",
		Password: sql.NullString{String: testPasswordHash, Valid: true},
	}, nil
}

// TestLFSLockDebugLogOmitsPassword checks that the transfer library's debug
// log of a lock, as in its "lock success" line, does not print the owner's
// password hash. A lock used to hold the owner's whole user record.
func TestLFSLockDebugLogOmitsPassword(t *testing.T) {
	dbx, err := db.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	l := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{
			ctx:    context.Background(),
			dbx:    dbx,
			store:  passwordLockStore{pagedLockStore{n: 1}},
			logger: log.New(io.Discard),
			repo:   lfsTestRepo{},
			op:     lfs.OperationUpload,
		},
		user: lfsTestUser{},
	}

	var buf bytes.Buffer
	logger := &lfsLogger{l: log.NewWithOptions(&buf, log.Options{Level: log.DebugLevel})}
	if _, err := l.Range("", 1, func(lock transfer.Lock) error {
		logger.Log("lock success", "lock", lock)
		return nil
	}); err != nil {
		t.Fatalf("Range: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "owner") {
		t.Errorf("log %q does not name the lock owner", out)
	}
	if strings.Contains(out, testPasswordHash) {
		t.Errorf("log %q includes the owner's password hash", out)
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
	ownerLock, err := datastore.GetLFSLockForUserPath(ctx, dbx, 1, users["owner"].id, "a.bin")
	if err != nil {
		t.Fatalf("owner lock: %v", err)
	}
	id := strconv.FormatInt(ownerLock.ID, 10)

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
	if _, err := datastore.GetLFSLockByID(ctx, dbx, 1, ownerLock.ID); err != nil {
		t.Fatalf("lock is gone after refused unlocks: %v", err)
	}

	expect("admin force unlock", session("boss", unlock(id, "force=true")),
		pkt("status 200\n"), pkt("id="+id+"\n"))
	if _, err := datastore.GetLFSLockByID(ctx, dbx, 1, ownerLock.ID); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("lock after admin force unlock: err = %v, want %v", err, db.ErrRecordNotFound)
	}
	expect("unlock missing", session("owner", unlock(id)), pkt("status 404\n"))

	// The owner still unlocks their own lock without force.
	session("other", lock("b.bin"))
	otherLock, err := datastore.GetLFSLockForUserPath(ctx, dbx, 1, users["other"].id, "b.bin")
	if err != nil {
		t.Fatalf("other lock: %v", err)
	}
	expect("other unlock own", session("other", unlock(strconv.FormatInt(otherLock.ID, 10))), pkt("status 200\n"))
}

// TestLFSLockFromIDRejectsInvalidID checks that unlocking by an ID that is
// not a number is a client error (400) rather than an internal one (500).
// The backend has no database on purpose: a refused ID must not reach it.
func TestLFSLockFromIDRejectsInvalidID(t *testing.T) {
	l := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{ctx: context.Background(), op: lfs.OperationUpload},
		user:        lfsTestUser{},
	}

	for _, id := range []string{"abc", "", "1x", "99999999999999999999"} {
		t.Run(id, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("FromID(%q) reached the database: %v", id, r)
				}
			}()
			if _, err := l.FromID(id); !errors.Is(err, transfer.ErrParseError) {
				t.Errorf("FromID(%q) error = %v, want %v", id, err, transfer.ErrParseError)
			}
		})
	}
}
