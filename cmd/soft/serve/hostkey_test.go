package serve

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// The start-up warning tells a host key that cannot be read from one that
// was read but cannot sign tokens.
func TestHostKeyWarning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// An X25519 key in PKCS#8 parses, but tokens cannot be signed with it.
	x, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(x)
	if err != nil {
		t.Fatal(err)
	}
	unsupported := filepath.Join(dir, "x25519_key")
	if err := os.WriteFile(unsupported, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, path, want string
	}{
		{"missing", filepath.Join(dir, "missing"), "cannot read the SSH host key"},
		{"empty path", "", "cannot read the SSH host key"},
		{"unsupported type", unsupported, "the SSH host key cannot sign tokens"},
	} {
		cfg := config.DefaultConfig()
		cfg.SSH.KeyPath = tc.path
		tokens, err := tokenSigner(cfg)
		if err == nil || tokens != nil {
			t.Fatalf("%s: tokenSigner => %v, %v, want an error", tc.name, tokens, err)
		}
		if got := hostKeyWarning(err); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: warning %q, want it to start with %q (err %v)", tc.name, got, tc.want, err)
		}
	}
}
