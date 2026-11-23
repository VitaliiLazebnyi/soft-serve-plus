package backend

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// Once an --lfs import has registered its repository, the owner can delete
// it and create another under the name while the LFS objects are fetched.
// A failed fetch then removes the import's own repository only: the one now
// under the name is left alone. The cleanup used to delete by name.
func TestFailedImportDeletesByIdentity(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx, cancel := context.WithCancel(asAdmin(t, be, backendTestContext(be, cfg)))
	defer cancel()
	admin := defaultAdmin(t, be)

	src := t.TempDir()
	if out, err := exec.CommandContext(context.Background(), "git", "init", "--bare", "--quiet", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	// Right after the import registered its repository: replace it, and
	// stop the import, so its LFS fetch fails.
	var replacement proto.Repository
	var origID int64
	be.afterRegister = func() {
		be.afterRegister = nil
		other := backendTestContext(be, cfg)
		orig, err := be.Repository(other, "imported")
		is.NoErr(err)
		origID = orig.ID()
		is.NoErr(be.DeleteRepository(other, "imported"))
		// What the import's LFS fetch had stored after the delete removed
		// the repository's LFS storage.
		is.NoErr(os.MkdirAll(filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(origID, 10), "objects"), 0o755))
		replacement, err = be.CreateRepository(other, "imported", admin, proto.RepositoryOptions{})
		is.NoErr(err)
		cancel()
	}

	imp := importSpec{
		name:        "imported",
		path:        be.repoPath("imported"),
		remote:      src,
		logRemote:   src,
		lfsEndpoint: "http://127.0.0.1:1/x.git",
		lfs:         &fakeLFSClient{},
	}
	is.NoErr(claimRepoPath(imp.path))
	_, err := be.importRepository(ctx, admin, imp)
	is.True(err != nil) // the fetch failed

	r, err := be.Repository(context.Background(), "imported")
	is.NoErr(err) // the repository now under the name is kept
	is.Equal(r.ID(), replacement.ID())

	// The import set its LFS endpoint in its own clone only, not in the
	// config of the repository now under the name.
	gr, err := r.Open()
	is.NoErr(err)
	lfsURL, _, err := gr.ConfigGet("lfs.url")
	is.NoErr(err)
	is.Equal(lfsURL, "")

	// The LFS storage of the import's own repository, gone with its row,
	// is removed.
	_, err = os.Stat(filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(origID, 10)))
	is.True(os.IsNotExist(err))
}
