package backend

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
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

	n, err := be.RemoveDaemonExportFiles(ctx)
	is.NoErr(err)
	is.Equal(n, 3)
	for _, f := range files {
		_, err := os.Stat(f)
		is.True(os.IsNotExist(err))
	}

	// Nothing left: a second run removes nothing and fails nothing.
	n, err = be.RemoveDaemonExportFiles(ctx)
	is.NoErr(err)
	is.Equal(n, 0)
}
