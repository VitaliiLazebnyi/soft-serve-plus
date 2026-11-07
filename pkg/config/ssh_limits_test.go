package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSSHLimitsDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SSH.HandshakeTimeout <= 0 || cfg.SSH.MaxConnections <= 0 || cfg.SSH.MaxConnectionsPerIP <= 0 || cfg.SSH.MaxSessions <= 0 {
		t.Fatalf("ssh limits default to off: %+v", cfg.SSH)
	}
}

func TestSSHLimitsEnv(t *testing.T) {
	t.Setenv("SOFT_SERVE_SSH_MAX_CONNECTIONS", "7")
	t.Setenv("SOFT_SERVE_SSH_MAX_CONNECTIONS_PER_IP", "3")
	t.Setenv("SOFT_SERVE_SSH_MAX_SESSIONS", "0")
	t.Setenv("SOFT_SERVE_SSH_HANDSHAKE_TIMEOUT", "30")
	cfg := DefaultConfig()
	if err := cfg.ParseEnv(); err != nil {
		t.Fatal(err)
	}
	if cfg.SSH.MaxConnections != 7 || cfg.SSH.MaxConnectionsPerIP != 3 || cfg.SSH.MaxSessions != 0 || cfg.SSH.HandshakeTimeout != 30 {
		t.Fatalf("env not applied: %+v", cfg.SSH)
	}
	env := cfg.Environ()
	for _, want := range []string{
		"SOFT_SERVE_SSH_MAX_CONNECTIONS=7",
		"SOFT_SERVE_SSH_MAX_CONNECTIONS_PER_IP=3",
		"SOFT_SERVE_SSH_MAX_SESSIONS=0",
		"SOFT_SERVE_SSH_HANDSHAKE_TIMEOUT=30",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("Environ() lacks %q", want)
		}
	}
}

func TestSSHLimitsConfigFileRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SSH.MaxConnections = 11
	cfg.SSH.MaxConnectionsPerIP = 5
	cfg.SSH.MaxSessions = 4
	cfg.SSH.HandshakeTimeout = 9
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(newConfigFile(cfg)), 0o600); err != nil {
		t.Fatal(err)
	}
	got := DefaultConfig()
	if err := parseFile(got, path); err != nil {
		t.Fatal(err)
	}
	if got.SSH.MaxConnections != 11 || got.SSH.MaxConnectionsPerIP != 5 || got.SSH.MaxSessions != 4 || got.SSH.HandshakeTimeout != 9 {
		t.Fatalf("round trip lost ssh limits: %+v", got.SSH)
	}
}

func TestSSHLimitsRejectNegative(t *testing.T) {
	for _, set := range []func(*Config){
		func(c *Config) { c.SSH.MaxConnections = -1 },
		func(c *Config) { c.SSH.MaxConnectionsPerIP = -1 },
		func(c *Config) { c.SSH.MaxSessions = -1 },
		func(c *Config) { c.SSH.HandshakeTimeout = -1 },
	} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		set(cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate accepted a negative ssh limit: %+v", cfg.SSH)
		}
	}
}
