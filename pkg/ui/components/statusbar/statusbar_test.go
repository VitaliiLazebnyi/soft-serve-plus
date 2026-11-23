package statusbar

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/charmbracelet/x/ansi"
)

func newTestModel(t *testing.T) *Model {
	t.Helper()
	m := New(uitest.NewCommon(t, context.Background(), nil, nil))
	m.SetSize(120, 1)
	return m
}

// An empty value or info clears what the previous status showed instead of
// leaving it behind.
func TestSetStatusClearsEmptyParts(t *testing.T) {
	t.Parallel()

	s := newTestModel(t)
	s.SetStatus("repo", "abc1234 by Alice", "p. 2/3", "* main")
	s.SetStatus("repo", "", "", "* main")

	view := ansi.Strip(s.View())
	for _, stale := range []string{"abc1234 by Alice", "p. 2/3"} {
		if strings.Contains(view, stale) {
			t.Errorf("status bar still shows %q:\n%s", stale, view)
		}
	}
	if !strings.Contains(view, "repo") || !strings.Contains(view, "* main") {
		t.Errorf("status bar lost its key or branch:\n%s", view)
	}
}

// A copy notice replaces the value without dropping the rest of the status,
// and goes away with the next status update.
func TestMessageIsTransient(t *testing.T) {
	t.Parallel()

	s := newTestModel(t)
	s.SetStatus("repo", "", "p. 1/1", "* main")
	s.SetMessage("Commit hash copied to clipboard")

	view := ansi.Strip(s.View())
	for _, want := range []string{"repo", "Commit hash copied to clipboard", "p. 1/1", "* main"} {
		if !strings.Contains(view, want) {
			t.Errorf("status bar lacks %q:\n%s", want, view)
		}
	}

	s.SetStatus("repo", "", "p. 1/1", "* main")
	if view := ansi.Strip(s.View()); strings.Contains(view, "copied") {
		t.Errorf("copy notice stuck in the status bar:\n%s", view)
	}
}
