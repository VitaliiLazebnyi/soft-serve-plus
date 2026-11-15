package common

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// Loader keeps one load of a kind in flight. Starting a load cancels the
// one before it: the newer request wins, and the older one's git processes
// stop instead of running to completion for a result nobody will see.
//
// The zero value is ready to use. A Loader belongs to the model that starts
// the loads, and only Update may call its methods.
type Loader struct {
	cancel context.CancelFunc
}

// Start cancels the load in flight, if any, and returns the context of a new
// one, derived from parent. The load's command must run its git commands
// under it, and drop its result once it is done.
func (l *Loader) Start(parent context.Context) context.Context {
	l.Stop()
	ctx, cancel := context.WithCancel(parent)
	l.cancel = cancel
	return ctx
}

// Stop cancels the load in flight, if any.
func (l *Loader) Stop() {
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
}

// LoadError returns the message for a load that failed with err: nothing
// when the load was cancelled, because a newer one replaces it or the
// session has ended, or else an ErrorMsg.
func LoadError(ctx context.Context, err error) tea.Msg {
	if ctx.Err() != nil {
		return nil
	}
	return ErrorMsg{Err: err}
}
