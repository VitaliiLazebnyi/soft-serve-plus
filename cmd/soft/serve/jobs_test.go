package serve

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/cron"
)

// recordingScheduler records the jobs scheduleJobs adds, by name, with
// their specs. It refuses the names in fail.
type recordingScheduler struct {
	specs map[string]string
	fail  map[string]bool
}

func (r *recordingScheduler) AddFunc(name, spec string, _ func()) error {
	if r.fail[name] {
		return errors.New("refused")
	}
	if r.specs == nil {
		r.specs = map[string]string{}
	}
	r.specs[name] = spec
	return nil
}

// An empty jobs.mirror_pull leaves the mirror job unscheduled, and a spec
// schedules it. lfs-sweep is scheduled either way, hourly when its spec is
// empty.
func TestScheduleJobs(t *testing.T) {
	for _, c := range []struct {
		mirror, sweep string
		want          map[string]string
	}{
		{"", "", map[string]string{"lfs-sweep": "@every 1h"}},
		{"@every 1h", "@daily", map[string]string{"mirror-pull": "@every 1h", "lfs-sweep": "@daily"}},
		{"*/5 * * * *", "", map[string]string{"mirror-pull": "*/5 * * * *", "lfs-sweep": "@every 1h"}},
	} {
		cfg := config.DefaultConfig()
		cfg.Jobs.MirrorPull = c.mirror
		cfg.Jobs.LFSSweep = c.sweep
		var buf bytes.Buffer
		var sched recordingScheduler
		scheduleJobs(t.Context(), cfg, &sched, log.New(&buf))

		if !maps.Equal(sched.specs, c.want) {
			t.Errorf("mirror_pull %q, lfs_sweep %q: scheduled %v, want %v", c.mirror, c.sweep, sched.specs, c.want)
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
