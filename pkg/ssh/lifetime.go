package ssh

import (
	"sync"
	"time"

	"charm.land/ssh"
)

// noAccountLifetime is how long an SSH connection may spend with no command
// running, unless a session on it runs as an account or with an admin key.
// Connections with a key no account owns, or with no key, are closed then.
//
// The server accepts every key, so without this a connection that can do no
// more than an anonymous clone keeps its place under max_connections and
// max_connections_per_ip for as long as it sends keepalives: eight addresses
// could hold every place. The idle timeout does not help, since every
// packet, a keepalive included, resets it.
//
// The clock stops while a handler runs and carries on from where it stopped
// when the handler ends, not from the start, so a client cannot keep its
// place by running a short command now and then, and an anonymous clone or
// push is never cut off mid-transfer. The trade-off is that a handler that
// stays up holds the connection open for as long as it runs: an idle TUI
// session, or a git command left waiting for the client's input, pauses the
// clock indefinitely. Nothing here bounds that; max_timeout does, and it
// defaults to 6 hours (config.DefaultMaxTimeout). What this does bound is
// a connection that runs no command and only answers keepalives, which
// would otherwise hold its place for as long as it likes.
const noAccountLifetime = 10 * time.Minute

// connLifetimeKey is the connection context key of the connection's
// connLifetime.
type connLifetimeKey struct{}

// connLifetime closes a connection once it has spent its lifetime with no
// command running, unless stopped first. See noAccountLifetime.
type connLifetime struct {
	mu      sync.Mutex
	timer   *time.Timer
	left    time.Duration // lifetime left when the clock last started
	since   time.Time     // when the clock last started
	running int           // commands running
	done    bool          // stopped, or the lifetime is over
}

// start starts the clock of a lifetime of d, calling expire when it is
// over.
func (l *connLifetime) start(d time.Duration, expire func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.left, l.since = d, time.Now()
	l.timer = time.AfterFunc(d, func() {
		l.mu.Lock()
		if l.done {
			l.mu.Unlock()
			return
		}
		l.done = true
		l.mu.Unlock()
		expire()
	})
}

// commandStarted stops the clock while a command runs.
func (l *connLifetime) commandStarted() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.running++
	if l.running == 1 && l.timer.Stop() {
		l.left -= time.Since(l.since)
	}
}

// commandEnded starts the clock again, with the time it had left, once no
// command runs.
func (l *connLifetime) commandEnded() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.running--
	if l.running == 0 {
		l.since = time.Now()
		l.timer.Reset(max(l.left, 0))
	}
}

// stop stops the clock for good.
func (l *connLifetime) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done = true
	l.timer.Stop()
}

// hasAccount keeps the connection of ctx open past noAccountLifetime.
func hasAccount(ctx ssh.Context) {
	if l, _ := ctx.Value(connLifetimeKey{}).(*connLifetime); l != nil {
		l.stop()
	}
}

// pauseLifetime wraps a session handler so that the lifetime of its
// connection, if it has no account, does not run while the handler does.
func pauseLifetime(next ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		l, _ := s.Context().Value(connLifetimeKey{}).(*connLifetime)
		if l == nil {
			next(s)
			return
		}
		l.commandStarted()
		defer l.commandEnded()
		next(s)
	}
}
