package selection

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

type fakeRepo struct {
	name        string
	private     bool
	projectName string
	description string
}

func (r fakeRepo) ID() int64           { return 1 }
func (r fakeRepo) Name() string        { return r.name }
func (r fakeRepo) ProjectName() string { return r.projectName }
func (r fakeRepo) Description() string {
	if r.description == "" {
		return "a description"
	}
	return r.description
}
func (r fakeRepo) IsPrivate() bool                { return r.private }
func (r fakeRepo) IsMirror() bool                 { return false }
func (r fakeRepo) IsHidden() bool                 { return false }
func (r fakeRepo) UserID() int64                  { return 1 }
func (r fakeRepo) CreatedAt() time.Time           { return time.Time{} }
func (r fakeRepo) UpdatedAt() time.Time           { return time.Time{} }
func (r fakeRepo) Open() (*git.Repository, error) { return nil, nil }

var _ proto.Repository = fakeRepo{}

func renderItem(t *testing.T, repo proto.Repository, width int) string {
	t.Helper()
	c := common.NewCommon(context.Background(), width, 20)
	item := NewItem(c, repo)
	pane := selectorPane
	d := NewItemDelegate(&c, &pane)
	m := list.New([]list.Item{item}, d, width, 20)
	var buf bytes.Buffer
	d.Render(&buf, m, 0, item)
	return buf.String()
}

// Every repository in the list shows its visibility, so a public one is
// never mistaken for a private one.
func TestItemVisibilityMarker(t *testing.T) {
	for _, tc := range []struct {
		private      bool
		want, absent string
	}{
		{private: true, want: "🔒", absent: "🌐"},
		{private: false, want: "🌐", absent: "🔒"},
	} {
		out := renderItem(t, fakeRepo{name: "repo", private: tc.private}, 80)
		title := strings.Split(out, "\n")[0]
		if !strings.Contains(title, "repo "+tc.want) {
			t.Errorf("private=%v: title %q lacks %q", tc.private, title, tc.want)
		}
		if strings.Contains(out, tc.absent) {
			t.Errorf("private=%v: item %q shows %q", tc.private, out, tc.absent)
		}
	}
}

// A long name is truncated to leave room for the marker, so the item is no
// wider than one with a short name.
func TestItemVisibilityMarkerLongName(t *testing.T) {
	const width = 30
	for _, private := range []bool{true, false} {
		short := renderItem(t, fakeRepo{name: "repo", private: private}, width)
		long := renderItem(t, fakeRepo{name: strings.Repeat("x", 100), private: private}, width)
		if !strings.Contains(long, common.VisibilityMarker(private)) {
			t.Errorf("private=%v: marker lost from %q", private, long)
		}
		if got, want := lipgloss.Width(long), lipgloss.Width(short); got != want {
			t.Errorf("private=%v: long name renders %d wide, short name %d", private, got, want)
		}
	}
}

// Filtering matches the repository name as well as the project name.
func TestItemFilterValue(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(context.Background(), 80, 20)
	for _, tc := range []struct {
		repo proto.Repository
		want []string
	}{
		{fakeRepo{name: "repo"}, []string{"repo"}},
		{fakeRepo{name: "repo", projectName: "Project"}, []string{"Project", "repo"}},
	} {
		got := NewItem(c, tc.repo).FilterValue()
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("FilterValue() = %q, lacks %q", got, w)
			}
		}
	}
}

// A filter that matches the repository name highlights nothing: the name is
// not shown, and the match must not spill onto the visibility marker.
func TestItemFilterHighlightsTitleOnly(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(context.Background(), 80, 20)
	item := NewItem(c, fakeRepo{name: "zzz", projectName: "Project"})
	pane := selectorPane
	d := NewItemDelegate(&c, &pane)
	m := list.New([]list.Item{item}, d, 80, 20)
	m.SetFilterText("zzz")
	var buf bytes.Buffer
	d.Render(&buf, m, 0, m.VisibleItems()[0])
	// The match lies past the title, so the title and its marker are
	// rendered in one piece, with nothing underlined.
	if want := "Project " + common.VisibilityMarker(false); !strings.Contains(buf.String(), want) {
		t.Errorf("item %q lacks an unbroken %q", buf.String(), want)
	}
}

// A project name or description with line breaks still renders on one line
// each, so the item keeps the height the list reserves for it.
func TestItemMultiLineFields(t *testing.T) {
	t.Parallel()
	const width = 80
	plain := renderItem(t, fakeRepo{name: "repo"}, width)
	out := renderItem(t, fakeRepo{
		name:        "repo",
		projectName: "My\nProject",
		description: "first line\r\nsecond\tline\n\nthird line\n",
	}, width)
	if got, want := lipgloss.Height(out), lipgloss.Height(plain); got != want {
		t.Fatalf("item renders %d lines, want %d:\n%s", got, want, out)
	}
	if !strings.Contains(out, "My Project") {
		t.Errorf("item %q lacks the joined project name", out)
	}
	if !strings.Contains(out, "first line second line third line") {
		t.Errorf("item %q lacks the joined description", out)
	}
}
