package syntax

import (
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
)

func strptr(s string) *string {
	return &s
}

// commentColor replaces glamour's comment colour, #676767, which lies exactly
// between the 256-colour palette's 241 (#626262) and 242 (#6c6c6c). chroma
// picks the nearest palette colour by ranging over a map and keeping the
// first of equally near ones, so each render took one of the two at random.
// This one is nearest to 241.
const commentColor = "#666666"

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
		chroma.Comment.Color = strptr(commentColor)
		s.CodeBlock.Chroma = &chroma
	}
	return s
}

// StyleRenderer returns a new Glamour renderer.
func StyleRenderer() gansi.RenderContext {
	return StyleRendererWithStyles(StyleConfig())
}

// StyleRendererWithStyles returns a new Glamour renderer.
func StyleRendererWithStyles(cfg gansi.StyleConfig) gansi.RenderContext {
	return gansi.NewRenderContext(gansi.Options{
		Styles: cfg,
	})
}
