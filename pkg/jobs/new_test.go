package jobs

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// The job constructors refuse a missing config or backend rather than build
// a job that fails at its first run.
func TestNewJobsNilDependencies(t *testing.T) {
	t.Parallel()
	be := new(backend.Backend)
	for _, tc := range []struct {
		name string
		new  func() (any, error)
		want error
	}{
		{"mirror-pull nil config", func() (any, error) { return NewMirrorPull(nil, be, nil) }, config.ErrNilConfig},
		{"mirror-pull nil backend", func() (any, error) { return NewMirrorPull(config.DefaultConfig(), nil, nil) }, errNilBackend},
		{"lfs-sweep nil config", func() (any, error) { return NewLFSSweep(nil, nil) }, config.ErrNilConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.new(); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A mirror-pull job with no worker count syncs as many mirrors at once as
// GOMAXPROCS allows when it runs, not when it was made: GOMAXPROCS follows
// the CPU limit of the server's cgroup.
func TestMirrorPullWorkerLimit(t *testing.T) {
	t.Parallel()
	m, err := NewMirrorPull(config.DefaultConfig(), new(backend.Backend), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.workers != 0 {
		t.Fatalf("workers = %d, want 0, read at run time", m.workers)
	}
	if got, want := m.workerLimit(), runtime.GOMAXPROCS(0); got != want {
		t.Errorf("workerLimit = %d, want GOMAXPROCS %d", got, want)
	}
	m.workers = 3
	if got := m.workerLimit(); got != 3 {
		t.Errorf("workerLimit with 3 workers = %d", got)
	}
}

// Run syncs up to its worker limit of mirrors at once: two workers, or
// GOMAXPROCS when the job has no worker count. Each fetch waits until the
// other mirror's fetch has started too, for at most a minute.
func TestMirrorPullSyncsConcurrently(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		workers int
	}{{"two workers", 2}, {"GOMAXPROCS", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.workers == 0 && runtime.GOMAXPROCS(0) < 2 {
				t.Skip("GOMAXPROCS is 1: one mirror syncs at a time")
			}
			ctx, cfg, be := newMirrorJobContext(t)
			if reason := be.ImportsDisabledReason(); reason != "" {
				t.Skipf("mirror syncs are disabled here: %s", reason)
			}
			admin, err := be.User(ctx, "admin")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b"} {
				if _, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{Mirror: true}); err != nil {
					t.Fatal(err)
				}
				runGit(t, filepath.Join(cfg.DataPath, "repos", name+".git"), "config", "remote.origin.url", "ssh://upstream.example/"+name)
			}

			m := newTestMirrorPull(t, cfg, be, io.Discard)
			m.workers = tc.workers
			var inside atomic.Int32
			both := make(chan struct{})
			m.runGit = func(context.Context, string, []string, ...string) error {
				// Counted only while inside, so the two fetches must
				// overlap: one after the other never reaches two.
				defer inside.Add(-1)
				if inside.Add(1) == 2 {
					close(both)
				}
				select {
				case <-both:
					return nil
				case <-time.After(time.Minute):
					return errors.New("the other mirror's fetch did not start")
				}
			}
			m.Run(ctx)

			select {
			case <-both:
			default:
				t.Fatal("the two mirrors never synced at once")
			}
		})
	}
}
