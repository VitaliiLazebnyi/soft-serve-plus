package ssh

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/log/v2"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// A client key path that cannot be checked, here because a file stands
// where its directory should be, stops the server from starting instead of
// leaving it without a client key.
func TestNewSSHServerReportsClientKeyStatError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	file := filepath.Join(cfg.DataPath, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.SSH.ClientKeyPath = filepath.Join(file, "client_ed25519")

	if _, err := NewSSHServer(serverContext(t, cfg, log.Default())); err == nil {
		t.Fatal("NewSSHServer succeeded, want a client key error")
	}
}
