package web

import (
	"context"
	"crypto/elliptic"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/golang-jwt/jwt/v5"
)

// TestParseJWTHostKeyTypes checks that tokens minted with the server's host
// key verify whatever the key type is. Only Ed25519 used to work, so an RSA
// or ECDSA host key (as suggested for older SSH clients) broke `soft jwt` and
// git-lfs over SSH.
func TestParseJWTHostKeyTypes(t *testing.T) {
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
			ctx := jwtTestContext(t, tc.opts...)
			token := signJWT(ctx, t, jwtTestClaims(ctx, "repo"))

			claims, err := parseJWT(ctx, token, []string{"repo"})
			if err != nil {
				t.Fatalf("parseJWT: %v", err)
			}
			if claims.Subject != "user#1" {
				t.Errorf("subject = %q, want %q", claims.Subject, "user#1")
			}

			parsed, _, err := jwt.NewParser().ParseUnverified(token, &jwt.RegisteredClaims{})
			if err != nil {
				t.Fatalf("ParseUnverified: %v", err)
			}
			if got := parsed.Method.Alg(); got != tc.alg {
				t.Errorf("alg = %q, want %q", got, tc.alg)
			}
		})
	}
}

// TestParseJWTRejectsOtherAlgorithms checks that a token is only accepted
// with the algorithm of the host key type, so a token signed with another
// key or algorithm does not verify.
func TestParseJWTRejectsOtherAlgorithms(t *testing.T) {
	t.Parallel()
	rsaCtx := jwtTestContext(t, keygen.WithKeyType(keygen.RSA), keygen.WithBitSize(2048))
	edCtx := jwtTestContext(t, keygen.WithKeyType(keygen.Ed25519))

	if _, err := parseJWT(rsaCtx, signJWT(edCtx, t, jwtTestClaims(edCtx, "repo")), []string{"repo"}); err == nil {
		t.Error("parseJWT accepted an EdDSA token for an RSA host key")
	}
	if _, err := parseJWT(edCtx, signJWT(rsaCtx, t, jwtTestClaims(rsaCtx, "repo")), []string{"repo"}); err == nil {
		t.Error("parseJWT accepted an RS256 token for an Ed25519 host key")
	}

	// An RSA-PSS token from the same RSA key is not the expected RS256.
	kp, err := config.KeyPair(config.FromContext(rsaCtx))
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	pss, err := jwt.NewWithClaims(jwt.SigningMethodPS256, jwtTestClaims(rsaCtx, "repo")).SignedString(kp.PrivateKey())
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := parseJWT(rsaCtx, pss, []string{"repo"}); err == nil {
		t.Error("parseJWT accepted a PS256 token for an RSA host key")
	}
}

func jwtTestContext(t *testing.T, opts ...keygen.Option) context.Context {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.KeyPath = filepath.Join(cfg.DataPath, "ssh", "host_key")
	if _, err := keygen.New(cfg.SSH.KeyPath, append(opts, keygen.WithWrite())...); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return config.WithContext(context.Background(), cfg)
}

func jwtTestClaims(ctx context.Context, audience ...string) jwt.RegisteredClaims {
	now := time.Now()
	return jwt.RegisteredClaims{
		Subject:   "user#1",
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		NotBefore: jwt.NewNumericDate(now),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    config.FromContext(ctx).HTTP.PublicURL,
		Audience:  audience,
	}
}

// TestBearerTokenWithKeyID checks a token minted before the "kid" header was
// dropped, which carries one, still authenticates its user until it expires.
func TestBearerTokenWithKeyID(t *testing.T) {
	f := newOracleFixture(t)
	kp, err := config.KeyPair(config.FromContext(f.ctx))
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	claims := jwtTestClaims(f.ctx, "secret")
	claims.Subject = fmt.Sprintf("%s#%d", f.owner.Username(), f.owner.ID())
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	// An RFC 7638 thumbprint, as the old code put there.
	token.Header["kid"] = "pXIvChkPX0Nik0T-8ADSGgxzCbqOXKyjqTUu4M4TYy0"
	signed, err := token.SignedString(kp.PrivateKey())
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	const path = "/secret.git/info/refs?service=git-upload-pack"
	if got := f.do(http.MethodGet, path, "Bearer "+signed).Code; got != http.StatusOK {
		t.Errorf("status = %d, want %d", got, http.StatusOK)
	}
	// Without the token the private repository is not readable.
	if got := f.do(http.MethodGet, path, "").Code; got != http.StatusUnauthorized {
		t.Errorf("anonymous status = %d, want %d", got, http.StatusUnauthorized)
	}
}
