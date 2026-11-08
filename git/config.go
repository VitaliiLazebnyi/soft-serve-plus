package git

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	gcfg "github.com/go-git/go-git/v5/plumbing/format/config"
)

// Config returns the repository Git configuration.
func (r *Repository) Config() (*gcfg.Config, error) {
	cp := filepath.Join(r.Path, "config")
	f, err := os.Open(cp)
	if err != nil {
		return nil, err
	}

	defer f.Close() //nolint: errcheck
	d := gcfg.NewDecoder(f)
	cfg := gcfg.New()
	if err := d.Decode(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// SetConfig sets the repository Git configuration.
//
// Like git, it writes the new configuration to config.lock and renames it
// over config, so a failed write leaves the old file intact and readers
// never see a partial one. It fails if config.lock already exists, which
// means git or another writer is changing the configuration.
func (r *Repository) SetConfig(cfg *gcfg.Config) (err error) {
	cp := filepath.Join(r.Path, "config")
	perm := os.FileMode(0o644)
	if fi, err := os.Stat(cp); err == nil {
		perm = fi.Mode().Perm()
	}

	lp := cp + ".lock"
	f, err := os.OpenFile(lp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("lock git config: %w", err)
	}
	defer func() {
		if err != nil {
			f.Close()     //nolint: errcheck,gosec
			os.Remove(lp) //nolint: errcheck,gosec
		}
	}()

	if err := gcfg.NewEncoder(f).Encode(cfg); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return renameRetry(lp, cp, os.Rename, isRenameRetryable, configRenameRetry)
}

// configRenameRetry is how long SetConfig keeps retrying the rename of
// config.lock over config when it fails with a transient error.
var configRenameRetry = renameRetryPolicy{first: 100 * time.Millisecond, total: 3 * time.Second}

// renameRetryPolicy waits first, doubles the wait after each attempt, and
// gives up once the next wait would go past total.
type renameRetryPolicy struct {
	first, total time.Duration
}

// renameRetry renames from over to, retrying while the rename fails with an
// error retryable accepts.
//
// On Windows, replacing config fails with a permission or sharing error while
// another process has it open without FILE_SHARE_DELETE, which Go's os.Open
// and git do whenever they read it. Git for Windows retries the rename in
// that case, and so does SetConfig (see isRenameRetryable). Elsewhere no
// error is retryable and the rename is tried once.
func renameRetry(from, to string, rename func(string, string) error, retryable func(error) bool, policy renameRetryPolicy) error {
	deadline := time.Now().Add(policy.total)
	wait := policy.first
	for {
		err := rename(from, to)
		if err == nil || !retryable(err) || time.Now().Add(wait).After(deadline) {
			return err
		}
		time.Sleep(wait)
		wait *= 2
	}
}
