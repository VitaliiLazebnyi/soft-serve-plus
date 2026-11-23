package selection

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
)

// The About tab says "loading…" instead of its scroll position while the
// readme renders, since what it shows meanwhile is not that readme.
func TestSelectionAboutStatusWhileRendering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(uitest.NewCommon(t, ctx, nil, nil))
	s.SetSize(80, 40)
	s.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	s.activePane = readmePane
	if v := s.View(); strings.Contains(v, "loading…") || !strings.Contains(v, "≡") {
		t.Fatalf("with no readme: the status is not the scroll position:\n%s", v)
	}

	cmd := s.setReadme("# Banner", "README.md")
	if cmd == nil {
		t.Fatal("setting the readme started no render")
	}
	if v := s.View(); !strings.Contains(v, "loading…") || strings.Contains(v, "≡") {
		t.Fatalf("while the readme renders: the status does not say loading:\n%s", v)
	}

	msg, ok := cmd().(code.RenderedMsg)
	if !ok {
		t.Fatal("the render returned no RenderedMsg")
	}
	s.Update(msg)
	if v := s.View(); strings.Contains(v, "loading…") || !strings.Contains(v, "≡") || !strings.Contains(v, "Banner") {
		t.Fatalf("once the readme rendered: it or its scroll position is missing:\n%s", v)
	}
}
