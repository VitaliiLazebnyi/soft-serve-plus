package browse

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// TestQuitKeepsZoneManagerRunning checks that quitting leaves the zone
// manager running for the program's last render, which blocks forever after
// the manager is closed once the view has more zones than its worker buffers.
func TestQuitKeepsZoneManagerRunning(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	t.Cleanup(c.Zone.Close)
	m := &model{common: c}

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("quit key returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("quit key did not quit")
	}

	var view strings.Builder
	for i := range 500 {
		view.WriteString(c.Zone.Mark(fmt.Sprintf("zone-%d", i), "x"))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Zone.Scan(view.String())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("render after quit blocked: the zone manager was closed on quit")
	}
}
