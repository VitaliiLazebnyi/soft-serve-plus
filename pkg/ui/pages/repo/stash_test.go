package repo

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
)

// copyMsgs runs cmd and every command batched under it, and returns the
// CopyMsgs they produce.
func copyMsgs(cmd tea.Cmd) []CopyMsg {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case CopyMsg:
		return []CopyMsg{msg}
	case tea.BatchMsg:
		var msgs []CopyMsg
		for _, c := range msg {
			msgs = append(msgs, copyMsgs(c)...)
		}
		return msgs
	}
	return nil
}

// Pressing the copy key on the stash list copies the selected stash message
// once, and does nothing when there are no stashes.
func TestStashCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	copyKey := tea.KeyPressMsg{Code: 'c', Text: "c"}

	for _, tc := range []struct {
		name    string
		stashes StashListMsg
		want    int
	}{
		{name: "no stashes", stashes: StashListMsg{}, want: 0},
		{name: "one stash", stashes: StashListMsg{stashes: []*gitm.Stash{{Index: 0, Message: "WIP on main"}}}, want: 1},
		{name: "two stashes", stashes: StashListMsg{stashes: []*gitm.Stash{
			{Index: 0, Message: "WIP on main"},
			{Index: 1, Message: "older"},
		}}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := NewStash(uitest.NewCommon(t, ctx, nil, nil))
			s.SetSize(80, 40)
			s.Init()
			s.Update(tc.stashes)

			cmd := s.Update(copyKey)
			msgs := copyMsgs(cmd)
			if len(msgs) != tc.want {
				t.Fatalf("copy produced %d CopyMsgs, want %d: %v", len(msgs), tc.want, msgs)
			}
			for _, msg := range msgs {
				if msg.Text != tc.stashes.stashes[0].Message {
					t.Errorf("copied %q, want %q", msg.Text, tc.stashes.stashes[0].Message)
				}
			}
		})
	}
}

// A patch arriving while no stash is selected renders without a title
// instead of panicking.
func TestStashPatchWithoutSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewStash(uitest.NewCommon(t, ctx, nil, nil))
	s.SetSize(80, 40)
	s.Init()
	s.Update(StashListMsg{})
	updateRendered(s, StashPatchMsg{Diff: &git.Diff{}})
	if s.state != stashStatePatch {
		t.Fatalf("state = %v, want the patch view", s.state)
	}
}
