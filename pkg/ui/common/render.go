package common

// RenderGate keeps one render of a view in flight. A render asked for while
// one runs is not started; it is remembered, and one render starts for all
// of them once the running one returns. A client that resizes its terminal
// over and over then costs one render at a time, not one goroutine and one
// copy of the content per resize.
//
// The zero value is ready to use. Only Update may call its methods. Every
// render it lets start must report back through Done with the epoch Start
// gave it, even when its result is stale. Reset forgets a render whose
// result may never come back, such as one started before the view was
// reset; its late Done is then ignored.
type RenderGate struct {
	inFlight bool
	dirty    bool
	epoch    int
}

// Start reports whether a render may start now, and the epoch the render
// must hand to Done. When one is in flight it returns false and remembers
// that another is wanted.
func (g *RenderGate) Start() (epoch int, ok bool) {
	if g.inFlight {
		g.dirty = true
		return g.epoch, false
	}
	g.inFlight = true
	return g.epoch, true
}

// Done records that the render started in epoch returned. It reports
// whether a render was asked for meanwhile; the caller then starts one, for
// what it shows now, through Start. A render from before the last Reset is
// ignored.
func (g *RenderGate) Done(epoch int) bool {
	if epoch != g.epoch {
		return false
	}
	g.inFlight = false
	again := g.dirty
	g.dirty = false
	return again
}

// Reset forgets the render in flight, if any, and any render asked for.
func (g *RenderGate) Reset() {
	g.epoch++
	g.inFlight = false
	g.dirty = false
}

// InFlight reports whether a render is in flight.
func (g *RenderGate) InFlight() bool {
	return g.inFlight
}
