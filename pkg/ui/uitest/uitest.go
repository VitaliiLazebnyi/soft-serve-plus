// Package uitest holds what the tests of the TUI share.
package uitest

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest/uiwait"
)

// HangGuard is how long a test waits for a command, or for the commands it
// started to settle, before it fails as hung; see uiwait.HangGuard.
const HangGuard = uiwait.HangGuard

// WaitFor reports whether cond holds within HangGuard; see uiwait.WaitFor.
func WaitFor(cond func() bool) bool { return uiwait.WaitFor(cond) }

// Receive returns what c gives, or false when WaitFor gives up on it.
func Receive[T any](c <-chan T) (v T, ok bool) { return uiwait.Receive(c) }

// Unbounded is a highlighting budget that never ends.
func Unbounded() <-chan time.Time { return uiwait.Unbounded() }

// WaitIdle waits until running, package common's Tokenising, returns 0; see
// uiwait.WaitIdle.
func WaitIdle(t testing.TB, running func() int) {
	t.Helper()
	uiwait.WaitIdle(t, running)
}

// NewCommon returns the Common of a test's session: ctx is the session's
// context, cfg its config (nil for config.DefaultConfig) and be its backend
// (nil for none). Its zone manager is closed when the test ends.
func NewCommon(tb testing.TB, ctx context.Context, cfg *config.Config, be *backend.Backend) *common.Common {
	tb.Helper()
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	c, err := common.New(ctx, cfg, be, nil)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(c.Zone.Close)
	return c
}
