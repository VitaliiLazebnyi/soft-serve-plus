package backend

import (
	"context"
	"sync"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

const (
	// maxConcurrentImports caps the imports running at once on the server,
	// server admins' included. Each holds a git process, a connection and a
	// growing directory until it finishes.
	maxConcurrentImports = 16
	// maxUserConcurrentImports caps the imports one user runs at once.
	// Server admins are exempt, and callers without an account count
	// together as one user, as they do for max_repos_per_user.
	maxUserConcurrentImports = 2
)

// importTimeout bounds a whole import: the clone, and the LFS fetch when
// asked for. The low-speed limits in ssrf.GitEnv catch a stalled http
// remote sooner; this catches one that trickles just fast enough, and the
// ssh and git transports, which have no such limit. Tests shorten it.
var importTimeout = time.Hour

// importTracker counts the imports in flight, so they can be capped and
// counted against max_repos_per_user until they register their repository,
// and stops them at shutdown.
type importTracker struct {
	// ctx is the parent of every import's context. Stop cancels it.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	stopped bool
	total   int
	// perUser counts the imports in flight by user ID, 0 being callers
	// without an account. It holds the per-user cap.
	perUser map[int64]int
	// pending counts, by the same key, the imports in flight that have not
	// registered their repository yet. It is what counts against
	// max_repos_per_user: once registered, the repository row counts
	// instead, while the import still holds its slots in total and perUser
	// until it returns (after the LFS fetch, say).
	pending map[int64]int
	wg      sync.WaitGroup
}

func newImportTracker(ctx context.Context) *importTracker {
	ctx, cancel := context.WithCancel(ctx)
	return &importTracker{ctx: ctx, cancel: cancel, perUser: map[int64]int{}, pending: map[int64]int{}}
}

// importUserKey is the key user's imports are counted under: callers
// without an account count together, as countRepos counts their
// repositories.
func importUserKey(user proto.User) int64 {
	if user == nil {
		return 0
	}
	return user.ID()
}

// importSlot is an import's place in the tracker; see reserveImport.
type importSlot struct {
	t   *importTracker
	key int64
	// registered and released are guarded by t.mu.
	registered, released bool
}

// decr lowers m[key] by one, dropping the key at zero. Call with the
// tracker's lock held.
func decr(m map[int64]int, key int64) {
	if m[key]--; m[key] <= 0 {
		delete(m, key)
	}
}

// markRegistered records that the import has committed its repository, so
// it stops counting against max_repos_per_user: the repository row counts
// from now on. The slot keeps counting against the concurrency caps until
// release. Calling it more than once, or after release, does nothing.
func (s *importSlot) markRegistered() {
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	if s.registered || s.released {
		return
	}
	s.registered = true
	decr(s.t.pending, s.key)
}

// release frees the slot. Call it once the import has finished, cleanup
// included; later calls do nothing.
func (s *importSlot) release() {
	s.t.mu.Lock()
	if s.released {
		s.t.mu.Unlock()
		return
	}
	s.released = true
	s.t.total--
	decr(s.t.perUser, s.key)
	if !s.registered {
		decr(s.t.pending, s.key)
	}
	s.t.mu.Unlock()
	s.t.wg.Done()
}

// reserveImport takes a slot for an import by user, or refuses it: with
// proto.ErrImportsDisabled once imports have been stopped, with
// proto.ErrTooManyImports when the server or the user already runs as many
// as allowed, and with the max_repos_per_user error when limited and the
// user's repositories and unregistered imports in flight together reach the
// limit. Call the slot's markRegistered once the repository is committed,
// and its release once the import has finished, cleanup included.
//
// The repository count is read under the tracker's lock, so imports racing
// each other at the limit take turns. A plain create racing an import can
// still take the last slot while the clone runs; registerRepository checks
// the limit again when it records the repository. Between that commit and
// markRegistered the import is briefly counted twice, which can only refuse
// a concurrent import, never let one past the limit.
func (d *Backend) reserveImport(ctx context.Context, user proto.User, admin, limited bool) (*importSlot, error) {
	t := d.imports
	key := importUserKey(user)

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stopped {
		return nil, proto.ErrImportsDisabled
	}
	if t.total >= maxConcurrentImports || (!admin && t.perUser[key] >= maxUserConcurrentImports) {
		return nil, proto.ErrTooManyImports
	}
	if limited {
		n, err := d.countRepos(ctx, d.db, user)
		if err != nil {
			return nil, err
		}
		if n+int64(t.pending[key]) >= int64(d.cfg.MaxReposPerUser) {
			return nil, d.errRepoLimit(user)
		}
	}

	t.total++
	t.perUser[key]++
	t.pending[key]++
	t.wg.Add(1)

	return &importSlot{t: t, key: key}, nil
}

// StopImports refuses new imports and cancels the ones in flight, which
// then remove what they had created. Call WaitForImports to wait for that.
func (d *Backend) StopImports() {
	d.imports.mu.Lock()
	d.imports.stopped = true
	d.imports.mu.Unlock()
	d.imports.cancel()
}

// WaitForImports waits until every import in flight has finished, or until
// ctx is done. Call StopImports first, or it waits for the imports to run
// to completion.
func (d *Backend) WaitForImports(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		d.imports.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		d.logger.Warn("not waiting any longer for imports to finish", "err", ctx.Err())
	}
}
