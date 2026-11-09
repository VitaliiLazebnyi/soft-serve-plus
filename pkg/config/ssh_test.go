package config

import (
	"crypto"
	"crypto/ed25519"
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

// samePublicKey reports whether got holds the public key of want.
func samePublicKey(got *HostKey, want *keygen.SSHKeyPair) bool {
	pub, ok := got.CryptoPublicKey().(ed25519.PublicKey)
	return ok && pub.Equal(want.CryptoPublicKey())
}

func TestGoodSSHKeyPair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_host_ed25519_key")
	want := writeTestHostKey(t, path)
	cfg := &Config{SSH: SSHConfig{KeyPath: path}}

	kp, err := KeyPair(cfg)
	if err != nil {
		t.Fatalf("cfg.SSH.KeyPair() => _, %v, want nil error", err)
	}
	if !samePublicKey(kp, want) {
		t.Errorf("KeyPair() returned a key that is not the one on disk")
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
	if !samePublicKey(third, other) {
		t.Errorf("KeyPair() kept the old key after the file was replaced")
	}
}

// The path is used as is. keygen.New expands environment variables, so a
// path holding a literal "$" used to miss the file and silently generate a
// throwaway key.
func TestSSHKeyPairPathWithDollar(t *testing.T) {
	base := t.TempDir()
	// keygen would expand the "$" too, so write the key elsewhere and copy
	// it into place.
	src := filepath.Join(base, "src_key")
	want := writeTestHostKey(t, src)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "soft$x")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "host_key")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{SSH: SSHConfig{KeyPath: path}}

	for range 2 {
		kp, err := KeyPair(cfg)
		if err != nil {
			t.Fatalf("KeyPair: %v", err)
		}
		if !samePublicKey(kp, want) {
			t.Fatalf("KeyPair() returned a key that is not the one at %s", path)
		}
	}
}

// Every host key type the SSH server accepts (Ed25519, RSA, ECDSA) is
// loaded from disk, so tokens can be signed with whichever key is set.
func TestSSHKeyPairKeyTypes(t *testing.T) {
	dir := t.TempDir()
	for _, kt := range []keygen.KeyType{keygen.Ed25519, keygen.RSA, keygen.ECDSA} {
		path := filepath.Join(dir, string(kt)+"_key")
		want, err := keygen.New(path, keygen.WithKeyType(kt), keygen.WithWrite())
		if err != nil {
			t.Fatalf("write %s key: %v", kt, err)
		}
		kp, err := KeyPair(&Config{SSH: SSHConfig{KeyPath: path}})
		if err != nil {
			t.Fatalf("KeyPair(%s) => _, %v, want nil error", kt, err)
		}
		pub, ok := kp.CryptoPublicKey().(interface{ Equal(crypto.PublicKey) bool })
		if !ok || !pub.Equal(want.CryptoPublicKey()) {
			t.Errorf("KeyPair(%s) returned a key that is not the one on disk", kt)
		}
	}
}

// A file that is not a key is an error rather than a reason to make one up.
func TestSSHKeyPairUnusableFile(t *testing.T) {
	junkPath := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(junkPath, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}

	if kp, err := KeyPair(&Config{SSH: SSHConfig{KeyPath: junkPath}}); err == nil {
		t.Errorf("KeyPair(junk) => %v, nil, want an error", kp)
	}
}
