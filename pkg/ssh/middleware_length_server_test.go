package ssh

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	gossh "golang.org/x/crypto/ssh"
)

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSSHServerRefusesOverlongCommand checks that the server built by
// NewSSHServer refuses an overlong exec command from an unregistered key with
// ErrCommandTooLong alone. A cobra "unknown command" error would mean the
// command handler parsed it, and a debug log line carrying the command would
// mean LoggingMiddleware parsed it, so the length check must be wired in
// ahead of both.
func TestSSHServerRefusesOverlongCommand(t *testing.T) {
	var logs syncBuffer
	logger := log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel})

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	noAccess := access.NoAccess
	cfg.AnonAccess = &noAccess
	allowKeyless := false
	cfg.AllowKeyless = &allowKeyless
	addr := startSSHServer(t, cfg, logger)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	run := func(cmd string) (string, error) {
		t.Helper()
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		defer sess.Close() //nolint: errcheck
		var stderr bytes.Buffer
		sess.Stderr = &stderr
		err = sess.Run(cmd)
		return stderr.String(), err
	}

	// An unknown command at the limit gets past the length check and is
	// refused further down the chain with some other error.
	stderr, err := run(strings.Repeat("a", MaxCommandLength))
	if err == nil {
		t.Fatal("command at the limit from an unregistered key succeeded")
	}
	if strings.Contains(stderr, ErrCommandTooLong.Error()) {
		t.Fatalf("command at the limit was refused as too long: %q", stderr)
	}

	// LoggingMiddleware logs the parsed command at debug level, so the log
	// shows whether it saw a command.
	if !strings.Contains(logs.String(), strings.Repeat("a", 64)) {
		t.Fatalf("debug log does not show the command at the limit, so it cannot show parsing: %q", logs.String())
	}

	// Suggestions stay on for short typos of a command name.
	stderr, _ = run("rpo")
	if !strings.Contains(stderr, "Did you mean this?") || !strings.Contains(stderr, "repo") {
		t.Fatalf("short typo: stderr %q does not suggest repo", stderr)
	}

	overlong := strings.Repeat("b", MaxCommandLength+1)
	stderr, err = run(overlong)
	var exitErr *gossh.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 1 {
		t.Fatalf("overlong command: got %v, want exit status 1 (stderr %q)", err, stderr)
	}
	if strings.TrimSpace(stderr) != ErrCommandTooLong.Error() {
		t.Fatalf("overlong command: stderr %q, want only %q", stderr, ErrCommandTooLong)
	}
	if strings.Contains(logs.String(), strings.Repeat("b", 64)) {
		t.Fatal("overlong command reached LoggingMiddleware")
	}
}
