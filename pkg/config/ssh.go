package config

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

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

// keyPairCache holds the last host key loaded per path, so callers on a hot
// path (every bearer-authenticated HTTP request) do not read and parse the
// key file each time. An entry is reused while the file's size and
// modification time are unchanged. That is a cheap check, not a content
// one: a key replaced with the same size and mtime (a restore that keeps
// timestamps, or a coarse-mtime filesystem) is not noticed until restart,
// which is also when the SSH server reloads its host key.
var keyPairCache = struct {
	sync.Mutex
	entries map[string]keyPairCacheEntry
}{entries: map[string]keyPairCacheEntry{}}

type keyPairCacheEntry struct {
	modTime time.Time
	size    int64
	key     *HostKey
}

// KeyPair returns the server's SSH host key, read from ssh.key_path. The
// file must exist and hold an unencrypted Ed25519, RSA or ECDSA key: the
// SSH server writes it on first start, and a key generated in memory here
// would differ on every call, so tokens signed with it could never be
// verified. The path is used as is, without expanding "~" or environment
// variables.
func KeyPair(cfg *Config) (*HostKey, error) {
	if cfg == nil {
		return nil, ErrNilConfig
	}

	path := cfg.SSH.KeyPath
	if path == "" {
		return nil, ErrEmptySSHKeyPath
	}

	// Stat and read the same open file, so the cached key always matches
	// the size and mtime it is stored under.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ssh host key: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("ssh host key: %w", err)
	}

	keyPairCache.Lock()
	defer keyPairCache.Unlock()

	if e, ok := keyPairCache.entries[path]; ok && e.size == fi.Size() && e.modTime.Equal(fi.ModTime()) {
		return e.key, nil
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("ssh host key %s: %w", path, err)
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

	key := &HostKey{priv: priv}
	keyPairCache.entries[path] = keyPairCacheEntry{modTime: fi.ModTime(), size: fi.Size(), key: key}
	return key, nil
}
