package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
)

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
		if !(got[0].exclude != nil) {
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
	if !(got[0].exclude != nil) {
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
	if !(time.Since(fullScan) < time.Hour) {
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
	if !(got[0].exclude != nil) {
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
