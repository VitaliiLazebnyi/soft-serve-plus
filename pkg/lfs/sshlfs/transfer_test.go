package sshlfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	_ "modernc.org/sqlite"
)

// Object IDs arrive raw off the pktline stream. They must be rejected before
// they can be joined into a storage path. The backend is left zero-valued on
// purpose: a guard that fires only after the store or filesystem is touched is
// not a guard.
func TestLFSTransferRejectsMalformedOid(t *testing.T) {
	t.Parallel()
	oids := []string{
		"../../../../../../../../etc/passwd",
		"../../ssh/soft_serve_host_ed25519",
		"objects/../../../soft-serve.db",
		"/etc/passwd",
		"",
		"abc",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.ToUpper(strings.Repeat("a", 64)),
	}

	var backend lfsTransfer
	for _, oid := range oids {
		t.Run(oid, func(t *testing.T) {
			t.Parallel()
			if _, _, err := backend.Download(oid, nil); !errors.Is(err, errInvalidOid) {
				t.Errorf("Download: got %v, want errInvalidOid", err)
			}
			if err := backend.Upload(oid, 1, strings.NewReader("x"), nil); !errors.Is(err, errInvalidOid) {
				t.Errorf("Upload: got %v, want errInvalidOid", err)
			}
			// Verify answers in-band with a bad request status: a malformed
			// object ID is a client error, not a conflict.
			status, err := backend.Verify(oid, 1, nil)
			if err != nil {
				t.Errorf("Verify: unexpected error %v", err)
			} else if status == nil || status.Code() != transfer.StatusBadRequest {
				t.Errorf("Verify: got %v, want a bad request status", status)
			}

			items := []transfer.BatchItem{{Pointer: transfer.Pointer{Oid: oid, Size: 1}}}
			if _, err := backend.Batch("download", items, nil); !errors.Is(err, errInvalidOid) {
				t.Errorf("Batch: got %v, want errInvalidOid", err)
			}
		})
	}
}

// A malformed object ID is a client error, so the processor reports it as a
// 400 rather than falling through to a generic internal error.
func TestInvalidOidIsAParseError(t *testing.T) {
	t.Parallel()
	if !errors.Is(errInvalidOid, transfer.ErrParseError) {
		t.Errorf("errInvalidOid does not wrap transfer.ErrParseError")
	}
	if !errors.Is(errInvalidOid, lfs.ErrInvalidOIDFormat) {
		t.Errorf("errInvalidOid does not wrap lfs.ErrInvalidOIDFormat")
	}
}

// newLFSUploadTransfer returns an upload-session lfsTransfer backed by a
// backendtest database holding repository 1 ("repo1"), and the context
// it was built from.
func newLFSUploadTransfer(t *testing.T) (*lfsTransfer, TransferDeps) {
	t.Helper()
	e := backendtest.New(t)
	ctx, cfg, dbx, datastore := e.Ctx, e.Cfg, e.DB, e.Store
	dp := cfg.DataPath
	if err := datastore.CreateUser(ctx, dbx, "owner", false, nil); err != nil {
		t.Fatal(err)
	}
	user, err := datastore.FindUserByUsername(ctx, dbx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := datastore.CreateRepo(ctx, dbx, models.Repo{Name: "repo1", UserID: user.ID}); err != nil {
		t.Fatal(err)
	}
	if r, err := datastore.GetRepoByName(ctx, dbx, "repo1"); err != nil || r.ID != (lfsTestRepo{}).ID() {
		t.Fatalf("repo1 has id %d (err %v), want %d", r.ID, err, (lfsTestRepo{}).ID())
	}
	logger := log.New(&bytes.Buffer{})
	deps := TransferDeps{Config: cfg, DB: dbx, Store: datastore, Logger: logger}

	return &lfsTransfer{
		ctx:     ctx,
		cfg:     cfg,
		dbx:     dbx,
		store:   datastore,
		logger:  logger,
		storage: storage.NewLocal(filepath.Join(dp, "lfs", "1")),
		repo:    lfsTestRepo{},
		op:      lfs.OperationUpload,
	}, deps
}

func lfsObjectPath(cfg *config.Config, oid string) string {
	return filepath.Join(cfg.DataPath, "lfs", "1", "objects", oid[0:2], oid[2:4], oid)
}

// assertNoLFSTempFiles fails if an upload left anything in the incomplete
// directory.
func assertNoLFSTempFiles(t *testing.T, cfg *config.Config) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cfg.DataPath, "lfs", "1", "incomplete"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, ent := range entries {
		t.Errorf("temp file left behind: %s", ent.Name())
	}
}

func oidOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Upload must check the data against the oid and the declared size itself,
// not trust its caller.
func TestLFSTransferUploadRejectsMismatchedContent(t *testing.T) {
	t.Parallel()
	const content = "hello lfs"
	oid := oidOf(content)
	cases := []struct {
		name string
		oid  string
		size int64
		data string
	}{
		{"wrong hash", oidOf("something else"), int64(len(content)), content},
		{"data shorter than size", oid, int64(len(content)) + 1, content},
		{"data longer than size", oid, int64(len(content)) - 1, content},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr, _ := newLFSUploadTransfer(t)
			err := tr.Upload(tc.oid, tc.size, strings.NewReader(tc.data), nil)
			if !errors.Is(err, transfer.ErrCorruptData) {
				t.Fatalf("Upload: got %v, want ErrCorruptData", err)
			}
			if _, err := os.Stat(lfsObjectPath(tr.cfg, tc.oid)); !os.IsNotExist(err) {
				t.Errorf("object stored despite mismatch (stat err: %v)", err)
			}
			if _, err := tr.store.GetLFSObjectByOid(tr.ctx, tr.dbx, 1, tc.oid); err == nil {
				t.Error("object recorded despite mismatch")
			}
			assertNoLFSTempFiles(t, tr.cfg)
		})
	}
}

// Uploading an object that is already recorded succeeds: the insert's
// unique constraint does not reach the client as an internal error.
func TestLFSTransferUploadDuplicate(t *testing.T) {
	t.Parallel()
	tr, _ := newLFSUploadTransfer(t)
	const content = "hello lfs"
	oid := oidOf(content)

	for i := range 2 {
		if err := tr.Upload(oid, int64(len(content)), strings.NewReader(content), nil); err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}

	got, err := os.ReadFile(lfsObjectPath(tr.cfg, oid))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("stored content %q, want %q", got, content)
	}
	obj, err := tr.store.GetLFSObjectByOid(tr.ctx, tr.dbx, 1, oid)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size != int64(len(content)) {
		t.Errorf("recorded size %d, want %d", obj.Size, len(content))
	}
	assertNoLFSTempFiles(t, tr.cfg)
}

// A foreign key violation is not a duplicate upload, which Upload reports as
// success.
func TestLFSTransferUploadRepoDeleted(t *testing.T) {
	t.Parallel()
	tr, _ := newLFSUploadTransfer(t)
	if err := tr.store.DeleteRepoByName(tr.ctx, tr.dbx, "repo1"); err != nil {
		t.Fatal(err)
	}
	const content = "orphan object"
	if err := tr.Upload(oidOf(content), int64(len(content)), strings.NewReader(content), nil); err == nil {
		t.Fatal("Upload succeeded for a repository that no longer exists")
	}
	assertNoLFSTempFiles(t, tr.cfg)
}

// Through the transfer protocol, an object whose data is longer than its
// declared size is refused with a 400, nothing is left on disk, and the
// next command is still read from the right place in the stream.
func TestLFSTransferPutObjectOversized(t *testing.T) {
	t.Parallel()
	tr, deps := newLFSUploadTransfer(t)
	data := "hello"
	oid := oidOf(data)

	var in strings.Builder
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("put-object "+oid+"\n") + pkt(fmt.Sprintf("size=%d\n", len(data)-2)) + "0001")
	in.WriteString(pkt(data) + "0000")
	in.WriteString(pkt("quit\n") + "0000")

	var out bytes.Buffer
	if err := Transfer(t.Context(), strings.NewReader(in.String()), &out, deps, lfsTestRepo{}, lfsTestUser{}, lfs.OperationUpload); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	var got []string
	for _, m := range regexp.MustCompile(`status (\d{3})`).FindAllStringSubmatch(out.String(), -1) {
		got = append(got, m[1])
	}
	want := []string{"200", "400", "200"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v\noutput: %q", got, want, out.String())
	}
	if _, err := os.Stat(lfsObjectPath(tr.cfg, oid)); !os.IsNotExist(err) {
		t.Errorf("object stored despite size mismatch (stat err: %v)", err)
	}
	assertNoLFSTempFiles(t, tr.cfg)
}

// A put-object with an invalid oid is refused with a 400 and its data is
// drained, so the next command is still read from the right place in the
// stream, not the data packet and flush.
func TestLFSTransferPutObjectInvalidOid(t *testing.T) {
	t.Parallel()
	_, deps := newLFSUploadTransfer(t)
	data := "hello"

	var in strings.Builder
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("put-object not-an-oid\n") + pkt(fmt.Sprintf("size=%d\n", len(data))) + "0001")
	in.WriteString(pkt(data) + "0000")
	in.WriteString(pkt("quit\n") + "0000")

	var out bytes.Buffer
	if err := Transfer(t.Context(), strings.NewReader(in.String()), &out, deps, lfsTestRepo{}, lfsTestUser{}, lfs.OperationUpload); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	var got []string
	for _, m := range regexp.MustCompile(`status (\d{3})`).FindAllStringSubmatch(out.String(), -1) {
		got = append(got, m[1])
	}
	want := []string{"200", "400", "200"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v\noutput: %q", got, want, out.String())
	}
}

// failRenameStorage is a storage whose Rename always fails, as renaming onto
// a file another upload is renaming or has open does on Windows.
type failRenameStorage struct{ storage.Storage }

func (failRenameStorage) Rename(string, string) error {
	return errors.New("rename: access is denied")
}

// When the rename into objects/ fails but the object is already there, as
// when a concurrent upload of it stored it first, the upload succeeds, as
// happens on Windows to one of two identical concurrent uploads. A
// destination that is not the object, even one of the same size, is not
// accepted.
func TestLFSTransferUploadRenameOntoExistingObject(t *testing.T) {
	t.Parallel()
	const content = "hello lfs"
	oid := oidOf(content)
	objPath := path.Join("objects", oid[0:2], oid[2:4], oid)

	cases := []struct {
		name    string
		dest    string // content already at objPath, "" for none
		wantErr bool
	}{
		{"destination holds the object", content, false},
		{"no destination", "", true},
		{"destination of another size", content + "!", true},
		{"destination of the same size but other content", "jello lfs", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr, _ := newLFSUploadTransfer(t)
			if tc.dest != "" {
				if _, err := tr.storage.Put(objPath, strings.NewReader(tc.dest)); err != nil {
					t.Fatal(err)
				}
			}
			tr.storage = failRenameStorage{tr.storage}

			err := tr.Upload(oid, int64(len(content)), strings.NewReader(content), nil)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("Upload => %v, want error %v", err, tc.wantErr)
			}
			_, gerr := tr.store.GetLFSObjectByOid(tr.ctx, tr.dbx, 1, oid)
			if recorded := gerr == nil; recorded == tc.wantErr {
				t.Errorf("object recorded = %v, want %v", recorded, !tc.wantErr)
			}
			assertNoLFSTempFiles(t, tr.cfg)
		})
	}
}

// Batch reports a recorded object present only if its file has the
// pointer's size. A short file, such as one a failed write left behind, is
// reported missing, so the client uploads it again rather than downloads
// serving it short.
func TestLFSTransferBatchShortObjectIsNotPresent(t *testing.T) {
	t.Parallel()
	tr, _ := newLFSUploadTransfer(t)
	const content = "hello lfs"
	oid := oidOf(content)
	size := int64(len(content))
	objPath := lfsObjectPath(tr.cfg, oid)
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Record the object at its real size, so only the file on disk decides
	// whether it is present.
	if err := tr.store.CreateLFSObject(tr.ctx, tr.dbx, 1, oid, size); err != nil {
		t.Fatal(err)
	}

	batch := func() bool {
		t.Helper()
		items, err := tr.Batch("upload", []transfer.BatchItem{{Pointer: transfer.Pointer{Oid: oid, Size: size}}}, nil)
		if err != nil {
			t.Fatalf("Batch: %v", err)
		}
		return items[0].Present
	}

	if err := os.WriteFile(objPath, []byte(content[:4]), 0o600); err != nil {
		t.Fatal(err)
	}
	if batch() {
		t.Error("short object reported present")
	}

	if err := os.WriteFile(objPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !batch() {
		t.Error("complete object reported missing")
	}
}

// TestLFSLockRangeRejectsInvalidCursor checks that listing locks over SSH
// refuses a cursor that is not a lock ID it hands out, rather than read it
// as the first page. The backend has no database on purpose: a refused
// cursor must not reach it.
func TestLFSLockRangeRejectsInvalidCursor(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
	dbx, err := db.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "test.db"), db.Options{})
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
// password hash: a lock holds only the owner's ID and name.
func TestLFSLockDebugLogOmitsPassword(t *testing.T) {
	t.Parallel()
	dbx, err := db.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "test.db"), db.Options{})
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

// lfsLockTestUser is a proto.User with an ID, a name and an admin flag.
type lfsLockTestUser struct {
	proto.User
	id    int64
	name  string
	admin bool
}

func (u lfsLockTestUser) ID() int64        { return u.id }
func (u lfsLockTestUser) Username() string { return u.name }
func (u lfsLockTestUser) IsAdmin() bool    { return u.admin }

// TestLFSLockOwnership checks that SSH lock commands see every user's locks
// and tell the caller's from other users', as the HTTP locks API does, so a
// conflicting lock is reported and another user's lock can be listed,
// refused or force-unlocked.
func TestLFSLockOwnership(t *testing.T) {
	t.Parallel()
	tr, deps := newLFSUploadTransfer(t)
	ctx := t.Context()
	// The store is stateless: this one sees what the transfer's does.
	datastore := database.New(nil)
	dbx := deps.DB

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
		users[u.name] = lfsLockTestUser{id: m.ID, name: u.name, admin: u.admin}
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
		utr.user = users[user]
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
	ownerLock, err := datastore.GetLFSLockForPath(ctx, dbx, 1, "a.bin")
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
	otherLock, err := datastore.GetLFSLockForPath(ctx, dbx, 1, "b.bin")
	if err != nil {
		t.Fatalf("other lock: %v", err)
	}
	expect("other unlock own", session("other", unlock(strconv.FormatInt(otherLock.ID, 10))), pkt("status 200\n"))
}

// TestLFSLockFromIDRejectsInvalidID checks that unlocking by an ID that is
// not a number is a client error (400) rather than an internal one (500).
// The backend has no database on purpose: a refused ID must not reach it.
func TestLFSLockFromIDRejectsInvalidID(t *testing.T) {
	t.Parallel()
	l := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{ctx: context.Background(), op: lfs.OperationUpload},
		user:        lfsTestUser{},
	}

	for _, id := range []string{"abc", "", "1x", "99999999999999999999"} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
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

// Batch only reads: for an object on disk but not recorded (an upload
// between storing and recording it), it records nothing, in download
// sessions too. The size is checked against the file, and an upload session
// reports the object missing so the upload records it.
func TestLFSTransferBatchDoesNotRecordObjects(t *testing.T) {
	t.Parallel()
	const content = "hello lfs"
	oid := oidOf(content)
	size := int64(len(content))

	cases := []struct {
		op   string
		size int64
		want bool
	}{
		{lfs.OperationDownload, 0, false},
		{lfs.OperationDownload, size + 1, false},
		{lfs.OperationDownload, size, true},
		{lfs.OperationUpload, 0, false},
		{lfs.OperationUpload, size, false},
	}
	for _, tc := range cases {
		tr, _ := newLFSUploadTransfer(t)
		tr.op = tc.op
		p := lfsObjectPath(tr.cfg, oid)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		items, err := tr.Batch(tc.op, []transfer.BatchItem{{Pointer: transfer.Pointer{Oid: oid, Size: tc.size}}}, nil)
		if err != nil {
			t.Fatalf("%s size %d: Batch: %v", tc.op, tc.size, err)
		}
		if items[0].Present != tc.want {
			t.Errorf("%s size %d: present %v, want %v", tc.op, tc.size, items[0].Present, tc.want)
		}
		if _, err := tr.store.GetLFSObjectByOid(tr.ctx, tr.dbx, 1, oid); !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("%s size %d: object recorded by Batch (err %v)", tc.op, tc.size, err)
		}
	}

	// A recorded object is present only at its recorded size.
	tr, _ := newLFSUploadTransfer(t)
	if err := tr.Upload(oid, size, strings.NewReader(content), nil); err != nil {
		t.Fatal(err)
	}
	tr.op = lfs.OperationDownload
	for _, s := range []int64{0, size} {
		items, err := tr.Batch(tr.op, []transfer.BatchItem{{Pointer: transfer.Pointer{Oid: oid, Size: s}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if items[0].Present != (s == size) {
			t.Errorf("recorded, size %d: present %v", s, items[0].Present)
		}
	}
}

// Transfer and Authenticate refuse missing dependencies before they answer
// the client, rather than fail at the first command that needs them.
func TestNilDependencies(t *testing.T) {
	t.Parallel()
	_, deps := newLFSUploadTransfer(t)
	noConfig, noDB, noStore := deps, deps, deps
	noConfig.Config = nil
	noDB.DB = nil
	noStore.Store = nil
	for _, tc := range []struct {
		name string
		run  func(out io.Writer) error
		want error
	}{
		{"transfer nil config", func(out io.Writer) error {
			return Transfer(t.Context(), strings.NewReader(""), out, noConfig, lfsTestRepo{}, lfsTestUser{}, lfs.OperationUpload)
		}, config.ErrNilConfig},
		{"transfer nil database", func(out io.Writer) error {
			return Transfer(t.Context(), strings.NewReader(""), out, noDB, lfsTestRepo{}, lfsTestUser{}, lfs.OperationUpload)
		}, errNilDeps},
		{"transfer nil store", func(out io.Writer) error {
			return Transfer(t.Context(), strings.NewReader(""), out, noStore, lfsTestRepo{}, lfsTestUser{}, lfs.OperationUpload)
		}, errNilDeps},
		{"authenticate nil config", func(out io.Writer) error {
			return Authenticate(out, nil, nil, log.New(io.Discard), lfsAuthRepo{}, lfsAuthUser{}, lfs.OperationDownload)
		}, config.ErrNilConfig},
		{"authenticate nil signer", func(out io.Writer) error {
			return Authenticate(out, config.DefaultConfig(), nil, log.New(io.Discard), lfsAuthRepo{}, lfsAuthUser{}, lfs.OperationDownload)
		}, jwk.ErrHostKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			if err := tc.run(&out); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("answered the client: %q", out.String())
			}
		})
	}
}

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

// lfsIDRepo and lfsIDUser carry the ID of a row in the test database.
type lfsIDRepo struct {
	proto.Repository
	id int64
}

func (r lfsIDRepo) ID() int64  { return r.id }
func (lfsIDRepo) Name() string { return "repo" }

type lfsIDUser struct {
	proto.User
	id int64
}

func (u lfsIDUser) ID() int64 { return u.id }

// TestLFSLockLookupsDoNotWaitForWriter checks that reading LFS locks does not
// take the SQLite write lock: the lookups must succeed while another
// connection holds a write transaction open, instead of waiting for it
// (busy_timeout) and then failing with "database is locked".
func TestLFSLockLookupsDoNotWaitForWriter(t *testing.T) {
	t.Parallel()
	e := backendtest.New(t)
	ctx, dbx, s := e.Ctx, e.DB, e.Store

	var repoID, userID int64
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		u, err := s.FindUserByUsername(ctx, tx, "admin")
		if err != nil {
			return err
		}
		userID = u.ID
		if _, err := s.CreateRepo(ctx, tx, models.Repo{Name: "repo", UserID: userID}); err != nil {
			return err
		}
		r, err := s.GetRepoByName(ctx, tx, "repo")
		if err != nil {
			return err
		}
		repoID = r.ID
		_, err = s.CreateLFSLockForUser(ctx, tx, repoID, userID, "foo.png", "refs/heads/main")
		return err
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	newBackend := func(ctx context.Context) *lfsLockBackend {
		return &lfsLockBackend{
			lfsTransfer: &lfsTransfer{
				ctx:    ctx,
				dbx:    dbx,
				store:  s,
				logger: log.New(io.Discard),
				repo:   lfsIDRepo{id: repoID},
				op:     lfs.OperationUpload,
			},
			user: lfsIDUser{id: userID},
		}
	}

	written := make(chan struct{})
	release := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		writerErr <- dbx.TransactionContext(ctx, func(tx *db.Tx) error {
			if err := s.CreateUser(ctx, tx, "writer", false, nil); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()
	select {
	case <-written:
	case err := <-writerErr:
		t.Fatalf("writer: %v", err)
	}

	func() {
		defer close(release)
		// Each lookup gets its own deadline, so a lookup that waits for the
		// writer fails, and does not use up the time of the lookups after
		// it. The deadline is a minute, so a lookup slowed by load is not
		// failed; one that waits for the writer fails all the same, at
		// busy_timeout or at the deadline.
		var cancels []context.CancelFunc
		defer func() {
			for _, cancel := range cancels {
				cancel()
			}
		}()
		backend := func() *lfsLockBackend {
			c, cancel := context.WithTimeout(ctx, time.Minute)
			cancels = append(cancels, cancel)
			return newBackend(c)
		}

		lock, err := backend().FromPath("foo.png")
		if err != nil {
			t.Errorf("FromPath: %v", err)
		} else if _, err := backend().FromID(lock.ID()); err != nil {
			t.Errorf("FromID: %v", err)
		}
		var n int
		if _, err := backend().Range("", 10, func(transfer.Lock) error { n++; return nil }); err != nil || n != 1 {
			t.Errorf("Range = %d locks, %v; want 1, nil", n, err)
		}
	}()

	if err := <-writerErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
}

// untouchedStore is a Store whose every method panics, for a session that
// must not reach the database.
type untouchedStore struct{ Store }

// lfsTestRepo is a proto.Repository with just an ID and a name. Anything
// else panics, which is what a refused command must never reach.
type lfsTestRepo struct{ proto.Repository }

func (lfsTestRepo) ID() int64    { return 1 }
func (lfsTestRepo) Name() string { return "repo1" }

// lfsTestUser is a proto.User that only needs to be non-nil.
type lfsTestUser struct{ proto.User }

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// A download session needs only read access, so it must not store objects
// or take locks, even though the transfer processor accepts put-object and
// lock in any session. The store and database are left out of the context on
// purpose: a refused command must not reach them.
func TestLFSTransferDownloadSessionRefusesWrites(t *testing.T) {
	t.Parallel()
	data := "hello"
	sum := sha256.Sum256([]byte(data))
	oid := hex.EncodeToString(sum[:])

	var in strings.Builder
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("put-object "+oid+"\n") + pkt(fmt.Sprintf("size=%d\n", len(data))) + "0001")
	in.WriteString(pkt(data) + "0000")
	in.WriteString(pkt("lock\n") + pkt("path=foo.png\n") + pkt("refname=refs/heads/main\n") + "0000")
	in.WriteString(pkt("quit\n") + "0000")

	tr, deps := newLFSUploadTransfer(t)
	cfg := tr.cfg
	// Nothing a refused command does reaches the store.
	deps.Store = untouchedStore{}
	deps.Logger = log.New(io.Discard)
	var out bytes.Buffer
	if err := Transfer(t.Context(), strings.NewReader(in.String()), &out, deps, lfsTestRepo{}, lfsTestUser{}, lfs.OperationDownload); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	var got []string
	for _, m := range regexp.MustCompile(`status (\d{3})`).FindAllStringSubmatch(out.String(), -1) {
		got = append(got, m[1])
	}
	// version, put-object, lock, quit. The object data is consumed, so it
	// is not read back as more commands.
	want := []string{"200", "403", "403", "200"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v\noutput: %q", got, want, out.String())
	}

	if _, err := os.Stat(filepath.Join(cfg.DataPath, "lfs", "1", "objects")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("object storage was written in a download session: %v", err)
	}
}

func TestLFSLockBackendDownloadSessionRefusesWrites(t *testing.T) {
	t.Parallel()
	l := &lfsLockBackend{lfsTransfer: &lfsTransfer{op: lfs.OperationDownload}, user: lfsTestUser{}}
	if _, err := l.Create("foo.png", "refs/heads/main"); !errors.Is(err, transfer.ErrForbidden) {
		t.Errorf("Create: got %v, want ErrForbidden", err)
	}
	if err := l.Unlock(&lfsLock{}); !errors.Is(err, transfer.ErrForbidden) {
		t.Errorf("Unlock: got %v, want ErrForbidden", err)
	}
}

// The SSH lock backend must refuse the same paths the HTTP locks API does,
// before it reaches the database: the backend has no store or database here,
// so a path that got through would panic instead of creating a lock row.
func TestLFSLockBackendRejectsInvalidPath(t *testing.T) {
	t.Parallel()
	l := &lfsLockBackend{lfsTransfer: &lfsTransfer{op: lfs.OperationUpload}, user: lfsTestUser{}}
	for _, p := range []string{
		"",
		"./a.bin",
		"dir/../a.bin",
		"../a.bin",
		"/etc/x",
		"a//b.bin",
		"a\x00b.bin",
		strings.Repeat("a", lfs.MaxLockPathLength+1),
	} {
		_, err := l.Create(p, "refs/heads/main")
		if !errors.Is(err, transfer.ErrParseError) || !errors.Is(err, lfs.ErrInvalidLockPath) {
			t.Errorf("Create(%q): got %v, want ErrParseError wrapping ErrInvalidLockPath", p, err)
		}
	}
}

// lockTestUser is a proto.User with just an ID.
type lockTestUser struct {
	proto.User
	id int64
}

func (u lockTestUser) ID() int64 { return u.id }

// A lock taken over SSH for a repository deleted meanwhile fails on the
// lock's foreign key. That must be reported as the repository being gone,
// not as a conflict with an existing lock.
func TestLFSLockOnDeletedRepository(t *testing.T) {
	t.Parallel()
	e := backendtest.New(t)
	ctx, cfg, dbx, st := e.Ctx, e.Cfg, e.DB, e.Store

	admin, err := st.FindUserByUsername(ctx, dbx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRepo(ctx, dbx, models.Repo{Name: "repo1", UserID: admin.ID}); err != nil {
		t.Fatal(err)
	}
	m, err := st.GetRepoByName(ctx, dbx, "repo1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRepoByName(ctx, dbx, "repo1"); err != nil {
		t.Fatal(err)
	}

	locks := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{
			ctx:    ctx,
			cfg:    cfg,
			dbx:    dbx,
			store:  st,
			logger: log.New(io.Discard),
			repo:   lfsTestRepo{},
			op:     lfs.OperationUpload,
		},
		user: lockTestUser{id: admin.ID},
	}
	if m.ID != (lfsTestRepo{}).ID() {
		t.Fatalf("repository ID %d, want %d", m.ID, (lfsTestRepo{}).ID())
	}

	if _, err := locks.Create("foo.png", "refs/heads/main"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatalf("Create: got %v, want proto.ErrRepoNotFound", err)
	}
}

// A "version" command with no argument makes the transfer library index
// past its fields. The session ends with an error instead of the process.
func TestLFSTransferBareVersion(t *testing.T) {
	t.Parallel()

	var in strings.Builder
	in.WriteString(pkt("version\n"))
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("quit\n") + "0000")

	_, deps := newLFSUploadTransfer(t)
	deps.Store = untouchedStore{}
	var logs bytes.Buffer
	deps.Logger = log.New(&logs)
	var out bytes.Buffer
	err := Transfer(t.Context(), strings.NewReader(in.String()), &out, deps, lfsTestRepo{}, lfsTestUser{}, lfs.OperationDownload)
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("Transfer => %v, want the processor's panic as an error", err)
	}
	if strings.Contains(out.String(), "status 200") {
		t.Errorf("commands after the bare version were served\noutput: %q", out.String())
	}
	// The log has where it panicked, which the error does not.
	if !strings.Contains(logs.String(), "processCommands") {
		t.Errorf("panic logged without its stack:\n%s", logs.String())
	}
}
