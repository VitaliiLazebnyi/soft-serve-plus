package ssh

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
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

// fixtureAdmin returns the admin account the first migration creates, the
// owner of fixture repositories: the backend refuses a create by a caller
// who may not create, as an anonymous caller may not by default.
func fixtureAdmin(t testing.TB, be *backend.Backend) proto.User {
	t.Helper()
	u, err := be.User(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return u
}
