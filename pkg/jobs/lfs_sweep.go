package jobs

import (
	"context"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

func init() {
	Register("lfs-sweep", lfsSweep{})
}

// lfsSweep removes the temporary files that interrupted LFS uploads and
// mirror downloads left behind.
type lfsSweep struct{}

// Spec returns the schedule of the sweep and implements Runner.
func (lfsSweep) Spec(ctx context.Context) string {
	cfg := config.FromContext(ctx)
	if cfg.Jobs.LFSSweep != "" {
		return cfg.Jobs.LFSSweep
	}
	return "@every 1h"
}

// Func runs the sweep and implements Runner.
func (lfsSweep) Func(ctx context.Context) func() {
	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("jobs.lfs-sweep")
	return func() {
		// Run even with LFS disabled: files left from when it was enabled
		// would otherwise stay until the repository is deleted.
		n, err := backend.SweepIncompleteLFS(ctx, cfg.DataPath, time.Now().Add(-backend.IncompleteLFSMaxAge))
		if n > 0 {
			logger.Info("removed stale lfs temp files", "count", n)
		}
		if err != nil {
			logger.Warn("error sweeping lfs temp files", "err", err)
		}
	}
}
