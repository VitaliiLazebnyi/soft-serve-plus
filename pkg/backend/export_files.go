package backend

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// daemonExportFile is the file a stock git daemon, and tools such as cgit,
// look for before they export a repository. The server no longer writes it;
// the database says which repositories are private.
const daemonExportFile = "git-daemon-export-ok"

// removeDaemonExportFiles removes git-daemon-export-ok from each of dirs,
// every repository directory, those without a row included (see
// CheckRepositoryDirs), and returns how many it removed. It runs at every
// start-up and removes every such file, whoever made it. Older versions
// wrote the file for each public repository and removed it when the
// repository was made private; this version no longer keeps it in step, so
// a file left behind would let a stock git daemon or cgit pointed at the
// data directory export a repository made private, or renamed, after the
// upgrade. Removing every file errs on the side of exporting nothing.
func removeDaemonExportFiles(dirs []string) (int, error) {
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

// reportDaemonExportFiles runs removeDaemonExportFiles and logs what it did.
func (d *Backend) reportDaemonExportFiles(dirs []string) {
	n, err := removeDaemonExportFiles(dirs)
	if n > 0 {
		d.logger.Info("removed git-daemon-export-ok files; the server no longer writes them, and removes them at every start", "removed", n)
	}
	if err != nil {
		d.logger.Error("failed to remove git-daemon-export-ok files; a stock git daemon or cgit may export repositories that are now private", "err", err)
	}
}
