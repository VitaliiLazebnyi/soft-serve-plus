package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/matryer/is"
)

var httpLimitsEnv = []string{
	"SOFT_SERVE_HTTP_MAX_TIMEOUT",
	"SOFT_SERVE_HTTP_IDLE_TIMEOUT",
	"SOFT_SERVE_HTTP_MAX_CONNECTIONS",
}

func TestHTTPLimits(t *testing.T) {
	is := is.New(t)
	// Ignore any value exported in the environment running the tests.
	for _, k := range httpLimitsEnv {
		t.Setenv(k, "")
		is.NoErr(os.Unsetenv(k))
	}
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.HTTP.MaxTimeout, 0)
	is.Equal(cfg.HTTP.IdleTimeout, 60)
	is.Equal(cfg.HTTP.MaxConnections, 64)

	t.Setenv("SOFT_SERVE_HTTP_MAX_TIMEOUT", "3600")
	t.Setenv("SOFT_SERVE_HTTP_IDLE_TIMEOUT", "30")
	t.Setenv("SOFT_SERVE_HTTP_MAX_CONNECTIONS", "8")
	cfg = DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.HTTP.MaxTimeout, 3600)
	is.Equal(cfg.HTTP.IdleTimeout, 30)
	is.Equal(cfg.HTTP.MaxConnections, 8)
	// Hooks re-read the config from the environment.
	env := cfg.Environ()
	for _, want := range []string{
		"SOFT_SERVE_HTTP_MAX_TIMEOUT=3600",
		"SOFT_SERVE_HTTP_IDLE_TIMEOUT=30",
		"SOFT_SERVE_HTTP_MAX_CONNECTIONS=8",
	} {
		is.True(slices.Contains(env, want))
	}
	for _, k := range httpLimitsEnv {
		is.NoErr(os.Unsetenv(k))
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	is.NoErr(os.WriteFile(path, []byte(`http:
  max_timeout: 600
  idle_timeout: 0
  max_connections: 16
`), 0o600))
	cfg = DefaultConfig()
	cfg.DataPath = dir
	is.NoErr(parseFile(cfg, path))
	is.Equal(cfg.HTTP.MaxTimeout, 600)
	is.Equal(cfg.HTTP.IdleTimeout, 0)
	is.Equal(cfg.HTTP.MaxConnections, 16)

	// The generated config file keeps the values.
	out := filepath.Join(dir, "written.yaml")
	is.NoErr(writeConfig(cfg, out))
	got := DefaultConfig()
	got.DataPath = dir
	is.NoErr(parseFile(got, out))
	is.Equal(got.HTTP.MaxTimeout, 600)
	is.Equal(got.HTTP.IdleTimeout, 0)
	is.Equal(got.HTTP.MaxConnections, 16)
}

func TestHTTPLimitsRejectNegative(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*HTTPConfig)
	}{
		{"http.max_timeout", func(c *HTTPConfig) { c.MaxTimeout = -1 }},
		{"http.idle_timeout", func(c *HTTPConfig) { c.IdleTimeout = -1 }},
		{"http.max_connections", func(c *HTTPConfig) { c.MaxConnections = -1 }},
	} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		tc.set(&cfg.HTTP)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%s: Validate() = %v, want an error naming it", tc.name, err)
		}
	}
}
