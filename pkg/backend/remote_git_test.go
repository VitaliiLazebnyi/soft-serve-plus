package backend

import (
	"bytes"
	"io"
	"net"
	"os"
	"strings"
	"testing"
)

// The error names the command, whose remote URL can carry credentials, as
// an import's clone does, so they are removed there too.
func TestRunRemoteGitRedactsCommand(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// A port nothing listens on, so git fails at once.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() //nolint:errcheck,gosec

	err = RunRemoteGit(t.Context(), t.TempDir(), nil, "ls-remote", "--", "http://user:secret@"+addr+"/x.git?token=abc")
	if err == nil {
		t.Fatal("git succeeded against a closed port")
	}
	msg := err.Error()
	if !strings.Contains(msg, "git ls-remote -- http://"+addr+"/x.git") {
		t.Errorf("error %q does not name the command", msg)
	}
	for _, secret := range []string{"user", "secret", "token=abc"} {
		if strings.Contains(msg, secret) {
			t.Errorf("error %q leaks %q", msg, secret)
		}
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := limitedBuffer{limit: 4}
	for _, s := range []string{"ab", "cdef", "gh"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := b.String(); got != "abcd" {
		t.Errorf("buffer = %q, want %q", got, "abcd")
	}
}

// os/exec copies git's stderr pipe into the buffer with io.Copy, which
// prefers the writer's ReadFrom, so the cap must hold on that path too.
func TestLimitedBufferCopyFromPipe(t *testing.T) {
	if _, ok := any(&limitedBuffer{}).(io.ReaderFrom); ok {
		t.Fatal("limitedBuffer implements io.ReaderFrom, which bypasses its limit")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck

	const total = 1 << 20
	go func() {
		defer w.Close() //nolint:errcheck
		_, _ = w.Write(bytes.Repeat([]byte("x"), total))
	}()

	b := limitedBuffer{limit: 4096}
	n, err := io.Copy(&b, r)
	if err != nil {
		t.Fatal(err)
	}
	if n != total {
		t.Errorf("io.Copy copied %d bytes, want %d", n, total)
	}
	if got := len(b.String()); got != 4096 {
		t.Errorf("buffer holds %d bytes, want 4096", got)
	}
}

func TestRedactURLs(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ in, want string }{
		{
			in:   "fatal: unable to access 'http://127.0.0.1:9/x.git?token=abc/': Failed to connect",
			want: "fatal: unable to access 'http://127.0.0.1:9/x.git': Failed to connect",
		},
		{
			in:   "remote: see https://u:p@example.com/a?k=v and git://h/r.git",
			want: "remote: see https://example.com/a and git://h/r.git",
		},
		{
			in:   "fatal: could not read from remote repository",
			want: "fatal: could not read from remote repository",
		},
	} {
		if got := redactURLs(tt.in); got != tt.want {
			t.Errorf("redactURLs(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
