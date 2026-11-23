package ssh

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/store"
	gossh "golang.org/x/crypto/ssh"
)

// The server presents the host key it is given, and does not read or
// create the file at ssh.key_path for it (tracker 343(a)).
func TestServerPresentsGivenHostKey(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := serverContext(t, cfg, log.Default())
	d := depsOf(ctx)
	d.hostKey = testHostKey(t, filepath.Join(t.TempDir(), "given"))
	if err := os.Remove(cfg.SSH.KeyPath); err != nil {
		t.Fatal(err)
	}
	want, err := gossh.NewPublicKey(d.hostKey.CryptoPublicKey())
	if err != nil {
		t.Fatal(err)
	}

	s, err := newTestSSHServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, s)

	var got gossh.PublicKey
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User: "test",
		Auth: []gossh.AuthMethod{gossh.KeyboardInteractive(noAnswers)},
		HostKeyCallback: func(_ string, _ net.Addr, key gossh.PublicKey) error {
			got = key
			return nil
		},
		Timeout: sessionLoopsWait,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if got == nil || !bytes.Equal(got.Marshal(), want.Marshal()) {
		t.Errorf("server presented host key %v, want the given one", got)
	}
	if _, err := os.Stat(cfg.SSH.KeyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s = %v, want no key file created", cfg.SSH.KeyPath, err)
	}
}

// NewServer refuses to build a server without a config or a host key.
func TestNewServerNeedsConfigAndHostKey(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	d := depsOf(serverContext(t, cfg, log.Default()))
	if _, err := NewServer(nil, d.dbx, d.st, d.be, d.hostKey, d.tokens, d.logger); !errors.Is(err, config.ErrNilConfig) {
		t.Errorf("NewServer without a config: %v, want %v", err, config.ErrNilConfig)
	}
	if _, err := NewServer(d.cfg, d.dbx, d.st, d.be, nil, d.tokens, d.logger); !errors.Is(err, errNilHostKey) {
		t.Errorf("NewServer without a host key: %v, want %v", err, errNilHostKey)
	}
}

// The session context carries the session's user, but none of the server's
// dependencies: the middleware and commands take those from the server
// (R63, SSH-17).
func TestSessionContextHasNoServerDependencies(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	d := depsOf(serverContext(t, cfg, log.Default()))
	hs := handlerServer(t, d.cfg, d.dbx, d.st, d.be, d.logger)
	sess := testsession.New(t, &ssh.Server{
		Handler: hs.loggerMiddleware(hs.authenticationMiddleware(func(s ssh.Session) {
			ctx := sessionContext(s)
			_, _ = fmt.Fprintf(s, "config=%t db=%t store=%t backend=%t",
				config.FromContext(ctx) != nil, db.FromContext(ctx) != nil,
				store.FromContext(ctx) != nil, backend.FromContext(ctx) != nil)
			_ = s.Exit(0)
		})),
	}, &gossh.ClientConfig{
		User:            "test",
		Auth:            []gossh.AuthMethod{gossh.KeyboardInteractive(noAnswers)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec // test server
	})
	out, err := sess.Output("")
	if err != nil {
		t.Fatal(err)
	}
	if want := "config=false db=false store=false backend=false"; string(out) != want {
		t.Errorf("session context: %s, want %s", out, want)
	}
}

// serve serves s on a local port until the test ends and returns its
// address.
func serve(t *testing.T, s *Server) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = s.Close() })
	return l.Addr().String()
}
