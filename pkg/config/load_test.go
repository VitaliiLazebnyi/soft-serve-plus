package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

// parseEnv applies the environment to cfg and validates it, as Load does
// when there is no config file.
func parseEnv(cfg *Config) error {
	if err := cfg.readEnv(); err != nil {
		return err
	}
	return cfg.Validate()
}

// parseFile reads the config file at path into cfg and validates it, as
// Load does when the environment sets nothing.
func parseFile(cfg *Config, path string) error {
	if err := cfg.readFile(path); err != nil {
		return err
	}
	return cfg.Validate()
}

// marshalConfig returns cfg as a config file.
func marshalConfig(cfg *Config) (string, error) {
	b, err := yaml.Marshal(cfg)
	return string(b), err
}

// writeConfig writes cfg to path as a config file.
func writeConfig(cfg *Config, path string) error {
	s, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(s), 0o600)
}

// writeTestConfig writes cfg to its config path as a config file.
func writeTestConfig(t *testing.T, cfg *Config) error {
	t.Helper()
	return writeConfig(cfg, cfg.ConfigPath())
}

// The file the server writes on its first start holds the defaults, and
// only keys the server reads (R68, CFG-13).
func TestDefaultConfigFileMatchesDefaults(t *testing.T) {
	var got Config
	if err := yaml.Unmarshal(defaultConfigFile, &got); err != nil {
		t.Fatal(err)
	}
	want := DefaultConfig()
	want.DataPath = "" // not a config file setting
	// Marshal both, so a list the file spells [] and a nil default compare
	// equal, and a difference shows as the settings that differ.
	g, err := marshalConfig(&got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := marshalConfig(want)
	if err != nil {
		t.Fatal(err)
	}
	if g != w {
		t.Errorf("config.yaml differs from DefaultConfig():\nconfig.yaml:\n%s\nDefaultConfig():\n%s", g, w)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(defaultConfigFile, &doc); err != nil {
		t.Fatal(err)
	}
	if keys := findUnknownKeys(&doc, reflect.TypeFor[Config](), ""); len(keys) != 0 {
		t.Errorf("config.yaml has unknown keys: %v", keys)
	}
}

// The README's sample config is the file the server writes.
func TestReadmeConfigMatchesDefaultFile(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Git may check README.md and config.yaml out with CRLF line endings
	// (the windows CI runner does).
	doc := strings.ReplaceAll(string(readme), "\r\n", "\n")
	want := strings.ReplaceAll(string(defaultConfigFile), "\r\n", "\n")
	i := strings.Index(doc, "### Server Configuration")
	if i < 0 {
		t.Fatal("README has no Server Configuration section")
	}
	doc = doc[i:]
	start := strings.Index(doc, "```yaml\n")
	if start < 0 {
		t.Fatal("README Server Configuration has no yaml block")
	}
	doc = doc[start+len("```yaml\n"):]
	got := doc[:strings.Index(doc, "```")]
	if got != want {
		gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
		for n := 0; n < len(gl) || n < len(wl); n++ {
			var g, w string
			if n < len(gl) {
				g = gl[n]
			}
			if n < len(wl) {
				w = wl[n]
			}
			if g != w {
				t.Fatalf("README sample config differs from pkg/config/config.yaml at line %d:\nREADME:      %q\nconfig.yaml: %q", n+1, g, w)
			}
		}
	}
}

// WriteDefaultFile writes config.yaml as it is, whatever the config holds.
func TestWriteDefaultFile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = filepath.Join(t.TempDir(), "data")
	cfg.Name = "not the default"
	if err := cfg.WriteDefaultFile(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(defaultConfigFile) {
		t.Error("WriteDefaultFile did not write config.yaml")
	}
}

// loadEnv points Load at a data directory with the given config file, or
// none, and clears the SOFT_SERVE_* variables the test environment may set.
func loadEnv(t *testing.T, file string) string {
	t.Helper()
	clearSoftServeEnv(t)
	dp := t.TempDir()
	t.Setenv("SOFT_SERVE_DATA_PATH", dp)
	if file != "" {
		if err := os.WriteFile(filepath.Join(dp, "config.yaml"), []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dp
}

// Load validates once, after the environment, so the environment can
// repair a value the file got wrong (R68, CFG-10).
func TestLoadEnvironmentRepairsFile(t *testing.T) {
	for name, tc := range map[string]struct {
		file string
		env  map[string]string
		ok   func(*Config) bool
	}{
		"log format": {
			file: "log:\n  format: pretty\n",
			env:  map[string]string{"SOFT_SERVE_LOG_FORMAT": "json"},
			ok:   func(c *Config) bool { return c.Log.Format == "json" },
		},
		"tls pair": {
			file: "http:\n  tls_key_path: key.pem\n",
			env:  map[string]string{"SOFT_SERVE_HTTP_TLS_CERT_PATH": "cert.pem"},
			ok:   func(c *Config) bool { return filepath.Base(c.HTTP.TLSCertPath) == "cert.pem" },
		},
		"git max connections": {
			file: "git:\n  max_connections: 0\n",
			env:  map[string]string{"SOFT_SERVE_GIT_ENABLED": "false"},
			ok:   func(c *Config) bool { return !c.Git.Enabled },
		},
	} {
		t.Run(name, func(t *testing.T) {
			loadEnv(t, tc.file)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			if !tc.ok(cfg) {
				t.Errorf("Load() did not apply the environment: %+v", cfg)
			}
		})
	}
}

// Load reads the file and then the environment, and validates the result.
func TestLoad(t *testing.T) {
	dp := loadEnv(t, "name: from file\nmax_repos_per_user: 4\nlog:\n  path: soft.log\n")
	t.Setenv("SOFT_SERVE_MAX_REPOS_PER_USER", "7")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "from file" || cfg.MaxReposPerUser != 7 {
		t.Errorf("Load() name %q, max_repos_per_user %d; want the file's name and the environment's limit", cfg.Name, cfg.MaxReposPerUser)
	}
	if want := filepath.Join(dp, "soft.log"); cfg.Log.Path != want {
		t.Errorf("Load() log path %q, want it resolved to %q", cfg.Log.Path, want)
	}

	// Without a file, the defaults and the environment.
	loadEnv(t, "")
	t.Setenv("SOFT_SERVE_NAME", "from env")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "from env" || cfg.MaxReposPerUser != 0 {
		t.Errorf("Load() name %q, max_repos_per_user %d; want the environment's name and the default limit", cfg.Name, cfg.MaxReposPerUser)
	}

	// An invalid result is an error.
	loadEnv(t, "log:\n  format: pretty\n")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "log.format") {
		t.Errorf("Load() = %v, want a log.format error", err)
	}
}

// A SQLite data source with no file name, empty or only options, is refused
// rather than opening the data directory, or a file named "?_pragma=…"
// (tracker 323). Postgres reads
// the PG* variables when it is empty, so it is allowed there.
func TestValidateRejectsEmptySQLiteDataSource(t *testing.T) {
	for _, ds := range []string{"", "  ", "?_pragma=busy_timeout(5000)", "file:?mode=rwc", " ?x=1"} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.DB.DataSource = ds
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "db.data_source") {
			t.Errorf("Validate() with sqlite data source %q = %v, want a db.data_source error", ds, err)
		}
	}
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = "postgres"
	cfg.DB.DataSource = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() with an empty postgres data source = %v", err)
	}
}

// A config that still sets a removed setting loads, whatever the value,
// and the setting is reported as removed, not as unknown (R68, Q3). Its
// value reaches nothing: hooks do not get it either.
func TestRemovedSettings(t *testing.T) {
	loadEnv(t, `ssh:
  kex_algorithms: ["diffie-hellman-group14-sha1"]
  ciphers: ["aes128-ctr"]
  macs: ["hmac-sha1"]
  max_sessions: -1
  handshake_timeout: -1
  listen_adr: ":1"
db:
  max_idle_conns: -1
  conn_max_lifetime: -1
  conn_max_idle_time: -1
jobs:
  lfs_sweep: "every hour"
max_blob_size: -1
`)
	t.Setenv("SOFT_SERVE_SSH_MACS", "hmac-sha1")
	t.Setenv("SOFT_SERVE_MAX_BLOB_SIZE", "-1")
	t.Setenv("SOFT_SERVE_SSH_CIPHERS", "") // empty sets nothing
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want a config that ignores the removed settings", err)
	}
	var got []string
	for _, r := range cfg.RemovedSettings() {
		if r.Now == "" {
			t.Errorf("%s: no word on what the server does instead", r.Key)
		}
		got = append(got, r.Key+" "+r.Source)
	}
	want := []string{
		"ssh.kex_algorithms line 2",
		"ssh.ciphers line 3",
		"ssh.macs line 4",
		"ssh.max_sessions line 5",
		"ssh.handshake_timeout line 6",
		"db.max_idle_conns line 9",
		"db.conn_max_lifetime line 10",
		"db.conn_max_idle_time line 11",
		"jobs.lfs_sweep line 13",
		"max_blob_size line 14",
		"ssh.macs SOFT_SERVE_SSH_MACS",
		"max_blob_size SOFT_SERVE_MAX_BLOB_SIZE",
	}
	if !slices.Equal(got, want) {
		t.Errorf("RemovedSettings() =\n%q\nwant\n%q", got, want)
	}
	if keys := cfg.UnknownKeys(); len(keys) != 1 || keys[0].Key != "ssh.listen_adr" {
		t.Errorf("UnknownKeys() = %v, want only ssh.listen_adr", keys)
	}
	for _, kv := range cfg.Environ() {
		for _, r := range removedSettings {
			if strings.HasPrefix(kv, r.env+"=") {
				t.Errorf("Environ() passes the removed %s on", kv)
			}
		}
	}

	// No field reads a removed setting any more.
	for _, r := range removedSettings {
		path := strings.Split(r.key, ".")
		src := path[len(path)-1] + ": 1\n"
		for i := len(path) - 2; i >= 0; i-- {
			src = path[i] + ":\n  " + strings.ReplaceAll(strings.TrimSuffix(src, "\n"), "\n", "\n  ") + "\n"
		}
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
			t.Fatal(err)
		}
		if keys := findUnknownKeys(&doc, reflect.TypeFor[Config](), ""); len(keys) != 1 || keys[0].Key != r.key {
			t.Errorf("%s: a field still reads it (unknown keys %v)", r.key, keys)
		}
	}
}
