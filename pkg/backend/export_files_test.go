package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
)

// Older versions wrote git-daemon-export-ok for each public repository and
// removed it when the repository was made private. This version no longer
// does, so the start-up sweep removes every such file, a stale one on a
// repository made private since and one in a directory without a row
// included, and a stock git daemon pointed at the data directory exports
// none of them.
func TestRemoveDaemonExportFiles(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	for _, name := range []string{"public", "made-private"} {
		_, err := be.CreateRepository(ctx, name, defaultAdmin(t, be), proto.RepositoryOptions{})
		is.NoErr(err)
	}
	is.NoErr(os.MkdirAll(be.repoPath("orphan"), 0o755))
	var files []string
	for _, name := range []string{"public", "made-private", "orphan"} {
		f := filepath.Join(be.repoPath(name), daemonExportFile)
		is.NoErr(os.WriteFile(f, nil, 0o600))
		files = append(files, f)
	}
	is.NoErr(be.SetPrivate(ctx, "made-private", true))

	scan, err := be.scanRepoDirs(ctx)
	is.NoErr(err)
	dirs := append(scan.repos, scan.orphans...)
	n, err := removeDaemonExportFiles(dirs)
	is.NoErr(err)
	is.Equal(n, 3)
	for _, f := range files {
		_, err := os.Stat(f)
		is.True(os.IsNotExist(err))
	}

	// Nothing left: a second run removes nothing and fails nothing.
	n, err = removeDaemonExportFiles(dirs)
	is.NoErr(err)
	is.Equal(n, 0)
}

// repoCountingStore counts GetAllRepos calls.
type repoCountingStore struct {
	store.Store
	n atomic.Int32
}

func (s *repoCountingStore) GetAllRepos(ctx context.Context, h db.Handler) ([]models.Repo, error) {
	s.n.Add(1)
	return s.Store.GetAllRepos(ctx, h)
}

// The start-up checks read the rows and walk the repos directory once for
// both the orphan report and the export-file sweep. They used to do both
// twice.
func TestCheckRepositoryDirsScansOnce(t *testing.T) {
	is := is.New(t)
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx := backendTestContext(be, cfg)
	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(os.MkdirAll(be.repoPath("orphan"), 0o755))
	is.NoErr(os.WriteFile(filepath.Join(be.repoPath("repo"), daemonExportFile), nil, 0o600))
	st := &repoCountingStore{Store: be.store}
	be.store = st

	be.CheckRepositoryDirs(ctx)
	is.Equal(st.n.Load(), int32(1))
	is.True(strings.Contains(logs.String(), "repository directory has no database row"))
	_, err = os.Stat(filepath.Join(be.repoPath("repo"), daemonExportFile))
	is.True(os.IsNotExist(err))
}
