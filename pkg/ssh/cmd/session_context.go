package cmd

import (
	"context"

	"charm.land/ssh"
)

// ContextKeySession is the context key for the SSH session.
var ContextKeySession = &struct{ string }{"session"}

// SessionFromContext returns the SSH session from the context.
func SessionFromContext(ctx context.Context) ssh.Session {
	if s, ok := ctx.Value(ContextKeySession).(ssh.Session); ok {
		return s
	}
	return nil
}
