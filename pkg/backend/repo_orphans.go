package backend

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxReportedOrphans bounds how many orphan directories reportOrphans
// names one by one.
const maxReportedOrphans = 20

// CheckRepositoryDirs runs the start-up checks of the repository
// directories, from one scan of the rows and of the repos directory: it
// reports the directories without a row (see reportOrphans) and removes the
// git-daemon-export-ok files (see removeDaemonExportFiles). The server runs
// it once at start-up, before it serves anything.
func (d *Backend) CheckRepositoryDirs(ctx context.Context) {
	scan, err := d.scanRepoDirs(ctx)
	if err != nil {
		d.logger.Error("failed to scan the repository directories; orphans are not reported, and git-daemon-export-ok files are not removed, so a stock git daemon or cgit may export repositories that are now private", "err", err)
		return
	}
	d.reportOrphans(scan.orphans)
	d.reportDaemonExportFiles(append(scan.repos, scan.orphans...))
}

// reportOrphans logs each of orphans, repository directories that have no
// database row (see OrphanRepositoryDirs).
//
// The directories are reported, not removed. A crash or a failed removal
// leaves one behind, but so does a database restored from an older backup,
// or a data source that names the wrong database, and then every newer
// repository would look like an orphan. An orphan is never served, and its
// name cannot be created, imported or renamed onto until an operator moves
// the directory away or removes it.
func (d *Backend) reportOrphans(dirs []string) {
	for i, dir := range dirs {
		if i == maxReportedOrphans {
			d.logger.Warn("more repository directories have no database row", "more", len(dirs)-i)
			return
		}
		d.logger.Warn("repository directory has no database row; it is not served, and its name stays taken until the directory is moved away or removed. A crash, a failed removal or a rename whose transaction failed leave one behind; a repository whose rename failed can be moved back by hand", "path", dir)
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
	scan, err := d.scanRepoDirs(ctx)
	return scan.orphans, err
}

// repoDirScan is what scanRepoDirs found.
type repoDirScan struct {
	// repos are the directories of the repositories in the database, and
	// orphans the ones without a row (see OrphanRepositoryDirs).
	repos, orphans []string
	// compares is how many identity comparisons the scan made.
	compares int
}

// scanRepoDirs reads every repository row and walks the repos directory
// once, for OrphanRepositoryDirs and the other start-up checks. Every
// directory whose path is a row's, or a group's above one, is matched by
// path alone, so a data directory with no orphans and no variant spellings
// costs no identity comparison, however many repositories and groups it
// has.
func (d *Backend) scanRepoDirs(ctx context.Context) (repoDirScan, error) {
	root := filepath.Join(d.cfg.DataPath, "repos")
	rows, err := d.store.GetAllRepos(ctx, d.db)
	if err != nil {
		return repoDirScan{}, err
	}
	scan := repoDirScan{repos: make([]string, 0, len(rows))}

	// The rows' directories, and the group directories above them, by
	// path and by identity.
	var compares int
	repos, groups := newDirSet(&compares), newDirSet(&compares)
	for _, m := range rows {
		rp := d.repoPath(m.Name)
		scan.repos = append(scan.repos, rp)
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
		// Paths first, in both sets: an identity comparison is made only
		// for a directory no row names exactly.
		switch {
		case groups.paths[p]:
			return nil
		case repos.paths[p]:
			return skip
		case groups.sameAs(fi):
			return nil
		case repos.sameAs(fi):
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
		return repoDirScan{}, err
	}
	scan.orphans, scan.compares = orphans, compares
	return scan, nil
}

// dirSet is a set of directories, matched by path and, failing that, by
// identity.
type dirSet struct {
	paths map[string]bool
	infos []os.FileInfo
	// compares counts the identity comparisons made.
	compares *int
}

func newDirSet(compares *int) *dirSet {
	return &dirSet{paths: make(map[string]bool), compares: compares}
}

// add adds the directory at p, by path, and by identity if it exists.
func (s *dirSet) add(p string) {
	s.paths[p] = true
	if fi, err := os.Stat(p); err == nil {
		s.infos = append(s.infos, fi)
	}
}

// sameAs reports whether the directory whose FileInfo is fi is the same
// directory as one in s.
func (s *dirSet) sameAs(fi os.FileInfo) bool {
	for _, other := range s.infos {
		*s.compares++
		if os.SameFile(fi, other) {
			return true
		}
	}
	return false
}
