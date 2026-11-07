package serve

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/cron"
	"github.com/charmbracelet/soft-serve/pkg/jobs"
)

// TestScheduleJobsMirrorPull checks that an empty jobs.mirror_pull leaves
// the mirror job unscheduled, and that a spec schedules it.
func TestScheduleJobsMirrorPull(t *testing.T) {
	j, ok := jobs.List()["mirror-pull"]
	if !ok {
		t.Fatal("mirror-pull job not registered")
	}
	t.Cleanup(func() { j.ID = 0 })

	for _, c := range []struct {
		spec    string
		entries int
	}{
		{"", 0},
		{"@every 1h", 1},
	} {
		cfg := config.DefaultConfig()
		cfg.Jobs.MirrorPull = c.spec
		ctx := config.WithContext(context.Background(), cfg)
		var buf bytes.Buffer
		logger := log.New(&buf)
		ctx = log.WithContext(ctx, logger)

		sched := cron.NewScheduler(ctx)
		scheduleJobs(ctx, sched, logger)

		if got := len(sched.Entries()); got != c.entries {
			t.Errorf("mirror_pull %q: %d scheduled jobs, want %d", c.spec, got, c.entries)
		}
		if c.spec == "" {
			if j.ID != 0 {
				t.Errorf("disabled job has ID %d, want 0", j.ID)
			}
			if !strings.Contains(buf.String(), "cron job disabled") {
				t.Errorf("no disabled message logged: %q", buf.String())
			}
		} else if j.ID == 0 {
			t.Error("scheduled job has no ID")
		}
	}
}
