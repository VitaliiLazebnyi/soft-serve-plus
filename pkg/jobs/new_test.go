package jobs

import (
	"errors"
	"runtime"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
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
