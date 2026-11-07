package git

import (
	"fmt"
	"os"
	"path/filepath"

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
	return os.Rename(lp, cp)
}
