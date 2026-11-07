package jwk

import (
	"crypto"
	"encoding/base64"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/go-jose/go-jose/v4"
)

func TestBadNewPair(t *testing.T) {
	_, err := NewPair(nil)
	if !errors.Is(err, config.ErrNilConfig) {
		t.Errorf("NewPair(nil) => %v, want %v", err, config.ErrNilConfig)
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.KeyPath = filepath.Join(cfg.DataPath, "host_key")
	if _, err := keygen.New(cfg.SSH.KeyPath, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite()); err != nil {
		t.Fatalf("write host key: %v", err)
	}
	return cfg
}

func TestGoodNewPair(t *testing.T) {
	if _, err := NewPair(testConfig(t)); err != nil {
		t.Errorf("NewPair(cfg) => _, %v, want nil error", err)
	}
}

// The host key is written by the SSH server; without it there is nothing to
// sign or verify tokens with.
func TestNewPairMissingHostKey(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.SSH.KeyPath = filepath.Join(t.TempDir(), "missing")
	if _, err := NewPair(cfg); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("NewPair(cfg) => _, %v, want an error wrapping %v", err, fs.ErrNotExist)
	}
}

// The key ID is the RFC 7638 thumbprint of the public key: the same for every
// pair made from one host key, and derived from public material only.
func TestNewPairKeyIDIsThumbprint(t *testing.T) {
	cfg := testConfig(t)

	a, err := NewPair(cfg)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	b, err := NewPair(cfg)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	if a.JWK().KeyID != b.JWK().KeyID {
		t.Errorf("kid differs between pairs of one key: %q, %q", a.JWK().KeyID, b.JWK().KeyID)
	}

	kp, err := config.KeyPair(cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	pub := jose.JSONWebKey{Key: kp.CryptoPublicKey()}
	thumb, err := pub.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatalf("Thumbprint: %v", err)
	}
	if want := base64.RawURLEncoding.EncodeToString(thumb); a.JWK().KeyID != want {
		t.Errorf("kid = %q, want RFC 7638 thumbprint %q", a.JWK().KeyID, want)
	}
}
