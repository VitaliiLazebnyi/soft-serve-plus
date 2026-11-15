package daemon

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/git"
)

// TestRequestLineAnswers pins the answer to each kind of request line, as
// the client sees it.
func TestRequestLineAnswers(t *testing.T) {
	ensureRepo(t, "answers-public")
	for _, tc := range []struct {
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
	} {
		line, err := rawRequest(t, []byte(pkt(tc.line)))
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
