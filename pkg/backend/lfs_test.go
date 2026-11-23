package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// fakeLFSClient serves every requested object from memory and records which
// objects were asked for.
type fakeLFSClient struct {
	objects map[string][]byte
	// errs holds the per-object errors the remote reports.
	errs map[string]error
	// err, if set, fails the whole download, like a failed batch request.
	err error
	// omit holds requested objects the remote leaves out of its response.
	omit map[string]bool
	// extra holds objects the remote sends without being asked for them,
	// with their content.
	extra map[lfs.Pointer][]byte
	// readErrs holds objects whose content fails with the error part way
	// through, like a download that breaks off.
	readErrs  map[string]error
	requested []string
	// onRead, if set, runs when an object's content is first read, standing
	// in for the time spent receiving it from the network.
	onRead func()
}

func (c *fakeLFSClient) Download(_ context.Context, pointers []lfs.Pointer, cb lfs.DownloadCallback) error {
	if c.err != nil {
		return c.err
	}
	for _, p := range pointers {
		c.requested = append(c.requested, p.Oid)
		if err := c.errs[p.Oid]; err != nil {
			if err := cb(p, nil, err); err != nil {
				return err
			}
			continue
		}
		if c.omit[p.Oid] {
			continue
		}
		var r io.Reader = bytes.NewReader(c.objects[p.Oid])
		if c.onRead != nil {
			r = &hookReader{r: r, hook: c.onRead}
		}
		if err := c.readErrs[p.Oid]; err != nil {
			r = io.MultiReader(io.LimitReader(r, 1), &failReader{err})
		}
		if err := cb(p, io.NopCloser(r), nil); err != nil {
			return err
		}
	}
	for p, content := range c.extra {
		if err := cb(p, io.NopCloser(bytes.NewReader(content)), nil); err != nil {
			return err
		}
	}
	return nil
}

// hookReader calls hook before the first Read.
type hookReader struct {
	r    io.Reader
	hook func()
	done bool
}

func (h *hookReader) Read(p []byte) (int, error) {
	if !h.done {
		h.done = true
		h.hook()
	}
	return h.r.Read(p)
}

// failReader fails every Read with err.
type failReader struct{ err error }

func (f *failReader) Read([]byte) (int, error) { return 0, f.err }

// lfsTest is a repository whose history holds LFS pointers, and a client
// that serves their content.
type lfsTest struct {
	be       *Backend
	ctx      context.Context
	repo     proto.Repository
	pointers []lfs.Pointer
	client   *fakeLFSClient
	// work is the work tree the repository is pushed from.
	work string
}

// newLFSTest creates the repository "lfs-repo" with one LFS pointer file per
// object, numbered "lfs object 0" and up.
func newLFSTest(t *testing.T, objects int) *lfsTest {
	t.Helper()
	return newLFSTestWith(t, objects, nil)
}

// newLFSTestWith is newLFSTest, with extra, if set, called to add more files
// to the work tree before it is committed.
func newLFSTestWith(t *testing.T, objects int, extra func(work string, pointers []lfs.Pointer)) *lfsTest {
	t.Helper()
	return newLFSTestWithQuery(t, objects, extra, "")
}

// newLFSTestWithQuery is newLFSTestWith on a backend whose data source has
// query added (see newTestBackendWithQuery).
func newLFSTestWithQuery(t *testing.T, objects int, extra func(work string, pointers []lfs.Pointer), query string) *lfsTest {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	be, _ := newTestBackendWithQuery(t, nil, query)
	ctx := context.Background()
	repo, err := be.CreateRepository(ctx, "lfs-repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	lt := &lfsTest{be: be, ctx: ctx, repo: repo, client: &fakeLFSClient{objects: map[string][]byte{}}, work: work}
	for i := 0; i < objects; i++ {
		content := []byte(fmt.Sprintf("lfs object %d", i))
		p, err := lfs.GeneratePointer(bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		lt.client.objects[p.Oid] = content
		lt.pointers = append(lt.pointers, p)
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("file%d.bin", i)), []byte(p.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if extra != nil {
		extra(work, lt.pointers)
	}

	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"commit", "-q", "-m", "add lfs pointers"},
		{"push", "-q", be.repoPath("lfs-repo"), "main"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	return lt
}

// store runs StoreRepoMissingLFSObjects with the test's client.
func (lt *lfsTest) store() error {
	return lt.be.StoreRepoMissingLFSObjects(lt.ctx, lt.repo, lt.client)
}

// path returns where the object of p is stored.
func (lt *lfsTest) path(p lfs.Pointer) string {
	return filepath.Join(lt.be.cfg.DataPath, "lfs", strconv.FormatInt(lt.repo.ID(), 10), "objects", p.RelativePath())
}

// incompleteFiles returns how many temporary files are in the repository's
// incomplete directory, which storing an object creates.
func (lt *lfsTest) incompleteFiles(t *testing.T) int {
	t.Helper()
	files, err := os.ReadDir(filepath.Join(lt.be.cfg.DataPath, "lfs", strconv.FormatInt(lt.repo.ID(), 10), "incomplete"))
	if err != nil {
		t.Fatalf("reading the incomplete directory: %v", err)
	}
	return len(files)
}

// TestStoreRepoMissingLFSObjects verifies that every LFS object is downloaded
// and recorded, including the final partial batch, and that a later sync only
// fetches objects that are missing on disk.
func TestStoreRepoMissingLFSObjects(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 21} {
		t.Run(fmt.Sprintf("%d objects", n), func(t *testing.T) {
			t.Parallel()
			lt := newLFSTest(t, n)

			if err := lt.store(); err != nil {
				t.Fatal(err)
			}
			if got := len(lt.client.requested); got != n {
				t.Fatalf("len(lt.client.requested) = %v, want %v", got, n)
			} // every LFS object is downloaded

			for _, p := range lt.pointers {
				_, err := lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
				if err != nil {
					t.Fatal(err)
				} // every LFS object is recorded
				got, err := os.ReadFile(lt.path(p))
				if err != nil {
					t.Fatal(err)
				}
				if got, want := got, lt.client.objects[p.Oid]; !reflect.DeepEqual(got, want) {
					t.Fatalf("got = %v, want %v", got, want)
				} // every LFS object is stored
			}

			// A second sync finds everything on disk and recorded.
			lt.client.requested = nil
			if err := lt.store(); err != nil {
				t.Fatal(err)
			}
			if got := len(lt.client.requested); got != 0 {
				t.Fatalf("len(lt.client.requested) = %v, want %v", got, 0)
			} // nothing is downloaded again

			// An object that is recorded but gone from disk is fetched again.
			p := lt.pointers[0]
			if err := os.Remove(lt.path(p)); err != nil {
				t.Fatal(err)
			}
			if err := lt.store(); err != nil {
				t.Fatal(err)
			}
			if got, want := lt.client.requested, ([]string{p.Oid}); !reflect.DeepEqual(got, want) {
				t.Fatalf("lt.client.requested = %v, want %v", got, want)
			}
			got, err := os.ReadFile(lt.path(p))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := got, lt.client.objects[p.Oid]; !reflect.DeepEqual(got, want) {
				t.Fatalf("got = %v, want %v", got, want)
			}
		})
	}
}

// TestStoreRepoMissingLFSObjectsNoTxDuringDownload verifies that receiving an
// object's content doesn't hold a database write transaction, so other
// writers aren't blocked for as long as the download takes.
func TestStoreRepoMissingLFSObjectsNoTxDuringDownload(t *testing.T) {
	t.Parallel()
	// The database is in rollback-journal mode, where an open read
	// transaction keeps a write from committing too, so the probe below
	// catches either kind. In WAL mode it would catch only a write one.
	const journal = "&_pragma=journal_mode(DELETE)"
	lt := newLFSTestWithQuery(t, 1, nil, journal)

	// probe is a second handle on the database that never waits for a
	// lock: with busy_timeout 0, its write transaction fails at once with
	// "database is locked" while any connection holds a write transaction,
	// or an open read transaction (then at COMMIT), and succeeds otherwise.
	// Unlike a write bounded by a deadline, it doesn't depend on how fast
	// the machine is.
	probe, err := db.Open(lt.ctx, "sqlite", filepath.Join(lt.be.cfg.DataPath, "test.db")+"?_pragma=busy_timeout(0)"+journal, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { probe.Close() }) //nolint:errcheck

	var called bool
	var probeErr, writeErr error
	lt.client.onRead = func() {
		called = true
		probeErr = probe.TransactionContext(lt.ctx, func(*db.Tx) error { return nil })
		if probeErr != nil {
			// A write through the backend would wait for the open
			// transaction, which this goroutine holds, until it gives up.
			return
		}
		// The deadline only keeps a regression from hanging the test; the
		// probe already showed that no transaction holds the lock.
		wctx, cancel := context.WithTimeout(lt.ctx, time.Minute)
		defer cancel()
		writeErr = lt.be.store.SetAllowKeylessAccess(wctx, lt.be.db, false)
	}

	if err := lt.store(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("not true: called")
	} // the content was read
	if probeErr != nil {
		t.Fatal(probeErr)
	} // no write transaction, nor a read one, is open while the object downloads
	if writeErr != nil {
		t.Fatal(writeErr)
	} // so another writer isn't blocked
	_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), lt.pointers[0].Oid)
	if err != nil {
		t.Fatal(err)
	}
}

// TestStoreRepoMissingLFSObjectsRepoDeleted verifies that a sync fails, rather
// than keep storing objects, once the repository's database row is gone.
// Recording an object then violates the foreign key, which must not be taken
// for an object recorded already.
func TestStoreRepoMissingLFSObjectsRepoDeleted(t *testing.T) {
	t.Parallel()
	for _, onDisk := range []bool{false, true} {
		t.Run(fmt.Sprintf("on disk %v", onDisk), func(t *testing.T) {
			t.Parallel()
			lt := newLFSTest(t, 1)

			if onDisk {
				// The object is on disk but not recorded, so it is only recorded.
				p := lt.pointers[0]
				if err := os.MkdirAll(filepath.Dir(lt.path(p)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(lt.path(p), lt.client.objects[p.Oid], 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if err := lt.be.store.DeleteRepoByName(lt.ctx, lt.be.db, lt.repo.Name()); err != nil {
				t.Fatal(err)
			}
			if lt.store() == nil {
				t.Fatal("not true: lt.store() != nil")
			} // the sync fails
		})
	}
}

// TestStoreRepoMissingLFSObjectsObjectError verifies that an object the
// remote reports an error for is skipped, the other objects are still
// stored, and the sync reports ErrLFSObjectsUnavailable.
func TestStoreRepoMissingLFSObjectsObjectError(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 2)
	missing, ok := lt.pointers[0], lt.pointers[1]
	lt.client.errs = map[string]error{missing.Oid: errors.New("object does not exist")}

	err := lt.store()
	if !errors.Is(err, ErrLFSObjectsUnavailable) {
		t.Fatal("not true: errors.Is(err, ErrLFSObjectsUnavailable)")
	} // the missing object is reported
	if got := err.Error(); got != "some lfs objects are unavailable: 1" {
		t.Fatalf("err.Error() = %v, want %v", got, "some lfs objects are unavailable: 1")
	} // as a count; each oid is logged on its own
	if got := len(lt.client.requested); got != 2 {
		t.Fatalf("len(lt.client.requested) = %v, want %v", got, 2)
	} // every object is attempted

	_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), ok.Oid)
	if err != nil {
		t.Fatal(err)
	} // the available object is recorded
	_, err = os.Stat(lt.path(ok))
	if err != nil {
		t.Fatal(err)
	} // and stored
	_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), missing.Oid)
	if err == nil {
		t.Fatal("not true: err != nil")
	} // the missing object is not recorded
}

// TestStoreRepoMissingLFSObjectsReadError verifies that a download that
// breaks off part way, which a later try may get past, stops the sync
// instead of being counted as an unavailable object, and leaves neither the
// object nor a temporary file behind.
func TestStoreRepoMissingLFSObjectsReadError(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 1)
	broken := lt.pointers[0]
	lt.client.readErrs = map[string]error{broken.Oid: errors.New("connection reset")}

	err := lt.store()
	if err == nil {
		t.Fatal("not true: err != nil")
	} // the sync fails
	if errors.Is(err, ErrLFSObjectsUnavailable) {
		t.Fatal("not true: !errors.Is(err, ErrLFSObjectsUnavailable)")
	} // rather than skipping the object
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatal("not true: strings.Contains(err.Error(), \"connection reset\")")
	} // with the download's error
	_, err = os.Stat(lt.path(broken))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	} // the object is not stored
	if got := lt.incompleteFiles(t); got != 0 {
		t.Fatalf("lt.incompleteFiles(t) = %v, want %v", got, 0)
	} // and no temporary file is left behind
}

// TestFetchImportLFSObjects verifies that an import succeeds when the remote
// lacks some LFS objects, and fails when fetching them fails otherwise.
func TestFetchImportLFSObjects(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 2)

	var logs bytes.Buffer
	lt.be.logger = log.New(&logs)
	lt.client.errs = map[string]error{lt.pointers[0].Oid: errors.New("object does not exist")}
	if err := lt.be.fetchImportLFSObjects(lt.ctx, lt.repo, lt.client); err != nil {
		t.Fatal(err)
	} // missing objects are only logged
	if !strings.Contains(logs.String(), "will not be fetched again") {
		t.Fatal("not true: strings.Contains(logs.String(), \"will not be fetched again\")")
	} // a plain import doesn't retry them
	if !strings.Contains(logs.String(), "missing=1") {
		t.Fatal("not true: strings.Contains(logs.String(), \"missing=1\")")
	} // the warning counts them

	lt.client.err = errors.New("batch request failed")
	if lt.be.fetchImportLFSObjects(lt.ctx, lt.repo, lt.client) == nil {
		t.Fatal("not true: lt.be.fetchImportLFSObjects(lt.ctx, lt.repo, lt.client) != nil")
	} // other errors fail the import

	lt.client.err, lt.client.errs = nil, nil
	lt.client.readErrs = map[string]error{lt.pointers[0].Oid: errors.New("connection reset")}
	if lt.be.fetchImportLFSObjects(lt.ctx, lt.repo, lt.client) == nil {
		t.Fatal("not true: lt.be.fetchImportLFSObjects(lt.ctx, lt.repo, lt.client) != nil")
	} // as does a download that breaks off
}

// TestStoreRepoMissingLFSObjectsBadContent verifies that content which
// doesn't match the requested pointer is neither stored nor recorded.
func TestStoreRepoMissingLFSObjectsBadContent(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]func([]byte) []byte{
		"wrong hash": func(b []byte) []byte { return bytes.ToUpper(b) },
		"too short":  func(b []byte) []byte { return b[:len(b)-1] },
		"too long":   func(b []byte) []byte { return append(b, '!') },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lt := newLFSTest(t, 1)
			p := lt.pointers[0]
			lt.client.objects[p.Oid] = content(lt.client.objects[p.Oid])

			if !errors.Is(lt.store(), ErrLFSObjectsUnavailable) {
				t.Fatal("not true: errors.Is(lt.store(), ErrLFSObjectsUnavailable)")
			} // the object is reported
			_, err := os.Stat(lt.path(p))
			if !os.IsNotExist(err) {
				t.Fatal("not true: os.IsNotExist(err)")
			} // the content is not stored
			_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
			if err == nil {
				t.Fatal("not true: err != nil")
			} // nor recorded
			if got := lt.incompleteFiles(t); got != 0 {
				t.Fatalf("lt.incompleteFiles(t) = %v, want %v", got, 0)
			} // no temporary file is left behind
		})
	}
}

// TestStoreRepoMissingLFSObjectsOmitted verifies that a requested object the
// remote leaves out of its response is reported.
func TestStoreRepoMissingLFSObjectsOmitted(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 1)
	lt.client.omit = map[string]bool{lt.pointers[0].Oid: true}
	if !errors.Is(lt.store(), ErrLFSObjectsUnavailable) {
		t.Fatal("not true: errors.Is(lt.store(), ErrLFSObjectsUnavailable)")
	}
}

// TestStoreRepoMissingLFSObjectsUnrequested verifies that objects the remote
// sends without being asked for, valid or not, are not stored.
func TestStoreRepoMissingLFSObjectsUnrequested(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 1)

	unasked := []byte("not in the repository")
	up, err := lfs.GeneratePointer(bytes.NewReader(unasked))
	if err != nil {
		t.Fatal(err)
	}
	invalid := lfs.Pointer{Oid: "../../../../escaped", Size: 1}
	lt.client.extra = map[lfs.Pointer][]byte{up: unasked, invalid: []byte("x")}

	if err := lt.store(); err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(lt.path(lt.pointers[0]))
	if err != nil {
		t.Fatal(err)
	} // the requested object is stored
	_, err = os.Stat(lt.path(up))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	} // the unrequested one is not
	_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), up.Oid)
	if err == nil {
		t.Fatal("not true: err != nil")
	} // nor recorded
	var found []string
	if err := filepath.Walk(lt.be.cfg.DataPath, func(path string, _ os.FileInfo, err error) error {
		if filepath.Base(path) == "escaped" || filepath.Base(filepath.Dir(path)) == "escaped" {
			found = append(found, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := len(found); got != 0 {
		t.Fatalf("len(found) = %v, want %v", got, 0)
	} // the invalid pointer wrote nothing
}

// TestStoreRepoMissingLFSObjectsTruncated verifies that a file on disk whose
// size doesn't match its pointer, such as one an interrupted download left
// behind, is downloaded again rather than recorded as it is.
func TestStoreRepoMissingLFSObjectsTruncated(t *testing.T) {
	t.Parallel()
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded %v", recorded), func(t *testing.T) {
			t.Parallel()
			lt := newLFSTest(t, 1)
			p := lt.pointers[0]
			content := lt.client.objects[p.Oid]

			if recorded {
				if err := lt.be.store.CreateLFSObject(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid, p.Size); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(lt.path(p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(lt.path(p), content[:len(content)/2], 0o600); err != nil {
				t.Fatal(err)
			}

			if err := lt.store(); err != nil {
				t.Fatal(err)
			}
			if got, want := lt.client.requested, ([]string{p.Oid}); !reflect.DeepEqual(got, want) {
				t.Fatalf("lt.client.requested = %v, want %v", got, want)
			} // the object is downloaded again
			got, err := os.ReadFile(lt.path(p))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := got, content; !reflect.DeepEqual(got, want) {
				t.Fatalf("got = %v, want %v", got, want)
			} // and stored in full
			_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
			if err != nil {
				t.Fatal(err)
			} // and recorded
		})
	}
}

// TestStoreRepoMissingLFSObjectsDuplicateOid verifies that an oid two
// different pointer blobs share is requested only once.
func TestStoreRepoMissingLFSObjectsDuplicateOid(t *testing.T) {
	t.Parallel()
	lt := newLFSTestWith(t, 1, func(work string, pointers []lfs.Pointer) {
		// A trailing line makes another blob that parses to the same pointer.
		if err := os.WriteFile(filepath.Join(work, "copy.bin"), []byte(pointers[0].String()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})

	if err := lt.store(); err != nil {
		t.Fatal(err)
	}
	if got, want := lt.client.requested, ([]string{lt.pointers[0].Oid}); !reflect.DeepEqual(got, want) {
		t.Fatalf("lt.client.requested = %v, want %v", got, want)
	} // requested once
}

// TestStoreRepoMissingLFSObjectsSameOidOtherSize verifies that a pointer
// blob with the right oid but the wrong size doesn't keep the object of the
// right pointer from being fetched, whichever of the two is scanned first,
// and that later syncs neither fetch nor report anything.
func TestStoreRepoMissingLFSObjectsSameOidOtherSize(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// file is where the bad pointer goes. Blobs are scanned in tree
		// order, so it decides which of the two pointers comes first.
		file string
		// unavailable is whether the first sync reports the bad pointer.
		unavailable bool
	}{
		{"bad pointer first", "a-bad.bin", true},
		{"bad pointer last", "z-bad.bin", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lt := newLFSTestWith(t, 1, func(work string, pointers []lfs.Pointer) {
				bad := lfs.Pointer{Oid: pointers[0].Oid, Size: pointers[0].Size + 1}
				if err := os.WriteFile(filepath.Join(work, tc.file), []byte(bad.String()), 0o600); err != nil {
					t.Fatal(err)
				}
			})
			p := lt.pointers[0]

			err := lt.store()
			if tc.unavailable {
				if !errors.Is(err, ErrLFSObjectsUnavailable) {
					t.Fatal("not true: errors.Is(err, ErrLFSObjectsUnavailable)")
				} // the bad pointer can't be fetched
			} else {
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := os.ReadFile(lt.path(p))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := got, lt.client.objects[p.Oid]; !reflect.DeepEqual(got, want) {
				t.Fatalf("got = %v, want %v", got, want)
			} // the right pointer's object is stored
			obj, err := lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
			if err != nil {
				t.Fatal(err)
			}
			if got := obj.Size; got != p.Size {
				t.Fatalf("obj.Size = %v, want %v", got, p.Size)
			} // and recorded with its size

			lt.client.requested = nil
			if err := lt.store(); err != nil {
				t.Fatal(err)
			} // the next sync is clean
			if got := len(lt.client.requested); got != 0 {
				t.Fatalf("len(lt.client.requested) = %v, want %v", got, 0)
			} // and fetches nothing
		})
	}
}

// TestStoreRepoMissingLFSObjectsWrongStoredObject verifies that an object
// whose file and record agree on a size other than the pointer's, as older
// versions could leave behind, is checked and fetched again when its content
// isn't the object.
func TestStoreRepoMissingLFSObjectsWrongStoredObject(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 1)
	p := lt.pointers[0]
	content := lt.client.objects[p.Oid]

	// A cut short file, recorded with its size.
	if err := os.MkdirAll(filepath.Dir(lt.path(p)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lt.path(p), content[:len(content)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lt.be.store.CreateLFSObject(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid, p.Size-1); err != nil {
		t.Fatal(err)
	}

	if err := lt.store(); err != nil {
		t.Fatal(err)
	}
	if got, want := lt.client.requested, ([]string{p.Oid}); !reflect.DeepEqual(got, want) {
		t.Fatalf("lt.client.requested = %v, want %v", got, want)
	} // fetched again
	got, err := os.ReadFile(lt.path(p))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := got, content; !reflect.DeepEqual(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	} // stored in full
	obj, err := lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
	if err != nil {
		t.Fatal(err)
	}
	if got := obj.Size; got != p.Size {
		t.Fatalf("obj.Size = %v, want %v", got, p.Size)
	} // and recorded with its size

	lt.client.requested = nil
	if err := lt.store(); err != nil {
		t.Fatal(err)
	}
	if got := len(lt.client.requested); got != 0 {
		t.Fatalf("len(lt.client.requested) = %v, want %v", got, 0)
	}
}

// TestStoreRepoMissingLFSObjectsWrongRecordedSize verifies that an object
// whose file is right but whose record holds the size of a malformed
// pointer, as older versions could leave behind, gets its record corrected,
// so later syncs neither fetch nor report anything.
func TestStoreRepoMissingLFSObjectsWrongRecordedSize(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		file        string
		unavailable bool
	}{
		{"bad pointer first", "a-bad.bin", true},
		{"bad pointer last", "z-bad.bin", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var bad lfs.Pointer
			lt := newLFSTestWith(t, 1, func(work string, pointers []lfs.Pointer) {
				bad = lfs.Pointer{Oid: pointers[0].Oid, Size: pointers[0].Size + 1}
				if err := os.WriteFile(filepath.Join(work, tc.file), []byte(bad.String()), 0o600); err != nil {
					t.Fatal(err)
				}
			})
			p := lt.pointers[0]
			if err := os.MkdirAll(filepath.Dir(lt.path(p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(lt.path(p), lt.client.objects[p.Oid], 0o600); err != nil {
				t.Fatal(err)
			}
			if err := lt.be.store.CreateLFSObject(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid, bad.Size); err != nil {
				t.Fatal(err)
			}

			err := lt.store()
			if tc.unavailable {
				if !errors.Is(err, ErrLFSObjectsUnavailable) {
					t.Fatal("not true: errors.Is(err, ErrLFSObjectsUnavailable)")
				} // the bad pointer was tried before the record was fixed
			} else {
				if err != nil {
					t.Fatal(err)
				}
			}
			obj, err := lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
			if err != nil {
				t.Fatal(err)
			}
			if got := obj.Size; got != p.Size {
				t.Fatalf("obj.Size = %v, want %v", got, p.Size)
			} // the record is corrected

			lt.client.requested = nil
			if err := lt.store(); err != nil {
				t.Fatal(err)
			} // the next sync is clean
			if got := len(lt.client.requested); got != 0 {
				t.Fatalf("len(lt.client.requested) = %v, want %v", got, 0)
			} // and fetches nothing
		})
	}
}

// lfsScan is one run of the pointer scan.
type lfsScan struct {
	include, exclude []string
	// oids holds the oids of the pointers it found.
	oids []string
}

// recordLFSScans makes the pointer scan record each of its runs.
func recordLFSScans(t *testing.T, be *Backend) func() []lfsScan {
	t.Helper()
	var mu sync.Mutex
	var scans []lfsScan
	orig := be.searchPointerBlobs
	be.searchPointerBlobs = func(ctx context.Context, r *git.Repository, include, exclude []string, pointerChan chan<- lfs.PointerBlob, errChan chan<- error) {
		mu.Lock()
		scans = append(scans, lfsScan{include: include, exclude: exclude})
		i := len(scans) - 1
		mu.Unlock()

		found := make(chan lfs.PointerBlob)
		go orig(ctx, r, include, exclude, found, errChan)
		defer close(pointerChan)
		for p := range found {
			mu.Lock()
			scans[i].oids = append(scans[i].oids, p.Oid)
			mu.Unlock()
			select {
			case pointerChan <- p:
			case <-ctx.Done():
			}
		}
	}
	return func() []lfsScan {
		mu.Lock()
		defer mu.Unlock()
		s := scans
		scans = nil
		return s
	}
}

// sync runs SyncRepoLFSObjects with the test's client.
func (lt *lfsTest) sync() error {
	lt.client.requested = nil
	return lt.be.SyncRepoLFSObjects(lt.ctx, lt.repo, lt.client)
}

// addObject commits a pointer to a new LFS object, which the client serves,
// and pushes it to main.
func (lt *lfsTest) addObject(t *testing.T) lfs.Pointer {
	t.Helper()
	p := lt.commitObject(t)
	lt.git(t, "push", "-q", lt.be.repoPath("lfs-repo"), "HEAD:main")
	return p
}

// commitObject commits a pointer to a new LFS object, which the client
// serves, without pushing it.
func (lt *lfsTest) commitObject(t *testing.T) lfs.Pointer {
	t.Helper()
	i := len(lt.pointers)
	content := []byte(fmt.Sprintf("lfs object %d", i))
	p, err := lfs.GeneratePointer(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	lt.client.objects[p.Oid] = content
	lt.pointers = append(lt.pointers, p)
	if err := os.WriteFile(filepath.Join(lt.work, fmt.Sprintf("file%d.bin", i)), []byte(p.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lt.git(t, "add", ".")
	lt.git(t, "commit", "-q", "-m", fmt.Sprintf("add lfs object %d", i))
	return p
}

// detachHead commits a pointer to a new LFS object and points the
// repository's HEAD straight at that commit, so no ref reaches it.
func (lt *lfsTest) detachHead(t *testing.T) lfs.Pointer {
	t.Helper()
	p := lt.commitObject(t)
	lt.git(t, "push", "-q", lt.be.repoPath("lfs-repo"), "HEAD:refs/tmp/detached")
	lt.bareGit(t, "update-ref", "--no-deref", "HEAD", "refs/tmp/detached")
	lt.bareGit(t, "update-ref", "-d", "refs/tmp/detached")
	return p
}

// git runs git in the work tree and returns its output.
func (lt *lfsTest) git(t *testing.T, args ...string) string {
	t.Helper()
	return lt.gitIn(t, lt.work, args...)
}

// bareGit runs git in the repository and returns its output.
func (lt *lfsTest) bareGit(t *testing.T, args ...string) string {
	t.Helper()
	return lt.gitIn(t, lt.be.repoPath("lfs-repo"), args...)
}

// gitIn runs git in dir and returns its output.
func (lt *lfsTest) gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// lfsDir returns the LFS directory of the test's repository.
func (lt *lfsTest) lfsDir() string {
	return filepath.Join(lt.be.cfg.DataPath, "lfs", strconv.FormatInt(lt.repo.ID(), 10))
}

// markerPath returns where the tips of the last complete sync are saved.
func (lt *lfsTest) markerPath() string {
	return filepath.Join(lt.lfsDir(), lfsScannedRefsFile)
}

func oids(pointers ...lfs.Pointer) []string {
	var s []string
	for _, p := range pointers {
		s = append(s, p.Oid)
	}
	return s
}

// TestSyncRepoLFSObjectsIncremental verifies that a mirror sync only scans
// the history added since the last one, and stores the objects in it.
func TestSyncRepoLFSObjectsIncremental(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 2)
	scans := recordLFSScans(t, lt.be)

	// The first sync scans everything.
	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	got := scans()
	if got := len(got); got != 1 {
		t.Fatalf("len(got) = %v, want %v", got, 1)
	}
	if got := got[0].exclude; got != nil {
		t.Fatalf("got[0].exclude = %v, want nil", got)
	}
	if got := len(got[0].oids); got != 2 {
		t.Fatalf("len(got[0].oids) = %v, want %v", got, 2)
	}
	tip1 := lt.git(t, "rev-parse", "HEAD")
	if got, want := got[0].include, ([]string{tip1}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got[0].include = %v, want %v", got, want)
	}

	// A sync that fetched nothing scans nothing.
	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	if got := len(scans()); got != 0 {
		t.Fatalf("len(scans()) = %v, want %v", got, 0)
	}
	if got := len(lt.client.requested); got != 0 {
		t.Fatalf("len(lt.client.requested) = %v, want %v", got, 0)
	}

	// A sync after a new commit scans only that commit's objects.
	p := lt.addObject(t)
	tip2 := lt.git(t, "rev-parse", "HEAD")
	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	got = scans()
	if got := len(got); got != 1 {
		t.Fatalf("len(got) = %v, want %v", got, 1)
	}
	if got, want := got[0].include, ([]string{tip2}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got[0].include = %v, want %v", got, want)
	}
	if got, want := got[0].exclude, ([]string{tip1}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got[0].exclude = %v, want %v", got, want)
	}
	if got, want := got[0].oids, oids(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("got[0].oids = %v, want %v", got, want)
	} // only the new object is scanned
	if got, want := lt.client.requested, oids(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("lt.client.requested = %v, want %v", got, want)
	} // and fetched
	content, err := os.ReadFile(lt.path(p))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := content, lt.client.objects[p.Oid]; !reflect.DeepEqual(got, want) {
		t.Fatalf("content = %v, want %v", got, want)
	} // and stored
	_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
	if err != nil {
		t.Fatal(err)
	} // and recorded
}

// TestSyncRepoLFSObjectsFullScan verifies the cases in which a mirror sync
// scans the whole history again.
func TestSyncRepoLFSObjectsFullScan(t *testing.T) {
	t.Parallel()
	t.Run("import", func(t *testing.T) {
		t.Parallel()
		lt := newLFSTest(t, 1)
		scans := recordLFSScans(t, lt.be)
		if err := lt.store(); err != nil {
			t.Fatal(err)
		}
		if err := lt.store(); err != nil {
			t.Fatal(err)
		}
		got := scans()
		if got := len(got); got != 2 {
			t.Fatalf("len(got) = %v, want %v", got, 2)
		} // an import always scans
		if got := got[1].exclude; got != nil {
			t.Fatalf("got[1].exclude = %v, want nil", got)
		}
		if got := len(got[1].oids); got != 1 {
			t.Fatalf("len(got[1].oids) = %v, want %v", got, 1)
		} // everything

		// It leaves the tips it scanned for the first mirror sync.
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		if got := len(scans()); got != 0 {
			t.Fatalf("len(scans()) = %v, want %v", got, 0)
		}
	})

	t.Run("old tip gone", func(t *testing.T) {
		t.Parallel()
		lt := newLFSTest(t, 1)
		scans := recordLFSScans(t, lt.be)
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		scans()

		// A tip of the last sync that's no longer in the repository makes
		// the shorter scan fail.
		missing := strings.Repeat("1", 40)
		if err := writeScannedRefs(lt.markerPath(), []string{missing}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(lt.path(lt.pointers[0])); err != nil {
			t.Fatal(err)
		}
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		got := scans()
		if got := len(got); got != 2 {
			t.Fatalf("len(got) = %v, want %v", got, 2)
		}
		if got, want := got[0].exclude, ([]string{missing}); !reflect.DeepEqual(got, want) {
			t.Fatalf("got[0].exclude = %v, want %v", got, want)
		}
		if got := got[1].exclude; got != nil {
			t.Fatalf("got[1].exclude = %v, want nil", got)
		} // so it scans everything
		if got := len(got[1].oids); got != 1 {
			t.Fatalf("len(got[1].oids) = %v, want %v", got, 1)
		}
		if got, want := lt.client.requested, oids(lt.pointers[0]); !reflect.DeepEqual(got, want) {
			t.Fatalf("lt.client.requested = %v, want %v", got, want)
		} // and fetches what's missing

		// And the next sync starts from the tips it scanned.
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		if got := len(scans()); got != 0 {
			t.Fatalf("len(scans()) = %v, want %v", got, 0)
		}
	})

	t.Run("unreadable tips", func(t *testing.T) {
		t.Parallel()
		lt := newLFSTest(t, 1)
		scans := recordLFSScans(t, lt.be)
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		scans()

		if err := os.WriteFile(lt.markerPath(), []byte("not an object id\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		got := scans()
		if got := len(got); got != 1 {
			t.Fatalf("len(got) = %v, want %v", got, 1)
		}
		if got := got[0].exclude; got != nil {
			t.Fatalf("got[0].exclude = %v, want nil", got)
		}
	})

	t.Run("lfs directory removed", func(t *testing.T) {
		t.Parallel()
		lt := newLFSTest(t, 2)
		scans := recordLFSScans(t, lt.be)
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		scans()

		// The objects are gone, and the record of what was scanned with
		// them, so everything is scanned and fetched again.
		if err := os.RemoveAll(lt.lfsDir()); err != nil {
			t.Fatal(err)
		}
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		got := scans()
		if got := len(got); got != 1 {
			t.Fatalf("len(got) = %v, want %v", got, 1)
		}
		if got := got[0].exclude; got != nil {
			t.Fatalf("got[0].exclude = %v, want nil", got)
		}
		if got := len(lt.client.requested); got != 2 {
			t.Fatalf("len(lt.client.requested) = %v, want %v", got, 2)
		}
		for _, p := range lt.pointers {
			_, err := os.Stat(lt.path(p))
			if err != nil {
				t.Fatal(err)
			}
		}
	})
}

// TestSyncRepoLFSObjectsRetry verifies that objects a sync couldn't store
// are tried again by later syncs, even when nothing new was fetched.
func TestSyncRepoLFSObjectsRetry(t *testing.T) {
	t.Parallel()
	t.Run("first sync", func(t *testing.T) {
		t.Parallel()
		lt := newLFSTest(t, 2)
		scans := recordLFSScans(t, lt.be)
		p := lt.pointers[1]
		lt.client.errs = map[string]error{p.Oid: errors.New("not there")}
		if !errors.Is(lt.sync(), ErrLFSObjectsUnavailable) {
			t.Fatal("not true: errors.Is(lt.sync(), ErrLFSObjectsUnavailable)")
		}
		scans()

		lt.client.errs = nil
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		got := scans()
		if got := len(got); got != 1 {
			t.Fatalf("len(got) = %v, want %v", got, 1)
		}
		if got := got[0].exclude; got != nil {
			t.Fatalf("got[0].exclude = %v, want nil", got)
		} // still everything
		if got, want := lt.client.requested, oids(p); !reflect.DeepEqual(got, want) {
			t.Fatalf("lt.client.requested = %v, want %v", got, want)
		} // and the object is fetched
		_, err := os.Stat(lt.path(p))
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("later sync", func(t *testing.T) {
		t.Parallel()
		lt := newLFSTest(t, 1)
		scans := recordLFSScans(t, lt.be)
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		tip1 := lt.git(t, "rev-parse", "HEAD")
		scans()

		p := lt.addObject(t)
		lt.client.errs = map[string]error{p.Oid: errors.New("not there")}
		if !errors.Is(lt.sync(), ErrLFSObjectsUnavailable) {
			t.Fatal("not true: errors.Is(lt.sync(), ErrLFSObjectsUnavailable)")
		}
		scans()

		// Nothing new was fetched, but the commit with the object that
		// was unavailable is scanned again.
		lt.client.errs = nil
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		got := scans()
		if got := len(got); got != 1 {
			t.Fatalf("len(got) = %v, want %v", got, 1)
		}
		if got, want := got[0].exclude, ([]string{tip1}); !reflect.DeepEqual(got, want) {
			t.Fatalf("got[0].exclude = %v, want %v", got, want)
		}
		if got, want := got[0].oids, oids(p); !reflect.DeepEqual(got, want) {
			t.Fatalf("got[0].oids = %v, want %v", got, want)
		}
		if got, want := lt.client.requested, oids(p); !reflect.DeepEqual(got, want) {
			t.Fatalf("lt.client.requested = %v, want %v", got, want)
		}
		_, err := os.Stat(lt.path(p))
		if err != nil {
			t.Fatal(err)
		}

		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		if got := len(scans()); got != 0 {
			t.Fatalf("len(scans()) = %v, want %v", got, 0)
		}
	})
}

// TestSyncRepoLFSObjectsDetachedHead verifies that the history only a
// detached HEAD reaches is scanned, like git rev-list --all does, in a full
// scan and in a scan of the new history.
func TestSyncRepoLFSObjectsDetachedHead(t *testing.T) {
	t.Parallel()
	t.Run("full scan", func(t *testing.T) {
		t.Parallel()
		lt := newLFSTest(t, 1)
		p := lt.detachHead(t)
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		if got, want := lt.client.requested, oids(lt.pointers...); !reflect.DeepEqual(got, want) {
			t.Fatalf("lt.client.requested = %v, want %v", got, want)
		}
		_, err := os.Stat(lt.path(p))
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("new history", func(t *testing.T) {
		t.Parallel()
		lt := newLFSTest(t, 1)
		scans := recordLFSScans(t, lt.be)
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		scans()

		p := lt.detachHead(t)
		if err := lt.sync(); err != nil {
			t.Fatal(err)
		}
		got := scans()
		if got := len(got); got != 1 {
			t.Fatalf("len(got) = %v, want %v", got, 1)
		}
		if got[0].exclude == nil {
			t.Fatal("not true: got[0].exclude != nil")
		} // only the new history
		if got, want := lt.client.requested, oids(p); !reflect.DeepEqual(got, want) {
			t.Fatalf("lt.client.requested = %v, want %v", got, want)
		}
		_, err := os.Stat(lt.path(p))
		if err != nil {
			t.Fatal(err)
		}
	})
}

// TestSyncRepoLFSObjectsPeriodicFullScan verifies that a mirror sync scans
// the whole history again once the last full scan is lfsFullScanInterval
// old, which fetches objects whose files went missing, and that scans of the
// new history in between don't count as full scans.
func TestSyncRepoLFSObjectsPeriodicFullScan(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 1)
	scans := recordLFSScans(t, lt.be)
	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	scans()
	_, fullScan := readScannedRefs(lt.markerPath())
	if fullScan.IsZero() {
		t.Fatal("not true: !fullScan.IsZero()")
	}

	// A scan of the new history keeps the time of the last full scan. It
	// is set an hour back, so a scan that saved the current time shows.
	tips, _ := readScannedRefs(lt.markerPath())
	fullScan = time.Now().Add(-time.Hour)
	if err := writeScannedRefs(lt.markerPath(), tips, fullScan); err != nil {
		t.Fatal(err)
	}
	p := lt.addObject(t)
	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	got := scans()
	if got := len(got); got != 1 {
		t.Fatalf("len(got) = %v, want %v", got, 1)
	}
	if got[0].exclude == nil {
		t.Fatal("not true: got[0].exclude != nil")
	}
	tips, kept := readScannedRefs(lt.markerPath())
	if got, want := kept.Unix(), fullScan.Unix(); got != want {
		t.Fatalf("kept.Unix() = %v, want %v", got, want)
	}

	// A file that went missing isn't noticed while nothing is fetched.
	if err := os.Remove(lt.path(p)); err != nil {
		t.Fatal(err)
	}
	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	if got := len(scans()); got != 0 {
		t.Fatalf("len(scans()) = %v, want %v", got, 0)
	}

	// Until the last full scan is too old.
	if err := writeScannedRefs(lt.markerPath(), tips, time.Now().Add(-lfsFullScanInterval-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	got = scans()
	if got := len(got); got != 1 {
		t.Fatalf("len(got) = %v, want %v", got, 1)
	}
	if got := got[0].exclude; got != nil {
		t.Fatalf("got[0].exclude = %v, want nil", got)
	} // the whole history is scanned
	if got, want := lt.client.requested, oids(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("lt.client.requested = %v, want %v", got, want)
	} // and the missing file fetched
	_, err := os.Stat(lt.path(p))
	if err != nil {
		t.Fatal(err)
	}
	_, fullScan = readScannedRefs(lt.markerPath())
	if time.Since(fullScan) >= time.Hour {
		t.Fatal("not true: time.Since(fullScan) < time.Hour")
	} // and the time of the full scan saved

	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	if got := len(scans()); got != 0 {
		t.Fatalf("len(scans()) = %v, want %v", got, 0)
	}

	// A full scan in the future means the clock went back.
	if err := writeScannedRefs(lt.markerPath(), tips, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	got = scans()
	if got := len(got); got != 1 {
		t.Fatalf("len(got) = %v, want %v", got, 1)
	}
	if got := got[0].exclude; got != nil {
		t.Fatalf("got[0].exclude = %v, want nil", got)
	}
}

// TestSyncRepoLFSObjectsRewrittenHistory verifies that a scan of the new
// history after upstream replaced it with unrelated history only finds and
// fetches the objects of the new history.
func TestSyncRepoLFSObjectsRewrittenHistory(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 2)
	scans := recordLFSScans(t, lt.be)
	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	tip1 := lt.git(t, "rev-parse", "HEAD")
	scans()

	// Force-push an orphan branch over main.
	lt.git(t, "checkout", "-q", "--orphan", "rewritten")
	lt.git(t, "rm", "-rqf", ".")
	p := lt.commitObject(t)
	lt.git(t, "push", "-q", "--force", lt.be.repoPath("lfs-repo"), "HEAD:main")
	tip2 := lt.git(t, "rev-parse", "HEAD")

	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	got := scans()
	if got := len(got); got != 1 {
		t.Fatalf("len(got) = %v, want %v", got, 1)
	}
	if got, want := got[0].include, ([]string{tip2}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got[0].include = %v, want %v", got, want)
	}
	if got, want := got[0].exclude, ([]string{tip1}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got[0].exclude = %v, want %v", got, want)
	}
	if got, want := got[0].oids, oids(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("got[0].oids = %v, want %v", got, want)
	}
	if got, want := lt.client.requested, oids(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("lt.client.requested = %v, want %v", got, want)
	}
	_, err := os.Stat(lt.path(p))
	if err != nil {
		t.Fatal(err)
	}

	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	if got := len(scans()); got != 0 {
		t.Fatalf("len(scans()) = %v, want %v", got, 0)
	}
}

// TestSyncRepoLFSObjectsTagOnly verifies that the objects of history only
// a tag reaches are scanned and fetched.
func TestSyncRepoLFSObjectsTagOnly(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 1)
	scans := recordLFSScans(t, lt.be)
	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	scans()

	lt.git(t, "checkout", "-q", "-b", "side")
	p := lt.commitObject(t)
	lt.git(t, "tag", "-a", "-m", "release", "v1")
	lt.git(t, "push", "-q", lt.be.repoPath("lfs-repo"), "refs/tags/v1")

	if err := lt.sync(); err != nil {
		t.Fatal(err)
	}
	got := scans()
	if got := len(got); got != 1 {
		t.Fatalf("len(got) = %v, want %v", got, 1)
	}
	if got[0].exclude == nil {
		t.Fatal("not true: got[0].exclude != nil")
	}
	if got, want := got[0].oids, oids(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("got[0].oids = %v, want %v", got, want)
	}
	if got, want := lt.client.requested, oids(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("lt.client.requested = %v, want %v", got, want)
	}
	_, err := os.Stat(lt.path(p))
	if err != nil {
		t.Fatal(err)
	}
}

// TestWriteScannedRefsDirMode verifies that the LFS root of a repository
// that writeScannedRefs creates is 0700, like the object directories
// pkg/storage creates under it.
func TestWriteScannedRefsDirMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("windows has no unix permissions")
	}
	dir := filepath.Join(t.TempDir(), "lfs", "1")
	if err := writeScannedRefs(filepath.Join(dir, lfsScannedRefsFile), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fi.Mode().Perm(), os.FileMode(0o700); got != want {
		t.Fatalf("fi.Mode().Perm() = %v, want %v", got, want)
	}
}

// batchRecorder is an LFSClient that records each batch it is asked for and
// reports every object as unavailable, so nothing is stored.
type batchRecorder struct {
	batches [][]string
}

func (c *batchRecorder) Download(_ context.Context, pointers []lfs.Pointer, cb lfs.DownloadCallback) error {
	var oids []string
	for _, p := range pointers {
		oids = append(oids, fmt.Sprintf("%s/%d", p.Oid[:2], p.Size))
		if err := cb(p, nil, errors.New("unavailable")); err != nil {
			return err
		}
	}
	c.batches = append(c.batches, oids)
	return nil
}

// Objects are asked for lfsBatchSize at a time, each oid once per batch: a
// second pointer with an oid already in the batch waits for a later one.
func TestLFSSyncBatches(t *testing.T) {
	t.Parallel()
	client := &batchRecorder{}
	s := &lfsSync{
		ctx:      context.Background(),
		repo:     &repo{name: "r"},
		client:   client,
		logger:   log.New(io.Discard),
		batched:  make(map[string]bool),
		verified: make(map[string]bool),
	}
	oid := func(i int) string { return fmt.Sprintf("%02x%062d", i, 0) }

	for i := range lfsBatchSize + 1 {
		if err := s.queue(lfs.Pointer{Oid: oid(i), Size: 1}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			// The same oid with another size, as a malformed pointer has.
			if err := s.queue(lfs.Pointer{Oid: oid(0), Size: 2}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := len(client.batches); got != 1 {
		t.Fatalf("len(client.batches) = %v, want %v", got, 1)
	} // the first batch went when full
	if got := len(client.batches[0]); got != lfsBatchSize {
		t.Fatalf("len(client.batches[0]) = %v, want %v", got, lfsBatchSize)
	}
	if got := len(s.deferred); got != 1 {
		t.Fatalf("len(s.deferred) = %v, want %v", got, 1)
	}

	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if got := len(client.batches); got != 2 {
		t.Fatalf("len(client.batches) = %v, want %v", got, 2)
	}
	if got, want := client.batches[1], ([]string{"14/1"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("client.batches[1] = %v, want %v", got, want)
	}

	if err := s.queue(s.deferred[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := client.batches[2], ([]string{"00/2"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("client.batches[2] = %v, want %v", got, want)
	}
	if got, want := s.unavailable, lfsBatchSize+2; got != want {
		t.Fatalf("s.unavailable = %v, want %v", got, want)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	} // an empty batch asks for nothing
	if got := len(client.batches); got != 3 {
		t.Fatalf("len(client.batches) = %v, want %v", got, 3)
	}
}
