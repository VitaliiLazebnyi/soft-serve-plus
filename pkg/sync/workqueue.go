package sync

import (
	"context"
	"runtime/debug"
	"sync"

	"golang.org/x/sync/semaphore"
)

// WorkPool is a pool of work to be done.
type WorkPool struct {
	workers int
	work    sync.Map
	sem     *semaphore.Weighted
	ctx     context.Context
	logger  func(string, ...interface{})
}

// WorkPoolOption is a function that configures a WorkPool.
type WorkPoolOption func(*WorkPool)

// WithWorkPoolLogger sets the logger to use.
func WithWorkPoolLogger(logger func(string, ...interface{})) WorkPoolOption {
	return func(wq *WorkPool) {
		wq.logger = logger
	}
}

// NewWorkPool creates a new work pool. The workers argument specifies the
// number of concurrent workers to run the work: at most that many jobs run at
// once, and the next one starts as soon as one finishes.
func NewWorkPool(ctx context.Context, workers int, opts ...WorkPoolOption) *WorkPool {
	wq := &WorkPool{
		workers: workers,
		ctx:     ctx,
	}

	for _, opt := range opts {
		opt(wq)
	}

	if wq.workers <= 0 {
		wq.workers = 1
	}

	wq.sem = semaphore.NewWeighted(int64(wq.workers))

	return wq
}

// Run starts the workers and waits for them to finish. Once the pool's
// context is done no more work starts, but Run still waits for the work
// already running, which is expected to wind down on the same context. It
// may be called again to run work added since.
func (wq *WorkPool) Run() {
	var wg sync.WaitGroup
	defer wg.Wait()
	wq.work.Range(func(key, value any) bool {
		id := key.(string)
		fn := value.(func())
		if err := wq.sem.Acquire(wq.ctx, 1); err != nil {
			wq.logf("workpool: %v", err)
			return false
		}

		wg.Add(1)
		go func(id string, fn func()) {
			defer wg.Done()
			defer wq.sem.Release(1)
			defer wq.work.Delete(id)
			// A panicking job must not take the whole process down with it.
			defer func() {
				if r := recover(); r != nil {
					wq.logf("workpool: job %s panicked: %v\n%s", id, r, debug.Stack())
				}
			}()
			fn()
		}(id, fn)

		return true
	})
}

// Add adds a new job to the pool.
// If the job already exists, it is a no-op.
func (wq *WorkPool) Add(id string, fn func()) {
	if _, ok := wq.work.Load(id); ok {
		return
	}
	wq.work.Store(id, fn)
}

// Status checks if a job is in the queue.
func (wq *WorkPool) Status(id string) bool {
	_, ok := wq.work.Load(id)
	return ok
}

func (wq *WorkPool) logf(format string, args ...interface{}) {
	if wq.logger != nil {
		wq.logger(format, args...)
	}
}
