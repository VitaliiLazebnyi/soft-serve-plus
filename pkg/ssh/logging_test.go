package ssh

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type logTestUser struct{ proto.User }

func (logTestUser) Username() string { return "alice" }

// TestLoggingMiddlewarePtyKeepsFields checks that a PTY session logs the
// terminal details in addition to the address, username and command, rather
// than in place of them.
func TestLoggingMiddlewarePtyKeepsFields(t *testing.T) {
	var buf syncBuffer
	logger := log.NewWithOptions(&buf, log.Options{Level: log.DebugLevel})

	sess := testsession.New(t, &ssh.Server{
		Handler: func(s ssh.Session) {
			s.Context().SetValue(log.ContextKey, logger)
			s.Context().SetValue(proto.ContextKeyUser, proto.User(logTestUser{}))
			LoggingMiddleware(func(s ssh.Session) {
				s.Exit(0) //nolint: errcheck
			})(s)
		},
	}, nil)

	if err := sess.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	if err := sess.Run("repo ls"); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := buf.String()
	var connected string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "connected") && !strings.Contains(line, "disconnected") {
			connected = line
		}
	}
	if connected == "" {
		t.Fatalf("no connected log line in output:\n%s", out)
	}
	for _, want := range []string{"username=alice", "addr=", `cmd="[repo ls]"`, "term=xterm", "width=80", "height=40"} {
		if !strings.Contains(connected, want) {
			t.Errorf("connected log line %q is missing %q", connected, want)
		}
	}
}
