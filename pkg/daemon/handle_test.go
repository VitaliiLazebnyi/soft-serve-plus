package daemon

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/git"
)

// TestRequestLineAnswers pins the answer to each kind of request line, as
// the client sees it. It has a daemon of its own with room for every case,
// so that connections still closing cannot crowd out the next case or the
// other tests' requests to the shared daemon.
func TestRequestLineAnswers(t *testing.T) {
	ensureRepo(t, "answers-public")
	cases := []struct {
		name string
		line string
		want error // nil: served
	}{
		{"no space", "git-upload-pack", git.ErrInvalidRequest},
		{"no host parameter", "git-upload-pack /answers-public.git", git.ErrInvalidRequest},
		{"unknown service", "git-upload-anything /answers-public.git\x00host=localhost\x00", git.ErrInvalidRequest},
		{"push", "git-receive-pack /answers-public.git\x00host=localhost\x00", ErrPushNotSupported},
		{"missing repo", "git-upload-pack /answers-missing.git\x00host=localhost\x00", git.ErrNotAuthed},
		{"missing repo, archive", "git-upload-archive /answers-missing.git\x00host=localhost\x00", git.ErrNotAuthed},
		{"public repo", "git-upload-pack /answers-public.git\x00host=localhost\x00", nil},
		{"public repo without .git", "git-upload-pack /answers-public\x00host=localhost\x00", nil},
		{"public repo, empty host", "git-upload-pack /answers-public.git\x00\x00", nil},
		{"public repo, v2", "git-upload-pack /answers-public.git\x00host=localhost\x00\x00version=2\x00", nil},
	}
	_, addr := startLimitedDaemon(t, len(cases))
	for _, tc := range cases {
		line, err := rawRequestTo(t, addr, []byte(pkt(tc.line)))
		switch {
		case tc.want != nil:
			wantErrLine(t, tc.name, err, tc.want)
		case err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case line == "":
			t.Errorf("%s: empty answer", tc.name)
		}
	}
}

// TestUploadArchiveServed pins that the daemon serves upload-archive for a
// public repository: git acknowledges the archive arguments.
func TestUploadArchiveServed(t *testing.T) {
	ensureRepo(t, "archive-public")
	c := dialDaemon(t)
	defer c.Close() //nolint:errcheck
	for _, p := range []string{
		pkt("git-upload-archive /archive-public.git\x00host=localhost\x00"),
		pkt("argument --format=tar\n"), pkt("argument HEAD\n"), "0000",
	} {
		if _, err := io.WriteString(c, p); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	line, err := readPktline(c)
	if err != nil {
		t.Fatalf("archive request refused: %v", err)
	}
	if line != "ACK" {
		t.Errorf("first line = %q, want ACK", line)
	}
}

// TestHangUpWithoutRequest pins that a client that hangs up before sending
// a request gets no answer and releases its connection.
func TestHangUpWithoutRequest(t *testing.T) {
	d, addr := startLimitedDaemon(t, 1)
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(c)
	if err != nil && !isReset(err) {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("answer to a client that sent nothing: %q", out)
	}
	waitConns(t, d, 0, 30*time.Second)
}

// waitConns waits up to within until d holds n connections.
func waitConns(t *testing.T, d *GitDaemon, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for d.conns.Size() != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections open after %v, want %d", d.conns.Size(), within, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitLog waits up to within until logs holds substr. A line the daemon
// writes after the client has seen the effect it goes with, such as a
// panic logged once its connection is closed, may not be there yet when
// the client sees that effect, so a test cannot read the logs just once.
func waitLog(t *testing.T, logs *syncBuffer, substr string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(logs.String(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("%q not logged after %v; logs:\n%s", substr, within, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
