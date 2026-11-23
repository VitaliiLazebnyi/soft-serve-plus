// Package common holds what the TUI's components share: the session's
// dependencies, the component interfaces, error and load messages, and
// rendering helpers.
package common

import (
	"context"
	"fmt"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/keymap"
	"github.com/charmbracelet/soft-serve/pkg/ui/styles"
	zone "github.com/lrstanley/bubblezone/v2"
)

// Common is what the components of one TUI session share. It is built once,
// when the session starts, and is not changed after that: every component
// holds the same pointer, and keeps its own size.
type Common struct {
	// Config is the server's configuration.
	Config *config.Config
	// Backend is the server's backend. It is nil in soft browse, which
	// shows a local repository and no server's list.
	Backend *backend.Backend
	// Logger logs for the session's UI.
	Logger *log.Logger

	Styles       *styles.Styles
	KeyMap       *keymap.KeyMap
	Zone         *zone.Manager
	HideCloneCmd bool

	// ctx is the session's context. Bubble Tea commands take no context,
	// so the session's is kept here for them. It ends when the session's
	// program exits. The UI reads only the session's user and key, and
	// its cancellation, from it.
	ctx context.Context
}

// MaxWidth and MaxHeight are the largest terminal size, in cells, the UI
// renders at. The size comes from the SSH client, and rendering costs memory
// in proportion to width x height, so a larger terminal gets a UI of this
// size.
const (
	MaxWidth  = 500
	MaxHeight = 200
)

// ClampSize returns width and height limited to 0..MaxWidth and 0..MaxHeight.
// A size from the client is a uint32, which is negative as an int on
// 32-bit platforms when over MaxInt32.
func ClampSize(width, height int) (int, int) {
	return max(0, min(width, MaxWidth)), max(0, min(height, MaxHeight))
}

// New returns the Common of a session whose context is ctx. The session's
// git work runs under ctx, so it should end when the session does. A nil
// logger means log.Default(); a nil config is an error.
//
// Its Zone manager runs a background worker. The caller must close it with
// Zone.Close once the program using it has exited, and not before: a render
// after Close blocks once the view has more zones than the worker buffers.
func New(ctx context.Context, cfg *config.Config, be *backend.Backend, logger *log.Logger) (*Common, error) {
	if cfg == nil {
		return nil, config.ErrNilConfig
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Common{
		ctx:     ctx,
		Config:  cfg,
		Backend: be,
		Logger:  logger.WithPrefix("ui"),
		Styles:  styles.DefaultStyles(),
		KeyMap:  keymap.DefaultKeyMap(),
		Zone:    zone.New(),
	}, nil
}

// Context returns the session's context.
func (c *Common) Context() context.Context {
	return c.ctx
}

// PublicKey returns the session's public key, or nil for a session without
// one.
func (c *Common) PublicKey() ssh.PublicKey {
	p, _ := c.ctx.Value(ssh.ContextKeyPublicKey).(ssh.PublicKey)
	return p
}

// CloneCmd returns the clone command string.
func (c *Common) CloneCmd(publicURL, name string) string {
	if c.HideCloneCmd {
		return ""
	}
	return fmt.Sprintf("git clone %s", RepoURL(publicURL, name))
}

// ScrollPercent returns a string representing the scroll percentage of the
// viewport.
func ScrollPercent(position int) string {
	return fmt.Sprintf("≡ %d%%", position)
}
