package lfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sync"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

var (
	// ErrObjectSizeMismatch is returned by StoreObject when the data is not
	// the declared size.
	ErrObjectSizeMismatch = errors.New("object size does not match")

	// ErrObjectOidMismatch is returned by StoreObject when the SHA-256 of
	// the data is not the object ID.
	ErrObjectOidMismatch = errors.New("object content does not match oid")

	// ErrObjectRead is returned by StoreObject when reading the data
	// fails, as when the client aborts an upload, rather than storing it.
	ErrObjectRead = errors.New("read object")
)

// errReader records the first error other than io.EOF that reading r
// returns, so a failed read can be told apart from a failed write.
type errReader struct {
	r   io.Reader
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && e.err == nil {
		e.err = err
	}
	return n, err
}

// StoreObject verifies an uploaded object and stores it in strg, then records
// it in datastore for the repository.
//
// The data is written to a temporary file under incomplete/ while it is
// hashed, reading at most one byte past size so an oversized object is not
// written out in full. Only if exactly size bytes were read and their SHA-256
// is oid is the file renamed into objects/ and recorded. A concurrent upload
// of the same object renames identical bytes over the same path; where that
// rename fails, as it can on Windows, a file already in place is taken to be
// the object if it is size bytes long and hashes to oid. Uploads of the same
// object to the same repository take turns at that step, so the check never
// runs while another upload's rename is still in flight. If the object is
// already recorded, by an earlier or concurrent upload, that counts as
// success. The temporary file is always removed.
//
// A mismatch is reported as ErrObjectSizeMismatch or ErrObjectOidMismatch,
// and a failure to read r as ErrObjectRead; any other error is a storage or
// database failure. size must not be
// negative. StoreObject does not read r past size+1 bytes; draining the rest
// is up to the caller.
func StoreObject(ctx context.Context, strg storage.Storage, dbx db.Handler, datastore store.LFSStore, repoID int64, oid string, size int64, r io.Reader) error {
	randBytes := make([]byte, 12)
	if _, err := rand.Read(randBytes); err != nil {
		return fmt.Errorf("generate temp name: %w", err)
	}
	tempName := path.Join("incomplete", fmt.Sprintf("%s%x", oid, randBytes))
	defer strg.Delete(tempName) //nolint: errcheck

	hash := sha256.New()
	body := &errReader{r: r}
	written, err := strg.Put(tempName, io.TeeReader(io.LimitReader(body, size+1), hash))
	if body.err != nil {
		return fmt.Errorf("%w: %w", ErrObjectRead, body.err)
	}
	if err != nil {
		return fmt.Errorf("write object: %w", err)
	}
	if written != size {
		return fmt.Errorf("%w: expected %d bytes, got at least %d", ErrObjectSizeMismatch, size, written)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != oid {
		return fmt.Errorf("%w: expected %s, got %s", ErrObjectOidMismatch, oid, got)
	}

	// Rename takes names relative to the storage root, not the absolute path
	// the temp file happens to live at.
	pointer := Pointer{Oid: oid, Size: size}
	objPath := path.Join("objects", pointer.RelativePath())
	if err := placeObject(strg, tempName, objPath, repoID, oid, size); err != nil {
		return err
	}

	if err := datastore.CreateLFSObject(ctx, dbx, repoID, oid, size); err != nil && !errors.Is(err, db.ErrDuplicateKey) {
		return fmt.Errorf("record object: %w", err)
	}

	return nil
}

// placeObject renames the verified temporary file tempName to objPath.
//
// On Windows, renaming onto a file that is open fails with "Access is
// denied", and so does opening a file that another rename still holds open:
// MoveFileEx keeps the source open for delete until it returns, and os.Open
// does not share delete access. Two concurrent uploads of one object could
// thus see their rename fail and then fail to open the file the other had
// just put in place, and answer the upload with an error. Holding a lock per
// object across the rename and the check below keeps a rename from running
// while another upload of the object is renaming or checking it.
func placeObject(strg storage.Storage, tempName, objPath string, repoID int64, oid string, size int64) error {
	unlock := objectLocks.lock(objectKey{repoID: repoID, oid: oid})
	defer unlock()

	if err := strg.Rename(tempName, objPath); err != nil {
		// On Windows, renaming onto a file a download has open fails. If
		// objPath already holds the object, the rename counts as done. Not
		// everything that writes objects/ verifies the content, so the
		// existing file must hash to the oid; otherwise the caller gets the
		// error and the client retries.
		if !isObject(strg, objPath, oid, size) {
			return fmt.Errorf("rename object: %w", err)
		}
	}
	return nil
}

// objectKey names an object in one repository's LFS storage.
type objectKey struct {
	repoID int64
	oid    string
}

// keyedMutex is a set of mutexes, one per key in use. A key's mutex is
// dropped once nobody holds or waits for it.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[objectKey]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// objectLocks serializes the placing of each object in this process.
var objectLocks = keyedMutex{locks: map[objectKey]*keyedLock{}}

// lock locks the mutex for key and returns the function that unlocks it.
func (k *keyedMutex) lock(key objectKey) (unlock func()) {
	k.mu.Lock()
	l, ok := k.locks[key]
	if !ok {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// HasObject reports whether strg holds the object oid of size bytes: its file
// under objects/ is a regular file of that size whose SHA-256 is oid. It reads
// the whole file.
func HasObject(strg storage.Storage, oid string, size int64) bool {
	p := Pointer{Oid: oid, Size: size}
	return isObject(strg, path.Join("objects", p.RelativePath()), oid, size)
}

// isObject reports whether objPath is a regular file of size bytes whose
// SHA-256 is oid.
func isObject(strg storage.Storage, objPath, oid string, size int64) bool {
	// Check the type before opening: opening a FIFO blocks until a writer
	// shows up. The file is checked again once open, in case it changed.
	regularOfSize := func(fi fs.FileInfo, err error) bool {
		return err == nil && fi.Mode().IsRegular() && fi.Size() == size
	}
	if !regularOfSize(strg.Stat(objPath)) {
		return false
	}
	f, err := strg.Open(objPath)
	if err != nil {
		return false
	}
	defer f.Close() //nolint: errcheck
	if !regularOfSize(f.Stat()) {
		return false
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return false
	}
	return hex.EncodeToString(hash.Sum(nil)) == oid
}
