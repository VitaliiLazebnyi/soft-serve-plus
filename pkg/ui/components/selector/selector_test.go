package selector

import (
	"context"
	"io"
	"testing"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

type item string

func (i item) ID() string          { return string(i) }
func (i item) Title() string       { return string(i) }
func (i item) Description() string { return "" }
func (i item) FilterValue() string { return string(i) }

type delegate struct{}

func (delegate) Height() int                                  { return 1 }
func (delegate) Spacing() int                                 { return 0 }
func (delegate) Update(tea.Msg, *list.Model) tea.Cmd          { return nil }
func (delegate) Render(io.Writer, list.Model, int, list.Item) {}

// "?" typed while filtering goes into the filter.
func TestSelectorTypesHelpKeyWhileFiltering(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(context.Background(), 80, 24)
	s := New(c, []IdentifiableItem{item("a?"), item("b")}, delegate{})
	for _, k := range []tea.KeyPressMsg{
		{Code: '/', Text: "/"},
		{Code: 'a', Text: "a"},
		{Code: '?', Text: "?"},
	} {
		s.Update(k)
	}
	if s.FilterState() != list.Filtering {
		t.Fatalf("filter state = %v, want filtering", s.FilterState())
	}
	if got := s.FilterValue(); got != "a?" {
		t.Errorf("filter = %q, want %q", got, "a?")
	}
}
