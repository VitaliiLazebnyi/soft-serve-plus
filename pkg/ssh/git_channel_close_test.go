package ssh

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// writeManyRefs gives the bare repository dir n branches on one empty
// commit, written as packed-refs, so its ref advertisement is about 60
// bytes per branch.
func writeManyRefs(t *testing.T, dir string, n int) {
	t.Helper()
	run := func(stdin string, args ...string) string {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	commit := run("", "commit-tree", run("", "mktree"), "-m", "refs")
	var b strings.Builder
	b.WriteString("# pack-refs with: peeled fully-peeled sorted \n")
	for i := range n {
		fmt.Fprintf(&b, "%s refs/heads/b%07d\n", commit, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// R32 / SSH-4: a fetch whose channel the client closes while the connection
// stays open, as interrupting a clone over an OpenSSH ControlMaster
// connection does, must not keep git running and the session's place under
// max_sessions. The session context is the connection's, so only the failed
// write to the closed channel can stop git.
func TestSSHFetchChannelClosedFreesSession(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.MaxSessions = 1
	addr, ctx := startTestServer(t, cfg)

	be := backend.FromContext(ctx)
	if _, err := be.CreateRepository(ctx, "big", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	// About 3 MB of advertisement: more than the client's channel window
	// and git's pipe together, so git blocks writing it while the client
	// reads nothing.
	writeManyRefs(t, filepath.Join(cfg.DataPath, "repos", "big.git"), 50_000)

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck

	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := s.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("git-upload-pack big"); err != nil {
		t.Fatal(err)
	}
	// Wait for the advertisement to start, then stop reading it and close
	// the channel, keeping the connection.
	if _, err := stdout.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	_ = s.Close()

	// The channel's place is freed when it closes, but the command's only
	// when its handler returned, which it does only once git is gone. Until
	// then another command on the connection is refused.
	ctxWait, cancel := context.WithTimeout(t.Context(), sessionLoopsWait)
	defer cancel()
	var last string
	for ctxWait.Err() == nil {
		s2, err := c.NewSession()
		if err != nil {
			last = err.Error()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var stderr bytes.Buffer
		s2.Stderr = &stderr
		err = s2.Run("repo list")
		_ = s2.Close()
		if err == nil {
			return
		}
		last = fmt.Sprintf("%v: %s", err, stderr.String())
		if !strings.Contains(stderr.String(), msgTooManySessions) {
			t.Fatalf("repo list: %s", last)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("command still refused after the fetch's channel closed: %s", last)
}
