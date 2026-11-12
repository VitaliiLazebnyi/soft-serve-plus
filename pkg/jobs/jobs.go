package jobs

import (
	"context"
	"maps"
	"sync"
)

// Job is a job that can be registered with the scheduler.
type Job struct {
	ID     int
	Runner Runner
}

// Runner is a job runner.
type Runner interface {
	// Spec returns the job's cron spec. An empty spec disables the job.
	Spec(context.Context) string
	Func(context.Context) func()
}

var (
	mtx  sync.Mutex
	jobs = make(map[string]*Job, 0)
)

// Register registers a job.
func Register(name string, runner Runner) {
	mtx.Lock()
	defer mtx.Unlock()
	jobs[name] = &Job{Runner: runner}
}

// List returns the registered jobs, by name. The map is a copy, so ranging
// over it is safe while jobs are registered; the jobs it points to are
// shared.
func List() map[string]*Job {
	mtx.Lock()
	defer mtx.Unlock()
	return maps.Clone(jobs)
}
