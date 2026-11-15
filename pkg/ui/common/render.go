package common

// RenderGate keeps one render of a view in flight. A render asked for while
// one runs is not started; it is remembered, and one render starts for all
// of them once the running one returns. A client that resizes its terminal
// over and over then costs one render at a time, not one goroutine and one
// copy of the content per resize.
//
// The zero value is ready to use. Only Update may call its methods, and every
// render it lets start must report back through Done, even when its result
// is stale.
type RenderGate struct {
	inFlight bool
	dirty    bool
}

// Start reports whether a render may start now. When one is in flight it
// returns false and remembers that another is wanted.
func (g *RenderGate) Start() bool {
	if g.inFlight {
		g.dirty = true
		return false
	}
	g.inFlight = true
	return true
}

// Done records that the render in flight returned. It reports whether a
// render was asked for meanwhile; the caller then starts one, for what it
// shows now, through Start.
func (g *RenderGate) Done() bool {
	g.inFlight = false
	again := g.dirty
	g.dirty = false
	return again
}

// InFlight reports whether a render is in flight.
func (g *RenderGate) InFlight() bool {
	return g.inFlight
}
