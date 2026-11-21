package common

import (
	"os"
	"strconv"

	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"github.com/charmbracelet/colorprofile"
)

// ColorProfile returns the color profile of the TUI over SSH: 256 colors,
// or none when SOFT_SERVE_NO_COLOR is true.
func ColorProfile() colorprofile.Profile {
	if noColor, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_NO_COLOR")); noColor {
		return colorprofile.NoTTY
	}
	return colorprofile.ANSI256
}

func strptr(s string) *string {
	return &s
}

// StyleConfig returns the default Glamour style configuration.
func StyleConfig() gansi.StyleConfig {
	noColor := strptr("")
	s := styles.DarkStyleConfig
	// This fixes an issue with the default style config. For example
	// highlighting empty spaces with red in Dockerfile type.
	// Chroma is a pointer into glamour's shared style. Change a copy, since
	// every TUI session calls StyleConfig and writing the shared one races.
	if s.CodeBlock.Chroma != nil {
		chroma := *s.CodeBlock.Chroma
		chroma.Error.BackgroundColor = noColor
		s.CodeBlock.Chroma = &chroma
	}
	return s
}

// StyleRenderer returns a new Glamour renderer.
func StyleRenderer() gansi.RenderContext {
	return StyleRendererWithStyles(StyleConfig())
}

// StyleRendererWithStyles returns a new Glamour renderer.
func StyleRendererWithStyles(styles gansi.StyleConfig) gansi.RenderContext {
	return gansi.NewRenderContext(gansi.Options{
		Styles: styles,
	})
}
