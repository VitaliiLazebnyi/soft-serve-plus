package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
)

// environExcluded lists the config fields that Environ leaves out on
// purpose, by their Go path, with the reason.
var environExcluded = map[string]string{
	// The server points hooks at their own log file
	// (SOFT_SERVE_LOG_PATH=<data>/log/hooks.log) and appends Environ after
	// it, so emitting the server's log path would override that.
	"Log.Path": "hooks log to their own file",
}

// roundTripConfig returns a valid config whose every field, except those in
// environExcluded, differs from DefaultConfig.
func roundTripConfig(t *testing.T) *Config {
	t.Helper()
	dp := t.TempDir()
	readWrite := access.ReadWriteAccess
	allowKeyless := false
	return &Config{
		Name: "Round Trip",
		SSH: SSHConfig{
			Enabled:                     false,
			ListenAddr:                  ":2222",
			PublicURL:                   "ssh://ssh.example.com:2222",
			KeyPath:                     filepath.Join(dp, "host key"),
			ClientKeyPath:               filepath.Join(dp, "client key"),
			ClientStrictHostKeyChecking: StrictHostKeyCheckingYes,
			ClientAllowedHosts:          []string{"git.example.com", "10.0.0.0/8"},
			MaxTimeout:                  11,
			IdleTimeout:                 12,
			KexAlgorithms:               []string{"curve25519-sha256"},
			Ciphers:                     []string{"aes256-gcm@openssh.com"},
			MACs:                        []string{"hmac-sha2-256-etm@openssh.com"},
		},
		Git: GitConfig{
			Enabled:        false,
			ListenAddr:     ":9419",
			PublicURL:      "git://git.example.com",
			MaxTimeout:     13,
			IdleTimeout:    14,
			MaxConnections: 15,
		},
		HTTP: HTTPConfig{
			Enabled:     false,
			ListenAddr:  ":8080",
			TLSKeyPath:  filepath.Join(dp, "tls.key"),
			TLSCertPath: filepath.Join(dp, "tls.crt"),
			PublicURL:   "https://git.example.com",
			CORS: CORSConfig{
				AllowedHeaders: []string{"Accept", "X-Custom"},
				AllowedOrigins: []string{"https://git.example.com", "https://other.example.com"},
				AllowedMethods: []string{"GET", "PATCH"},
			},
		},
		Stats: StatsConfig{
			Enabled:    false,
			ListenAddr: "localhost:9999",
		},
		Log: LogConfig{
			Format:     "json",
			TimeFormat: "15:04",
		},
		DB: DBConfig{
			Driver:          "postgres",
			DataSource:      "postgres://user@localhost/soft?sslmode=disable",
			MaxOpenConns:    16,
			MaxIdleConns:    17,
			ConnMaxLifetime: 18,
			ConnMaxIdleTime: 19,
		},
		LFS: LFSConfig{
			Enabled:    false,
			SSHEnabled: true,
		},
		Jobs: JobsConfig{
			MirrorPull: "@every 1h",
		},
		Webhook: WebhookConfig{
			AllowedHosts: []string{"hooks.example.com:8443"},
		},
		InitialAdminKeys: []string{
			"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8",
		},
		AnonAccess:      &readWrite,
		AllowKeyless:    &allowKeyless,
		AdminOnlyCreate: true,
		MaxReposPerUser: 20,
		DefaultPrivate:  true,
		DefaultRepo:     "home",
		DataPath:        dp,
	}
}

// leafFields calls fn with the Go path and value of every exported field of
// v that is not a nested config struct.
func leafFields(v reflect.Value, prefix string, fn func(path string, f reflect.StructField, v reflect.Value)) {
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		path := prefix + f.Name
		if f.Type.Kind() == reflect.Struct {
			leafFields(v.Field(i), path+".", fn)
			continue
		}
		fn(path, f, v.Field(i))
	}
}

// clearSoftServeEnv unsets every SOFT_SERVE_* variable for the test.
func clearSoftServeEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "SOFT_SERVE_") {
			t.Setenv(k, "") // restores the old value after the test
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Hooks and other subprocesses get the server's config only through
// Environ, which a child reads back with ParseEnv. Every field must survive
// that trip, so a field added to Config but not to Environ fails here.
func TestEnvironRoundTrip(t *testing.T) {
	clearSoftServeEnv(t)

	want := roundTripConfig(t)
	if err := want.Validate(); err != nil {
		t.Fatalf("validate the test config: %v", err)
	}

	// Every field must differ from the default, or a field missing from
	// Environ would come back as the default and pass unnoticed.
	def := reflect.ValueOf(DefaultConfig()).Elem()
	leafFields(reflect.ValueOf(want).Elem(), "", func(path string, _ reflect.StructField, v reflect.Value) {
		if _, ok := environExcluded[path]; ok {
			return
		}
		dv := def.FieldByIndex(fieldIndex(t, def.Type(), path))
		if reflect.DeepEqual(v.Interface(), dv.Interface()) {
			t.Errorf("%s: the test config leaves it at the default; set it in roundTripConfig", path)
		}
	})

	for _, kv := range want.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("Environ entry %q has no =", kv)
		}
		t.Setenv(k, v)
	}

	got := DefaultConfig()
	if err := got.ParseEnv(); err != nil {
		t.Fatalf("ParseEnv(Environ()): %v", err)
	}

	gv := reflect.ValueOf(got).Elem()
	leafFields(reflect.ValueOf(want).Elem(), "", func(path string, f reflect.StructField, wv reflect.Value) {
		if _, ok := environExcluded[path]; ok {
			return
		}
		if f.Tag.Get("env") == "" {
			t.Errorf("%s: no env tag, so it cannot be passed to hooks", path)
			return
		}
		gotv := gv.FieldByIndex(fieldIndex(t, gv.Type(), path))
		if !reflect.DeepEqual(wv.Interface(), gotv.Interface()) {
			t.Errorf("%s: ParseEnv(Environ()) = %#v, want %#v", path, deref(gotv), deref(wv))
		}
	})
}

// fieldIndex returns the index sequence of the field at the dotted Go path.
func fieldIndex(t *testing.T, typ reflect.Type, path string) []int {
	t.Helper()
	var idx []int
	for _, name := range strings.Split(path, ".") {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Fatalf("no field %s", path)
		}
		idx = append(idx, f.Index...)
		typ = f.Type
	}
	return idx
}

func deref(v reflect.Value) any {
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		return v.Elem().Interface()
	}
	return v.Interface()
}
