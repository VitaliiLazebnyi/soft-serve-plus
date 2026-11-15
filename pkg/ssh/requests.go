package ssh

import (
	"charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// gitProtocolEnv is the one environment variable a session may set. The git
// commands pass it on to git; nothing else reads the session's environment.
const gitProtocolEnv = "GIT_PROTOCOL"

// maxGitProtocolLength is the longest GIT_PROTOCOL value, in bytes, a session
// may set. Git sends a few words such as "version=2".
const maxGitProtocolLength = 256

// filterSessionRequests wraps a session channel handler so that the handler
// only sees the channel requests the server can afford to pass on.
//
// charm.land/ssh v0.4.3 handles every request in one loop per channel, and
// two kinds cost the server without bound:
//
//   - window-change: with an emulated pty the loop sends each new size to a
//     channel that holds one. Only a running TUI reads it, so a client that
//     sends pty-req and then window-change before any command blocks the
//     loop for ever, and the goroutine, the session and what the client
//     wrote to its channel outlive the connection.
//   - env: every request before the command is stored in the session until
//     the connection closes. Requests are not flow-controlled, and each may
//     be up to 256 KiB.
//
// The library's send would better be non-blocking, but the server does not
// carry a fork of it, so this filter stands in front of the loop: it refuses
// window-change until a shell or exec request has started the session's
// handler (the server has no subsystems), and accepts one short GIT_PROTOCOL
// and no other env request, as OpenSSH does without AcceptEnv. Once the
// handler runs, forwardWindowChanges keeps the loop from blocking. Refused
// requests get a false reply when the client wants one; clients carry on
// without them.
func filterSessionRequests(next ssh.ChannelHandler) ssh.ChannelHandler {
	return func(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
		next(srv, conn, filteredChannel{newChan}, ctx)
	}
}

// filteredChannel is a new session channel whose requests, once accepted,
// pass through filterRequests.
type filteredChannel struct {
	gossh.NewChannel
}

func (c filteredChannel) Accept() (gossh.Channel, <-chan *gossh.Request, error) {
	ch, reqs, err := c.NewChannel.Accept()
	if err != nil {
		return ch, reqs, err
	}
	out := make(chan *gossh.Request)
	go filterRequests(reqs, out)
	return ch, out, nil
}

// filterRequests passes the requests from in to out, refusing those
// filterSessionRequests describes, and closes out once in is closed.
func filterRequests(in <-chan *gossh.Request, out chan<- *gossh.Request) {
	defer close(out)
	started, hasGitProtocol := false, false
	for req := range in {
		switch req.Type {
		case "shell", "exec":
			started = true
		case "window-change":
			if !started {
				_ = req.Reply(false, nil)
				continue
			}
		case "env":
			if started || hasGitProtocol || !isGitProtocolEnv(req.Payload) {
				_ = req.Reply(false, nil)
				continue
			}
			hasGitProtocol = true
		}
		out <- req
	}
}

// isGitProtocolEnv reports whether an env request's payload sets
// GIT_PROTOCOL to a value of at most maxGitProtocolLength bytes.
func isGitProtocolEnv(payload []byte) bool {
	var kv struct{ Key, Value string }
	if err := gossh.Unmarshal(payload, &kv); err != nil {
		return false
	}
	return kv.Key == gitProtocolEnv && len(kv.Value) <= maxGitProtocolLength
}

// forwardWindowChanges wraps a session handler so that, from the moment the
// handler starts, the session's window changes are read from the library's
// one-element channel and never left to block its request loop. It is the
// outermost session wrapper, so the loop keeps draining through every inner
// middleware.
//
// charm.land/ssh fills that channel from the request loop and only a running
// TUI drains it. Without another reader the loop blocks on the next
// window-change, and the whole connection then stops: the loop stops reading
// requests, x/crypto's request buffer fills, and the connection read loop
// blocks behind it, so the server never sees the window adjust, the
// disconnect or a timeout. Draining only after the handler returns is not
// enough, because the handler can block first -- on a write the client is
// not reading (the length or auth check, the session-limit refusal, the "no
// access" message) or inside the TUI whose renderer the client has stalled.
//
// So this reads the channel for the whole life of the handler and hands the
// inner middleware a session whose Pty returns a latest-wins copy of it: the
// TUI still resizes, and every other middleware can write to the client
// without the loop blocking behind it.
//
// The forwarding goroutine reads only the channel, never the session's pty,
// so it does not race with the library's unlocked write of the pty's window
// on each change. It ends when the library closes the channel, which it does
// when the request loop returns at session end. The context is not used to
// stop it: a goroutine that stopped early could leave the loop blocked on a
// send with no reader, the very deadlock this prevents.
func forwardWindowChanges(next ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		// Read the pty once, here at the start. Pty copies the window the
		// request loop writes without a lock, so a later read by inner
		// middleware would race with a window-change; windowSession hands
		// them this captured value instead. This first read can still race
		// with a change in flight, which is the library's own race.
		pty, winch, ok := s.Pty()
		if winch == nil {
			next(s)
			return
		}
		latest := make(chan ssh.Window, 1)
		go func() {
			defer close(latest)
			for w := range winch {
				// Keep only the newest size, and never block the loop.
				select {
				case <-latest:
				default:
				}
				latest <- w
			}
		}()
		next(windowSession{Session: s, pty: pty, winch: latest, ok: ok})
	}
}

// windowSession is a session whose Pty returns a window-change channel that
// never blocks the library's request loop (see forwardWindowChanges), and
// the pty captured when the handler started.
type windowSession struct {
	ssh.Session
	pty   ssh.Pty
	winch <-chan ssh.Window
	ok    bool
}

func (s windowSession) Pty() (ssh.Pty, <-chan ssh.Window, bool) {
	return s.pty, s.winch, s.ok
}
