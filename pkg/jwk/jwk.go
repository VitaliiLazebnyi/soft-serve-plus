package jwk

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/base64"
	"fmt"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

// SigningMethodFor returns the JSON Web Token signing method for a public
// key: EdDSA for Ed25519, RS256 for RSA and ES256, ES384 or ES512 for ECDSA
// on P-256, P-384 or P-521. Tokens are signed with the server's SSH host key,
// whose type follows the key at ssh.key_path.
func SigningMethodFor(pub crypto.PublicKey) (jwt.SigningMethod, error) {
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

// Pair is a JSON Web Key pair.
type Pair struct {
	privateKey crypto.PrivateKey
	jwk        jose.JSONWebKey
	method     jwt.SigningMethod
}

// PrivateKey returns the private key.
func (p Pair) PrivateKey() crypto.PrivateKey {
	return p.privateKey
}

// JWK returns the JSON Web Key.
func (p Pair) JWK() jose.JSONWebKey {
	return p.jwk
}

// SigningMethod returns the signing method that matches the key type.
func (p Pair) SigningMethod() jwt.SigningMethod {
	return p.method
}

// NewPair creates a new JSON Web Key pair.
func NewPair(cfg *config.Config) (Pair, error) {
	kp, err := config.KeyPair(cfg)
	if err != nil {
		return Pair{}, err
	}

	method, err := SigningMethodFor(kp.CryptoPublicKey())
	if err != nil {
		return Pair{}, err
	}

	jwk := jose.JSONWebKey{
		Key:       kp.CryptoPublicKey(),
		Algorithm: method.Alg(),
	}

	// The key ID is the RFC 7638 thumbprint of the public key, so it is
	// stable for a given host key and reveals nothing about the private key.
	thumb, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return Pair{}, fmt.Errorf("jwk thumbprint: %w", err)
	}
	jwk.KeyID = base64.RawURLEncoding.EncodeToString(thumb)

	return Pair{privateKey: kp.PrivateKey(), jwk: jwk, method: method}, nil
}
