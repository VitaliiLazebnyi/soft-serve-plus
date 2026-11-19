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

// importTracker counts the imports in flight, so they can be capped, and
// stops them at shutdown.
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
	wg      sync.WaitGroup
}

func newImportTracker(ctx context.Context) *importTracker {
	ctx, cancel := context.WithCancel(ctx)
	return &importTracker{ctx: ctx, cancel: cancel, perUser: map[int64]int{}}
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
	// released is guarded by t.mu.
	released bool
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
	if s.t.perUser[s.key]--; s.t.perUser[s.key] <= 0 {
		delete(s.t.perUser, s.key)
	}
	s.t.mu.Unlock()
	s.t.wg.Done()
}

// reserveImport takes a slot for an import by user, or refuses it: with
// the max_repos_per_user error when limited and the user's repositories
// already reach the limit, with proto.ErrImportsDisabled once imports have
// been stopped, and with proto.ErrTooManyImports when the server or the
// user already runs as many as allowed. Call the slot's release once the
// import has finished, cleanup included.
//
// The repository count is an early answer only, as for a create (see
// checkRepoLimit): imports in flight do not count, so a user one below the
// limit can start two. registerRepository checks the limit again when it
// records the repository, after the clone, and the import that is over it
// then fails and removes what it cloned.
func (d *Backend) reserveImport(ctx context.Context, user proto.User, admin, limited bool) (*importSlot, error) {
	if limited {
		if err := d.checkRepoLimit(ctx, user); err != nil {
			return nil, err
		}
	}

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

	t.total++
	t.perUser[key]++
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
