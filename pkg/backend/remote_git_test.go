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
	ln.Close() //nolint:errcheck

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

// The buffer keeps all of a short output, and of a long one its start and
// its end, cut to whole words, with a mark for what it left out between.
func TestHeadTailBuffer(t *testing.T) {
	t.Parallel()
	short := headTailBuffer{limit: 8}
	writeAll(&short, "ab", "cdef ", "gh")
	if got := short.String(); got != "abcdef gh" {
		t.Errorf("short output = %q, want all of it", got)
	}

	long := headTailBuffer{limit: 8}
	writeAll(&long, "one two three", " four five six ", "seven eight")
	// Kept: "one two " (head) and "en eight" (tail), cut forward to
	// " eight".
	want := "one two \n... (23 bytes left out) ...\n eight"
	if got := long.String(); got != want {
		t.Errorf("long output = %q, want %q", got, want)
	}

	// A single write longer than the buffer.
	big := headTailBuffer{limit: 4}
	writeAll(&big, "a b c d e f g h")
	if got, want := big.String(), "a b \n... (7 bytes left out) ...\n g h"; got != want {
		t.Errorf("one long write = %q, want %q", got, want)
	}
}

// writeAll writes each chunk to b, as os/exec does with what it reads from
// git's pipe.
func writeAll(b *headTailBuffer, chunks ...string) {
	for _, c := range chunks {
		_, _ = b.Write([]byte(c))
	}
}

// A URL cut by the cap must not survive in part: cut just before "@", the
// rest reads as a URL whose host is the token, which redaction keeps. The
// kept text ends and starts at whitespace instead, whichever end of the
// buffer the URL falls on and wherever in it the cut lands, the limit
// itself included.
func TestRemoteGitOutputCutURL(t *testing.T) {
	t.Parallel()
	const token = "ghp_SECRETTOKEN"
	url := "https://" + token + "@github.com/o/r.git"
	filler := strings.Repeat("remote: noise\n", remoteGitOutputLimit/7)
	half := remoteGitOutputLimit / 2
	for cut := 0; cut <= len(url); cut++ {
		// The head ends cut bytes into the URL.
		b := headTailBuffer{limit: half}
		pad := strings.Repeat("x", half-len(" fatal: ")-cut)
		writeAll(&b, pad+" fatal: "+url+" more\n", filler)
		assertNoToken(t, "head", cut, remoteGitOutput(&b), token)

		// The tail starts cut bytes into the URL.
		b = headTailBuffer{limit: half}
		after := url[cut:] + " tail\n"
		writeAll(&b, filler, "remote: "+url+" tail\n", strings.Repeat("y", half-len(after)-1)+"\n")
		assertNoToken(t, "tail", cut, remoteGitOutput(&b), token)
	}
}

func assertNoToken(t *testing.T, where string, cut int, out, token string) {
	t.Helper()
	// Even a prefix of the token is too much.
	if strings.Contains(out, token[:4]) {
		t.Errorf("%s cut at %d keeps part of the token: %q", where, cut, out)
	}
	if !strings.Contains(out, "bytes left out") {
		t.Errorf("%s cut at %d: the output is not marked as cut", where, cut)
	}
}

// os/exec copies git's stderr pipe into the buffer with io.Copy, which
// prefers the writer's ReadFrom, so the cap must hold on that path too.
func TestHeadTailBufferCopyFromPipe(t *testing.T) {
	t.Parallel()
	if _, ok := any(&headTailBuffer{}).(io.ReaderFrom); ok {
		t.Fatal("headTailBuffer implements io.ReaderFrom, which bypasses its limit")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck

	const total = 1 << 20
	go func() {
		defer w.Close() //nolint:errcheck
		_, _ = w.Write(bytes.Repeat([]byte("x "), total/2))
	}()

	b := headTailBuffer{limit: 2048}
	n, err := io.Copy(&b, r)
	if err != nil {
		t.Fatal(err)
	}
	if n != total {
		t.Errorf("io.Copy copied %d bytes, want %d", n, total)
	}
	if len(b.head) != 2048 || len(b.tail) != 2048 || b.dropped != total-4096 {
		t.Errorf("kept %d+%d bytes, left out %d; want 2048+2048 and %d", len(b.head), len(b.tail), b.dropped, total-4096)
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
