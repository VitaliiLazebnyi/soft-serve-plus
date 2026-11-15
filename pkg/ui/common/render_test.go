package common

import "testing"

// Renders asked for while one runs collapse into one more, started when the
// running one returns.
func TestRenderGate(t *testing.T) {
	var g RenderGate
	epoch, ok := g.Start()
	if !ok {
		t.Fatal("the first render did not start")
	}
	for range 10 {
		if _, ok := g.Start(); ok {
			t.Fatal("a second render started while one was in flight")
		}
	}
	if !g.InFlight() {
		t.Fatal("no render in flight")
	}
	if !g.Done(epoch) {
		t.Fatal("the renders asked for meanwhile were forgotten")
	}
	if g.InFlight() {
		t.Fatal("a render still in flight after Done")
	}
	epoch, ok = g.Start()
	if !ok {
		t.Fatal("the coalesced render did not start")
	}
	if g.Done(epoch) {
		t.Fatal("Done asked for another render when none was wanted")
	}
}

// After Reset a render starts at once, and the late result of the render
// before it does not end the new one.
func TestRenderGateReset(t *testing.T) {
	var g RenderGate
	lost, _ := g.Start()
	g.Start()
	g.Reset()
	current, ok := g.Start()
	if !ok {
		t.Fatal("no render started after Reset")
	}
	if g.Done(lost) || !g.InFlight() {
		t.Fatal("the result of a render from before Reset ended the current one")
	}
	if g.Done(current) {
		t.Fatal("a render asked for before Reset was kept")
	}
}
