package config

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"errors"
	"fmt"
	"os"

	gossh "golang.org/x/crypto/ssh"
)

var (
	// ErrNilConfig is returned when a nil config is passed to a function.
	ErrNilConfig = errors.New("nil config")

	// ErrEmptySSHKeyPath is returned when the SSH key path is empty.
	ErrEmptySSHKeyPath = errors.New("empty SSH key path")
)

// HostKey is the server's SSH host key (Ed25519, RSA or ECDSA), used to
// sign and verify tokens.
type HostKey struct {
	priv crypto.Signer
}

// PrivateKey returns the private key.
func (k *HostKey) PrivateKey() crypto.PrivateKey {
	return k.priv
}

// CryptoPublicKey returns the public key.
func (k *HostKey) CryptoPublicKey() crypto.PublicKey {
	return k.priv.Public()
}

// KeyPair reads the server's SSH host key at ssh.key_path. The file must
// exist and hold an unencrypted Ed25519, RSA or ECDSA key: `soft serve`
// writes it on first start, and a key generated in memory here would differ
// on every call, so tokens signed with it could never be verified.
//
// `soft serve` reads it once, at start, and hands the key to the SSH server
// and the token signer, so a key replaced on disk is used from the next
// start.
func KeyPair(cfg *Config) (*HostKey, error) {
	if cfg == nil {
		return nil, ErrNilConfig
	}
	return readHostKey(cfg.SSH.KeyPath)
}

// readHostKey reads the host key at path. The path is used as is, without
// expanding "~" or environment variables.
func readHostKey(path string) (*HostKey, error) {
	if path == "" {
		return nil, ErrEmptySSHKeyPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ssh host key: %w", err)
	}
	raw, err := gossh.ParseRawPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("ssh host key %s: %w", path, err)
	}

	var priv crypto.Signer
	switch k := raw.(type) {
	case *ed25519.PrivateKey:
		priv = *k
	case ed25519.PrivateKey:
		priv = k
	case *rsa.PrivateKey:
		priv = k
	case *ecdsa.PrivateKey:
		priv = k
	default:
		return nil, fmt.Errorf("ssh host key %s: unsupported key type %T, want ed25519, rsa or ecdsa", path, raw)
	}
	return &HostKey{priv: priv}, nil
}
