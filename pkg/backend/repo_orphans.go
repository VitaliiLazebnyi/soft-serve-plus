package backend

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxReportedOrphans bounds how many orphan directories
// ReportOrphanRepositories names one by one.
const maxReportedOrphans = 20

// ReportOrphanRepositories logs each repository directory that has no
// database row (see OrphanRepositoryDirs). The server runs it once at
// start-up, before it serves anything.
//
// The directories are reported, not removed. A crash or a failed removal
// leaves one behind, but so does a database restored from an older backup,
// or a data source that names the wrong database, and then every newer
// repository would look like an orphan. An orphan is never served, and its
// name cannot be created, imported or renamed onto until an operator moves
// the directory away or removes it.
func (d *Backend) ReportOrphanRepositories(ctx context.Context) {
	dirs, err := d.OrphanRepositoryDirs(ctx)
	if err != nil {
		d.logger.Error("failed to look for repository directories without a database row", "err", err)
		return
	}
	for i, dir := range dirs {
		if i == maxReportedOrphans {
			d.logger.Warn("more repository directories have no database row", "more", len(dirs)-i)
			return
		}
		d.logger.Warn("repository directory has no database row; it is not served, and its name stays taken until the directory is moved away or removed", "path", dir)
	}
}

// OrphanRepositoryDirs returns the directories under the repos directory
// that look like repositories, a name ending in ".git", but are not the
// directory of any repository in the database, in lexical order. A
// directory reached through a symbolic link is compared by what it points
// to and is not descended into.
//
// A filesystem that ignores case (macOS, Windows) or Unicode normalization
// (APFS) can lead the path of a different spelling to a repository's
// directory, so a directory whose path is no row's is compared with every
// row's directory by identity before it is called an orphan.
//
// Nothing may create, rename or delete repositories meanwhile: a create or
// import in progress has its directory before its row.
func (d *Backend) OrphanRepositoryDirs(ctx context.Context) ([]string, error) {
	root := filepath.Join(d.cfg.DataPath, "repos")
	rows, err := d.store.GetAllRepos(ctx, d.db)
	if err != nil {
		return nil, err
	}

	// The rows' directories, and the group directories above them, by
	// path and by identity.
	repos, groups := newDirSet(), newDirSet()
	for _, m := range rows {
		rp := d.repoPath(m.Name)
		repos.add(rp)
		for dir := filepath.Dir(rp); dir != root && filepath.Dir(dir) != dir; dir = filepath.Dir(dir) {
			if groups.paths[dir] {
				break // and so are the ones above it
			}
			groups.add(dir)
		}
	}

	var orphans []string
	err = filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			d.logger.Warn("failed to read repository directory, not looking for orphans in it", "path", p, "err", err)
			return nil
		}
		if p == root {
			return nil
		}
		link := de.Type()&fs.ModeSymlink != 0
		if !de.IsDir() && !link {
			return nil
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			return nil
		}
		// skip leaves a directory out of the walk. SkipDir on an entry
		// that is not a directory, as a link is to WalkDir, would skip
		// the rest of its parent instead, and WalkDir does not follow a
		// link anyway.
		skip := fs.SkipDir
		if link {
			skip = nil
		}
		switch {
		case groups.has(p, fi):
			return nil
		case repos.has(p, fi):
			return skip
		case strings.HasSuffix(de.Name(), ".git"):
			orphans = append(orphans, p)
			return skip
		default:
			// A group directory left with no repository, or something an
			// operator put there; orphans may be further down.
			return nil
		}
	})
	if err != nil {
		return nil, err
	}
	return orphans, nil
}

// dirSet is a set of directories, matched by path and, failing that, by
// identity.
type dirSet struct {
	paths map[string]bool
	infos []os.FileInfo
}

func newDirSet() *dirSet {
	return &dirSet{paths: make(map[string]bool)}
}

// add adds the directory at p, by path, and by identity if it exists.
func (s *dirSet) add(p string) {
	s.paths[p] = true
	if fi, err := os.Stat(p); err == nil {
		s.infos = append(s.infos, fi)
	}
}

// has reports whether the directory at p, whose FileInfo is fi, is in s.
func (s *dirSet) has(p string, fi os.FileInfo) bool {
	if s.paths[p] {
		return true
	}
	for _, other := range s.infos {
		if os.SameFile(fi, other) {
			return true
		}
	}
	return false
}
