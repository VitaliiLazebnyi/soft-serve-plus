package web

import (
	"context"
	"errors"
	"sync"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/safego"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// pushCreateTimeout is how long a repository created by an HTTP push may
// stay without refs, with no request to it in flight, before it is removed.
// git sends the push in a separate request after the ref advertisement that
// created the repository, and git-lfs uploads objects in between, so the
// repository cannot be removed when the advertisement ends.
const pushCreateTimeout = 10 * time.Minute

// pushCreates tracks repositories created by HTTP push-to-create that have
// no refs yet. The SSH push creates and pushes in one session and removes
// the repository when the push fails; over HTTP the push may fail in a later
// request or never arrive, so this list decides when to call the same
// removal, backend.RemovePushCreatedRepository. A listed repository is removed when a
// receive-pack request to it fails and leaves no ref, or when nothing has
// reached it for pushCreateTimeout and it still has no ref. It is kept
// instead if it was used meanwhile or a push over SSH to it is running; see
// backend.RemovePushCreatedRepository. A receive-pack request that succeeds
// without updating a ref, such as the probe git sends before a pack larger
// than http.postBuffer, only restarts the timeout. A repository leaves the
// list once a request to it ends and it has a ref.
//
// A removal runs after the request that created the repository has ended,
// so it logs on the server's logger, not on that request's.
//
// The list is kept in memory, so a repository still on it when the server
// stops, or when close is called, is kept, empty, until someone deletes it. That errs on the side of
// keeping: nothing on disk tells such a repository apart from one created
// empty on purpose.
type pushCreates struct {
	be     *backend.Backend
	logger *log.Logger

	// mu guards the map and the entries' fields. It is never held while
	// running git or deleting a repository, since every HTTP git request
	// takes it.
	mu    sync.Mutex
	repos map[string]*pushCreate
	// closed is set by close. No timer is started and no removal starts
	// once it is set.
	closed bool
	// waiting, if set, is called when a request starts waiting for a
	// removal. Tests use it to end the removal only once a request waits.
	waiting func(name string)
}

// pushCreate is a repository created by an HTTP push that has no refs yet.
type pushCreate struct {
	// user is the caller who created the repository, or nil for an
	// anonymous one. Its removal is done as that caller.
	user proto.User
	// repo is the created repository as it was created. It is only deleted
	// if the name still belongs to it, not to one renamed or created in its
	// place since, and nothing about it changed.
	repo     proto.Repository
	inflight int
	// gen changes whenever a request starts or a decision is taken, so a
	// decision made outside the lock, or a timer, that is out of date does
	// nothing.
	gen   int
	timer *time.Timer
	// removing is closed once a removal that has started is over. Requests
	// to the repository wait for it rather than run against a repository
	// being deleted.
	removing chan struct{}
}

// newPushCreates returns an empty list whose repositories be removes,
// logging on logger.
func newPushCreates(be *backend.Backend, logger *log.Logger) *pushCreates {
	return &pushCreates{
		be:     be,
		logger: logger,
		repos:  make(map[string]*pushCreate),
	}
}

// add records that user (nil for an anonymous caller) created repo, under
// name.
func (p *pushCreates) add(name string, repo proto.Repository, user proto.User) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.repos[name] = &pushCreate{user: user, repo: repo}
}

// begin marks a request to the repository name as in flight, so it is not
// removed under the request. It returns the function to call when the
// request ends, saying whether it was a receive-pack request that failed,
// or nil when name is not on the list.
//
// If the repository is being removed, begin waits for the removal to end
// and returns waited true, with nil: the repository the caller resolved
// before may be gone, so the caller must resolve it, and the caller's
// access to it, again.
func (p *pushCreates) begin(name string) (done func(failed bool), waited bool) {
	p.mu.Lock()
	pc, ok := p.repos[name]
	if !ok {
		p.mu.Unlock()
		return nil, false
	}
	if pc.removing != nil {
		ch := pc.removing
		waiting := p.waiting
		p.mu.Unlock()
		if waiting != nil {
			waiting(name)
		}
		<-ch
		return nil, true
	}
	pc.inflight++
	pc.gen++
	if pc.timer != nil {
		pc.timer.Stop()
		pc.timer = nil
	}
	p.mu.Unlock()
	return func(failed bool) { p.end(name, pc, failed) }, false
}

// end is called when a request to a listed repository ends. Once no
// request is in flight, a failed push hands the repository to the
// backend's removal, which keeps one with refs and points its HEAD at a
// pushed branch (a push can fail after its refs landed). Otherwise a
// repository with refs leaves the list, and one without gets
// pushCreateTimeout for the push to arrive.
func (p *pushCreates) end(name string, pc *pushCreate, failed bool) {
	p.mu.Lock()
	pc.inflight--
	if p.repos[name] != pc || pc.inflight > 0 || pc.removing != nil {
		p.mu.Unlock()
		return
	}
	pc.gen++
	gen := pc.gen
	p.mu.Unlock()

	if failed {
		p.remove(name, pc, gen)
		return
	}
	// The request has ended, and its context with it: a check cut short
	// would count as having refs and drop the repository from the list.
	// The list outlives requests, as remove's context does.
	refs := p.be.RepositoryHasRefs(context.Background(), pc.repo)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.repos[name] != pc || pc.gen != gen {
		return // a request started meanwhile; its end decides
	}
	if refs {
		delete(p.repos, name)
		return
	}
	if p.closed {
		return
	}
	// The timer's goroutine is the server's own, so nothing else would
	// recover a panic in the removal.
	pc.timer = time.AfterFunc(pushCreateTimeout, func() {
		_ = safego.Run(p.logger, func() { p.remove(name, pc, gen) })
	})
}

// remove takes the repository off the list and deletes it if it is still
// unused; see backend.RemovePushCreatedRepository. It does nothing if a
// request started since gen was taken.
func (p *pushCreates) remove(name string, pc *pushCreate, gen int) {
	p.mu.Lock()
	if p.closed || p.repos[name] != pc || pc.gen != gen || pc.inflight > 0 || pc.removing != nil {
		p.mu.Unlock()
		return
	}
	pc.timer = nil
	pc.removing = make(chan struct{})
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		if p.repos[name] == pc {
			delete(p.repos, name)
		}
		close(pc.removing)
		p.mu.Unlock()
	}()

	ctx := proto.WithUserContext(context.Background(), pc.user)
	removed, err := p.be.RemovePushCreatedRepository(ctx, pc.repo)
	switch {
	case errors.Is(err, proto.ErrRepoNotFound):
		// Deleted, or renamed, meanwhile.
	case err != nil:
		p.logger.Error("failed to remove repository created by a push", "repo", name, "err", err)
	case removed:
		p.logger.Info("removed repository created by a push that sent no refs", "repo", name)
	}
}

// close stops the removal timers and keeps any removal from starting, so
// none runs against a backend that is being closed. It then waits for the
// removals already running to end, or for ctx to be done, and returns
// ctx's error in that case.
func (p *pushCreates) close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.closed = true
	var running []chan struct{}
	for _, pc := range p.repos {
		if pc.timer != nil {
			pc.timer.Stop()
			pc.timer = nil
		}
		if pc.removing != nil {
			running = append(running, pc.removing)
		}
	}
	p.mu.Unlock()

	for _, ch := range running {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
