package sshlfs

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

type lfsAuthUser struct{ proto.User }

func (lfsAuthUser) ID() int64        { return 7 }
func (lfsAuthUser) Username() string { return "alice" }

type lfsAuthRepo struct{ proto.Repository }

func (lfsAuthRepo) Name() string { return "repo" }

// The token Authenticate signs is a bearer credential: it goes to the
// client and never into the logs, even at debug level.
func TestLFSAuthenticateDoesNotLogToken(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.KeyPath = filepath.Join(cfg.DataPath, "host_key")
	tokens := testSigner(t, cfg)

	var out bytes.Buffer
	if err := Authenticate(&out, cfg, tokens, logger, lfsAuthRepo{}, lfsAuthUser{}, lfs.OperationDownload); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	var res lfs.AuthenticateResponse
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	token, ok := strings.CutPrefix(res.Header["Authorization"], "Bearer ")
	if !ok || token == "" {
		t.Fatalf("no bearer token in response: %q", out.String())
	}

	got := logs.String()
	if !strings.Contains(got, "generated token") {
		t.Errorf("expected the debug line to be logged:\n%s", got)
	}
	if strings.Contains(got, token) {
		t.Errorf("log contains the signed token:\n%s", got)
	}
}

// The git-lfs spec gives expires_in in seconds, and git-lfs reads it into an
// int, which is 32 bits wide on 32-bit platforms. A response in nanoseconds
// does not fit there and reads as thousands of years on 64-bit clients.
func TestLFSAuthenticateExpiresInSeconds(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.KeyPath = filepath.Join(cfg.DataPath, "host_key")
	tokens := testSigner(t, cfg)

	var out bytes.Buffer
	if err := Authenticate(&out, cfg, tokens, log.New(io.Discard), lfsAuthRepo{}, lfsAuthUser{}, lfs.OperationDownload); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// Decode as a 32-bit git-lfs client would.
	var res struct {
		ExpiresIn int32 `json:"expires_in"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.ExpiresIn != 300 {
		t.Errorf("expires_in = %d, want 300 seconds", res.ExpiresIn)
	}
}

// testSigner writes the server's SSH host key at cfg.SSH.KeyPath, which
// the server otherwise creates on first start, and returns the token signer
// the server builds from it.
func testSigner(t *testing.T, cfg *config.Config) *jwk.Signer {
	t.Helper()
	if _, err := keygen.New(cfg.SSH.KeyPath, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite()); err != nil {
		t.Fatalf("write host key: %v", err)
	}
	kp, err := config.KeyPair(cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	tokens, err := jwk.NewSigner(kp, cfg.HTTP.PublicURL)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return tokens
}
