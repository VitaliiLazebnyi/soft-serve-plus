package config

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/keygen"
)

var (
	// ErrNilConfig is returned when a nil config is passed to a function.
	ErrNilConfig = errors.New("nil config")

	// ErrEmptySSHKeyPath is returned when the SSH key path is empty.
	ErrEmptySSHKeyPath = errors.New("empty SSH key path")
)

// keyPairCache holds the last host key pair loaded per path, so callers on a
// hot path (every bearer-authenticated HTTP request) do not read and parse the
// key file each time. An entry is reused only while the file's size and
// modification time are unchanged, so a replaced key is picked up.
var keyPairCache = struct {
	sync.Mutex
	entries map[string]keyPairCacheEntry
}{entries: map[string]keyPairCacheEntry{}}

type keyPairCacheEntry struct {
	modTime time.Time
	size    int64
	kp      *keygen.SSHKeyPair
}

// KeyPair returns the server's SSH key pair, read from ssh.key_path. The file
// must exist: the SSH server writes it on first start, and a key generated in
// memory here would differ on every call, so tokens signed with it could
// never be verified.
func KeyPair(cfg *Config) (*keygen.SSHKeyPair, error) {
	if cfg == nil {
		return nil, ErrNilConfig
	}

	path := cfg.SSH.KeyPath
	if path == "" {
		return nil, ErrEmptySSHKeyPath
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("ssh host key: %w", err)
	}

	keyPairCache.Lock()
	defer keyPairCache.Unlock()

	if e, ok := keyPairCache.entries[path]; ok && e.size == fi.Size() && e.modTime.Equal(fi.ModTime()) {
		return e.kp, nil
	}

	kp, err := keygen.New(path, keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		return nil, fmt.Errorf("ssh host key: %w", err)
	}

	keyPairCache.entries[path] = keyPairCacheEntry{modTime: fi.ModTime(), size: fi.Size(), kp: kp}
	return kp, nil
}
