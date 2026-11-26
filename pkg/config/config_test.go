package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"go.yaml.in/yaml/v4"
)

func TestParseMultipleKeys(t *testing.T) {
	td := t.TempDir()
	if err := os.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "testdata/k1.pub\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8 a@b"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("SOFT_SERVE_DATA_PATH", td); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_INITIAL_ADMIN_KEYS"); err != nil {
			t.Fatal(err)
		}
		if err := os.Unsetenv("SOFT_SERVE_DATA_PATH"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.InitialAdminKeys, ([]string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8",
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.InitialAdminKeys = %v, want %v", got, want)
	}
}

func TestMergeInitAdminKeys(t *testing.T) {
	if err := os.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "testdata/k1.pub"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_INITIAL_ADMIN_KEYS"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := &Config{
		DB:               DBConfig{Driver: "sqlite", DataSource: ":memory:"},
		DataPath:         t.TempDir(),
		InitialAdminKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8 a@b"},
	}
	if err := writeTestConfig(t, cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.load(); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.InitialAdminKeys, ([]string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8",
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.InitialAdminKeys = %v, want %v", got, want)
	}
}

func TestValidateInitAdminKeys(t *testing.T) {
	cfg := &Config{
		DB:       DBConfig{Driver: "sqlite", DataSource: ":memory:"},
		DataPath: t.TempDir(),
		InitialAdminKeys: []string{
			"testdata/k1.pub",
			"not a key",
			"",
		},
	}
	if err := writeTestConfig(t, cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.load(); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.InitialAdminKeys, ([]string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.InitialAdminKeys = %v, want %v", got, want)
	}
	// "not a key" was dropped as neither a key nor a path; the blank entry
	// is not counted.
	if got := cfg.IgnoredAdminKeys(); got != 1 {
		t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 1)
	}
	if got, want := cfg.UnreadableAdminKeyFiles(), []string(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.UnreadableAdminKeyFiles() = %v, want %v", got, want)
	}
}

// TestAdminKeysReturnsCopy checks that changing the slice AdminKeys returns,
// as the admin key sync does when it sorts the keys, leaves the config's
// keys alone: concurrent boots share one config.
func TestAdminKeysReturnsCopy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.InitialAdminKeys = []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(cfg.AdminKeys())
	if len(want) != 2 {
		t.Fatalf("admin keys = %d, want 2", len(want))
	}

	got := cfg.AdminKeys()
	got[0], got[1] = got[1], nil
	if !slices.EqualFunc(cfg.AdminKeys(), want, proto.KeysEqual) {
		t.Error("changing the returned slice changed the config's admin keys")
	}
}

// TestAllowedHostsReturnCopies checks that changing the allowlists
// ParsedClientAllowedHosts and ParsedAllowedHosts return leaves the
// config's own alone, as AdminKeys does.
func TestAllowedHostsReturnCopies(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.ClientAllowedHosts = []string{"git.internal.example", "10.0.0.0/8:2222"}
	cfg.Webhook.AllowedHosts = []string{"hooks.internal", "10.0.0.0/8:8080"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		get  func() ssrf.AllowedHosts
	}{
		{"ssh", cfg.SSH.ParsedClientAllowedHosts},
		{"webhook", cfg.Webhook.ParsedAllowedHosts},
	} {
		want := slices.Clone(tc.get())
		if len(want) != 2 {
			t.Fatalf("%s allowed hosts = %d, want 2", tc.name, len(want))
		}
		got := tc.get()
		got[0], got[1] = got[1], got[0]
		if !reflect.DeepEqual(tc.get(), want) {
			t.Errorf("changing the returned %s allowlist changed the config's", tc.name)
		}
	}
}

func TestUnreadableAdminKeyFiles(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-mounted-yet.pub")
	notAKey := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(notAKey, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		DB:       DBConfig{Driver: "sqlite", DataSource: ":memory:"},
		DataPath: t.TempDir(),
		InitialAdminKeys: []string{
			"testdata/k1.pub",
			"  ",
			"# ops keys",
			missing,
			notAKey,
			"keys/admin",
			// A file name without a directory or an extension is
			// still a path.
			"admin_key",
			"ssh-ed25519 AAAAnot-base64",
			"not a key",
		},
	}
	want := []string{"admin_key", "keys/admin", missing, notAKey}
	slices.Sort(want)
	// Before Validate nothing is parsed, so nothing is known.
	if got, want := cfg.UnreadableAdminKeyFiles(), []string(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.UnreadableAdminKeyFiles() = %v, want %v", got, want)
	}
	if got := cfg.IgnoredAdminKeys(); got != 0 {
		t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 0)
	}
	if got := len(cfg.AdminKeys()); got != 0 {
		t.Fatalf("len(cfg.AdminKeys()) = %v, want %v", got, 0)
	}

	// Validate drops them from the list but remembers them.
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.InitialAdminKeys); got != 1 {
		t.Fatalf("len(cfg.InitialAdminKeys) = %v, want %v", got, 1)
	}
	if got, want := cfg.UnreadableAdminKeyFiles(), want; !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.UnreadableAdminKeyFiles() = %v, want %v", got, want)
	}
	if got := cfg.IgnoredAdminKeys(); got != 2 {
		t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 2)
	}
	if got := len(cfg.AdminKeys()); got != 1 {
		t.Fatalf("len(cfg.AdminKeys()) = %v, want %v", got, 1)
	}

	// A config whose entries all parse has none.
	ok := &Config{DB: DBConfig{Driver: "sqlite", DataSource: ":memory:"}, InitialAdminKeys: []string{"testdata/k1.pub", "", "# comment"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := len(ok.AdminKeys()); got != 1 {
		t.Fatalf("len(ok.AdminKeys()) = %v, want %v", got, 1)
	}
	if got, want := ok.UnreadableAdminKeyFiles(), []string(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("ok.UnreadableAdminKeyFiles() = %v, want %v", got, want)
	}
	if got := ok.IgnoredAdminKeys(); got != 0 {
		t.Fatalf("ok.IgnoredAdminKeys() = %v, want %v", got, 0)
	}
}

// The config file and the environment may list the same entries. They are
// recorded once.
func TestUnreadableAdminKeyFilesNotDuplicated(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-mounted-yet.pub")
	t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", missing+"\nnot a key")
	cfg := &Config{
		DB:               DBConfig{Driver: "sqlite", DataSource: ":memory:"},
		DataPath:         t.TempDir(),
		InitialAdminKeys: []string{missing, "not a key"},
	}
	if err := writeTestConfig(t, cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.load(); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.UnreadableAdminKeyFiles(), ([]string{missing}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.UnreadableAdminKeyFiles() = %v, want %v", got, want)
	}
	if got := cfg.IgnoredAdminKeys(); got != 1 {
		t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 1)
	}
}

func TestCustomConfigLocation(t *testing.T) {
	td := t.TempDir()
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_CONFIG_LOCATION"); err != nil {
			t.Fatal(err)
		}
	})

	// Test that we get data from the custom file location, and not from the data dir.
	if err := os.Setenv("SOFT_SERVE_CONFIG_LOCATION", "testdata/config.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("SOFT_SERVE_DATA_PATH", td); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if err := cfg.load(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Name; got != "Test server name" {
		t.Fatalf("cfg.Name = %v, want %v", got, "Test server name")
	}
	// If we unset the custom location, then use the default location.
	if err := os.Unsetenv("SOFT_SERVE_CONFIG_LOCATION"); err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	if got := cfg.Name; got != "Soft Serve Plus" {
		t.Fatalf("cfg.Name = %v, want %v", got, "Soft Serve Plus")
	}
	// Test that if the custom config location doesn't exist, default to datapath config.
	if err := os.Setenv("SOFT_SERVE_CONFIG_LOCATION", "testdata/config_nonexistent.yaml"); err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	if got := cfg.Name; got != "Soft Serve Plus" {
		t.Fatalf("cfg.Name = %v, want %v", got, "Soft Serve Plus")
	}
}

func TestLoadChecksConfigLocation(t *testing.T) {
	td := t.TempDir()
	t.Setenv("SOFT_SERVE_DATA_PATH", td)
	existing := filepath.Join(td, "custom.yaml")
	if err := os.WriteFile(existing, []byte("name: custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		location string
		wantErr  bool
	}{
		"unset":                    {location: ""},
		"existing file":            {location: existing},
		"missing file":             {location: filepath.Join(td, "missing.yaml"), wantErr: true},
		"missing default location": {location: filepath.Join(td, "config.yaml")},
		"missing directory":        {location: filepath.Join(td, "nodir", "config.yaml"), wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SOFT_SERVE_CONFIG_LOCATION", tc.location)
			err := DefaultConfig().load()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "SOFT_SERVE_CONFIG_LOCATION") {
					t.Fatalf("load() = %v, want an error naming the variable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("load() = %v, want nil", err)
			}
		})
	}
}

func TestDefaultRepoDisabledByDefault(t *testing.T) {
	if got := DefaultConfig().DefaultRepo; got != "" {
		t.Fatalf("DefaultConfig().DefaultRepo = %v, want %v", got, "")
	}
}

func TestParseDefaultRepo(t *testing.T) {
	if err := os.Setenv("SOFT_SERVE_DEFAULT_REPO", "gitops"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_DEFAULT_REPO"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DefaultRepo; got != "gitops" {
		t.Fatalf("cfg.DefaultRepo = %v, want %v", got, "gitops")
	}
}

func TestParseDefaultRepoFromFile(t *testing.T) {
	cfg := &Config{DataPath: t.TempDir(), DB: DBConfig{Driver: "sqlite", DataSource: ":memory:"}}
	if err := parseFile(cfg, "testdata/config_default_repo.yaml"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DefaultRepo; got != "gitops" {
		t.Fatalf("cfg.DefaultRepo = %v, want %v", got, "gitops")
	}
}

func TestParseEmptyFile(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"empty":        "",
		"whitespace":   "\n  \n",
		"comment only": "# soft-serve config\n# name: \"Soft Serve\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			cfg.DataPath = t.TempDir()
			want := cfg.Name
			if err := parseFile(cfg, path); err != nil {
				t.Fatal(err)
			}
			if got := cfg.Name; got != want {
				t.Fatalf("cfg.Name = %v, want %v", got, want)
			}
		})
	}
}

func TestParseMultipleHeaders(t *testing.T) {
	if err := os.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_HEADERS", "Accept,Accept-Language,User-Agent"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_HEADERS"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.HTTP.CORS.AllowedHeaders, ([]string{
		"Accept",
		"Accept-Language",
		"User-Agent",
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.HTTP.CORS.AllowedHeaders = %v, want %v", got, want)
	}
}

func TestParseMultipleOrigins(t *testing.T) {
	if err := os.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS", "http://example.com,https://example.com"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.HTTP.CORS.AllowedOrigins, ([]string{
		"http://example.com",
		"https://example.com",
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.HTTP.CORS.AllowedOrigins = %v, want %v", got, want)
	}
	if got, want := cfg.CORSAllowedOrigins(), ([]string{
		"http://localhost:23232",
		"http://example.com",
		"https://example.com",
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.CORSAllowedOrigins() = %v, want %v", got, want)
	}
}

func TestValidateDoesNotDuplicatePublicURLOrigin(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	if err := writeTestConfig(t, cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.load(); err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.HTTP.CORS.AllowedOrigins); got != 0 {
		t.Fatalf("len(cfg.HTTP.CORS.AllowedOrigins) = %v, want %v", got, 0)
	}
	if got, want := cfg.CORSAllowedOrigins(), ([]string{"http://localhost:23232"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.CORSAllowedOrigins() = %v, want %v", got, want)
	}
	if !slices.Contains(cfg.Environ(), "SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS=") {
		t.Fatal("not true: slices.Contains(cfg.Environ(), \"SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS=\")")
	}
}

// The public URL is allowed as an origin wherever it was set last: an
// environment override of it does not leave the config file's URL, or the
// default one, allowed too. An empty public URL adds no empty origin; the
// web server then rejects every origin (see TestCORSHandlerOrigins).
func TestCORSAllowedOriginsFollowPublicURL(t *testing.T) {
	t.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS", "")
	if err := os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS"); err != nil {
		t.Fatal(err)
	}
	file := DefaultConfig()
	file.DataPath = t.TempDir()
	file.HTTP.PublicURL = "https://file.example.com"
	path := filepath.Join(file.DataPath, "config.yaml")
	if err := writeConfig(file, path); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SOFT_SERVE_HTTP_PUBLIC_URL", "https://env.example.com//")
	cfg := DefaultConfig()
	if err := parseFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.HTTP.PublicURL; got != "https://env.example.com" {
		t.Fatalf("cfg.HTTP.PublicURL = %v, want %v", got, "https://env.example.com")
	}
	if got, want := cfg.CORSAllowedOrigins(), ([]string{"https://env.example.com"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.CORSAllowedOrigins() = %v, want %v", got, want)
	}

	cfg.HTTP.PublicURL = ""
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.CORSAllowedOrigins()); got != 0 {
		t.Fatalf("len(cfg.CORSAllowedOrigins()) = %v, want %v", got, 0)
	}
}

// A trailing slash on any public URL is dropped, so URLs built from it, such
// as the clone URLs in webhook payloads, have no "//".
func TestValidateTrimsPublicURLs(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.PublicURL = "ssh://soft.example:23231/"
	cfg.Git.PublicURL = "git://soft.example/"
	cfg.HTTP.PublicURL = "https://soft.example/"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.SSH.PublicURL; got != "ssh://soft.example:23231" {
		t.Fatalf("cfg.SSH.PublicURL = %v, want %v", got, "ssh://soft.example:23231")
	}
	if got := cfg.Git.PublicURL; got != "git://soft.example" {
		t.Fatalf("cfg.Git.PublicURL = %v, want %v", got, "git://soft.example")
	}
	if got := cfg.HTTP.PublicURL; got != "https://soft.example" {
		t.Fatalf("cfg.HTTP.PublicURL = %v, want %v", got, "https://soft.example")
	}
}

func TestParseMultipleMethods(t *testing.T) {
	if err := os.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_METHODS", "GET,POST,PUT"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_METHODS"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.HTTP.CORS.AllowedMethods, ([]string{
		"GET",
		"POST",
		"PUT",
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.HTTP.CORS.AllowedMethods = %v, want %v", got, want)
	}
}

func TestAdminOnlyCreateOffByDefault(t *testing.T) {
	// Ignore any value exported in the environment running the tests;
	// t.Setenv restores it afterwards.
	t.Setenv("SOFT_SERVE_ADMIN_ONLY_CREATE", "")
	if err := os.Unsetenv("SOFT_SERVE_ADMIN_ONLY_CREATE"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AdminOnlyCreate {
		t.Fatal("not true: !cfg.AdminOnlyCreate")
	}
}

func TestParseAdminOnlyCreateEnv(t *testing.T) {
	if err := os.Setenv("SOFT_SERVE_ADMIN_ONLY_CREATE", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_ADMIN_ONLY_CREATE"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.AdminOnlyCreate {
		t.Fatal("not true: cfg.AdminOnlyCreate")
	}
}

func TestAdminOnlyCreateInEnviron(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AdminOnlyCreate = true
	if !slices.Contains(cfg.Environ(), "SOFT_SERVE_ADMIN_ONLY_CREATE=true") {
		t.Fatal("not true: slices.Contains(cfg.Environ(), \"SOFT_SERVE_ADMIN_ONLY_CREATE=true\")")
	}
}

// The hook subprocess re-reads its config from the environment, and git
// inherits the server's own environment, so the flag is always emitted
// explicitly. An off flag must read as "=false", never as a value inherited
// from elsewhere.
func TestAdminOnlyCreateFalseInEnviron(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AdminOnlyCreate = false
	envs := cfg.Environ()
	if !slices.Contains(envs, "SOFT_SERVE_ADMIN_ONLY_CREATE=false") {
		t.Fatal("not true: slices.Contains(envs, \"SOFT_SERVE_ADMIN_ONLY_CREATE=false\")")
	}
	if slices.Contains(envs, "SOFT_SERVE_ADMIN_ONLY_CREATE=true") {
		t.Fatal("not true: !slices.Contains(envs, \"SOFT_SERVE_ADMIN_ONLY_CREATE=true\")")
	}
}

func TestAnonAccessEnvUnsetByDefault(t *testing.T) {
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	// nil is the "no override" sentinel.
	if cfg.AnonAccess != nil {
		t.Fatal("not true: cfg.AnonAccess == nil")
	}
}

func TestParseAnonAccessEnv(t *testing.T) {
	if err := os.Setenv("SOFT_SERVE_ANON_ACCESS", access.AdminAccess.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_ANON_ACCESS"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AnonAccess == nil {
		t.Fatal("not true: cfg.AnonAccess != nil")
	}
	if got := *cfg.AnonAccess; got != access.AdminAccess {
		t.Fatalf("*cfg.AnonAccess = %v, want %v", got, access.AdminAccess)
	}
}

// An invalid anon-access level is rejected at parse time by access.Level's
// TextUnmarshaler, so it never reaches Validate as a bad value.
func TestParseRejectsInvalidAnonAccess(t *testing.T) {
	if err := os.Setenv("SOFT_SERVE_ANON_ACCESS", "not-a-real-access-level"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_ANON_ACCESS"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := DefaultConfig()
	err := parseEnv(cfg)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

func TestAllowKeylessEnvUnsetByDefault(t *testing.T) {
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	// nil is the "no override" sentinel — distinct from an explicit false.
	if cfg.AllowKeyless != nil {
		t.Fatal("not true: cfg.AllowKeyless == nil")
	}
}

func TestParseAllowKeylessEnvTrue(t *testing.T) {
	if err := os.Setenv("SOFT_SERVE_ALLOW_KEYLESS", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_ALLOW_KEYLESS"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AllowKeyless == nil {
		t.Fatal("not true: cfg.AllowKeyless != nil")
	}
	if got := *cfg.AllowKeyless; got != true {
		t.Fatalf("*cfg.AllowKeyless = %v, want %v", got, true)
	}
}

func TestParseAllowKeylessEnvFalse(t *testing.T) {
	if err := os.Setenv("SOFT_SERVE_ALLOW_KEYLESS", "false"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv("SOFT_SERVE_ALLOW_KEYLESS"); err != nil {
			t.Fatal(err)
		}
	})
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	// Explicit false must survive as a real override, not collapse back to
	// "unset" — that's the whole reason this field is a *bool.
	if cfg.AllowKeyless == nil {
		t.Fatal("not true: cfg.AllowKeyless != nil")
	}
	if got := *cfg.AllowKeyless; got != false {
		t.Fatalf("*cfg.AllowKeyless = %v, want %v", got, false)
	}
}

func TestValidateClientStrictHostKeyChecking(t *testing.T) {
	for _, tt := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", StrictHostKeyCheckingAcceptNew, false},
		{"accept-new", StrictHostKeyCheckingAcceptNew, false},
		{"yes", StrictHostKeyCheckingYes, false},
		// "no" is exactly the setting being removed; "ask" cannot work
		// without a terminal.
		{"no", "", true},
		{"ask", "", true},
		{"off", "", true},
	} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.SSH.ClientStrictHostKeyChecking = tt.in
		err := cfg.Validate()
		if tt.wantErr {
			if err == nil {
				t.Errorf("Validate() with %q: expected error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("Validate() with %q: %v", tt.in, err)
		}
		if cfg.SSH.ClientStrictHostKeyChecking != tt.want {
			t.Errorf("Validate() with %q = %q, want %q", tt.in, cfg.SSH.ClientStrictHostKeyChecking, tt.want)
		}
	}
}

func TestValidateRejectsUnquotableDataPath(t *testing.T) {
	for _, dp := range []string{`/data/"quoted"`, "/data/line\nbreak", "/data/cr\r"} {
		cfg := DefaultConfig()
		cfg.DataPath = dp
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() accepted data path %q", dp)
		}
	}

	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir() + "/Application Support/soft serve"
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() rejected a data path with spaces: %v", err)
	}
}

func TestValidateParsesAllowedHosts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	if len(cfg.SSH.ParsedClientAllowedHosts()) != 0 {
		t.Fatal("allowlist populated before Validate")
	}

	cfg.SSH.ClientAllowedHosts = []string{"git.internal.example", "", "10.0.0.0/8:2222", " "}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.SSH.ClientAllowedHosts; len(got) != 2 {
		t.Errorf("blank entries kept: %q", got)
	}
	if got := cfg.SSH.ParsedClientAllowedHosts(); len(got) != 2 {
		t.Errorf("parsed %d entries, want 2", len(got))
	}
	policy := ssrf.GitRemotePolicy{AllowedSSHHosts: cfg.SSH.ParsedClientAllowedHosts()}
	if _, err := ssrf.ValidateGitRemote(t.Context(), "ssh://git@git.internal.example/x.git", policy); err != nil {
		t.Errorf("parsed allowlist does not match its own entry: %v", err)
	}

	cfg.SSH.ClientAllowedHosts = []string{"10.0.0.0/99"}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() accepted an invalid network")
	}
}

func TestMergeAdminKeys(t *testing.T) {
	const k1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH"
	const k2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8 a@b"
	for name, tc := range map[string]struct {
		env, file, want []string
	}{
		"env replaced the list": {[]string{k1}, []string{k2}, []string{k1, k2}},
		"env unset":             {[]string{k2}, []string{k2}, []string{k2}},
		"env repeats file keys": {[]string{k1, k2, " " + k2}, []string{k2}, []string{k1, k2}},
		"no keys":               {nil, nil, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			got := mergeAdminKeys(tc.env, tc.file)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("mergeAdminKeys(%q, %q) = %q, want %q", tc.env, tc.file, got, tc.want)
			}
		})
	}
}

// An authorized_keys file can hold several keys, one per line. Every key is
// an admin key, and a line that is not a key is counted as ignored.
func TestAdminKeyFileWithSeveralKeys(t *testing.T) {
	k1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH"
	k2 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8"
	file := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(file, []byte("# ops\n"+k1+" a@b\r\n\nssh-ed25519 AAAAtypo\n"+k2+" c@d\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		DB:               DBConfig{Driver: "sqlite", DataSource: ":memory:"},
		DataPath:         t.TempDir(),
		InitialAdminKeys: []string{file},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.AdminKeys()); got != 2 {
		t.Fatalf("len(cfg.AdminKeys()) = %v, want %v", got, 2)
	}
	if got, want := cfg.InitialAdminKeys, ([]string{k1, k2}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.InitialAdminKeys = %v, want %v", got, want)
	}
	if got := cfg.IgnoredAdminKeys(); got != 1 {
		t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 1)
	}
	if got, want := cfg.UnreadableAdminKeyFiles(), []string(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.UnreadableAdminKeyFiles() = %v, want %v", got, want)
	}
}

// The admin keys are parsed once, by Validate. A file that appears in the
// data directory afterwards, named like a listed key, does not replace that
// key with the keys it holds, and the answers do not change.
func TestAdminKeysParsedOnlyByValidate(t *testing.T) {
	k1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH"
	k2 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8"
	dp := t.TempDir()
	cfg := &Config{
		DB:               DBConfig{Driver: "sqlite", DataSource: ":memory:"},
		DataPath:         dp,
		InitialAdminKeys: []string{k1},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	keys := cfg.AdminKeys()
	if got := len(keys); got != 1 {
		t.Fatalf("len(keys) = %v, want %v", got, 1)
	}
	if got := strings.TrimSpace(proto.MarshalAuthorizedKey(keys[0])); got != k1 {
		t.Fatalf("strings.TrimSpace(proto.MarshalAuthorizedKey(keys[0])) = %v, want %v", got, k1)
	}

	// k1 has no "/", so its text is a file name in the data directory.
	if err := os.WriteFile(filepath.Join(dp, k1), []byte(k2+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	keys = cfg.AdminKeys()
	if got := len(keys); got != 1 {
		t.Fatalf("len(keys) = %v, want %v", got, 1)
	}
	if got := strings.TrimSpace(proto.MarshalAuthorizedKey(keys[0])); got != k1 {
		t.Fatalf("strings.TrimSpace(proto.MarshalAuthorizedKey(keys[0])) = %v, want %v", got, k1)
	}
	if got, want := cfg.UnreadableAdminKeyFiles(), []string(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.UnreadableAdminKeyFiles() = %v, want %v", got, want)
	}
	if got := cfg.IgnoredAdminKeys(); got != 0 {
		t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 0)
	}
}

// A key listed inline and again in a key file, there twice with different
// comments and spacing, is one admin key.
func TestAdminKeyFileRepeatsKey(t *testing.T) {
	k1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH"
	file := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(file, []byte(k1+" a@b\n"+k1+"  c@d\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		DB:               DBConfig{Driver: "sqlite", DataSource: ":memory:"},
		DataPath:         t.TempDir(),
		InitialAdminKeys: []string{k1 + " x@y", file},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.AdminKeys()); got != 1 {
		t.Fatalf("len(cfg.AdminKeys()) = %v, want %v", got, 1)
	}
	if got := cfg.IgnoredAdminKeys(); got != 0 {
		t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 0)
	}
	if got := len(cfg.UnreadableAdminKeyFiles()); got != 0 {
		t.Fatalf("len(cfg.UnreadableAdminKeyFiles()) = %v, want %v", got, 0)
	}
}

func TestMaxReposPerUser(t *testing.T) {
	// Ignore any value exported in the environment running the tests.
	t.Setenv("SOFT_SERVE_MAX_REPOS_PER_USER", "")
	if err := os.Unsetenv("SOFT_SERVE_MAX_REPOS_PER_USER"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.MaxReposPerUser; got != 0 {
		t.Fatalf("cfg.MaxReposPerUser = %v, want %v", got, 0)
	} // no limit by default

	t.Setenv("SOFT_SERVE_MAX_REPOS_PER_USER", "3")
	cfg = DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.MaxReposPerUser; got != 3 {
		t.Fatalf("cfg.MaxReposPerUser = %v, want %v", got, 3)
	}
	// Hooks re-read the config from the environment.
	if !slices.Contains(cfg.Environ(), "SOFT_SERVE_MAX_REPOS_PER_USER=3") {
		t.Fatal("not true: slices.Contains(cfg.Environ(), \"SOFT_SERVE_MAX_REPOS_PER_USER=3\")")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("max_repos_per_user: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	if err := parseFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	if got := cfg.MaxReposPerUser; got != 5 {
		t.Fatalf("cfg.MaxReposPerUser = %v, want %v", got, 5)
	}

	t.Setenv("SOFT_SERVE_MAX_REPOS_PER_USER", "-1")
	cfg = DefaultConfig()
	if parseEnv(cfg) == nil {
		t.Fatal("not true: parseEnv(cfg) != nil")
	}
}

func TestDefaultPrivate(t *testing.T) {
	// Ignore any value exported in the environment running the tests.
	t.Setenv("SOFT_SERVE_DEFAULT_PRIVATE", "")
	if err := os.Unsetenv("SOFT_SERVE_DEFAULT_PRIVATE"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultPrivate {
		t.Fatal("not true: !cfg.DefaultPrivate")
	} // public by default, as upstream
	if !slices.Contains(cfg.Environ(), "SOFT_SERVE_DEFAULT_PRIVATE=false") {
		t.Fatal("not true: slices.Contains(cfg.Environ(), \"SOFT_SERVE_DEFAULT_PRIVATE=false\")")
	}

	t.Setenv("SOFT_SERVE_DEFAULT_PRIVATE", "true")
	cfg = DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.DefaultPrivate {
		t.Fatal("not true: cfg.DefaultPrivate")
	}
	// Hooks re-read the config from the environment.
	if !slices.Contains(cfg.Environ(), "SOFT_SERVE_DEFAULT_PRIVATE=true") {
		t.Fatal("not true: slices.Contains(cfg.Environ(), \"SOFT_SERVE_DEFAULT_PRIVATE=true\")")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_private: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	if err := parseFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	if !cfg.DefaultPrivate {
		t.Fatal("not true: cfg.DefaultPrivate")
	}
}

// Docker Compose's list syntax keeps quotes in the value, as in
// "- SOFT_SERVE_INITIAL_ADMIN_KEYS='ssh-ed25519 AAAA...'". A key or key file
// path wrapped in one pair of matching quotes is read without them.
func TestQuotedInitAdminKeys(t *testing.T) {
	k1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH"
	k2 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8"
	for _, tc := range []struct {
		name string
		env  string
		want []string
	}{
		{"single quotes", "'" + k2 + " a@b'", []string{k2}},
		{"double quotes", `"` + k2 + `"`, []string{k2}},
		{"quoted key file", `"testdata/k1.pub"`, []string{k1}},
		{"several quoted keys", "'testdata/k1.pub'\n \"" + k2 + "\" ", []string{k1, k2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", tc.env)
			t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
			cfg := DefaultConfig()
			if err := parseEnv(cfg); err != nil {
				t.Fatal(err)
			}
			if got, want := cfg.InitialAdminKeys, tc.want; !reflect.DeepEqual(got, want) {
				t.Fatalf("cfg.InitialAdminKeys = %v, want %v", got, want)
			}
			if got := cfg.IgnoredAdminKeys(); got != 0 {
				t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 0)
			}
		})
	}

	// Unmatched or inner quotes are not stripped.
	cfg := &Config{DB: DBConfig{Driver: "sqlite", DataSource: ":memory:"}, InitialAdminKeys: []string{"'" + k2 + `"`, `"` + k2}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.AdminKeys()); got != 0 {
		t.Fatalf("len(cfg.AdminKeys()) = %v, want %v", got, 0)
	}
	if got := cfg.IgnoredAdminKeys(); got != 2 {
		t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 2)
	}
}

// SOFT_SERVE_INITIAL_ADMIN_KEYS is split on newlines before each entry's
// quotes are trimmed, so a value with several keys in one outer pair of
// quotes must not lose its first and last keys.
func TestOuterQuotedInitAdminKeys(t *testing.T) {
	k1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH"
	k2 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8"
	for _, tc := range []struct {
		name string
		env  string
		want []string
	}{
		{"single quotes", "'" + k1 + "\n" + k2 + " a@b'", []string{k1, k2}},
		{"double quotes", `"` + k1 + "\n" + k2 + `"`, []string{k1, k2}},
		{"key file and blank lines", "\n 'testdata/k1.pub\n\n" + k2 + "' \n", []string{k1, k2}},
		{"quotes on their own lines", "'\n" + k1 + "\n" + k2 + "\n'", []string{k1, k2}},
		{"inner quoted entry", "'" + k1 + "\n\"" + k2 + "\"'", []string{k1, k2}},
		{"each entry quoted", "'" + k1 + "'\n'" + k2 + "'", []string{k1, k2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", tc.env)
			t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
			cfg := DefaultConfig()
			if err := parseEnv(cfg); err != nil {
				t.Fatal(err)
			}
			if got, want := cfg.InitialAdminKeys, tc.want; !reflect.DeepEqual(got, want) {
				t.Fatalf("cfg.InitialAdminKeys = %v, want %v", got, want)
			}
			if got := cfg.IgnoredAdminKeys(); got != 0 {
				t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 0)
			}
		})
	}

	// The outer quotes are stripped before the config file's keys are
	// merged in.
	t.Run("with config file keys", func(t *testing.T) {
		t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "'"+k1+"\n"+k2+"'")
		t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
		cfg := DefaultConfig()
		cfg.InitialAdminKeys = []string{k2}
		if err := parseEnv(cfg); err != nil {
			t.Fatal(err)
		}
		if got, want := cfg.InitialAdminKeys, ([]string{k1, k2}); !reflect.DeepEqual(got, want) {
			t.Fatalf("cfg.InitialAdminKeys = %v, want %v", got, want)
		}
		if got := cfg.IgnoredAdminKeys(); got != 0 {
			t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 0)
		}
	})

	// A variable that is set but blank, as in the Docker image, leaves the
	// config file's keys in place, and their quotes are not stripped as if
	// they were the environment's.
	t.Run("blank variable with config file keys", func(t *testing.T) {
		t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "")
		t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
		cfg := DefaultConfig()
		cfg.InitialAdminKeys = []string{"'" + k1, k2 + "'"}
		if err := parseEnv(cfg); err != nil {
			t.Fatal(err)
		}
		if got := len(cfg.InitialAdminKeys); got != 0 {
			t.Fatalf("len(cfg.InitialAdminKeys) = %v, want %v", got, 0)
		}
		if got := cfg.IgnoredAdminKeys(); got != 2 {
			t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 2)
		}
	})

	// Mismatched outer quotes are not stripped.
	t.Run("mismatched quotes", func(t *testing.T) {
		t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "'"+k1+"\n"+k2+`"`)
		t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
		cfg := DefaultConfig()
		if err := parseEnv(cfg); err != nil {
			t.Fatal(err)
		}
		if got := len(cfg.InitialAdminKeys); got != 0 {
			t.Fatalf("len(cfg.InitialAdminKeys) = %v, want %v", got, 0)
		}
		if got := cfg.IgnoredAdminKeys(); got != 2 {
			t.Fatalf("cfg.IgnoredAdminKeys() = %v, want %v", got, 2)
		}
	})
}

func TestWebhookAllowedHosts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	if len(cfg.Webhook.ParsedAllowedHosts()) != 0 {
		t.Fatal("webhook allowlist populated by default")
	}

	cfg.Webhook.AllowedHosts = []string{"hooks.internal", "", "10.0.0.0/8:8080", " "}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Webhook.AllowedHosts; len(got) != 2 {
		t.Errorf("blank entries kept: %q", got)
	}
	if got := cfg.Webhook.ParsedAllowedHosts(); len(got) != 2 {
		t.Errorf("parsed %d entries, want 2", len(got))
	}
	if !slices.Contains(cfg.Environ(), "SOFT_SERVE_WEBHOOK_ALLOWED_HOSTS=hooks.internal,10.0.0.0/8:8080") {
		t.Error("Environ does not pass the webhook allowlist on")
	}

	cfg.Webhook.AllowedHosts = []string{"10.0.0.0/99"}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() accepted an invalid network")
	}
}

func TestWebhookAllowedHostsFromEnvAndFile(t *testing.T) {
	t.Setenv("SOFT_SERVE_WEBHOOK_ALLOWED_HOSTS", "127.0.0.1,hooks.internal:9000")
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Webhook.AllowedHosts, []string{"127.0.0.1", "hooks.internal:9000"}) {
		t.Fatalf("AllowedHosts = %q", cfg.Webhook.AllowedHosts)
	}
	if len(cfg.Webhook.ParsedAllowedHosts()) != 2 {
		t.Fatal("env allowlist not parsed")
	}

	// The written config file round-trips the list.
	if err := writeTestConfig(t, cfg); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("SOFT_SERVE_WEBHOOK_ALLOWED_HOSTS") //nolint:errcheck // restored by t.Setenv above
	t.Setenv("SOFT_SERVE_CONFIG_LOCATION", cfg.FilePath())
	fromFile := &Config{DataPath: cfg.DataPath}
	if err := fromFile.load(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fromFile.Webhook.AllowedHosts, []string{"127.0.0.1", "hooks.internal:9000"}) {
		t.Fatalf("AllowedHosts from file = %q", fromFile.Webhook.AllowedHosts)
	}
}

func TestGitMaxConnectionsValidate(t *testing.T) {
	for _, n := range []int{0, -1} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.Git.MaxConnections = n
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "git.max_connections") {
			t.Errorf("max_connections %d: Validate() = %v, want an error naming it", n, err)
		}
		// The limit does not matter while the daemon is off.
		cfg.Git.Enabled = false
		if err := cfg.Validate(); err != nil {
			t.Errorf("max_connections %d with the daemon disabled: Validate() = %v", n, err)
		}
	}
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Git.MaxConnections = 1
	if err := cfg.Validate(); err != nil {
		t.Errorf("max_connections 1: Validate() = %v", err)
	}
}

// A misspelt key is ignored, so it is reported with its dotted path and line
// for the server to warn about.
func TestUnknownConfigKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`name: "x"
admin_only_creat: true
log_format: "text"
anon_access: read-only
initial_admin_keys:
  - "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8"
ssh: &ssh
  listen_addr: ":23231"
  listen_adr: ":1"
http:
  cors:
    allowed_origin: ["*"]
git:
  <<: *ssh
  max_connections: 3
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if err := parseFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	if cfg.AdminOnlyCreate {
		t.Fatal("not true: !cfg.AdminOnlyCreate")
	}
	if got, want := cfg.UnknownKeys(), ([]UnknownKey{
		{Key: "admin_only_creat", Line: 2},
		{Key: "log_format", Line: 3},
		{Key: "ssh.listen_adr", Line: 9},
		{Key: "http.cors.allowed_origin", Line: 12},
		// The merged ssh keys land in git, which only knows listen_addr.
		{Key: "git.listen_adr", Line: 9},
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("cfg.UnknownKeys() = %v, want %v", got, want)
	}

	// A config file without unknown keys reports none.
	if err := os.WriteFile(path, []byte("admin_only_create: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	if err := parseFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	if !cfg.AdminOnlyCreate {
		t.Fatal("not true: cfg.AdminOnlyCreate")
	}
	if got := len(cfg.UnknownKeys()); got != 0 {
		t.Fatalf("len(cfg.UnknownKeys()) = %v, want %v", got, 0)
	}
}

// The generated config file and the README's sample use only known keys.
func TestConfigSamplesHaveNoUnknownKeys(t *testing.T) {
	var doc yaml.Node
	file := string(defaultConfigFile)
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil {
		t.Fatal(err)
	}
	if got, want := findUnknownKeys(&doc, reflect.TypeFor[Config](), ""), []UnknownKey(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("findUnknownKeys(&doc, reflect.TypeFor[Config](), \"\") = %v, want %v", got, want)
	}

	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(readme), "\r\n", "\n")
	const start = "```yaml\n# Soft Serve Plus Server configurations\n"
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatal("not true: i >= 0")
	} // README has the sample config
	sample := s[i+len("```yaml\n"):]
	sample = sample[:strings.Index(sample, "\n```")]
	doc = yaml.Node{}
	if err := yaml.Unmarshal([]byte(sample), &doc); err != nil {
		t.Fatal(err)
	}
	if got, want := findUnknownKeys(&doc, reflect.TypeFor[Config](), ""), []UnknownKey(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("findUnknownKeys(&doc, reflect.TypeFor[Config](), \"\") = %v, want %v", got, want)
	}
}

// WriteDefaultFile keeps the config, which can hold a database password,
// and a data directory it creates private to the server's user.
func TestWriteDefaultFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	cfg := DefaultConfig()
	cfg.DataPath = filepath.Join(t.TempDir(), "data")
	if err := cfg.WriteDefaultFile(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		cfg.DataPath:   0o700,
		cfg.FilePath(): 0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %v, want %v", path, got, want)
		}
	}
}

// A database driver other than the two db.Open takes fails validation,
// naming the setting.
func TestValidateRejectsUnknownDBDriver(t *testing.T) {
	for _, driver := range []string{"", "sqlite3", "mysql", "Postgres"} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.DB.Driver = driver
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "db.driver") {
			t.Errorf("Validate() with driver %q => %v, want a db.driver error", driver, err)
		}
	}
	for _, driver := range []string{"sqlite", "postgres"} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.DB.Driver = driver
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() with driver %q => %v", driver, err)
		}
	}
}

func TestDBPoolLimits(t *testing.T) {
	// Ignore any value exported in the environment running the tests.
	t.Setenv("SOFT_SERVE_DB_MAX_OPEN_CONNS", "")
	if err := os.Unsetenv("SOFT_SERVE_DB_MAX_OPEN_CONNS"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DB.MaxOpenConns; got != 0 {
		t.Fatalf("cfg.DB.MaxOpenConns = %v, want %v", got, 0)
	}

	t.Setenv("SOFT_SERVE_DB_MAX_OPEN_CONNS", "20")
	cfg = DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DB.MaxOpenConns; got != 20 {
		t.Fatalf("cfg.DB.MaxOpenConns = %v, want %v", got, 20)
	}
	// Hooks re-read the config from the environment.
	if !slices.Contains(cfg.Environ(), "SOFT_SERVE_DB_MAX_OPEN_CONNS=20") {
		t.Fatal("not true: slices.Contains(cfg.Environ(), \"SOFT_SERVE_DB_MAX_OPEN_CONNS=20\")")
	}
	if err := os.Unsetenv("SOFT_SERVE_DB_MAX_OPEN_CONNS"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("db:\n  max_open_conns: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	cfg.DataPath = dir
	if err := parseFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DB.MaxOpenConns; got != 10 {
		t.Fatalf("cfg.DB.MaxOpenConns = %v, want %v", got, 10)
	}
}

func TestDBPoolLimitsRejectNegative(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.MaxOpenConns = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "db.max_open_conns") {
		t.Errorf("Validate() = %v, want an error naming db.max_open_conns", err)
	}
}

// Environ starts with the path of the running binary, which the hooks run.
func TestEnvironBinPath(t *testing.T) {
	t.Parallel()
	ex, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	if got, want := DefaultConfig().Environ()[0], "SOFT_SERVE_BIN_PATH="+filepath.ToSlash(ex); got != want {
		t.Errorf("Environ()[0] = %q, want %q", got, want)
	}
}

// environExcluded lists the config fields that Environ leaves out on
// purpose, by their Go path, with the reason.
var environExcluded = map[string]string{
	// The server points hooks at their own log file
	// (SOFT_SERVE_LOG_PATH=<data>/log/hooks.log) and appends Environ after
	// it, so emitting the server's log path would override that.
	"Log.Path": "hooks log to their own file",
	// The binary's version, which the soft command sets, not a setting.
	"Version": "not a setting",
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
		},
		Git: GitConfig{
			Enabled:        false,
			ListenAddr:     ":9419",
			PublicURL:      "git://git.example.com",
			MaxTimeout:     13,
			IdleTimeout:    14,
			MaxConnections: 15,
			// Set apart from MaxConnections so a swap of the two shows.
			MaxConnectionsPerIP: 16,
		},
		HTTP: HTTPConfig{
			Enabled:        false,
			ListenAddr:     ":8080",
			TLSKeyPath:     filepath.Join(dp, "tls.key"),
			TLSCertPath:    filepath.Join(dp, "tls.crt"),
			PublicURL:      "https://git.example.com",
			MaxTimeout:     17,
			IdleTimeout:    18,
			MaxConnections: 19,
			// Set apart from MaxConnections so a swap of the two shows.
			MaxConnectionsPerIP: 21,
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
			Driver:       "postgres",
			DataSource:   "postgres://user@localhost/soft?sslmode=disable",
			MaxOpenConns: 16,
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
// Environ, which a child reads back as Load does. Every field must survive
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
	if err := parseEnv(got); err != nil {
		t.Fatalf("load(Environ()): %v", err)
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
			t.Errorf("%s: load(Environ()) = %#v, want %#v", path, deref(gotv), deref(wv))
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

func TestGitMaxConnectionsPerIP(t *testing.T) {
	// Ignore any value exported in the environment running the tests.
	t.Setenv("SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP", "")
	if err := os.Unsetenv("SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Git.MaxConnectionsPerIP; got != 8 {
		t.Fatalf("cfg.Git.MaxConnectionsPerIP = %v, want %v", got, 8)
	}
	// A share of the total, so that one address cannot take every slot.
	if cfg.Git.MaxConnectionsPerIP <= 0 || cfg.Git.MaxConnectionsPerIP >= cfg.Git.MaxConnections {
		t.Fatal("not true: cfg.Git.MaxConnectionsPerIP > 0 && cfg.Git.MaxConnectionsPerIP < cfg.Git.MaxConnections")
	}

	t.Setenv("SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP", "3")
	cfg = DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Git.MaxConnectionsPerIP; got != 3 {
		t.Fatalf("cfg.Git.MaxConnectionsPerIP = %v, want %v", got, 3)
	}
	// Hooks re-read the config from the environment.
	if !slices.Contains(cfg.Environ(), "SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP=3") {
		t.Fatal("not true: slices.Contains(cfg.Environ(), \"SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP=3\")")
	}
	if err := os.Unsetenv("SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("git:\n  max_connections_per_ip: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	cfg.DataPath = dir
	if err := parseFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Git.MaxConnectionsPerIP; got != 0 {
		t.Fatalf("cfg.Git.MaxConnectionsPerIP = %v, want %v", got, 0)
	}

	// The generated config file keeps the value.
	cfg.Git.MaxConnectionsPerIP = 5
	out := filepath.Join(dir, "written.yaml")
	if err := writeConfig(cfg, out); err != nil {
		t.Fatal(err)
	}
	got := DefaultConfig()
	got.DataPath = dir
	if err := parseFile(got, out); err != nil {
		t.Fatal(err)
	}
	if got := got.Git.MaxConnectionsPerIP; got != 5 {
		t.Fatalf("got.Git.MaxConnectionsPerIP = %v, want %v", got, 5)
	}
}

func TestGitMaxConnectionsPerIPValidate(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Git.MaxConnectionsPerIP = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "git.max_connections_per_ip") {
		t.Errorf("Validate() = %v, want an error naming git.max_connections_per_ip", err)
	}
	cfg.Git.MaxConnectionsPerIP = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("max_connections_per_ip 0: Validate() = %v", err)
	}
}

var httpLimitsEnv = []string{
	"SOFT_SERVE_HTTP_MAX_TIMEOUT",
	"SOFT_SERVE_HTTP_IDLE_TIMEOUT",
	"SOFT_SERVE_HTTP_MAX_CONNECTIONS",
	"SOFT_SERVE_HTTP_MAX_CONNECTIONS_PER_IP",
}

func TestHTTPLimits(t *testing.T) {
	// Ignore any value exported in the environment running the tests.
	for _, k := range httpLimitsEnv {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.HTTP.MaxTimeout; got != DefaultMaxTimeout {
		t.Fatalf("cfg.HTTP.MaxTimeout = %v, want %v", got, DefaultMaxTimeout)
	}
	if got := cfg.HTTP.IdleTimeout; got != 60 {
		t.Fatalf("cfg.HTTP.IdleTimeout = %v, want %v", got, 60)
	}
	if got := cfg.HTTP.MaxConnections; got != 64 {
		t.Fatalf("cfg.HTTP.MaxConnections = %v, want %v", got, 64)
	}
	if got := cfg.HTTP.MaxConnectionsPerIP; got != 16 {
		t.Fatalf("cfg.HTTP.MaxConnectionsPerIP = %v, want %v", got, 16)
	}

	t.Setenv("SOFT_SERVE_HTTP_MAX_TIMEOUT", "3600")
	t.Setenv("SOFT_SERVE_HTTP_IDLE_TIMEOUT", "30")
	t.Setenv("SOFT_SERVE_HTTP_MAX_CONNECTIONS", "8")
	t.Setenv("SOFT_SERVE_HTTP_MAX_CONNECTIONS_PER_IP", "2")
	cfg = DefaultConfig()
	if err := parseEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.HTTP.MaxTimeout; got != 3600 {
		t.Fatalf("cfg.HTTP.MaxTimeout = %v, want %v", got, 3600)
	}
	if got := cfg.HTTP.IdleTimeout; got != 30 {
		t.Fatalf("cfg.HTTP.IdleTimeout = %v, want %v", got, 30)
	}
	if got := cfg.HTTP.MaxConnections; got != 8 {
		t.Fatalf("cfg.HTTP.MaxConnections = %v, want %v", got, 8)
	}
	if got := cfg.HTTP.MaxConnectionsPerIP; got != 2 {
		t.Fatalf("cfg.HTTP.MaxConnectionsPerIP = %v, want %v", got, 2)
	}
	// Hooks re-read the config from the environment.
	env := cfg.Environ()
	for _, want := range []string{
		"SOFT_SERVE_HTTP_MAX_TIMEOUT=3600",
		"SOFT_SERVE_HTTP_IDLE_TIMEOUT=30",
		"SOFT_SERVE_HTTP_MAX_CONNECTIONS=8",
		"SOFT_SERVE_HTTP_MAX_CONNECTIONS_PER_IP=2",
	} {
		if !slices.Contains(env, want) {
			t.Fatal("not true: slices.Contains(env, want)")
		}
	}
	for _, k := range httpLimitsEnv {
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`http:
  max_timeout: 600
  idle_timeout: 0
  max_connections: 16
  max_connections_per_ip: 0
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	cfg.DataPath = dir
	if err := parseFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	if got := cfg.HTTP.MaxTimeout; got != 600 {
		t.Fatalf("cfg.HTTP.MaxTimeout = %v, want %v", got, 600)
	}
	if got := cfg.HTTP.IdleTimeout; got != 0 {
		t.Fatalf("cfg.HTTP.IdleTimeout = %v, want %v", got, 0)
	}
	if got := cfg.HTTP.MaxConnections; got != 16 {
		t.Fatalf("cfg.HTTP.MaxConnections = %v, want %v", got, 16)
	}
	if got := cfg.HTTP.MaxConnectionsPerIP; got != 0 {
		t.Fatalf("cfg.HTTP.MaxConnectionsPerIP = %v, want %v", got, 0)
	}

	// The generated config file keeps the values.
	out := filepath.Join(dir, "written.yaml")
	if err := writeConfig(cfg, out); err != nil {
		t.Fatal(err)
	}
	got := DefaultConfig()
	got.DataPath = dir
	if err := parseFile(got, out); err != nil {
		t.Fatal(err)
	}
	if got := got.HTTP.MaxTimeout; got != 600 {
		t.Fatalf("got.HTTP.MaxTimeout = %v, want %v", got, 600)
	}
	if got := got.HTTP.IdleTimeout; got != 0 {
		t.Fatalf("got.HTTP.IdleTimeout = %v, want %v", got, 0)
	}
	if got := got.HTTP.MaxConnections; got != 16 {
		t.Fatalf("got.HTTP.MaxConnections = %v, want %v", got, 16)
	}
	if got := got.HTTP.MaxConnectionsPerIP; got != 0 {
		t.Fatalf("got.HTTP.MaxConnectionsPerIP = %v, want %v", got, 0)
	}
}

// Every server has a total time limit by default: the idle timeouts are
// re-armed by every byte, so without one a client that trickles bytes holds
// its connection, and its place under max_connections, for ever.
func TestDefaultMaxTimeout(t *testing.T) {
	cfg := DefaultConfig()
	if DefaultMaxTimeout <= 0 {
		t.Fatal("not true: DefaultMaxTimeout > 0")
	}
	if got := cfg.SSH.MaxTimeout; got != DefaultMaxTimeout {
		t.Fatalf("cfg.SSH.MaxTimeout = %v, want %v", got, DefaultMaxTimeout)
	}
	if got := cfg.Git.MaxTimeout; got != DefaultMaxTimeout {
		t.Fatalf("cfg.Git.MaxTimeout = %v, want %v", got, DefaultMaxTimeout)
	}
	if got := cfg.HTTP.MaxTimeout; got != DefaultMaxTimeout {
		t.Fatalf("cfg.HTTP.MaxTimeout = %v, want %v", got, DefaultMaxTimeout)
	}
	// Long enough for a large clone over a slow link: hours, not minutes.
	if DefaultMaxTimeout < 60*60 {
		t.Fatal("not true: DefaultMaxTimeout >= 60*60")
	}
}

func TestHTTPLimitsRejectNegative(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*HTTPConfig)
	}{
		{"http.max_timeout", func(c *HTTPConfig) { c.MaxTimeout = -1 }},
		{"http.idle_timeout", func(c *HTTPConfig) { c.IdleTimeout = -1 }},
		{"http.max_connections", func(c *HTTPConfig) { c.MaxConnections = -1 }},
		{"http.max_connections_per_ip", func(c *HTTPConfig) { c.MaxConnectionsPerIP = -1 }},
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
	return writeConfig(cfg, cfg.FilePath())
}

// The file the server writes on its first start holds the defaults, and
// only keys the server reads.
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
	got, err := os.ReadFile(cfg.FilePath())
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
// repair a value the file got wrong.
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
// rather than opening the data directory, or a file named "?_pragma=…".
// Postgres reads the PG* variables when it is empty, so it is allowed there.
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
// and the setting is reported as removed, not as unknown. Its
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

// A relative SOFT_SERVE_CONFIG_LOCATION is resolved once, at load, against
// the working directory the server starts in. FilePath and the
// environment hooks get name that file wherever they are used from: a hook
// starts in a repository's directory, where the relative path named another
// file and every push failed.
func TestRelativeConfigLocation(t *testing.T) {
	clearSoftServeEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("conf.yaml", []byte("name: relative\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOFT_SERVE_DATA_PATH", filepath.Join(dir, "data"))
	t.Setenv("SOFT_SERVE_CONFIG_LOCATION", "conf.yaml")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "relative" {
		t.Fatalf("Load() name %q, want the relative file's", cfg.Name)
	}
	want := filepath.Join(dir, "conf.yaml")
	if got := cfg.FilePath(); got != want {
		t.Errorf("FilePath() = %q, want %q", got, want)
	}

	// From another directory, as a hook runs.
	t.Chdir(t.TempDir())
	if got := cfg.FilePath(); got != want {
		t.Errorf("FilePath() from another directory = %q, want %q", got, want)
	}
	if !slices.Contains(cfg.Environ(), "SOFT_SERVE_CONFIG_LOCATION="+want) {
		t.Errorf("Environ() does not name %s", want)
	}
	if !cfg.Exist() {
		t.Error("Exist() = false from another directory")
	}
}

func TestResolveSQLiteDataSource(t *testing.T) {
	dp := filepath.Join(string(filepath.Separator)+"srv", "soft serve#1")
	if runtime.GOOS == "windows" {
		dp = `C:\srv\soft serve#1`
	}
	uriBase := "file:" + (map[bool]string{true: "///C:/srv/soft%20serve%231", false: "/srv/soft%20serve%231"})[runtime.GOOS == "windows"]

	abs := filepath.Join(dp, "x.db")
	for _, c := range []struct{ in, want string }{
		{"soft-serve.db?_pragma=foreign_keys(1)", filepath.Join(dp, "soft-serve.db") + "?_pragma=foreign_keys(1)"},
		{"sub/x.db", filepath.Join(dp, "sub", "x.db")},
		{abs, abs},
		{":memory:", ":memory:"},
		{":memory:?_pragma=foreign_keys(1)", ":memory:?_pragma=foreign_keys(1)"},
		{"file::memory:", "file::memory:"},
		{"file::memory:?cache=shared", "file::memory:?cache=shared"},
		{"file:shared?mode=memory&cache=shared", "file:shared?mode=memory&cache=shared"},
		{"file:/abs/x.db?_pragma=foreign_keys(1)", "file:/abs/x.db?_pragma=foreign_keys(1)"},
		{"file:///abs/x.db", "file:///abs/x.db"},
		{"file:", "file:"},
		{"file:x.db", uriBase + "/x.db"},
		{"file:sub/x%20y.db?mode=rwc&_pragma=foreign_keys(1)", uriBase + "/sub/x%20y.db?mode=rwc&_pragma=foreign_keys(1)"},
	} {
		if got := resolveSQLiteDataSource(dp, c.in); got != c.want {
			t.Errorf("resolveSQLiteDataSource(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A UNC data path (\\server\share\data on Windows) must not end up as the
// authority of the URI: SQLite refuses file://server/... unless built with
// SQLITE_ALLOW_URI_AUTHORITY.
func TestResolveSQLiteDataSourceUNC(t *testing.T) {
	dp := filepath.FromSlash("//server/share/soft data")
	for _, c := range []struct{ in, want string }{
		{"file:x.db", "file:////server/share/soft%20data/x.db"},
		{"file:sub/x.db?mode=rwc", "file:////server/share/soft%20data/sub/x.db?mode=rwc"},
	} {
		if got := resolveSQLiteDataSource(dp, c.in); got != c.want {
			t.Errorf("resolveSQLiteDataSource(%q, %q) = %q, want %q", dp, c.in, got, c.want)
		}
	}
}

func TestValidateSQLiteMemoryDataSource(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.DataSource = ":memory:"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.DB.DataSource != ":memory:" {
		t.Errorf("data_source = %q, want %q", cfg.DB.DataSource, ":memory:")
	}
}

func TestValidatePostgresDataSourceUnchanged(t *testing.T) {
	const dsn = "postgres://u@localhost/soft?sslmode=disable"
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = "postgres"
	cfg.DB.DataSource = dsn
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.DB.DataSource != dsn {
		t.Errorf("data_source = %q, want %q", cfg.DB.DataSource, dsn)
	}
}

func TestValidateLogPath(t *testing.T) {
	dp := t.TempDir()
	abs := filepath.Join(t.TempDir(), "abs.log")
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"soft-serve.log", filepath.Join(dp, "soft-serve.log")},
		{filepath.Join("logs", "s.log"), filepath.Join(dp, "logs", "s.log")},
		{abs, abs},
	} {
		cfg := DefaultConfig()
		cfg.DataPath = dp
		cfg.Log.Path = c.in
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Log.Path != c.want {
			t.Errorf("log.path %q = %q, want %q", c.in, cfg.Log.Path, c.want)
		}
	}
}

const (
	testKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH"
	testKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8"
)

// TestAdminKeyFileRelativeToDataPath checks that a relative key file path is
// read from the data directory, before the working directory.
func TestAdminKeyFileRelativeToDataPath(t *testing.T) {
	dp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dp, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dp, "keys", "admin.pub"), []byte(testKeyB+" a@b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// testdata/k1.pub exists in the working directory too; the data
	// directory's copy wins.
	if err := os.MkdirAll(filepath.Join(dp, "testdata"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dp, "testdata", "k1.pub"), []byte(testKeyB+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.DataPath = dp
	cfg.InitialAdminKeys = []string{filepath.Join("keys", "admin.pub"), filepath.Join("testdata", "k1.pub")}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.InitialAdminKeys) != 1 || cfg.InitialAdminKeys[0] != testKeyB {
		t.Errorf("initial_admin_keys = %q, want [%q]", cfg.InitialAdminKeys, testKeyB)
	}
	if got := cfg.UnreadableAdminKeyFiles(); len(got) != 0 {
		t.Errorf("unreadable admin key files = %q, want none", got)
	}
}

// TestAdminKeyFileFallsBackToWorkingDirectory checks that a relative key file
// that is not in the data directory is still read from the working directory.
func TestAdminKeyFileFallsBackToWorkingDirectory(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.InitialAdminKeys = []string{filepath.Join("testdata", "k1.pub"), filepath.Join("keys", "missing.pub")}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.InitialAdminKeys) != 1 || cfg.InitialAdminKeys[0] != testKeyA {
		t.Errorf("initial_admin_keys = %q, want [%q]", cfg.InitialAdminKeys, testKeyA)
	}
	got := cfg.UnreadableAdminKeyFiles()
	if len(got) != 1 || got[0] != filepath.Join("keys", "missing.pub") {
		t.Errorf("unreadable admin key files = %q, want the missing file", got)
	}
}

// TestDefaultConfigFileAnonAccessAllowKeylessCommented checks that the
// written config documents anon_access and allow_keyless but leaves them
// commented out: they override the database for as long as they are set, so
// a generated file must not set them.
func TestDefaultConfigFileAnonAccessAllowKeylessCommented(t *testing.T) {
	s := string(defaultConfigFile)
	for _, want := range []string{"\n#anon_access: ", "\n#allow_keyless: "} {
		if !strings.Contains(s, want) {
			t.Errorf("written config lacks %q", want)
		}
	}

	var got Config
	if err := yaml.Unmarshal([]byte(s), &got); err != nil {
		t.Fatalf("decode written config: %v", err)
	}
	if got.AnonAccess != nil || got.AllowKeyless != nil {
		t.Errorf("written config sets anon_access=%v allow_keyless=%v, want both unset", got.AnonAccess, got.AllowKeyless)
	}
}

func TestParseEmptyMirrorPull(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{"jobs:\n  mirror_pull: \"\"\n", ""},
		{"jobs:\n  mirror_pull: \"@every 1h\"\n", "@every 1h"},
		{"jobs: {}\n", "@every 10m"},
		{"name: x\n", "@every 10m"},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(c.file), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		if err := parseFile(cfg, path); err != nil {
			t.Fatal(err)
		}
		if cfg.Jobs.MirrorPull != c.want {
			t.Errorf("%q: mirror_pull = %q, want %q", c.file, cfg.Jobs.MirrorPull, c.want)
		}

		// The written config keeps an empty spec empty.
		var back Config
		back.Jobs.MirrorPull = "@every 10m"
		written, err := marshalConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal([]byte(written), &back); err != nil {
			t.Fatal(err)
		}
		if back.Jobs.MirrorPull != c.want {
			t.Errorf("%q: written mirror_pull = %q, want %q", c.file, back.Jobs.MirrorPull, c.want)
		}
	}
}

// A config.yaml an earlier version wrote holds the removed settings with
// the values it wrote then. They give no warning: every upgraded server
// would warn at each start about values its operator never set, and the
// server does what they say. Another value is still reported.
func TestRemovedSettingsAtOldDefaultsAreSilent(t *testing.T) {
	old := `ssh:
  handshake_timeout: 120
  max_sessions: 10
  kex_algorithms: []
  ciphers: []
  macs: []
db:
  max_open_conns: 0
  max_idle_conns: 0
  conn_max_lifetime: 0
  conn_max_idle_time: 0
jobs:
  mirror_pull: "@every 10m"
  lfs_sweep: "@every 1h"
max_blob_size: 1048576
`
	loadEnv(t, old)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RemovedSettings(); len(got) != 0 {
		t.Errorf("RemovedSettings() = %+v, want none for the values earlier versions wrote", got)
	}
	if got := cfg.UnknownKeys(); len(got) != 0 {
		t.Errorf("UnknownKeys() = %+v, want none", got)
	}

	for _, changed := range []string{
		"  max_sessions: 3\n",
		"  kex_algorithms: [\"curve25519-sha256\"]\n",
		"  handshake_timeout: \"120\"\n", // quoted, still the same text
	} {
		file := strings.Replace(old, "  max_sessions: 10\n", "", 1)
		file = strings.Replace(file, "  kex_algorithms: []\n", "", 1)
		file = strings.Replace(file, "  handshake_timeout: 120\n", "", 1)
		file = strings.Replace(file, "ssh:\n", "ssh:\n"+changed, 1)
		loadEnv(t, file)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		got := cfg.RemovedSettings()
		want := 1
		if strings.Contains(changed, `"120"`) {
			want = 0
		}
		if len(got) != want {
			t.Errorf("%q: RemovedSettings() = %+v, want %d", strings.TrimSpace(changed), got, want)
		}
		for _, r := range got {
			if !r.InFile {
				t.Errorf("%s: InFile = false for a config file key", r.Key)
			}
		}
	}
}

// Each removed setting's oldDefault is what a config.yaml written before it
// was removed holds, and what the server now does: only then may a config that
// holds it go without a warning. A fixed value that changes, such as
// SSHMaxSessions, fails here, and the entry must then drop its oldDefault.
func TestRemovedSettingsOldDefaults(t *testing.T) {
	for _, tc := range []struct {
		key string
		// written is what config.yaml held before these settings went.
		written string
		// now is the setting's value for what the server now does.
		now string
	}{
		{"ssh.kex_algorithms", "[]", "[]"}, // empty: the secure defaults
		{"ssh.ciphers", "[]", "[]"},
		{"ssh.macs", "[]", "[]"},
		{"ssh.max_sessions", "10", strconv.Itoa(SSHMaxSessions)},
		{"ssh.handshake_timeout", "120", strconv.Itoa(int(SSHHandshakeTimeout / time.Second))},
		{"db.max_idle_conns", "0", "0"}, // 0: database/sql's default
		{"db.conn_max_lifetime", "0", "0"},
		{"db.conn_max_idle_time", "0", "0"},
		// jobs.LFSSweepSpec; TestReadmeLFSSweepHourly in cmd/soft/serve
		// pins it to this.
		{"jobs.lfs_sweep", "@every 1h", "@every 1h"},
		{"max_blob_size", "1048576", strconv.Itoa(MaxBlobSize)},
	} {
		var r *removedSetting
		for i := range removedSettings {
			if removedSettings[i].key == tc.key {
				r = &removedSettings[i]
			}
		}
		if r == nil {
			t.Errorf("%s is not a removed setting", tc.key)
			continue
		}
		if tc.written != tc.now {
			if len(r.oldDefault) != 0 {
				t.Errorf("%s: earlier versions wrote %q but the server now does %q; a config holding %q must warn, so drop its oldDefault",
					tc.key, tc.written, tc.now, tc.written)
			}
			continue
		}
		if len(r.oldDefault) != 1 || r.oldDefault[0] != tc.written {
			t.Errorf("%s: oldDefault %q, want %q, what earlier versions wrote", tc.key, r.oldDefault, tc.written)
		}
	}
	if len(removedSettings) != 10 {
		t.Errorf("%d removed settings; give each new one a row here", len(removedSettings))
	}
}

// The README's Removed settings table lists every removed setting with its
// variable, and states the fixed values the server uses instead.
func TestReadmeRemovedSettingsTable(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(readme), "\r\n", "\n")
	i := strings.Index(doc, "#### Removed settings")
	if i < 0 {
		t.Fatal("README has no Removed settings section")
	}
	doc = doc[i:]
	doc = doc[:strings.Index(doc[1:], "\n#")+1]

	rows := map[string][]string{}
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if len(cells) != 3 {
			t.Fatalf("table row %q has %d cells, want 3", line, len(cells))
		}
		rows[strings.Trim(cells[0], "`")] = cells
	}
	if len(rows) != len(removedSettings) {
		t.Errorf("the table has %d rows, want one per removed setting (%d)", len(rows), len(removedSettings))
	}
	for _, r := range removedSettings {
		row, ok := rows[r.key]
		if !ok {
			t.Errorf("the table has no row for %s", r.key)
			continue
		}
		if row[1] != "`"+r.env+"`" {
			t.Errorf("%s: variable %s, want %s", r.key, row[1], r.env)
		}
	}

	if SSHHandshakeTimeout%time.Minute != 0 || MaxBlobSize%(1<<20) != 0 {
		t.Fatal("the table states the handshake timeout in minutes and the blob size in MiB")
	}
	for key, want := range map[string]string{
		"ssh.max_sessions":      fmt.Sprintf("allows %d sessions", SSHMaxSessions),
		"ssh.handshake_timeout": fmt.Sprintf("gives a client %d minutes", SSHHandshakeTimeout/time.Minute),
		"max_blob_size":         fmt.Sprintf("up to %d MiB", MaxBlobSize>>20),
	} {
		if row := rows[key]; row == nil || !strings.Contains(row[2], want) {
			t.Errorf("%s: the table says %q, want %q", key, row, want)
		}
	}
}

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

// TestValidateRejectsUnusableValues checks that values which parse but
// cannot work are refused at startup rather than degrading silently.
func TestValidateRejectsUnusableValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(*Config)
		want string // substring of the error; empty means valid
	}{
		{"defaults", func(*Config) {}, ""},
		{"mirror pull standard spec", func(c *Config) { c.Jobs.MirrorPull = "*/5 * * * *" }, ""},
		{"mirror pull empty", func(c *Config) { c.Jobs.MirrorPull = "" }, ""},
		{"mirror pull invalid", func(c *Config) { c.Jobs.MirrorPull = "every 10 minutes" }, "jobs.mirror_pull"},
		{"mirror pull seconds field", func(c *Config) { c.Jobs.MirrorPull = "0 */5 * * * *" }, "jobs.mirror_pull"},
		{"log format upper case", func(c *Config) { c.Log.Format = "JSON" }, ""},
		{"log format empty", func(c *Config) { c.Log.Format = "" }, ""},
		{"log format unknown", func(c *Config) { c.Log.Format = "yaml" }, "log.format"},
		{"public url https with path", func(c *Config) { c.HTTP.PublicURL = "https://git.example.com/soft/" }, ""},
		{"public url no scheme", func(c *Config) { c.HTTP.PublicURL = "git.example.com" }, "http.public_url"},
		{"public url host port no scheme", func(c *Config) { c.HTTP.PublicURL = "localhost:23232" }, "http.public_url"},
		{"public url other scheme", func(c *Config) { c.HTTP.PublicURL = "ftp://git.example.com" }, "http.public_url"},
		{"public url no host", func(c *Config) { c.HTTP.PublicURL = "http:///path" }, "http.public_url"},
		{"tls both", func(c *Config) { c.HTTP.TLSKeyPath, c.HTTP.TLSCertPath = "key.pem", "cert.pem" }, ""},
		{"tls key only", func(c *Config) { c.HTTP.TLSKeyPath = "key.pem" }, "tls_cert_path"},
		{"tls cert only", func(c *Config) { c.HTTP.TLSCertPath = "cert.pem" }, "tls_key_path"},
		{"ssh max timeout negative", func(c *Config) { c.SSH.MaxTimeout = -1 }, "ssh.max_timeout"},
		{"ssh idle timeout negative", func(c *Config) { c.SSH.IdleTimeout = -1 }, "ssh.idle_timeout"},
		{"git max timeout negative", func(c *Config) { c.Git.MaxTimeout = -1 }, "git.max_timeout"},
		{"git idle timeout negative", func(c *Config) { c.Git.IdleTimeout = -1 }, "git.idle_timeout"},
		{"timeouts zero", func(c *Config) {
			c.SSH.MaxTimeout, c.SSH.IdleTimeout, c.Git.MaxTimeout, c.Git.IdleTimeout = 0, 0, 0, 0
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.DataPath = t.TempDir()
			tc.set(cfg)
			err := cfg.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("Validate() = %v, want an error naming %s", err, tc.want)
			}
		})
	}
}
