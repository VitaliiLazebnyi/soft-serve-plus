package ssh

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	gossh "golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"
)

// TestAuthenticationMiddlewareUserLookup checks which user a public-key
// session runs as: the key's owner, anonymous for an unregistered key, and
// no session at all when the lookup fails for another reason, so a database
// error cannot downgrade a registered user to anonymous.
func TestAuthenticationMiddlewareUserLookup(t *testing.T) {
	t.Parallel()

	dp := t.TempDir()
	ctx := context.Background()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	ctx = config.WithContext(ctx, cfg)

	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)

	newKey := func(name string) gossh.Signer {
		t.Helper()
		kp, err := keygen.New(dp+"/"+name, keygen.WithKeyType(keygen.Ed25519))
		if err != nil {
			t.Fatal(err)
		}
		signer, err := gossh.NewSignerFromKey(kp.PrivateKey())
		if err != nil {
			t.Fatal(err)
		}
		return signer
	}
	alice := newKey("alice")
	stranger := newKey("stranger")
	if _, err := be.CreateUser(ctx, "alice", proto.UserOptions{
		PublicKeys: []gossh.PublicKey{alice.PublicKey()},
	}); err != nil {
		t.Fatal(err)
	}

	// The handler prints the session's user, or "anon" without one.
	srv := &ssh.Server{
		PublicKeyHandler: (&SSHServer{}).PublicKeyHandler,
		Handler: ContextMiddleware(cfg, dbx, dbstore, be, log.New(&bytes.Buffer{}))(AuthenticationMiddleware(func(s ssh.Session) {
			name := "anon"
			if u := proto.UserFromContext(s.Context()); u != nil {
				name = u.Username()
			}
			_, _ = s.Write([]byte(name))
			_ = s.Exit(0)
		})),
	}
	addr := testsession.Listen(t, srv)

	run := func(signer gossh.Signer) (string, string, error) {
		t.Helper()
		sess, err := testsession.NewClientSession(t, addr, &gossh.ClientConfig{
			User: "test",
			Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)},
		})
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		sess.Stdout = &stdout
		sess.Stderr = &stderr
		err = sess.Run("whoami")
		return stdout.String(), stderr.String(), err
	}

	if out, _, err := run(alice); err != nil || out != "alice" {
		t.Fatalf("registered key: got %q, %v; want alice", out, err)
	}
	if out, _, err := run(stranger); err != nil || out != "anon" {
		t.Fatalf("unregistered key: got %q, %v; want anon", out, err)
	}

	// A failing database makes the lookup fail with something other than
	// proto.ErrUserNotFound.
	if err := dbx.Close(); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := run(alice)
	var exitErr *gossh.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() == 0 {
		t.Fatalf("lookup failure: got exit %v, want a non-zero exit status", err)
	}
	if out != "" {
		t.Fatalf("lookup failure: the session ran as %q", out)
	}
	if !strings.Contains(errOut, ErrUserLookupFailed.Error()) {
		t.Fatalf("lookup failure: stderr %q does not contain %q", errOut, ErrUserLookupFailed)
	}
}
