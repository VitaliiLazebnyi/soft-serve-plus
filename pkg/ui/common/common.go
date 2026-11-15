package common

import (
	"context"
	"fmt"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/keymap"
	"github.com/charmbracelet/soft-serve/pkg/ui/styles"
	zone "github.com/lrstanley/bubblezone/v2"
)

// Common is a struct all components should embed.
type Common struct {
	ctx           context.Context
	Width, Height int
	Styles        *styles.Styles
	KeyMap        *keymap.KeyMap
	Zone          *zone.Manager
	Logger        *log.Logger
	HideCloneCmd  bool
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

// NewCommon returns a new Common struct, with its size limited by
// ClampSize.
//
// Its Zone manager runs a background worker. The caller must close it with
// Zone.Close once the program using it has exited, and not before: a render
// after Close blocks once the view has more zones than the worker buffers.
func NewCommon(ctx context.Context, width, height int) Common {
	if ctx == nil {
		ctx = context.TODO()
	}
	width, height = ClampSize(width, height)
	return Common{
		ctx:    ctx,
		Width:  width,
		Height: height,
		Styles: styles.DefaultStyles(),
		KeyMap: keymap.DefaultKeyMap(),
		Zone:   zone.New(),
		Logger: log.FromContext(ctx).WithPrefix("ui"),
	}
}

// SetSize sets the width and height of the common struct, limited by
// ClampSize.
func (c *Common) SetSize(width, height int) {
	c.Width, c.Height = ClampSize(width, height)
}

// Context returns the context.
func (c *Common) Context() context.Context {
	return c.ctx
}

// Config returns the server config.
func (c *Common) Config() *config.Config {
	return config.FromContext(c.ctx)
}

// Backend returns the Soft Serve backend.
func (c *Common) Backend() *backend.Backend {
	return backend.FromContext(c.ctx)
}

// PublicKey returns the public key.
func (c *Common) PublicKey() ssh.PublicKey {
	v := c.ctx.Value(ssh.ContextKeyPublicKey)
	if p, ok := v.(ssh.PublicKey); ok {
		return p
	}
	return nil
}

// CloneCmd returns the clone command string.
func (c *Common) CloneCmd(publicURL, name string) string {
	if c.HideCloneCmd {
		return ""
	}
	return fmt.Sprintf("git clone %s", RepoURL(publicURL, name))
}

// IsFileMarkdown returns true if the file is markdown.
// It uses chroma lexers to analyze and determine the language.
func IsFileMarkdown(content, ext string) bool {
	var lang string
	lexer := lexers.Match(ext)
	if lexer == nil {
		lexer = lexers.Analyse(content)
	}
	if lexer != nil && lexer.Config() != nil {
		lang = lexer.Config().Name
	}
	return lang == "markdown"
}

// ScrollPercent returns a string representing the scroll percentage of the
// viewport.
func ScrollPercent(position int) string {
	return fmt.Sprintf("≡ %d%%", position)
}
