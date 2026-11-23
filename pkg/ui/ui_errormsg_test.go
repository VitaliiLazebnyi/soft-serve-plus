package ui

import "testing"

// errorLike is a message that happens to have an Error method.
type errorLike struct{}

func (errorLike) Error() string { return "not an error report" }

// Only common.ErrorMsg shows the error screen, not any message with an Error
// method.
func TestUIErrorScreenOnlyForErrorMsg(t *testing.T) {
	t.Parallel()
	ui, p := newTestUI(t, selectionPage)
	ui.Update(errorLike{})
	if ui.state == errorState || ui.error != nil {
		t.Fatal("a message with an Error method showed the error screen")
	}
	if len(p.msgs) != 1 {
		t.Fatalf("page got %d messages; want the message passed on", len(p.msgs))
	}
}
