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
// handler runs, discard keeps the loop from blocking. Refused
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

// discardWindowChangesAfter wraps a session handler so that the session's
// window changes are discarded once the handler returns. The TUI reads them
// while it runs, and nothing does after it exits. It must wrap every
// middleware that may end a session, including the session limit.
func discardWindowChangesAfter(next ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		// Take the channel now, as the other middleware read the pty when
		// the session starts: Pty also copies the window size, which the
		// library's request loop writes without a lock on every window
		// change, so a later call would race with one more of them.
		_, winch, _ := s.Pty()
		defer discard(winch)
		next(s)
	}
}

// discard reads and drops the window changes on winch, if any, until the
// request loop closes it, so the loop never blocks on a window change nobody
// reads (see filterSessionRequests). CommandMiddleware calls it for a
// command run with a pty, which has no TUI to read them.
func discard(winch <-chan ssh.Window) {
	if winch == nil {
		return
	}
	go func() {
		for range winch {
		}
	}()
}
