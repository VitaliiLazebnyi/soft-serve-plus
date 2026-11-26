package jwk

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
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

// testIssuer is the issuer of the tokens the tests sign.
const testIssuer = "http://localhost:23232"

// testKey writes a host key made with opts (Ed25519 by default) and returns
// it as the server loads it.
func testKey(t *testing.T, opts ...keygen.Option) *config.HostKey {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.SSH.KeyPath = filepath.Join(t.TempDir(), "host_key")
	if len(opts) == 0 {
		opts = []keygen.Option{keygen.WithKeyType(keygen.Ed25519)}
	}
	if _, err := keygen.New(cfg.SSH.KeyPath, append(opts, keygen.WithWrite())...); err != nil {
		t.Fatalf("write host key: %v", err)
	}
	kp, err := config.KeyPair(cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	return kp
}

// testSigner returns a Signer for a new host key made with opts.
func testSigner(t *testing.T, opts ...keygen.Option) *Signer {
	t.Helper()
	s, err := NewSigner(testKey(t, opts...), testIssuer)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

// Without a host key there is nothing to sign or verify tokens with: a nil
// *Signer, which is what a server whose key could not be loaded holds,
// refuses both as a host key fault.
func TestNoHostKey(t *testing.T) {
	t.Parallel()
	if _, err := NewSigner(nil, testIssuer); !errors.Is(err, ErrHostKey) {
		t.Errorf("NewSigner(nil) => %v, want an error wrapping %v", err, ErrHostKey)
	}
	var s *Signer
	if _, err := s.Sign(testUser{name: "u", id: 1}, []string{"repo"}, time.Now().Add(time.Hour)); !errors.Is(err, ErrHostKey) {
		t.Errorf("Sign => %v, want an error wrapping %v", err, ErrHostKey)
	}
	if _, err := s.SignClaims(jwt.RegisteredClaims{}); !errors.Is(err, ErrHostKey) {
		t.Errorf("SignClaims => %v, want an error wrapping %v", err, ErrHostKey)
	}
	if _, err := s.Verify("a.b.c", []string{"repo"}); !errors.Is(err, ErrHostKey) {
		t.Errorf("Verify => %v, want an error wrapping %v", err, ErrHostKey)
	}
}

// A host key the server loads but tokens cannot be signed with, an ECDSA
// key on P-224, is refused.
func TestNewSignerUnsupportedKey(t *testing.T) {
	t.Parallel()
	priv, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.SSH.KeyPath = filepath.Join(t.TempDir(), "host_key")
	if err := os.WriteFile(cfg.SSH.KeyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	kp, err := config.KeyPair(cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	if _, err := NewSigner(kp, testIssuer); !errors.Is(err, ErrHostKey) {
		t.Errorf("NewSigner(P-224) => %v, want an error wrapping %v", err, ErrHostKey)
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
			s := testSigner(t, tc.opts...)
			exp := time.Now().Add(time.Hour)
			token, err := s.Sign(testUser{name: "alice", id: 7}, []string{"a", "b"}, exp)
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

			claims, err := s.Verify(token, []string{"b"})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if claims.Subject != "alice#7" {
				t.Errorf("subject = %q, want %q", claims.Subject, "alice#7")
			}
			if claims.Issuer != testIssuer {
				t.Errorf("issuer = %q, want %q", claims.Issuer, testIssuer)
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
	kp := testKey(t)
	s, err := NewSigner(kp, testIssuer)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   "alice#7",
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		NotBefore: jwt.NewNumericDate(now),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    testIssuer,
		Audience:  []string{"repo"},
	}
	// The RFC 7638 thumbprint of the Ed25519 key, as such tokens carry it,
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
		got, err := s.Verify(signed, []string{"repo"})
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
	s := testSigner(t)
	other := testSigner(t)
	user := testUser{name: "alice", id: 7}
	now := time.Now()
	valid := jwt.RegisteredClaims{
		Subject:   Subject(user),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    testIssuer,
		Audience:  []string{"repo"},
	}
	sign := func(c *Signer, mutate func(*jwt.RegisteredClaims)) string {
		claims := valid
		if mutate != nil {
			mutate(&claims)
		}
		token, err := c.SignClaims(claims)
		if err != nil {
			t.Fatalf("SignClaims: %v", err)
		}
		return token
	}

	if _, err := s.Verify(sign(s, nil), []string{"repo"}); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	for _, tc := range []struct {
		name     string
		token    string
		audience []string
	}{
		{"no audience asked", sign(s, nil), nil},
		{"other audience", sign(s, nil), []string{"other"}},
		{"other key", sign(other, nil), []string{"repo"}},
		{"expired", sign(s, func(c *jwt.RegisteredClaims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Minute)) }), []string{"repo"}},
		{"no expiry", sign(s, func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil }), []string{"repo"}},
		{"other issuer", sign(s, func(c *jwt.RegisteredClaims) { c.Issuer = "https://elsewhere" }), []string{"repo"}},
		{"issued in the future", sign(s, func(c *jwt.RegisteredClaims) { c.IssuedAt = jwt.NewNumericDate(now.Add(time.Hour)) }), []string{"repo"}},
		{"garbage", "a.b.c", []string{"repo"}},
	} {
		if _, err := s.Verify(tc.token, tc.audience); !errors.Is(err, errInvalid) || errors.Is(err, ErrHostKey) {
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
