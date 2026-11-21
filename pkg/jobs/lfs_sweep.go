package jobs

import (
	"context"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// LFSSweepSpec is the schedule of SweepLFS: hourly. It removes files left
// for more than a day, so how often it runs matters little.
const LFSSweepSpec = "@every 1h"

// SweepLFS removes the temporary files that interrupted LFS uploads and
// mirror downloads left behind.
func SweepLFS(ctx context.Context) {
	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("jobs.lfs-sweep")
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
