package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"go.yaml.in/yaml/v4"
)

// ConfigEnv names the file that holds the server's effective config for
// `soft hook` (see WriteConfig). The server sets it for receive-pack.
const ConfigEnv = "SOFT_SERVE_HOOK_CONFIG"

// ConfigPath returns the file WriteConfig writes under cfg's data path.
func ConfigPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataPath, "hook-config.yaml")
}

// WriteConfig writes cfg, the server's effective config, as YAML to
// ConfigPath, readable and writable only by the server's user, with the
// hook log as its log file. `soft hook` reads it back with ReadConfig
// instead of loading config.yaml and the environment again, so an edit to
// config.yaml after the server started, or a typo in it, does not reach the
// hooks, and they run with the settings the server runs with. The server
// writes it once at start; the file is replaced in one step, so a hook never
// reads half of it.
func WriteConfig(cfg *config.Config) error {
	hc := *cfg
	hc.Log.Path = LogPath(cfg)
	data, err := yaml.Marshal(&hc)
	if err != nil {
		return fmt.Errorf("encode hook config: %w", err)
	}
	path := ConfigPath(cfg)
	f, err := os.CreateTemp(filepath.Dir(path), ".hook-config-*.yaml")
	if err != nil {
		return fmt.Errorf("write hook config: %w", err)
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// CreateTemp makes the file 0600; say so whatever the platform.
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp) //nolint: errcheck
		return fmt.Errorf("write hook config: %w", err)
	}
	return nil
}

// ReadConfig reads the config WriteConfig wrote to path, for a server whose
// data path is dataPath (which the file leaves out). It does not read
// config.yaml or the environment.
func ReadConfig(path, dataPath string) (*config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read hook config: %w", err)
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = dataPath
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode hook config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("hook config %s: %w", path, err)
	}
	return cfg, nil
}
