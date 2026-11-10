package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// repoDirHolds records the repository names whose directory a create, import,
// rename or delete in progress owns while it has no database row: a create or
// import claims the directory before it records the row, a rename moves the
// directory to the new name before the renamed row is committed, and a delete
// removes the directory after the row is gone. Such a directory looks like an
// orphan but is not one, so DeleteRepository must leave it alone.
//
// Names are held case-insensitively, so a hold also covers the names that
// share its directory on a case-insensitive filesystem.
type repoDirHolds struct {
	mu sync.Mutex
	n  map[string]int
}

func repoDirKey(name string) string {
	return strings.ToLower(utils.SanitizeRepo(name))
}

// hold marks name's directory as owned by an operation in progress until the
// returned function is called. The function may be called more than once.
func (h *repoDirHolds) hold(name string) (release func()) {
	key := repoDirKey(name)
	h.mu.Lock()
	if h.n == nil {
		h.n = make(map[string]int)
	}
	h.n[key]++
	h.mu.Unlock()
	return sync.OnceFunc(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.n[key]--; h.n[key] <= 0 {
			delete(h.n, key)
		}
	})
}

// removeOrphanRepoDir removes name's directory when it is an orphan: no
// operation other than the caller, which holds name own times, holds it, no
// row names it, its name on disk is name exactly, and no repository's
// directory is the same one (isOrphanRepoPath). Otherwise it leaves the
// directory alone and returns proto.ErrRepoNotFound.
//
// Each delete holds the name before it looks for the row, so when two deletes
// of the same orphan overlap, each sees the other's hold and both return
// proto.ErrRepoNotFound. The orphan then stays until a later delete, which
// removes it.
//
// The check and the removal run under the holds' lock, which a create or
// import takes (through hold) before claiming the directory, a rename before
// its transaction and a delete before looking for its row, so none of them
// can start or finish in between.
// Nothing takes that lock while it has a database connection open, so the
// lookup here cannot wait on a connection held by someone waiting on the
// lock.
func (d *Backend) removeOrphanRepoDir(ctx context.Context, name string, own int) error {
	d.repoDirs.mu.Lock()
	defer d.repoDirs.mu.Unlock()

	if d.repoDirs.n[repoDirKey(name)] > own {
		return proto.ErrRepoNotFound
	}

	// A create may have recorded its row and let go of its hold after the
	// caller looked for the row.
	//
	// On a case-insensitive filesystem a case variant of a repository's
	// name reaches that repository's directory, which is not an orphan.
	//
	// A directory spelled exactly as name can still be the directory of a
	// repository whose name the filesystem maps onto it, so it is an orphan
	// only when no repository's directory is the same one.
	err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		_, err := d.store.GetRepoByName(ctx, tx, name)
		switch {
		case err == nil:
			return proto.ErrRepoNotFound
		case !errors.Is(err, db.ErrRecordNotFound):
			return err
		}
		if !d.repoPathExact(name) {
			return proto.ErrRepoNotFound
		}
		orphan, err := d.isOrphanRepoPath(ctx, tx, name)
		if err != nil {
			return err
		}
		if !orphan {
			return proto.ErrRepoNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}

	return d.removeRepoDir(d.repoPath(name))
}

// repoPathExact reports whether name's directory exists with every path
// component spelled exactly as in name, rather than as a variant that a
// case- or normalization-insensitive filesystem maps onto it.
func (d *Backend) repoPathExact(name string) bool {
	dir := filepath.Join(d.cfg.DataPath, "repos")
	for _, part := range strings.Split(utils.SanitizeRepo(name)+".git", "/") {
		f, err := os.Open(dir)
		if err != nil {
			return false
		}
		names, err := f.Readdirnames(-1)
		f.Close() //nolint:errcheck
		if err != nil || !slices.Contains(names, part) {
			return false
		}
		dir = filepath.Join(dir, part)
	}
	return true
}

// testHookRepoDirUnrecorded, when set, runs while name's directory exists
// with no committed row for name: in a create or import right after the
// directory is claimed, in a rename right after the directory is moved to its
// new name, inside the transaction, and in a delete right after the row's
// removal is committed, before the directory is removed.
var testHookRepoDirUnrecorded func(name string)
