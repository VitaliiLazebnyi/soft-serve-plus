package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

func runJWT(ctx context.Context, args ...string) (string, error) {
	c := JWTCommand()
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
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ctx = withUser(t, ctx, be, "alice", false)

	out, err := runJWT(ctx)
	is.True(err != nil)
	is.Equal(out, "")
}

// A session whose key belongs to no user is told so before the server
// loads its signing key, so a key problem does not hide that answer.
func TestJWTChecksUserBeforeKey(t *testing.T) {
	is := is.New(t)
	ctx, _ := newAuthTestContextWith(t, func(c *config.Config) {
		c.SSH.KeyPath = ""
	})
	kp, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	is.NoErr(err)
	ctx = context.WithValue(ctx, ssh.ContextKeyPublicKey, kp.PublicKey())

	_, err = runJWT(ctx, "repo")
	is.True(errors.Is(err, proto.ErrUserNotFound))
}

// A host key the server cannot read fails jwt on the server: the client
// gets the generic error, not the key's path, which is logged.
func TestJWTHostKeyFailureIsLogged(t *testing.T) {
	is := is.New(t)
	keyPath := filepath.Join(t.TempDir(), "missing_host_key")
	ctx, be := newAuthTestContextWith(t, func(c *config.Config) {
		c.SSH.KeyPath = keyPath
	})
	var logs bytes.Buffer
	ctx = log.WithContext(ctx, log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel}))
	ctx = withUser(t, ctx, be, "alice", false)

	_, err := runJWT(ctx, "repo")
	is.True(errors.Is(err, errLookupFailed))
	is.True(!strings.Contains(err.Error(), keyPath))
	is.Equal(strings.Count(logs.String(), "ERROR"), 1)
	is.True(strings.Contains(logs.String(), "missing_host_key"))
}
