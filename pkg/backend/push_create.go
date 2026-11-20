package backend

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	gitsvc "github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// pushGates orders pushes to a repository against the removal of a
// repository created by a push that never sent refs. A push holds its
// repository's gate shared while it runs (BeginPush), and the removal holds
// it exclusively from its last check for refs until the repository is gone
// (RemovePushCreatedRepository). Without it, a push over SSH could write
// refs after the removal checked for them and before it deleted the
// directory, and the pushed refs would be lost.
type pushGates struct {
	mu    sync.Mutex
	gates map[string]*pushGate
}

// pushGate is one repository's gate. users counts the callers holding or
// waiting for it, so it can be dropped from the map when the last one is
// done.
type pushGate struct {
	rw    sync.RWMutex
	users int
}

// acquire returns the gate for name, creating it if needed, counted as used
// until release.
func (p *pushGates) acquire(name string) *pushGate {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gates == nil {
		p.gates = make(map[string]*pushGate)
	}
	g, ok := p.gates[name]
	if !ok {
		g = &pushGate{}
		p.gates[name] = g
	}
	g.users++
	return g
}

// release undoes acquire.
func (p *pushGates) release(name string, g *pushGate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	g.users--
	if g.users == 0 {
		delete(p.gates, name)
	}
}

// testHookBeforePushCreatedDelete, when set, runs in
// RemovePushCreatedRepository after the check for refs and before the
// transaction that checks the rest and deletes the repository, with the
// repository's gate held.
var testHookBeforePushCreatedDelete func()

// BeginPush marks a push to the existing repository repo as running until
// the returned function is called; RemovePushCreatedRepository does not
// remove a repository while a push to it runs. If a removal of the
// repository has started, BeginPush waits for it to end, and then returns
// proto.ErrRepoNotFound if the repository is gone or its name now belongs
// to another repository.
func (d *Backend) BeginPush(ctx context.Context, repo proto.Repository) (func(), error) {
	name := utils.SanitizeRepo(repo.Name())
	g := d.pushGates.acquire(name)
	g.rw.RLock()
	done := func() {
		g.rw.RUnlock()
		d.pushGates.release(name, g)
	}

	cur, err := d.Repository(ctx, name)
	if err != nil {
		done()
		return nil, err
	}
	if cur.ID() != repo.ID() {
		done()
		return nil, proto.ErrRepoNotFound
	}
	return done, nil
}

// RemovePushCreatedRepository deletes created, a repository a push to a
// missing name created, if it was never used, and reports whether it did.
// It is the one cleanup of push-to-create for both transports: HTTP calls it
// when the push failed, or never arrived, without any ref (see pkg/web), and
// SSH when the push that created the repository failed. It is kept if:
//
//   - a push to it is running (see BeginPush);
//   - it has a ref, in which case its HEAD is pointed at a pushed branch, as
//     after a push that succeeded: receive-pack writes the refs before the
//     hooks run, so a push can fail after its refs landed;
//   - its name now belongs to another repository;
//   - its project name, description, visibility, hidden or mirror flag or
//     owner differ from when it was created;
//   - it has a collaborator, a webhook, an LFS object or an LFS lock;
//   - any of this cannot be checked.
//
// The repository's settings are compared with created field by field: the
// one-second resolution of repos.updated_at could not tell a change from
// the creation.
//
// Once the row is gone, a directory the delete could not remove is tried
// again for a few seconds (see removePushCreatedDir).
func (d *Backend) RemovePushCreatedRepository(ctx context.Context, created proto.Repository) (bool, error) {
	name := utils.SanitizeRepo(created.Name())
	g := d.pushGates.acquire(name)
	defer d.pushGates.release(name, g)
	if !g.rw.TryLock() {
		return false, nil // a push is running; it may write refs
	}
	defer g.rw.Unlock()

	// Checked with the gate held: no push can start until the repository
	// is gone, so none can write a ref after this check.
	// What the refs look like before the check for refs, so the retry
	// below can tell a ref written since from what was always there: a
	// lingering git process that writes one after the check is then still
	// seen.
	rp := d.repoPath(name)
	refs := readRefState(rp)
	if hasRefs(rp) {
		d.headOnPushedBranch(ctx, name, created)
		return false, nil
	}

	if testHookBeforePushCreatedDelete != nil {
		testHookBeforePushCreatedDelete()
	}

	// The rest is checked in the transaction that deletes the row, so a
	// setting, collaborator or webhook added meanwhile, or another
	// repository now under the name, is seen.
	err := d.deleteRepository(ctx, name, func(tx *db.Tx, m models.Repo) (bool, error) {
		return d.pushCreatedRepositoryUsed(ctx, tx, m, created)
	})
	switch {
	case errors.Is(err, errRepoKept):
		return false, nil
	case err != nil:
		return false, err
	}
	d.removePushCreatedDir(name, rp, refs)
	return true, nil
}

// headOnPushedBranch points the HEAD of created, kept because a failed push
// wrote refs to it, at a pushed branch, as after a push that succeeded. It
// does so only while the name still belongs to created: another repository
// renamed or created under it meanwhile is not touched.
func (d *Backend) headOnPushedBranch(ctx context.Context, name string, created proto.Repository) {
	m, err := d.store.GetRepoByName(ctx, d.db, name)
	if err != nil || m.ID != created.ID() {
		return
	}
	if err := gitsvc.EnsureDefaultBranch(ctx, d.repoPath(name)); err != nil {
		d.logger.Warn("failed to set the default branch of a repository created by a push", "repo", name, "err", err)
	}
}

// retryPolicy waits first, doubles the wait after each attempt, and gives up
// once the sum of the waits would go past total. Only the waits count, not
// the time the attempts take, so the number of attempts is the same however
// slow the host is.
type retryPolicy struct {
	first, total time.Duration
}

// pushCreatedDirRetry is how removePushCreatedDir retries by default: waits
// of 100ms to 1.6s, 3.1s in all.
var pushCreatedDirRetry = retryPolicy{first: 100 * time.Millisecond, total: 5 * time.Second}

// removePushCreatedDir removes rp, the directory of the push-created
// repository name whose row is gone, if the delete could not. On Windows the
// git process of the failed push can hold the directory open for a moment
// after the push ended. Nothing else uses the directory meanwhile: while it
// is there, its name can be neither created, imported nor renamed onto, and
// a delete needs a row. That lingering git process can still write refs,
// though, so a directory whose refs changed since refs, read before the
// delete, is left in place, without a row, and reported at start-up rather
// than removed with what was pushed.
func (d *Backend) removePushCreatedDir(name, rp string, refs refState) {
	policy := d.pushCreatedRetry
	var waited time.Duration
	for wait := policy.first; ; wait *= 2 {
		if _, err := os.Lstat(rp); errors.Is(err, fs.ErrNotExist) {
			return
		}
		if refsWritten(refs, readRefState(rp)) {
			d.logger.Error("a failed push wrote refs to the repository after its row was deleted; leaving its directory, which is reported at start-up", "repo", name, "path", rp)
			return
		}
		if waited+wait > policy.total {
			d.logger.Error("failed to remove the directory of a repository created by a failed push; it is reported at start-up", "repo", name, "path", rp)
			return
		}
		time.Sleep(wait)
		waited += wait
		if err := d.removeRepoDir(rp); err != nil {
			d.logger.Debug("failed to remove the directory of a repository created by a failed push, retrying", "repo", name, "err", err)
		}
	}
}

// refState is what a repository's ref storage holds on disk: the loose ref
// files and their contents, packed-refs, and, for a reftable repository,
// reftable/tables.list. A reftable repository always has a file at
// refs/heads and a table, since HEAD lives in the reftable, so whether
// refs exist cannot be told from the files alone; whether they changed
// can.
type refState struct {
	loose  map[string]string
	packed string
	tables string
	// unreadable is set when part of it could not be read.
	unreadable bool
}

// readRefState reads the ref storage of the repository at rp. A file that
// is gone is left out.
func readRefState(rp string) refState {
	st := refState{loose: map[string]string{}}
	read := func(p string) string {
		data, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			st.unreadable = true
		}
		return string(data)
	}
	st.packed = read(filepath.Join(rp, "packed-refs"))
	st.tables = read(filepath.Join(rp, "reftable", "tables.list"))
	err := filepath.WalkDir(filepath.Join(rp, "refs"), func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				st.unreadable = true
			}
			return nil
		}
		if !de.IsDir() {
			st.loose[p] = read(p)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		st.unreadable = true
	}
	return st
}

// refsWritten reports whether the ref storage now holds something it did not
// at before: a loose ref that is new or changed, or a packed-refs or
// tables.list that differs. What a removal took away does not count, so a
// directory half removed is not mistaken for one written to. Anything that
// cannot be read counts as written, so pushed refs are never removed by
// mistake.
func refsWritten(before, now refState) bool {
	if now.unreadable {
		return true
	}
	if now.packed != "" && now.packed != before.packed {
		return true
	}
	if now.tables != "" && now.tables != before.tables {
		return true
	}
	for p, content := range now.loose {
		if old, ok := before.loose[p]; !ok || old != content {
			return true
		}
	}
	return false
}

// RepositoryHasRefs reports whether the repository r has any ref. A
// repository that cannot be read counts as having refs, so a push-created
// repository is never removed by mistake.
func (d *Backend) RepositoryHasRefs(r proto.Repository) bool {
	return hasRefs(d.repoPath(r.Name()))
}

// pushCreatedRepositoryUsed reports whether m, the row now under created's
// name, is no longer created as it was made, or has anything recorded for
// it since.
func (d *Backend) pushCreatedRepositoryUsed(ctx context.Context, tx *db.Tx, m models.Repo, created proto.Repository) (bool, error) {
	if m.ID != created.ID() ||
		m.ProjectName != created.ProjectName() ||
		m.Description != created.Description() ||
		m.Private != created.IsPrivate() ||
		m.Hidden != created.IsHidden() ||
		m.Mirror != created.IsMirror() ||
		m.UserID != created.UserID() {
		return true, nil
	}

	collabs, err := d.store.ListCollabsByRepo(ctx, tx, m.Name)
	if err != nil || len(collabs) > 0 {
		return true, err
	}
	hooks, err := d.store.GetWebhooksByRepoID(ctx, tx, m.ID)
	if err != nil || len(hooks) > 0 {
		return true, err
	}
	objs, err := d.store.GetLFSObjects(ctx, tx, m.ID)
	if err != nil || len(objs) > 0 {
		return true, err
	}
	locks, err := d.store.GetLFSLocks(ctx, tx, m.ID, 0, 1)
	if err != nil || len(locks) > 0 {
		return true, err
	}
	return false, nil
}

// hasRefs reports whether the repository at dir has any ref. A repository
// that cannot be read counts as having refs, so it is never removed by
// mistake.
func hasRefs(dir string) bool {
	out, err := git.NewCommand("for-each-ref", "--count=1", "--format=%(refname)").RunInDir(dir)
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(out)) != ""
}
