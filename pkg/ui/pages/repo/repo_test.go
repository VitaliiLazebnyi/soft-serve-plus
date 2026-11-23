package repo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
)

type fakeRepo struct {
	private     bool
	projectName string
	description string
}

func (r fakeRepo) ID() int64                                     { return 1 }
func (r fakeRepo) Name() string                                  { return "repo" }
func (r fakeRepo) ProjectName() string                           { return r.projectName }
func (r fakeRepo) Description() string                           { return r.description }
func (r fakeRepo) IsPrivate() bool                               { return r.private }
func (r fakeRepo) IsMirror() bool                                { return false }
func (r fakeRepo) IsHidden() bool                                { return false }
func (r fakeRepo) UserID() int64                                 { return 1 }
func (r fakeRepo) CreatedAt() time.Time                          { return time.Time{} }
func (r fakeRepo) UpdatedAt() time.Time                          { return time.Time{} }
func (r fakeRepo) Open(context.Context) (*git.Repository, error) { return nil, nil }

var _ proto.Repository = fakeRepo{}

// The repository page header shows the repository's visibility next to its
// name, public as well as private.
func TestHeaderVisibilityMarker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		private      bool
		want, absent string
	}{
		{private: true, want: "🔒", absent: "🌐"},
		{private: false, want: "🌐", absent: "🔒"},
	} {
		r := &Repo{
			common:       uitest.NewCommon(t, context.Background(), nil, nil),
			width:        80,
			height:       24,
			selectedRepo: fakeRepo{private: tc.private},
		}
		header := r.headerView()
		if !strings.Contains(header, tc.want) {
			t.Errorf("private=%v: header %q lacks %q", tc.private, header, tc.want)
		}
		if strings.Contains(header, tc.absent) {
			t.Errorf("private=%v: header %q shows %q", tc.private, header, tc.absent)
		}
	}
}

// Switching to a tab with nothing to show, such as an empty Tags tab, must
// not keep the previous tab's status value, and a copy notice must go away
// once the status is refreshed.
func TestStatusBarDoesNotKeepStaleValues(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	r := New(c, NewLog(c), NewRefs(c, git.RefsTags))
	r.SetSize(120, 24)
	r.selectedRepo = fakeRepo{}
	r.state = readyState

	r.statusbar.SetStatus("repo", "abc1234 by Alice", "p. 2/3", "*")
	r.activeTab = 1
	r.setStatusBarInfo()
	view := r.statusbar.View()
	for _, stale := range []string{"abc1234", "p. 2/3"} {
		if strings.Contains(view, stale) {
			t.Errorf("status bar still shows %q:\n%s", stale, view)
		}
	}

	r.Update(CopyMsg{Text: "x", Message: "Reference copied to clipboard"})
	if view := r.statusbar.View(); !strings.Contains(view, "Reference copied to clipboard") {
		t.Errorf("status bar lacks the copy notice:\n%s", view)
	}
	r.setStatusBarInfo()
	if view := r.statusbar.View(); strings.Contains(view, "copied") {
		t.Errorf("copy notice stuck in the status bar:\n%s", view)
	}
}
