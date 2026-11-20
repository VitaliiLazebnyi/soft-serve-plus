package serve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/cron"
)

// recordingScheduler records the jobs scheduleJobs adds, by name, with
// their specs and functions. It refuses the names in fail.
type recordingScheduler struct {
	specs map[string]string
	fns   map[string]func()
	fail  map[string]bool
}

func (r *recordingScheduler) AddFunc(name, spec string, fn func()) error {
	if r.fail[name] {
		return errors.New("refused")
	}
	if r.specs == nil {
		r.specs, r.fns = map[string]string{}, map[string]func(){}
	}
	r.specs[name], r.fns[name] = spec, fn
	return nil
}

// An empty jobs.mirror_pull leaves the mirror job unscheduled, and a spec
// schedules it. lfs-sweep is scheduled hourly either way.
func TestScheduleJobs(t *testing.T) {
	for _, c := range []struct {
		mirror string
		want   map[string]string
	}{
		{"", map[string]string{"lfs-sweep": "@every 1h"}},
		{"@every 1h", map[string]string{"mirror-pull": "@every 1h", "lfs-sweep": "@every 1h"}},
		{"*/5 * * * *", map[string]string{"mirror-pull": "*/5 * * * *", "lfs-sweep": "@every 1h"}},
	} {
		cfg := config.DefaultConfig()
		cfg.Jobs.MirrorPull = c.mirror
		var buf bytes.Buffer
		var sched recordingScheduler
		scheduleJobs(t.Context(), cfg, &sched, log.New(&buf))

		if !maps.Equal(sched.specs, c.want) {
			t.Errorf("mirror_pull %q: scheduled %v, want %v", c.mirror, sched.specs, c.want)
		}
		if disabled := strings.Contains(buf.String(), "cron job disabled"); disabled != (c.mirror == "") {
			t.Errorf("mirror_pull %q: disabled message logged %v: %q", c.mirror, disabled, buf.String())
		}
	}
}

// A job the scheduler refuses is logged, and the other is still scheduled.
func TestScheduleJobsLogsRefusedJob(t *testing.T) {
	cfg := config.DefaultConfig()
	var buf bytes.Buffer
	sched := recordingScheduler{fail: map[string]bool{"mirror-pull": true}}
	scheduleJobs(t.Context(), cfg, &sched, log.New(&buf))

	if got := slices.Collect(maps.Keys(sched.specs)); !slices.Equal(got, []string{"lfs-sweep"}) {
		t.Errorf("scheduled %v, want only lfs-sweep", got)
	}
	if out := buf.String(); !strings.Contains(out, "error adding cron job") || !strings.Contains(out, "mirror-pull") {
		t.Errorf("refused job not logged: %q", out)
	}
}

// *cron.Scheduler is what the server passes.
var _ jobScheduler = (*cron.Scheduler)(nil)

// The jobs run with the jobCtx scheduleJobs is given, not with whatever
// context the scheduler has: the lfs-sweep it schedules sweeps the data path
// of the config in jobCtx. Once jobCtx is done, a tick starts nothing.
func TestScheduleJobsRunWithJobContext(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	stale := filepath.Join(cfg.DataPath, "lfs", "1", "incomplete", "stale")
	writeStale := func() {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * backend.IncompleteLFSMaxAge)
		if err := os.Chtimes(stale, old, old); err != nil {
			t.Fatal(err)
		}
	}
	jobCtx, cancel := context.WithCancel(config.WithContext(t.Context(), cfg))
	defer cancel()
	// The jobs log through jobCtx's logger, so it shows whether one ran.
	var jobLogs bytes.Buffer
	jobLogger := log.New(&jobLogs)
	jobLogger.SetLevel(log.DebugLevel)
	jobCtx = log.WithContext(jobCtx, jobLogger)

	// The config passed alongside has no data path: only jobCtx's does.
	var sched recordingScheduler
	scheduleJobs(jobCtx, config.DefaultConfig(), &sched, log.New(io.Discard))

	writeStale()
	sched.fns["lfs-sweep"]()
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale temp file not swept with jobCtx's config: %v", err)
	}

	if !strings.Contains(jobLogs.String(), "removed stale lfs temp files") {
		t.Errorf("the sweep did not log through jobCtx's logger: %q", jobLogs.String())
	}

	// A tick once jobCtx is done starts nothing: the sweep would stop on
	// the done context by itself, but only after it started and logged
	// the cancellation as a failed sweep.
	writeStale()
	jobLogs.Reset()
	cancel()
	sched.fns["lfs-sweep"]()
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("a tick after jobCtx was cancelled ran the sweep: %v", err)
	}
	if jobLogs.Len() != 0 {
		t.Errorf("a tick after jobCtx was cancelled started the sweep: %q", jobLogs.String())
	}
}
