package jwk

import (
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/golang-jwt/jwt/v5"
)

// testUser is a proto.User with a name and an ID.
type testUser struct {
	proto.User
	name string
	id   int64
}

func (u testUser) Username() string { return u.name }
func (u testUser) ID() int64        { return u.id }

func testConfig(t *testing.T, opts ...keygen.Option) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.KeyPath = filepath.Join(cfg.DataPath, "host_key")
	if len(opts) == 0 {
		opts = []keygen.Option{keygen.WithKeyType(keygen.Ed25519)}
	}
	if _, err := keygen.New(cfg.SSH.KeyPath, append(opts, keygen.WithWrite())...); err != nil {
		t.Fatalf("write host key: %v", err)
	}
	return cfg
}

func TestSignNilConfig(t *testing.T) {
	_, err := Sign(nil, testUser{name: "u", id: 1}, []string{"repo"}, time.Now().Add(time.Hour))
	if !errors.Is(err, config.ErrNilConfig) || !errors.Is(err, ErrHostKey) {
		t.Errorf("Sign(nil) => %v, want an error wrapping %v and %v", err, ErrHostKey, config.ErrNilConfig)
	}
}

// The host key is written by the SSH server; without it there is nothing to
// sign or verify tokens with.
func TestMissingHostKey(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.SSH.KeyPath = filepath.Join(t.TempDir(), "missing")
	_, err := Sign(cfg, testUser{name: "u", id: 1}, []string{"repo"}, time.Now().Add(time.Hour))
	if !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, ErrHostKey) {
		t.Errorf("Sign => %v, want an error wrapping %v and %v", err, ErrHostKey, fs.ErrNotExist)
	}
	_, err = Verify(cfg, "a.b.c", []string{"repo"})
	if !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, ErrHostKey) {
		t.Errorf("Verify => %v, want an error wrapping %v and %v", err, ErrHostKey, fs.ErrNotExist)
	}
}

// A signed token verifies for its audience and carries the claims a token
// for the user needs, with no "kid" header.
func TestSignVerify(t *testing.T) {
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
			cfg := testConfig(t, tc.opts...)
			exp := time.Now().Add(time.Hour)
			token, err := Sign(cfg, testUser{name: "alice", id: 7}, []string{"a", "b"}, exp)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}

			parsed, _, err := jwt.NewParser().ParseUnverified(token, &jwt.RegisteredClaims{})
			if err != nil {
				t.Fatalf("ParseUnverified: %v", err)
			}
			if got := parsed.Method.Alg(); got != tc.alg {
				t.Errorf("alg = %q, want %q", got, tc.alg)
			}
			if kid, ok := parsed.Header["kid"]; ok {
				t.Errorf("token has a kid header %v", kid)
			}

			claims, err := Verify(cfg, token, []string{"b"})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if claims.Subject != "alice#7" {
				t.Errorf("subject = %q, want %q", claims.Subject, "alice#7")
			}
			if claims.Issuer != cfg.HTTP.PublicURL {
				t.Errorf("issuer = %q, want %q", claims.Issuer, cfg.HTTP.PublicURL)
			}
			if !claims.ExpiresAt.Equal(exp.Truncate(time.Second)) {
				t.Errorf("expires = %v, want %v", claims.ExpiresAt, exp)
			}
			if claims.IssuedAt == nil || claims.NotBefore == nil {
				t.Errorf("iat = %v, nbf = %v, want both set", claims.IssuedAt, claims.NotBefore)
			}
		})
	}
}

// Tokens issued before the "kid" header was dropped carry the RFC 7638
// thumbprint of the host key there. They must still verify until they
// expire, and so must a token with any other kid: there is one key.
func TestVerifyIgnoresKeyID(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	kp, err := config.KeyPair(cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   "alice#7",
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		NotBefore: jwt.NewNumericDate(now),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    cfg.HTTP.PublicURL,
		Audience:  []string{"repo"},
	}
	// The RFC 7638 thumbprint of the Ed25519 key, as the old code minted it,
	// and some other value.
	pub := kp.CryptoPublicKey().(ed25519.PublicKey)
	thumb := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}`))
	for _, kid := range []string{base64.RawURLEncoding.EncodeToString(thumb[:]), "other"} {
		token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
		token.Header["kid"] = kid
		signed, err := token.SignedString(kp.PrivateKey())
		if err != nil {
			t.Fatalf("SignedString: %v", err)
		}
		got, err := Verify(cfg, signed, []string{"repo"})
		if err != nil {
			t.Fatalf("Verify(kid=%q): %v", kid, err)
		}
		if got.Subject != "alice#7" {
			t.Errorf("subject = %q, want %q", got.Subject, "alice#7")
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	other := testConfig(t)
	user := testUser{name: "alice", id: 7}
	now := time.Now()
	valid := jwt.RegisteredClaims{
		Subject:   Subject(user),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    cfg.HTTP.PublicURL,
		Audience:  []string{"repo"},
	}
	sign := func(c *config.Config, mutate func(*jwt.RegisteredClaims)) string {
		claims := valid
		if mutate != nil {
			mutate(&claims)
		}
		s, err := SignClaims(c, claims)
		if err != nil {
			t.Fatalf("SignClaims: %v", err)
		}
		return s
	}

	if _, err := Verify(cfg, sign(cfg, nil), []string{"repo"}); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	for _, tc := range []struct {
		name     string
		token    string
		audience []string
	}{
		{"no audience asked", sign(cfg, nil), nil},
		{"other audience", sign(cfg, nil), []string{"other"}},
		{"other key", sign(other, nil), []string{"repo"}},
		{"expired", sign(cfg, func(c *jwt.RegisteredClaims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Minute)) }), []string{"repo"}},
		{"no expiry", sign(cfg, func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil }), []string{"repo"}},
		{"other issuer", sign(cfg, func(c *jwt.RegisteredClaims) { c.Issuer = "https://elsewhere" }), []string{"repo"}},
		{"issued in the future", sign(cfg, func(c *jwt.RegisteredClaims) { c.IssuedAt = jwt.NewNumericDate(now.Add(time.Hour)) }), []string{"repo"}},
		{"garbage", "a.b.c", []string{"repo"}},
	} {
		if _, err := Verify(cfg, tc.token, tc.audience); !errors.Is(err, errInvalid) || errors.Is(err, ErrHostKey) {
			t.Errorf("%s: Verify => %v, want errInvalid", tc.name, err)
		}
	}
}

func TestSigningMethodForUnsupportedKey(t *testing.T) {
	t.Parallel()
	if _, err := signingMethodFor("not a key"); err == nil {
		t.Error("signingMethodFor(string) => nil error, want error")
	}
}
