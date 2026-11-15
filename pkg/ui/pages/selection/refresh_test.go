package selection

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// A refresh cancels the listing in flight, which then lists nothing.
func TestRefreshCancelsListing(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	s := New(common.NewCommon(ctx, 80, 40))
	first := s.Init()
	s.Init()
	ran := func() (ran bool) {
		// The test has no backend, so a listing that runs panics.
		defer func() {
			if recover() != nil {
				ran = true
			}
		}()
		return first() != nil
	}()
	if ran {
		t.Fatal("a superseded listing ran")
	}
}
