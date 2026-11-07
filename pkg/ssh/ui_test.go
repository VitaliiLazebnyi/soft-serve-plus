package ssh

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// TestUIQuitKeepsZoneManagerRunning checks that quitting leaves the zone
// manager running for the program's last render. A render after the manager
// is closed blocks forever once the view has more zones than its worker
// buffers, which a tall terminal on a long list reaches.
func TestUIQuitKeepsZoneManagerRunning(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 80, 24)
	t.Cleanup(c.Zone.Close)
	ui := NewUI(c, "")

	_, cmd := ui.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("quit key returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("quit key did not quit")
	}

	// The last render scans every zone of the view.
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
