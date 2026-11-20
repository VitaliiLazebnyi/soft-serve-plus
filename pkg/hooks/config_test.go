package hooks

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// The hook config is the server's effective config: what WriteConfig writes,
// ReadConfig reads back with every setting the same, the hook log included,
// and the file is private to the server's user.
func TestHookConfigRoundTrip(t *testing.T) {
	dataPath := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dataPath
	cfg.Name = "Hooked"
	anon := access.ReadWriteAccess
	cfg.AnonAccess = &anon
	keyless := false
	cfg.AllowKeyless = &keyless
	cfg.SSH.MaxTimeout = 42
	cfg.SSH.ClientAllowedHosts = []string{"example.com:22"}
	cfg.HTTP.PublicURL = "https://git.example.com"
	cfg.HTTP.CORS.AllowedOrigins = []string{"https://a.example.com"}
	cfg.Webhook.AllowedHosts = []string{"10.0.0.1:8080"}
	cfg.DB.DataSource = "postgres://u:secret@db.example.com/soft"
	cfg.DB.Driver = "postgres"
	cfg.MaxReposPerUser = 7
	cfg.DefaultPrivate = true
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

	got, err := ReadConfig(path, dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Environ(), cfg.Environ()) {
		t.Errorf("read back\n%q\nwant\n%q", got.Environ(), cfg.Environ())
	}
	if want := LogPath(cfg); got.Log.Path != want {
		t.Errorf("log path %q, want the hook log %q", got.Log.Path, want)
	}

	// Writing again replaces it.
	cfg.Name = "Rehooked"
	if err := WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err = ReadConfig(path, dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Rehooked" {
		t.Errorf("name %q after rewrite", got.Name)
	}
}

// A hook config that cannot be read, decoded or validated is an error that
// names the file.
func TestReadConfigErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("ssh: [not a map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("db:\n  driver: mysql\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "missing.yaml"), bad, invalid} {
		if _, err := ReadConfig(path, dir); err == nil || !strings.Contains(err.Error(), filepath.Base(path)) {
			t.Errorf("ReadConfig(%s) = %v, want an error naming it", filepath.Base(path), err)
		}
	}
}
