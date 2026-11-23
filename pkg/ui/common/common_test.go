package common_test

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// The UI's size comes from the SSH client; ClampSize keeps it to at most
// 500x200 cells.
func TestClampSize(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		width, height         int
		wantWidth, wantHeight int
	}{
		{80, 24, 80, 24},
		{500, 200, 500, 200},
		{501, 201, 500, 200},
		{30000, 30000, 500, 200},
		{30000, 10, 500, 10},
		{10, 30000, 10, 200},
		{0, 0, 0, 0},
		// A uint32 size over MaxInt32 is negative as an int on 32-bit
		// platforms.
		{-5, -7, 0, 0},
		{-1 << 31, 80, 0, 80},
	} {
		w, h := common.ClampSize(tc.width, tc.height)
		if w != tc.wantWidth || h != tc.wantHeight {
			t.Errorf("ClampSize(%d, %d) = %d, %d, want %d, %d", tc.width, tc.height, w, h, tc.wantWidth, tc.wantHeight)
		}
	}
}

// New takes its dependencies as parameters: a nil config is an error, and a
// nil logger means the default one.
func TestNew(t *testing.T) {
	t.Parallel()
	if _, err := common.New(t.Context(), nil, nil, nil); !errors.Is(err, config.ErrNilConfig) {
		t.Fatalf("New with a nil config: err = %v, want %v", err, config.ErrNilConfig)
	}
	cfg := config.DefaultConfig()
	c, err := common.New(t.Context(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Zone.Close()
	if c.Config != cfg || c.Backend != nil || c.Logger == nil || c.Context() != t.Context() {
		t.Fatalf("New returned %+v", c)
	}
}
