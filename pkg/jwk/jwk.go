package jwk

import (
	"crypto"
	"encoding/base64"
	"fmt"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

// SigningMethod is a JSON Web Token signing method. It uses Ed25519 keys to
// sign and verify tokens.
var SigningMethod = &jwt.SigningMethodEd25519{}

// Pair is a JSON Web Key pair.
type Pair struct {
	privateKey crypto.PrivateKey
	jwk        jose.JSONWebKey
}

// PrivateKey returns the private key.
func (p Pair) PrivateKey() crypto.PrivateKey {
	return p.privateKey
}

// JWK returns the JSON Web Key.
func (p Pair) JWK() jose.JSONWebKey {
	return p.jwk
}

// NewPair creates a new JSON Web Key pair.
func NewPair(cfg *config.Config) (Pair, error) {
	kp, err := config.KeyPair(cfg)
	if err != nil {
		return Pair{}, err
	}

	jwk := jose.JSONWebKey{
		Key:       kp.CryptoPublicKey(),
		Algorithm: SigningMethod.Alg(),
	}

	// The key ID is the RFC 7638 thumbprint of the public key, so it is
	// stable for a given host key and reveals nothing about the private key.
	thumb, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return Pair{}, fmt.Errorf("jwk thumbprint: %w", err)
	}
	jwk.KeyID = base64.RawURLEncoding.EncodeToString(thumb)

	return Pair{privateKey: kp.PrivateKey(), jwk: jwk}, nil
}
