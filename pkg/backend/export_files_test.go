package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// Older versions wrote git-daemon-export-ok for each public repository and
// removed it when the repository was made private. This version no longer
// does, so the start-up sweep removes every such file, a stale one on a
// repository made private since and one in a directory without a row
// included, and a stock git daemon pointed at the data directory exports
// none of them.
func TestRemoveDaemonExportFiles(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	for _, name := range []string{"public", "made-private"} {
		_, err := be.CreateRepository(ctx, name, defaultAdmin(t, be), proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(be.repoPath("orphan"), 0o755); err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, name := range []string{"public", "made-private", "orphan"} {
		f := filepath.Join(be.repoPath(name), daemonExportFile)
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if err := be.SetPrivate(ctx, "made-private", true); err != nil {
		t.Fatal(err)
	}

	// Through the start-up check itself, so the orphans' files are seen to
	// go too, and nothing is left for a second run.
	var logs syncBuffer
	be.logger = log.New(&logs)
	be.CheckRepositoryDirs(ctx)
	for _, f := range files {
		_, err := os.Stat(f)
		if !os.IsNotExist(err) {
			t.Fatal("not true: os.IsNotExist(err)")
		}
	}
	if !strings.Contains(logs.String(), "removed=3") {
		t.Fatal("not true: strings.Contains(logs.String(), \"removed=3\")")
	}

	// A second start-up check, over every directory, finds nothing left,
	// and neither fails to scan nor fails to remove anything.
	var again syncBuffer
	be.logger = log.New(&again)
	be.CheckRepositoryDirs(ctx)
	if strings.Contains(again.String(), daemonExportFile) {
		t.Fatal("not true: !strings.Contains(again.String(), daemonExportFile)")
	}
	if strings.Contains(again.String(), "ERRO") {
		t.Fatal("not true: !strings.Contains(again.String(), \"ERRO\")")
	}
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
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx := backendTestContext(be, cfg)
	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(be.repoPath("orphan"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(be.repoPath("repo"), daemonExportFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st := &repoCountingStore{Store: be.store}
	be.store = st

	be.CheckRepositoryDirs(ctx)
	if got, want := st.n.Load(), int32(1); got != want {
		t.Fatalf("st.n.Load() = %v, want %v", got, want)
	}
	if !strings.Contains(logs.String(), "repository directory has no database row") {
		t.Fatal("not true: strings.Contains(logs.String(), \"repository directory has no database row\")")
	}
	_, err = os.Stat(filepath.Join(be.repoPath("repo"), daemonExportFile))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}
