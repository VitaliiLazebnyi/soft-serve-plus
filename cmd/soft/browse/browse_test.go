package browse

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/key"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
)

func hasHelp(bindings []key.Binding, k string) bool {
	for _, b := range bindings {
		if b.Help().Key == k {
			return true
		}
	}
	return false
}

// The help footer of soft browse shows how to quit and toggle help.
func TestBrowseHelpShowsQuitAndHelp(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(context.Background(), 80, 24)
	m := &model{model: repo.New(c, repo.NewLog(c)), common: c}
	short := m.ShortHelp()
	var full []key.Binding
	for _, g := range m.FullHelp() {
		full = append(full, g...)
	}
	for _, k := range []string{"q", "?"} {
		if !hasHelp(short, k) {
			t.Errorf("short help lacks %q", k)
		}
		if !hasHelp(full, k) {
			t.Errorf("full help lacks %q", k)
		}
	}
}
