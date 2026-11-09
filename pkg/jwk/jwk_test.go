package jwk

import (
	"crypto"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
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

func TestNewPairHostKeyTypes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		alg  string
		opts []keygen.Option
	}{
		{"ed25519", "EdDSA", []keygen.Option{keygen.WithKeyType(keygen.Ed25519)}},
		{"rsa", "RS256", []keygen.Option{keygen.WithKeyType(keygen.RSA), keygen.WithBitSize(2048)}},
		{"ecdsa-p256", "ES256", []keygen.Option{keygen.WithKeyType(keygen.ECDSA), keygen.WithEllipticCurve(elliptic.P256())}},
		{"ecdsa-p384", "ES384", []keygen.Option{keygen.WithKeyType(keygen.ECDSA), keygen.WithEllipticCurve(elliptic.P384())}},
		{"ecdsa-p521", "ES512", []keygen.Option{keygen.WithKeyType(keygen.ECDSA), keygen.WithEllipticCurve(elliptic.P521())}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.DefaultConfig()
			cfg.SSH.KeyPath = filepath.Join(t.TempDir(), "host_key")
			if _, err := keygen.New(cfg.SSH.KeyPath, append(tc.opts, keygen.WithWrite())...); err != nil {
				t.Fatalf("keygen: %v", err)
			}

			kp, err := NewPair(cfg)
			if err != nil {
				t.Fatalf("NewPair: %v", err)
			}
			if got := kp.SigningMethod().Alg(); got != tc.alg {
				t.Errorf("alg = %q, want %q", got, tc.alg)
			}
			if got := kp.JWK().Algorithm; got != tc.alg {
				t.Errorf("JWK alg = %q, want %q", got, tc.alg)
			}

			signed, err := jwt.NewWithClaims(kp.SigningMethod(), jwt.RegisteredClaims{Subject: "s"}).SignedString(kp.PrivateKey())
			if err != nil {
				t.Fatalf("SignedString: %v", err)
			}
			if _, err := jwt.Parse(signed, func(*jwt.Token) (interface{}, error) {
				return kp.JWK().Key, nil
			}, jwt.WithValidMethods([]string{tc.alg})); err != nil {
				t.Errorf("Parse: %v", err)
			}
		})
	}
}

func TestSigningMethodForUnsupportedKey(t *testing.T) {
	t.Parallel()
	if _, err := SigningMethodFor("not a key"); err == nil {
		t.Error("SigningMethodFor(string) => nil error, want error")
	}
}
