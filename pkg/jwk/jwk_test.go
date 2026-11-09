package jwk

import (
	"crypto/elliptic"
	"errors"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/golang-jwt/jwt/v5"
)

func TestBadNewPair(t *testing.T) {
	_, err := NewPair(nil)
	if !errors.Is(err, config.ErrNilConfig) {
		t.Errorf("NewPair(nil) => %v, want %v", err, config.ErrNilConfig)
	}
}

func TestGoodNewPair(t *testing.T) {
	cfg := config.DefaultConfig()
	if _, err := NewPair(cfg); err != nil {
		t.Errorf("NewPair(cfg) => _, %v, want nil error", err)
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
