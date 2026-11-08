package jobs

import (
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

func TestLFSSweepRegistered(t *testing.T) {
	t.Parallel()

	j, ok := List()["lfs-sweep"]
	if !ok {
		t.Fatal("lfs-sweep job is not registered")
	}

	cfg := config.DefaultConfig()
	if got := j.Runner.Spec(config.WithContext(t.Context(), cfg)); got != "@every 1h" {
		t.Errorf("default spec = %q, want @every 1h", got)
	}

	cfg.Jobs.LFSSweep = "@daily"
	if got := j.Runner.Spec(config.WithContext(t.Context(), cfg)); got != "@daily" {
		t.Errorf("configured spec = %q, want @daily", got)
	}
}
