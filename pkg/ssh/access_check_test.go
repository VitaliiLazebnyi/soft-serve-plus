package ssh

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	gossh "golang.org/x/crypto/ssh"
)

// While the settings cannot be read, a session whose access depends on them
// is told its access could not be checked, not that it is refused: a keyless
// session (the keyless setting) and a TUI session with an unregistered key
// (the anonymous level). The cause stays in the server log.
func TestSessionAccessCheckFailure(t *testing.T) {
	ctx := context.Background()
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = dbx.DriverName()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx = config.WithContext(ctx, cfg)
	if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	st := database.New()
	ctx = store.WithContext(ctx, st)
	be := backend.New(ctx, cfg, dbx, st)
	logs := &syncBuffer{}

	// Authentication lets every client in, so the session handlers decide.
	srv := &ssh.Server{
		PublicKeyHandler: func(ssh.Context, ssh.PublicKey) bool { return true },
		KeyboardInteractiveHandler: func(ssh.Context, gossh.KeyboardInteractiveChallenge) bool {
			return true
		},
		Handler: ContextMiddleware(cfg, dbx, st, be, log.New(logs))(AuthenticationMiddleware(BubbleTeaMiddleware(func(s ssh.Session) {
			_ = s.Exit(0)
		}))),
	}
	addr := testsession.Listen(t, srv)

	kp, err := keygen.New(cfg.DataPath+"/stranger", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(kp.PrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	keyless := gossh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) { return nil, nil })

	if _, err := dbx.ExecContext(ctx, "ALTER TABLE settings RENAME TO settings_gone"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		auth gossh.AuthMethod
		pty  bool
	}{
		{"keyless", keyless, false},
		{"unregistered key, TUI", gossh.PublicKeys(signer), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := testsession.NewClientSession(t, addr, &gossh.ClientConfig{
				User: "test",
				Auth: []gossh.AuthMethod{tc.auth},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.pty {
				if err := sess.RequestPty("xterm", 40, 80, nil); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			sess.Stderr = &stderr
			sess.Stdout = &bytes.Buffer{}
			if err := sess.Run(""); err == nil {
				t.Fatal("session ran; want it ended")
			}
			if !strings.Contains(stderr.String(), ErrAccessCheckFailed.Error()) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), ErrAccessCheckFailed)
			}
			if strings.Contains(stderr.String(), "settings") {
				t.Fatalf("stderr = %q names the cause", stderr.String())
			}
		})
	}
	if !strings.Contains(logs.String(), "failed to check the session's access") {
		t.Errorf("logs = %q, want the failed check", logs.String())
	}
}

// An access check canceled because the client went away is logged at Debug.
func TestAccessCheckFailureCanceledLoggedAtDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logAccessCheckFailure(ctx, logger, context.Canceled)
	if out := buf.String(); !strings.Contains(out, "access check canceled") || strings.Contains(out, "ERRO") {
		t.Errorf("logs = %q, want the canceled check at Debug only", out)
	}
}
