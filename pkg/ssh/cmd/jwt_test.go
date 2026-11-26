package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/golang-jwt/jwt/v5"
)

func runJWT(ctx context.Context, args ...string) (string, error) {
	c := cmdsOf(ctx).JWTCommand()
	// The SSH root command silences usage, so errors are the only output.
	c.SilenceUsage = true
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&bytes.Buffer{})
	c.SetArgs(args)
	err := c.ExecuteContext(ctx)
	return out.String(), err
}

// The HTTP server only accepts a token whose audience names the requested
// repository, so jwt refuses to mint one that names none.
func TestJWTRequiresRepository(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ctx = withUser(t, ctx, be, "alice", false)

	out, err := runJWT(ctx)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if got := out; got != "" {
		t.Fatalf("out = %v, want %v", got, "")
	}
}

// A session whose key belongs to no user is told so before the server
// signs with its host key, so a missing key does not hide that answer.
func TestJWTChecksUserBeforeKey(t *testing.T) {
	t.Parallel()
	ctx, _ := newAuthTestContext(t)
	kp, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, ssh.ContextKeyPublicKey, kp.PublicKey())

	_, err = runJWT(ctx, "repo")
	if !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
	}
}

// Without a host key, jwt fails on the server: the client gets the generic
// error, and the cause is logged once.
func TestJWTHostKeyFailureIsLogged(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	var logs bytes.Buffer
	ctx = withTestLogger(ctx, log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel}))
	ctx = withUser(t, ctx, be, "alice", false)

	_, err := runJWT(ctx, "repo")
	if !errors.Is(err, errLookupFailed) {
		t.Fatal("not true: errors.Is(err, errLookupFailed)")
	}
	if got := strings.Count(logs.String(), "ERROR"); got != 1 {
		t.Fatalf("strings.Count(logs.String(), \"ERROR\") = %v, want %v", got, 1)
	}
	if !strings.Contains(logs.String(), jwk.ErrHostKey.Error()) {
		t.Fatal("not true: strings.Contains(logs.String(), jwk.ErrHostKey.Error())")
	}
}

// jwt signs with the host key the server was given, not with whatever is
// at ssh.key_path now.
func TestJWTSignsWithGivenHostKey(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	cmds := cmdsOf(ctx)
	if err := os.MkdirAll(filepath.Dir(cmds.cfg.SSH.KeyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := keygen.New(cmds.cfg.SSH.KeyPath, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite())
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := config.KeyPair(cmds.cfg)
	if err != nil {
		t.Fatal(err)
	}
	given := hostKeyAt(t, filepath.Join(t.TempDir(), "given"))
	cmds.tokens, err = jwk.NewSigner(given, cmds.cfg.HTTP.PublicURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx = withUser(t, ctx, be, "alice", false)

	out, err := runJWT(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(out)
	verify := func(key *config.HostKey) error {
		_, err := jwt.Parse(token, func(*jwt.Token) (any, error) { return key.CryptoPublicKey(), nil })
		return err
	}
	if err := verify(given); err != nil {
		t.Fatal(err)
	}
	if verify(onDisk) == nil {
		t.Fatal("not true: verify(onDisk) != nil")
	}
}

// hostKeyAt writes a new Ed25519 key at path and returns it as a host key.
func hostKeyAt(t *testing.T, path string) *config.HostKey {
	t.Helper()
	if _, err := keygen.New(path, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite()); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.SSH.KeyPath = path
	key, err := config.KeyPair(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
