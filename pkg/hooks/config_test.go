package hooks

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// The hook config holds what a hook uses, the log settings with the hook log
// as the log file, and reads back as the server's; it holds no secret, and
// is private to the server's user: not the whole config, whose database
// data source can hold a password.
func TestHookConfigRoundTrip(t *testing.T) {
	dataPath := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dataPath
	cfg.Log.Format = "json"
	cfg.Log.TimeFormat = "15:04"
	cfg.DB.Driver = "postgres"
	cfg.DB.DataSource = "postgres://u:s3cret-pass@db.example.com/soft"
	cfg.InitialAdminKeys = []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOQnsdQmTcJbqtCLDVnUn0Gq7cuP9XHAQ1fa3hSQ4ZKr admin"}
	cfg.SSH.KeyPath = "ssh/custom_host_key"
	cfg.HTTP.TLSKeyPath = "tls/custom.key"
	cfg.HTTP.TLSCertPath = "tls/custom.crt"
	anon := access.ReadWriteAccess
	cfg.AnonAccess = &anon
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	if err := WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	path := ConfigPath(cfg)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("hook config mode %v, want 0600", fi.Mode().Perm())
	}
	// Only the file itself is left in the data path.
	entries, err := os.ReadDir(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("WriteConfig left %s", e.Name())
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"s3cret-pass", "data_source", "AAAAC3Nza", "initial_admin_keys", "custom_host_key", "custom.key", "anon_access"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("hook config has %q:\n%s", secret, raw)
		}
	}

	got, err := ReadConfig(path, dataPath)
	if err != nil {
		t.Fatal(err)
	}
	want := cfg.Log
	want.Path = LogPath(cfg)
	if got.Log != want {
		t.Errorf("log settings read back %+v, want %+v", got.Log, want)
	}
	if got.DataPath != dataPath {
		t.Errorf("data path %q, want %q", got.DataPath, dataPath)
	}

	// Writing again replaces it.
	cfg.Log.Format = "logfmt"
	if err := WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err = ReadConfig(path, dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Log.Format != "logfmt" {
		t.Errorf("log format %q after rewrite", got.Log.Format)
	}
}

// Keys the reader does not know, as in a newer server's file after a
// downgrade or a log setting this version lacks, are ignored, and the known
// settings are read. Refusing them would fail every push through an older
// version's hook until the server rewrote the file.
func TestReadConfigIgnoresUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hook-config.yaml")
	if err := os.WriteFile(path, []byte("log:\n  format: json\n  color: never\nfuture:\n  setting: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(path, dir)
	if err != nil {
		t.Fatalf("ReadConfig with unknown keys: %v", err)
	}
	if cfg.Log.Format != "json" {
		t.Errorf("log format %q, want json", cfg.Log.Format)
	}
}

// A hook config that cannot be read or decoded is an error that names the
// file.
func TestReadConfigErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("log: [not a map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "missing.yaml"), bad} {
		if _, err := ReadConfig(path, dir); err == nil || !strings.Contains(err.Error(), filepath.Base(path)) {
			t.Errorf("ReadConfig(%s) = %v, want an error naming it", filepath.Base(path), err)
		}
	}
}
