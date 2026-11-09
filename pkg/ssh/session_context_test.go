package ssh

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	gossh "golang.org/x/crypto/ssh"
)

// TestSessionContextPerSession checks that concurrent sessions multiplexed over
// one connection (as OpenSSH's ControlMaster does) each see their own session,
// and so their own GIT_PROTOCOL, in the context the commands run with.
func TestSessionContextPerSession(t *testing.T) {
	t.Parallel()

	var arrived sync.WaitGroup
	arrived.Add(2)
	allArrived := make(chan struct{})
	go func() {
		arrived.Wait()
		close(allArrived)
	}()

	handler := func(s ssh.Session) {
		ctx := sessionContext(s)
		// Hold every session until both have gone through the
		// middleware, so a session stored per connection would be
		// overwritten by the other one.
		arrived.Done()
		select {
		case <-allArrived:
		case <-time.After(10 * time.Second):
			_, _ = io.WriteString(s.Stderr(), "timed out waiting for the other session")
			_ = s.Exit(1)
			return
		}
		var proto string
		if sess := sshutils.SessionFromContext(ctx); sess != nil {
			for _, env := range sess.Environ() {
				if v, ok := strings.CutPrefix(env, "GIT_PROTOCOL="); ok {
					proto = v
				}
			}
		}
		_, _ = io.WriteString(s, proto)
	}

	srv := &ssh.Server{
		Handler: ContextMiddleware(nil, nil, nil, nil, log.Default())(handler),
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = srv.Close() })

	c, err := gossh.Dial("tcp", l.Addr().String(), &gossh.ClientConfig{
		User:            "test",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	protos := []string{"version=1", "version=2"}
	got := make([]string, len(protos))
	errs := make([]error, len(protos))
	var wg sync.WaitGroup
	for i, p := range protos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, err := c.NewSession()
			if err != nil {
				errs[i] = err
				return
			}
			defer sess.Close() //nolint: errcheck
			if err := sess.Setenv("GIT_PROTOCOL", p); err != nil {
				errs[i] = err
				return
			}
			out, err := sess.Output("git-upload-pack 'repo'")
			got[i], errs[i] = string(out), err
		}()
	}
	wg.Wait()

	for i, p := range protos {
		if errs[i] != nil {
			t.Fatalf("session %d: %v", i, errs[i])
		}
		if got[i] != p {
			t.Errorf("session %d saw GIT_PROTOCOL %q, want %q", i, got[i], p)
		}
	}
}
