package selector

import (
	"sync"
	"testing"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

type testItem string

func (i testItem) ID() string          { return string(i) }
func (i testItem) Title() string       { return string(i) }
func (i testItem) Description() string { return "" }
func (i testItem) FilterValue() string { return string(i) }

// runCmd runs cmd and every command it batches, and returns the messages
// they produce.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, c := range batch {
			msgs = append(msgs, runCmd(c)...)
		}
		return msgs
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// bubbletea runs every command in its own goroutine while Update keeps
// changing the list. The select command must carry the item that was selected
// when Update returned, and must not read the list, which raced with Update.
func TestSelectItemCmdDoesNotReadModel(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(t.Context(), 80, 40)
	items := []IdentifiableItem{testItem("a"), testItem("b")}
	s := New(c, items, list.NewDefaultDelegate())

	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter returned no command")
	}

	var (
		wg   sync.WaitGroup
		msgs []tea.Msg
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		msgs = runCmd(cmd)
	}()
	for range 100 {
		s.SetItems(nil)
		s.Select(0)
		s.SetItems([]IdentifiableItem{testItem("c")})
	}
	wg.Wait()

	var got []SelectMsg
	for _, msg := range msgs {
		if m, ok := msg.(SelectMsg); ok {
			got = append(got, m)
		}
	}
	if len(got) != 1 || got[0].IdentifiableItem != testItem("a") {
		t.Fatalf("select messages = %#v, want one for item a", got)
	}
}
