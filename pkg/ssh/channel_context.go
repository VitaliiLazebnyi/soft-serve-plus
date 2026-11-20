package ssh

import (
	"context"
	"time"

	"charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// channelContext wraps a session channel handler so that the session runs
// with a context of its own, done once the channel's request loop returns,
// that is once the channel is closed, or once the connection's context is
// done. charm.land/ssh gives every session the connection's context, which
// ends only with the connection, so over a connection that carries several
// sessions (OpenSSH's ControlMaster) a client that closed one session's
// channel would otherwise leave its command running as if the client were
// still there, and a git service that fails because the client went away
// would be taken for a server failure.
func channelContext(next ssh.ChannelHandler) ssh.ChannelHandler {
	return func(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
		done, cancel := context.WithCancel(ctx)
		defer cancel()
		next(srv, conn, newChan, &sessionCtx{Context: ctx, done: done})
	}
}

// sessionCtx is the connection's ssh.Context with the cancellation of one
// session channel. Values, and everything else ssh.Context offers, are the
// connection's.
type sessionCtx struct {
	ssh.Context
	done context.Context
}

// Deadline implements context.Context.
func (c *sessionCtx) Deadline() (time.Time, bool) { return c.done.Deadline() }

// Done implements context.Context.
func (c *sessionCtx) Done() <-chan struct{} { return c.done.Done() }

// Err implements context.Context.
func (c *sessionCtx) Err() error { return c.done.Err() }
