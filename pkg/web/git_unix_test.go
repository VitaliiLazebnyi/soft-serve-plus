//go:build !windows

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// Git runs under the request's context, so a client that goes
// away ends it. The wrapper named git first on PATH holds upload-pack on a
// fifo nobody writes, so only the end of the request can end it: the
// handler then stops git (see pkg/gitserve's gitServiceHandler). Unbound, git
// and the handler would wait on the fifo for good.
func TestInfoRefsGitEndsWithRequest(t *testing.T) { // serial: t.Setenv
	f := newOracleFixture(t)
	const name = "held"
	if _, err := f.be.CreateRepository(f.ctx, name, f.owner, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	held := filepath.Join(dir, "held")
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"*' upload-pack '*) : > " + quote(held) + "; read -r _ < " + quote(fifo) + ";;\n" +
		"esac\n" +
		"exec " + quote(gitPath) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// If the test fails with git still held, as when git is not bound to
	// the request, let it go rather than leave the shell on the fifo. A
	// non-blocking open fails when nothing waits on the fifo.
	t.Cleanup(func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return
		}
		_, _ = f.WriteString("go\n")
		_ = f.Close()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+name+".git/info/refs?service=git-upload-pack", nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		done <- w
	}()
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(held); err == nil {
			break
		}
		select {
		case w := <-done:
			t.Fatalf("the request ended with %d before git upload-pack started", w.Code)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("git upload-pack never started")
		}
	}
	cancel()
	select {
	case w := <-done:
		if strings.Contains(w.Body.String(), "refs/heads") {
			t.Errorf("git advertised refs after the client went away: %q", w.Body.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("git and the handler still running after the client went away")
	}
}
