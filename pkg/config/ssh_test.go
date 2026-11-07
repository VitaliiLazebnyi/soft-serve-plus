package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/keygen"
)

func TestBadSSHKeyPair(t *testing.T) {
	for _, cfg := range []*Config{
		nil,
		{},
	} {
		if _, err := KeyPair(cfg); err == nil {
			t.Errorf("cfg.SSH.KeyPair() => _, nil, want non-nil error")
		}
	}
}

func writeTestHostKey(t *testing.T, path string) *keygen.SSHKeyPair {
	t.Helper()
	kp, err := keygen.New(path, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite())
	if err != nil {
		t.Fatalf("write host key: %v", err)
	}
	return kp
}

func TestGoodSSHKeyPair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_host_ed25519_key")
	want := writeTestHostKey(t, path)
	cfg := &Config{SSH: SSHConfig{KeyPath: path}}

	kp, err := KeyPair(cfg)
	if err != nil {
		t.Fatalf("cfg.SSH.KeyPair() => _, %v, want nil error", err)
	}
	if string(kp.AuthorizedKey()) != string(want.AuthorizedKey()) {
		t.Errorf("KeyPair() returned %q, want the key on disk %q", kp.AuthorizedKey(), want.AuthorizedKey())
	}
}

// A missing host key is an error: a key generated in memory would differ on
// every call, so a token signed with one could never be verified.
func TestSSHKeyPairMissingFile(t *testing.T) {
	cfg := &Config{SSH: SSHConfig{KeyPath: filepath.Join(t.TempDir(), "missing")}}

	for range 2 {
		if _, err := KeyPair(cfg); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("KeyPair() => _, %v, want an error wrapping %v", err, fs.ErrNotExist)
		}
	}
	if _, err := os.Stat(cfg.SSH.KeyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("KeyPair() created %s: %v", cfg.SSH.KeyPath, err)
	}
}

// The key pair is loaded once and reused until the file changes.
func TestSSHKeyPairCached(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host_key")
	writeTestHostKey(t, path)
	cfg := &Config{SSH: SSHConfig{KeyPath: path}}

	first, err := KeyPair(cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	second, err := KeyPair(cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	if first != second {
		t.Errorf("KeyPair() parsed the unchanged key file again")
	}

	// Replace the key; the next call must load the new one.
	other := writeTestHostKey(t, filepath.Join(dir, "other_key"))
	data, err := os.ReadFile(filepath.Join(dir, "other_key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	third, err := KeyPair(cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	if string(third.AuthorizedKey()) != string(other.AuthorizedKey()) {
		t.Errorf("KeyPair() kept the old key after the file was replaced")
	}
}
