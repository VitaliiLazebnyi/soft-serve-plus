package ssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	gossh "golang.org/x/crypto/ssh"
)

// The tests below run AuthenticationMiddleware behind a real SSH handshake
// on a server built by NewSSHServer. They read the user a session runs as
// from the access tokens `token create` makes, as that command acts as the
// user AuthenticationMiddleware puts in the session context.

func newSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// offeredKey offers pub, a key the client does not hold: the server
// approves it when asked about it, but the client cannot sign with it. The
// signature it sends instead names an algorithm the server does not accept,
// so the server refuses the key and lets the client try another way. (A
// signature that fails to verify would end the connection.)
type offeredKey struct {
	pub gossh.PublicKey
}

func (k offeredKey) PublicKey() gossh.PublicKey { return k.pub }

func (k offeredKey) Sign(io.Reader, []byte) (*gossh.Signature, error) {
	return &gossh.Signature{Format: "unsigned", Blob: []byte{0}}, nil
}

// noAnswers answers keyboard-interactive authentication, which asks no
// questions here.
func noAnswers(string, string, []string, []bool) ([]string, error) { return nil, nil }

// runCommand connects to addr with the given auth methods and runs command.
// It returns the handshake error, or the command's stderr and exit error.
func runCommand(t *testing.T, addr string, command string, auth ...gossh.AuthMethod) (stderr string, runErr, dialErr error) {
	t.Helper()
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		Auth:            auth,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
		Timeout:         10 * time.Second,
	})
	if err != nil {
		return "", nil, err
	}
	defer c.Close() //nolint: errcheck
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint: errcheck
	var errBuf bytes.Buffer
	s.Stderr = &errBuf
	runErr = s.Run(command)
	return errBuf.String(), runErr, nil
}

// tokenCount returns how many access tokens user has.
func tokenCount(ctx context.Context, t *testing.T, user proto.User) int {
	t.Helper()
	tokens, err := backend.FromContext(ctx).ListAccessTokens(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	return len(tokens)
}

func newAuthServer(t *testing.T) (string, context.Context, *config.Config) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, ctx := startTestServer(t, cfg)
	return addr, ctx, cfg
}

// A client that offers the admin's public key without holding its private
// key, then signs with its own key, runs as its own user. The server
// approves the offered key before the signature check fails, so the
// session must take its user from the key that was signed with.
func TestAuthenticationUsesSignedKey(t *testing.T) {
	addr, ctx, _ := newAuthServer(t)
	be := backend.FromContext(ctx)

	adminKey := newSigner(t)
	attackerKey := newSigner(t)
	admin, err := be.CreateUser(ctx, "testadmin", proto.UserOptions{
		Admin:      true,
		PublicKeys: []gossh.PublicKey{adminKey.PublicKey()},
	})
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := be.CreateUser(ctx, "testattacker", proto.UserOptions{
		PublicKeys: []gossh.PublicKey{attackerKey.PublicKey()},
	})
	if err != nil {
		t.Fatal(err)
	}

	stderr, runErr, err := runCommand(t, addr, "token create probe",
		gossh.PublicKeys(offeredKey{pub: adminKey.PublicKey()}, attackerKey))
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if runErr != nil {
		t.Fatalf("token create: %v\n%s", runErr, stderr)
	}
	if n := tokenCount(ctx, t, attacker); n != 1 {
		t.Errorf("testattacker has %d tokens, want 1", n)
	}
	if n := tokenCount(ctx, t, admin); n != 0 {
		t.Errorf("testadmin has %d tokens, want 0: the session ran as the offered key's user", n)
	}
}

// Keyless access is refused when it is turned off, and when it is on, a
// keyless session runs as nobody, even after the client offered a user's
// key it could not sign with.
func TestAuthenticationKeyless(t *testing.T) {
	addr, ctx, _ := newAuthServer(t)
	be := backend.FromContext(ctx)

	adminKey := newSigner(t)
	admin, err := be.CreateUser(ctx, "testadmin", proto.UserOptions{
		Admin:      true,
		PublicKeys: []gossh.PublicKey{adminKey.PublicKey()},
	})
	if err != nil {
		t.Fatal(err)
	}
	offered := gossh.PublicKeys(offeredKey{pub: adminKey.PublicKey()})
	keyless := gossh.KeyboardInteractive(noAnswers)

	t.Run("allowed", func(t *testing.T) {
		if !be.AllowKeyless(ctx) {
			t.Fatal("keyless access is off by default")
		}
		stderr, runErr, err := runCommand(t, addr, "token create probe", offered, keyless)
		if err != nil {
			t.Fatalf("keyless handshake: %v", err)
		}
		if runErr == nil {
			t.Fatal("token create succeeded in a keyless session")
		}
		t.Logf("keyless token create: %v: %s", runErr, stderr)
		if n := tokenCount(ctx, t, admin); n != 0 {
			t.Errorf("testadmin has %d tokens, want 0: the keyless session ran as the offered key's user", n)
		}
	})

	t.Run("refused", func(t *testing.T) {
		if err := be.SetAllowKeyless(ctx, false); err != nil {
			t.Fatal(err)
		}
		for name, auth := range map[string][]gossh.AuthMethod{
			"keyboard-interactive":              {keyless},
			"offered key, keyboard-interactive": {offered, keyless},
		} {
			if _, _, err := runCommand(t, addr, "info", auth...); err == nil {
				t.Errorf("%s: client connected with keyless access off", name)
			}
		}
		// A key that authenticates still gets in.
		if _, runErr, err := runCommand(t, addr, "info", gossh.PublicKeys(adminKey)); err != nil || runErr != nil {
			t.Errorf("admin key with keyless access off: handshake %v, run %v", err, runErr)
		}
	})
}

// A key listed in initial_admin_keys that a non-admin account holds is
// refused a session, with an error that says how to fix it.
func TestAuthenticationRefusesClaimedAdminKey(t *testing.T) {
	adminKey := newSigner(t)
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.InitialAdminKeys = []string{sshutils.MarshalAuthorizedKey(adminKey.PublicKey())}
	addr, ctx := startTestServer(t, cfg)
	be := backend.FromContext(ctx)

	// The listed key went to the admin account; hand it to a non-admin
	// one, as `pubkey add` before the key was listed would have.
	owner, err := be.UserByPublicKey(ctx, adminKey.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := be.RemovePublicKey(ctx, owner.Username(), adminKey.PublicKey()); err != nil {
		t.Fatal(err)
	}
	mallory, err := be.CreateUser(ctx, "mallory", proto.UserOptions{
		PublicKeys: []gossh.PublicKey{adminKey.PublicKey()},
	})
	if err != nil {
		t.Fatal(err)
	}

	stderr, runErr, err := runCommand(t, addr, "token create probe", gossh.PublicKeys(adminKey))
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if runErr == nil {
		t.Fatal("session with a claimed admin key succeeded")
	}
	if !strings.Contains(stderr, "initial_admin_keys") || !strings.Contains(stderr, "user set-admin mallory true") {
		t.Errorf("stderr = %q, want the claimed admin key error", stderr)
	}
	if n := tokenCount(ctx, t, mallory); n != 0 {
		t.Errorf("mallory has %d tokens, want 0", n)
	}
}
