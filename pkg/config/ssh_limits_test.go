package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSSHLimitsDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SSH.MaxConnections <= 0 || cfg.SSH.MaxConnectionsPerIP <= 0 {
		t.Fatalf("ssh limits default to off: %+v", cfg.SSH)
	}
}

func TestSSHLimitsEnv(t *testing.T) {
	t.Setenv("SOFT_SERVE_SSH_MAX_CONNECTIONS", "7")
	t.Setenv("SOFT_SERVE_SSH_MAX_CONNECTIONS_PER_IP", "3")
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SSH.MaxConnections != 7 || cfg.SSH.MaxConnectionsPerIP != 3 {
		t.Fatalf("env not applied: %+v", cfg.SSH)
	}
	env := cfg.Environ()
	for _, want := range []string{
		"SOFT_SERVE_SSH_MAX_CONNECTIONS=7",
		"SOFT_SERVE_SSH_MAX_CONNECTIONS_PER_IP=3",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("Environ() lacks %q", want)
		}
	}
}

func TestSSHLimitsConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("ssh:\n  max_connections: 11\n  max_connections_per_ip: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := DefaultConfig()
	if err := parseFile(got, path); err != nil {
		t.Fatal(err)
	}
	if got.SSH.MaxConnections != 11 || got.SSH.MaxConnectionsPerIP != 5 {
		t.Fatalf("ssh limits not read from the file: %+v", got.SSH)
	}
}

func TestSSHLimitsRejectNegative(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Config)
	}{
		{"ssh.max_connections", func(c *Config) { c.SSH.MaxConnections = -1 }},
		{"ssh.max_connections_per_ip", func(c *Config) { c.SSH.MaxConnectionsPerIP = -1 }},
	} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		tc.set(cfg)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("Validate accepted a negative %s: %+v", tc.name, cfg.SSH)
			continue
		}
		if !strings.HasPrefix(err.Error(), tc.name+": ") {
			t.Errorf("Validate error for negative %s does not name the field: %v", tc.name, err)
		}
	}
}
