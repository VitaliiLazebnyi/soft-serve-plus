package backend

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// daemonExportFile is the file a stock git daemon, and tools such as cgit,
// look for before they export a repository. The server no longer writes it;
// the database says which repositories are private.
const daemonExportFile = "git-daemon-export-ok"

// RemoveDaemonExportFiles removes git-daemon-export-ok from every
// repository directory, those without a row included, and returns how many
// it removed. The server runs it once at start-up, before it serves
// anything. Older versions wrote the file for each public repository and
// removed it when the repository was made private; this version no longer
// keeps it in step, so a file left behind would let a stock git daemon or
// cgit pointed at the data directory export a repository made private, or
// renamed, after the upgrade. Removing every file errs on the side of
// exporting nothing.
func (d *Backend) RemoveDaemonExportFiles(ctx context.Context) (int, error) {
	rows, err := d.store.GetAllRepos(ctx, d.db)
	if err != nil {
		return 0, err
	}
	dirs := make([]string, 0, len(rows))
	for _, m := range rows {
		dirs = append(dirs, d.repoPath(m.Name))
	}
	orphans, err := d.OrphanRepositoryDirs(ctx)
	if err != nil {
		return 0, err
	}
	dirs = append(dirs, orphans...)

	var removed int
	var errs []error
	for _, dir := range dirs {
		err := os.Remove(filepath.Join(dir, daemonExportFile))
		switch {
		case err == nil:
			removed++
		case !errors.Is(err, fs.ErrNotExist):
			errs = append(errs, err)
		}
	}
	return removed, errors.Join(errs...)
}

// ReportDaemonExportFiles runs RemoveDaemonExportFiles and logs what it did.
func (d *Backend) ReportDaemonExportFiles(ctx context.Context) {
	n, err := d.RemoveDaemonExportFiles(ctx)
	if n > 0 {
		d.logger.Info("removed git-daemon-export-ok files that older versions wrote; the server no longer writes them", "removed", n)
	}
	if err != nil {
		d.logger.Error("failed to remove git-daemon-export-ok files; a stock git daemon or cgit may export repositories that are now private", "err", err)
	}
}
