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

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/golang-jwt/jwt/v5"
)

// ErrHostKey wraps the error of a token that could not be signed or verified
// because the host key could not be loaded or used. That is a server fault,
// not a bad token.
var ErrHostKey = errors.New("no usable host key")

// errNoHostKey is the error of a nil *Signer: the server has no host key it
// can sign with.
var errNoHostKey = fmt.Errorf("%w: the host key was not loaded", ErrHostKey)

// errInvalid is wrapped by the error of a token that does not verify.
var errInvalid = errors.New("invalid token")

// HostKey is the key tokens are signed with: the server's SSH host key, as
// *config.HostKey holds it.
type HostKey interface {
	PrivateKey() crypto.PrivateKey
	CryptoPublicKey() crypto.PublicKey
}

// Signer signs and verifies the server's tokens with its host key. A nil
// *Signer stands for a server without a usable host key: its methods return
// an error wrapping ErrHostKey.
type Signer struct {
	priv   crypto.PrivateKey
	pub    crypto.PublicKey
	method jwt.SigningMethod
	// issuer is the server's HTTP public URL, the issuer of its tokens.
	issuer string
}

// NewSigner returns a Signer for key that issues tokens as issuer, the
// server's HTTP public URL. The error wraps ErrHostKey when key is nil or of
// a type tokens cannot be signed with. key must hold a key: pass nil, not a
// nil *config.HostKey, for none.
func NewSigner(key HostKey, issuer string) (*Signer, error) {
	if key == nil {
		return nil, errNoHostKey
	}
	pub := key.CryptoPublicKey()
	method, err := signingMethodFor(pub)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHostKey, err)
	}
	return &Signer{priv: key.PrivateKey(), pub: pub, method: method, issuer: issuer}, nil
}

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

// Subject returns the subject of a token for user: the username and the
// user ID, so a token does not carry over to a new account of the same name.
func Subject(user proto.User) string {
	return fmt.Sprintf("%s#%d", user.Username(), user.ID())
}

// Sign returns a token for user that is valid from now until expiresAt, for
// the repositories in audience.
func (s *Signer) Sign(user proto.User, audience []string, expiresAt time.Time) (string, error) {
	if s == nil {
		return "", errNoHostKey
	}
	now := time.Now()
	return s.SignClaims(jwt.RegisteredClaims{
		Subject:   Subject(user),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		NotBefore: jwt.NewNumericDate(now),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    s.issuer,
		Audience:  audience,
	})
}

// SignClaims signs claims with the host key, using the algorithm of its key
// type. Sign is the way to issue a token; this is for callers, such as
// tests, that need other claims.
func (s *Signer) SignClaims(claims jwt.RegisteredClaims) (string, error) {
	if s == nil {
		return "", errNoHostKey
	}
	return jwt.NewWithClaims(s.method, claims).SignedString(s.priv)
}

// Verify checks a token signed with the host key, in the algorithm of its
// key type, issued by s's issuer, with an expiry, and with an audience that
// names one of audience. It returns the token's claims. The error wraps
// ErrHostKey when s is nil; otherwise the token does not verify.
//
// Any "kid" header is ignored: tokens issued before it was dropped carry
// one, and there is only one key to verify with.
func (s *Signer) Verify(token string, audience []string) (*jwt.RegisteredClaims, error) {
	if len(audience) == 0 {
		// jwt.WithAudience checks nothing without an audience.
		return nil, errInvalid
	}
	if s == nil {
		return nil, errNoHostKey
	}

	parsed, err := jwt.ParseWithClaims(token, &jwt.RegisteredClaims{}, func(*jwt.Token) (any, error) {
		return s.pub, nil
	},
		jwt.WithValidMethods([]string{s.method.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithIssuedAt(),
		jwt.WithExpirationRequired(),
		jwt.WithAudience(audience...),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errInvalid, err)
	}

	claims, ok := parsed.Claims.(*jwt.RegisteredClaims)
	if !parsed.Valid || !ok {
		return nil, errInvalid
	}
	return claims, nil
}
