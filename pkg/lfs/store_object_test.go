package lfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// failRenameStorage is a storage whose Rename always fails, as renaming onto
// a file another upload is renaming or a download has open does on Windows.
type failRenameStorage struct{ storage.Storage }

func (failRenameStorage) Rename(string, string) error {
	return errors.New("rename: access is denied")
}

// memLFSStore records LFS objects in memory. Only the two methods
// StoreObject uses are implemented.
type memLFSStore struct {
	store.LFSStore
	objects map[string]int64
}

func (m *memLFSStore) CreateLFSObject(_ context.Context, _ db.Handler, _ int64, oid string, size int64) error {
	if _, ok := m.objects[oid]; ok {
		return db.ErrDuplicateKey
	}
	m.objects[oid] = size
	return nil
}

func (m *memLFSStore) GetLFSObjectByOid(_ context.Context, _ db.Handler, _ int64, oid string) (models.LFSObject, error) {
	size, ok := m.objects[oid]
	if !ok {
		return models.LFSObject{}, db.ErrRecordNotFound
	}
	return models.LFSObject{Oid: oid, Size: size}, nil
}

// When the rename into objects/ fails, StoreObject accepts a file already
// there only if it is the object: the right size and hashing to the oid.
// Not everything that writes objects/ verifies the content, so a file of the
// right size alone is not enough.
func TestStoreObjectRenameFailure(t *testing.T) {
	const content = "hello lfs"
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
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
			root := t.TempDir()
			local := storage.NewLocalStorage(root)
			if tc.dest != "" {
				if _, err := local.Put(objPath, strings.NewReader(tc.dest)); err != nil {
					t.Fatal(err)
				}
			}
			datastore := &memLFSStore{objects: map[string]int64{}}

			err := StoreObject(context.Background(), failRenameStorage{local}, nil, datastore, 1, oid, int64(len(content)), strings.NewReader(content))
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("StoreObject => %v, want error %v", err, tc.wantErr)
			}
			if _, recorded := datastore.objects[oid]; recorded == tc.wantErr {
				t.Errorf("object recorded = %v, want %v", recorded, !tc.wantErr)
			}
			if tc.dest != "" {
				got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(objPath)))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != tc.dest {
					t.Errorf("destination changed to %q", got)
				}
			}
			entries, err := os.ReadDir(filepath.Join(root, "incomplete"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			for _, ent := range entries {
				t.Errorf("temp file left behind: %s", ent.Name())
			}
		})
	}
}

// A failure to read the data, as when the client aborts an upload, is
// reported as ErrObjectRead, apart from storage failures, and nothing is
// stored.
func TestStoreObjectReadFailure(t *testing.T) {
	const content = "hello lfs"
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
	root := t.TempDir()
	datastore := &memLFSStore{objects: map[string]int64{}}

	r := io.MultiReader(strings.NewReader(content[:4]), iotest.ErrReader(io.ErrUnexpectedEOF))
	err := StoreObject(context.Background(), storage.NewLocalStorage(root), nil, datastore, 1, oid, int64(len(content)), r)
	if !errors.Is(err, ErrObjectRead) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("StoreObject => %v, want ErrObjectRead wrapping io.ErrUnexpectedEOF", err)
	}
	if len(datastore.objects) != 0 {
		t.Error("object recorded from a failed read")
	}
	entries, err := os.ReadDir(filepath.Join(root, "incomplete"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, ent := range entries {
		t.Errorf("temp file left behind: %s", ent.Name())
	}
}

// windowsRenameStorage mimics how Windows treats a rename while it is in
// flight: MoveFileEx holds the file open for delete until it returns, so
// another rename onto the target and an os.Open of it fail with "Access is
// denied". Each rename stays in flight for hold after the file is in place.
type windowsRenameStorage struct {
	storage.Storage
	hold time.Duration

	mu       sync.Mutex
	inFlight map[string]bool
}

func (w *windowsRenameStorage) busy(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inFlight[name]
}

func (w *windowsRenameStorage) Rename(oldName, newName string) error {
	w.mu.Lock()
	if w.inFlight[newName] {
		w.mu.Unlock()
		return errors.New("rename: Access is denied.")
	}
	w.inFlight[newName] = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.inFlight, newName)
		w.mu.Unlock()
	}()
	if err := w.Storage.Rename(oldName, newName); err != nil {
		return err
	}
	time.Sleep(w.hold)
	return nil
}

func (w *windowsRenameStorage) Open(name string) (storage.Object, error) {
	if w.busy(name) {
		return nil, errors.New("open: The process cannot access the file because it is being used by another process.")
	}
	return w.Storage.Open(name)
}

// Concurrent uploads of one object both succeed even where a rename in
// flight keeps the other upload from renaming onto or opening the object,
// as on Windows.
func TestStoreObjectConcurrentRename(t *testing.T) {
	const content = "hello lfs"
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
	root := t.TempDir()
	strg := &windowsRenameStorage{
		Storage:  storage.NewLocalStorage(root),
		hold:     20 * time.Millisecond,
		inFlight: map[string]bool{},
	}
	var mu sync.Mutex
	datastore := &memLFSStore{objects: map[string]int64{}}

	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = StoreObject(context.Background(), strg, nil, lockedLFSStore{datastore, &mu}, 1, oid, int64(len(content)), strings.NewReader(content))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("upload %d: %v", i, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(root, "objects", oid[0:2], oid[2:4], oid))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("stored content %q, want %q", got, content)
	}
	if _, ok := datastore.objects[oid]; !ok {
		t.Error("object not recorded")
	}
}

// lockedLFSStore makes a memLFSStore safe for concurrent use.
type lockedLFSStore struct {
	*memLFSStore
	mu *sync.Mutex
}

func (l lockedLFSStore) CreateLFSObject(ctx context.Context, h db.Handler, repoID int64, oid string, size int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.memLFSStore.CreateLFSObject(ctx, h, repoID, oid, size)
}
