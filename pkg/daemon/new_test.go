package daemon

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// NewServer refuses a missing config or backend rather than build a
// daemon that fails at its first request.
func TestNewServerNilDependencies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		be   *backend.Backend
		want error
	}{
		{"nil config", nil, testBackend, config.ErrNilConfig},
		{"nil backend", testDaemon.cfg, nil, errNilBackend},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := NewServer(tc.cfg, tc.be, nil)
			if !errors.Is(err, tc.want) || d != nil {
				t.Errorf("NewServer = %v, %v; want nil, %v", d, err, tc.want)
			}
		})
	}
}
