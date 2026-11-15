// Package jwk signs and verifies the JSON Web Tokens the server issues: the
// ones `soft jwt` prints and git-lfs-authenticate hands out, which the HTTP
// server accepts as bearer tokens. They are signed with the SSH host key.
package jwk

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/golang-jwt/jwt/v5"
)

// ErrHostKey wraps the error of a token that could not be signed or verified
// because the host key could not be loaded or used. That is a server fault,
// not a bad token.
var ErrHostKey = errors.New("no usable host key")

// ErrInvalid is returned for a token that does not verify.
var ErrInvalid = errors.New("invalid token")

// signingMethodFor returns the JSON Web Token signing method for a public
// key: EdDSA for Ed25519, RS256 for RSA and ES256, ES384 or ES512 for ECDSA
// on P-256, P-384 or P-521. Tokens are signed with the server's SSH host key,
// whose type follows the key at ssh.key_path.
func signingMethodFor(pub crypto.PublicKey) (jwt.SigningMethod, error) {
	switch k := pub.(type) {
	case ed25519.PublicKey:
		return jwt.SigningMethodEdDSA, nil
	case *rsa.PublicKey:
		return jwt.SigningMethodRS256, nil
	case *ecdsa.PublicKey:
		switch k.Curve.Params().Name {
		case "P-256":
			return jwt.SigningMethodES256, nil
		case "P-384":
			return jwt.SigningMethodES384, nil
		case "P-521":
			return jwt.SigningMethodES512, nil
		}
		return nil, fmt.Errorf("unsupported ECDSA curve for JWT signing: %s", k.Curve.Params().Name)
	default:
		return nil, fmt.Errorf("unsupported key type for JWT signing: %T", pub)
	}
}

// hostKey returns the host key and the signing method for its type.
func hostKey(cfg *config.Config) (*config.HostKey, jwt.SigningMethod, error) {
	kp, err := config.KeyPair(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrHostKey, err)
	}
	method, err := signingMethodFor(kp.CryptoPublicKey())
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrHostKey, err)
	}
	return kp, method, nil
}

// Subject returns the subject of a token for user: the username and the
// user ID, so a token does not carry over to a new account of the same name.
func Subject(user proto.User) string {
	return fmt.Sprintf("%s#%d", user.Username(), user.ID())
}

// Sign returns a token for user that is valid from now until expiresAt, for
// the repositories in audience, issued by the server at cfg.HTTP.PublicURL.
func Sign(cfg *config.Config, user proto.User, audience []string, expiresAt time.Time) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("%w: %w", ErrHostKey, config.ErrNilConfig)
	}
	now := time.Now()
	return SignClaims(cfg, jwt.RegisteredClaims{
		Subject:   Subject(user),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		NotBefore: jwt.NewNumericDate(now),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    cfg.HTTP.PublicURL,
		Audience:  audience,
	})
}

// SignClaims signs claims with the host key, using the algorithm of its key
// type. Sign is the way to issue a token; this is for callers, such as
// tests, that need other claims.
func SignClaims(cfg *config.Config, claims jwt.RegisteredClaims) (string, error) {
	kp, method, err := hostKey(cfg)
	if err != nil {
		return "", err
	}
	return jwt.NewWithClaims(method, claims).SignedString(kp.PrivateKey())
}

// Verify checks a token signed with the host key, in the algorithm of its
// key type, issued by the server at cfg.HTTP.PublicURL, with an expiry, and
// with an audience that names one of audience. It returns the token's
// claims. The error wraps ErrHostKey when the host key cannot be used, and
// ErrInvalid otherwise.
//
// Any "kid" header is ignored: tokens issued before it was dropped carry
// one, and there is only one key to verify with.
func Verify(cfg *config.Config, token string, audience []string) (*jwt.RegisteredClaims, error) {
	if len(audience) == 0 {
		// jwt.WithAudience checks nothing without an audience.
		return nil, ErrInvalid
	}
	kp, method, err := hostKey(cfg)
	if err != nil {
		return nil, err
	}

	pub := kp.CryptoPublicKey()
	parsed, err := jwt.ParseWithClaims(token, &jwt.RegisteredClaims{}, func(*jwt.Token) (any, error) {
		return pub, nil
	},
		jwt.WithValidMethods([]string{method.Alg()}),
		jwt.WithIssuer(cfg.HTTP.PublicURL),
		jwt.WithIssuedAt(),
		jwt.WithExpirationRequired(),
		jwt.WithAudience(audience...),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	claims, ok := parsed.Claims.(*jwt.RegisteredClaims)
	if !parsed.Valid || !ok {
		return nil, ErrInvalid
	}
	return claims, nil
}
