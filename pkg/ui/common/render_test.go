package common

import "testing"

// Renders asked for while one runs collapse into one more, started when the
// running one returns.
func TestRenderGate(t *testing.T) {
	var g RenderGate
	if !g.Start() {
		t.Fatal("the first render did not start")
	}
	for range 10 {
		if g.Start() {
			t.Fatal("a second render started while one was in flight")
		}
	}
	if !g.InFlight() {
		t.Fatal("no render in flight")
	}
	if !g.Done() {
		t.Fatal("the renders asked for meanwhile were forgotten")
	}
	if g.InFlight() {
		t.Fatal("a render still in flight after Done")
	}
	if !g.Start() {
		t.Fatal("the coalesced render did not start")
	}
	if g.Done() {
		t.Fatal("Done asked for another render when none was wanted")
	}
}
