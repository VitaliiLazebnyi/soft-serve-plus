package backend

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// IncompleteLFSMaxAge is how long a temporary file of an LFS upload or mirror
// download may go without being written to before SweepIncompleteLFS takes it
// to be abandoned.
const IncompleteLFSMaxAge = 24 * time.Hour

// SweepIncompleteLFS removes the temporary files under the incomplete/
// directory of every repository's LFS storage in dataPath that were last
// modified before cutoff, and returns how many it removed.
//
// Uploads and mirror downloads write an object there before moving it into
// place, and remove it when they fail, but a process that is killed or
// crashes mid-transfer leaves it behind. A transfer still writing keeps the
// modification time current, so only files idle since cutoff are removed.
// Directories are left alone. Errors are collected and returned together; a
// file that cannot be removed does not stop the sweep.
func SweepIncompleteLFS(ctx context.Context, dataPath string, cutoff time.Time) (int, error) {
	repos, err := os.ReadDir(filepath.Join(dataPath, "lfs"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	var removed int
	var errs []error
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(errs, err)...)
		}
		if !repo.IsDir() {
			continue
		}

		dir := filepath.Join(dataPath, "lfs", repo.Name(), "incomplete")
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}

		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err)
				}
				continue
			}
			if !info.ModTime().Before(cutoff) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, fmt.Errorf("remove stale lfs temp file: %w", err))
				}
				continue
			}
			removed++
		}
	}

	return removed, errors.Join(errs...)
}
