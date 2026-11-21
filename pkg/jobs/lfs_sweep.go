package jobs

import (
	"context"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// LFSSweepSpec is the schedule of the LFS sweep: hourly. It removes files
// left for more than a day, so how often it runs matters little.
const LFSSweepSpec = "@every 1h"

// LFSSweep is the lfs-sweep job: it removes the temporary files that
// interrupted LFS uploads and mirror downloads left behind under the data
// path. Make one with NewLFSSweep.
type LFSSweep struct {
	cfg    *config.Config
	logger *log.Logger
}

// NewLFSSweep returns the lfs-sweep job of the server with cfg, logging to
// logger, or log.Default() when it is nil. A nil cfg is an error.
func NewLFSSweep(cfg *config.Config, logger *log.Logger) (*LFSSweep, error) {
	if cfg == nil {
		return nil, config.ErrNilConfig
	}
	if logger == nil {
		logger = log.Default()
	}
	return &LFSSweep{cfg: cfg, logger: logger.WithPrefix("jobs.lfs-sweep")}, nil
}

// Run sweeps once. It runs even with LFS disabled: files left from when it
// was enabled would otherwise stay until the repository is deleted.
func (s *LFSSweep) Run(ctx context.Context) {
	n, err := backend.SweepIncompleteLFS(ctx, s.cfg.DataPath, time.Now().Add(-backend.IncompleteLFSMaxAge))
	if n > 0 {
		s.logger.Info("removed stale lfs temp files", "count", n)
	}
	if err != nil {
		s.logger.Warn("error sweeping lfs temp files", "err", err)
	}
}
