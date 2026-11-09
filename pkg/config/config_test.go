package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/matryer/is"
	"go.yaml.in/yaml/v4"
)

func TestParseMultipleKeys(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	is.NoErr(os.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "testdata/k1.pub\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8 a@b"))
	is.NoErr(os.Setenv("SOFT_SERVE_DATA_PATH", td))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_INITIAL_ADMIN_KEYS"))
		is.NoErr(os.Unsetenv("SOFT_SERVE_DATA_PATH"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.InitialAdminKeys, []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8",
	})
}

func TestMergeInitAdminKeys(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "testdata/k1.pub"))
	t.Cleanup(func() { is.NoErr(os.Unsetenv("SOFT_SERVE_INITIAL_ADMIN_KEYS")) })
	cfg := &Config{
		DataPath:         t.TempDir(),
		InitialAdminKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8 a@b"},
	}
	is.NoErr(cfg.WriteConfig())
	is.NoErr(cfg.Parse())
	is.Equal(cfg.InitialAdminKeys, []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8",
	})
}

func TestValidateInitAdminKeys(t *testing.T) {
	is := is.New(t)
	cfg := &Config{
		DataPath: t.TempDir(),
		InitialAdminKeys: []string{
			"testdata/k1.pub",
			"not a key",
			"",
		},
	}
	is.NoErr(cfg.WriteConfig())
	is.NoErr(cfg.Parse())
	is.Equal(cfg.InitialAdminKeys, []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
	})
	// "not a key" was dropped as neither a key nor a path; the blank entry
	// is not counted.
	is.Equal(cfg.IgnoredAdminKeys(), 1)
	is.Equal(cfg.UnreadableAdminKeyFiles(), []string(nil))
}

func TestUnreadableAdminKeyFiles(t *testing.T) {
	is := is.New(t)
	missing := filepath.Join(t.TempDir(), "not-mounted-yet.pub")
	notAKey := filepath.Join(t.TempDir(), "id_ed25519")
	is.NoErr(os.WriteFile(notAKey, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600))
	cfg := &Config{
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
	// Before Validate the entries are checked as they are.
	is.Equal(cfg.UnreadableAdminKeyFiles(), want)
	is.Equal(cfg.IgnoredAdminKeys(), 2)

	// Validate drops them from the list but remembers them.
	is.NoErr(cfg.Validate())
	is.Equal(len(cfg.InitialAdminKeys), 1)
	is.Equal(cfg.UnreadableAdminKeyFiles(), want)
	is.Equal(cfg.IgnoredAdminKeys(), 2)
	is.Equal(len(cfg.AdminKeys()), 1)

	// A config whose entries all parse has none.
	ok := &Config{InitialAdminKeys: []string{"testdata/k1.pub", "", "# comment"}}
	is.Equal(ok.UnreadableAdminKeyFiles(), []string(nil))
	is.Equal(ok.IgnoredAdminKeys(), 0)
}

// Parse runs Validate after the file and again after the environment, and a
// caller may parse again, so Validate sees the same entries several times.
// They are recorded once.
func TestUnreadableAdminKeyFilesNotDuplicated(t *testing.T) {
	is := is.New(t)
	missing := filepath.Join(t.TempDir(), "not-mounted-yet.pub")
	t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", missing+"\nnot a key")
	cfg := &Config{
		DataPath:         t.TempDir(),
		InitialAdminKeys: []string{missing, "not a key"},
	}
	is.NoErr(cfg.WriteConfig())
	is.NoErr(cfg.Parse())
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.UnreadableAdminKeyFiles(), []string{missing})
	is.Equal(cfg.IgnoredAdminKeys(), 1)
}

func TestCustomConfigLocation(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_CONFIG_LOCATION"))
	})

	// Test that we get data from the custom file location, and not from the data dir.
	is.NoErr(os.Setenv("SOFT_SERVE_CONFIG_LOCATION", "testdata/config.yaml"))
	is.NoErr(os.Setenv("SOFT_SERVE_DATA_PATH", td))
	cfg := DefaultConfig()
	is.NoErr(cfg.Parse())
	is.Equal(cfg.Name, "Test server name")
	// If we unset the custom location, then use the default location.
	is.NoErr(os.Unsetenv("SOFT_SERVE_CONFIG_LOCATION"))
	cfg = DefaultConfig()
	is.Equal(cfg.Name, "Soft Serve Plus")
	// Test that if the custom config location doesn't exist, default to datapath config.
	is.NoErr(os.Setenv("SOFT_SERVE_CONFIG_LOCATION", "testdata/config_nonexistent.yaml"))
	cfg = DefaultConfig()
	is.Equal(cfg.Name, "Soft Serve Plus")
}

func TestCheckConfigLocation(t *testing.T) {
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
			err := DefaultConfig().CheckConfigLocation()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "SOFT_SERVE_CONFIG_LOCATION") {
					t.Fatalf("CheckConfigLocation() = %v, want an error naming the variable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckConfigLocation() = %v, want nil", err)
			}
		})
	}
}

func TestDefaultRepoDisabledByDefault(t *testing.T) {
	is := is.New(t)
	is.Equal(DefaultConfig().DefaultRepo, "")
}

func TestParseDefaultRepo(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_DEFAULT_REPO", "gitops"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_DEFAULT_REPO"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.DefaultRepo, "gitops")
}

func TestParseDefaultRepoFromFile(t *testing.T) {
	is := is.New(t)
	cfg := &Config{DataPath: t.TempDir()}
	is.NoErr(parseFile(cfg, "testdata/config_default_repo.yaml"))
	is.Equal(cfg.DefaultRepo, "gitops")
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
			is := is.New(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			is.NoErr(os.WriteFile(path, []byte(content), 0o600))
			cfg := DefaultConfig()
			cfg.DataPath = t.TempDir()
			want := cfg.Name
			is.NoErr(parseFile(cfg, path))
			is.Equal(cfg.Name, want)
		})
	}
}

func TestParseMultipleHeaders(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_HEADERS", "Accept,Accept-Language,User-Agent"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_HEADERS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.HTTP.CORS.AllowedHeaders, []string{
		"Accept",
		"Accept-Language",
		"User-Agent",
	})
}

func TestParseMultipleOrigins(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS", "http://example.com,https://example.com"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.HTTP.CORS.AllowedOrigins, []string{
		"http://example.com",
		"https://example.com",
	})
	is.Equal(cfg.CORSAllowedOrigins(), []string{
		"http://localhost:23232",
		"http://example.com",
		"https://example.com",
	})
}

func TestValidateDoesNotDuplicatePublicURLOrigin(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	is.NoErr(cfg.WriteConfig())
	// cmd/soft parses the config file and then the environment, and each
	// step runs Validate.
	is.NoErr(cfg.Parse())
	is.NoErr(cfg.ParseEnv())
	is.NoErr(cfg.Validate())
	is.Equal(len(cfg.HTTP.CORS.AllowedOrigins), 0)
	is.Equal(cfg.CORSAllowedOrigins(), []string{"http://localhost:23232"})
	is.True(slices.Contains(cfg.Environ(), "SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS="))
}

// The public URL is allowed as an origin wherever it was set last: an
// environment override of it does not leave the config file's URL, or the
// default one, allowed too. An empty public URL adds no empty origin; the
// web server then rejects every origin (see TestCORSHandlerOrigins).
func TestCORSAllowedOriginsFollowPublicURL(t *testing.T) {
	is := is.New(t)
	t.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS", "")
	is.NoErr(os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS"))
	file := DefaultConfig()
	file.DataPath = t.TempDir()
	file.HTTP.PublicURL = "https://file.example.com"
	path := filepath.Join(file.DataPath, "config.yaml")
	is.NoErr(writeConfig(file, path))

	t.Setenv("SOFT_SERVE_HTTP_PUBLIC_URL", "https://env.example.com//")
	cfg := DefaultConfig()
	is.NoErr(parseFile(cfg, path))
	is.NoErr(parseEnv(cfg))
	is.Equal(cfg.HTTP.PublicURL, "https://env.example.com")
	is.Equal(cfg.CORSAllowedOrigins(), []string{"https://env.example.com"})

	cfg.HTTP.PublicURL = ""
	is.NoErr(cfg.Validate())
	is.Equal(len(cfg.CORSAllowedOrigins()), 0)
}

func TestParseMultipleMethods(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_METHODS", "GET,POST,PUT"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_METHODS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.HTTP.CORS.AllowedMethods, []string{
		"GET",
		"POST",
		"PUT",
	})
}

func TestAdminOnlyCreateOffByDefault(t *testing.T) {
	is := is.New(t)
	// Ignore any value exported in the environment running the tests;
	// t.Setenv restores it afterwards.
	t.Setenv("SOFT_SERVE_ADMIN_ONLY_CREATE", "")
	is.NoErr(os.Unsetenv("SOFT_SERVE_ADMIN_ONLY_CREATE"))
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.True(!cfg.AdminOnlyCreate)
}

func TestParseAdminOnlyCreateEnv(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_ADMIN_ONLY_CREATE", "true"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_ADMIN_ONLY_CREATE"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.True(cfg.AdminOnlyCreate)
}

func TestAdminOnlyCreateInEnviron(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.AdminOnlyCreate = true
	is.True(slices.Contains(cfg.Environ(), "SOFT_SERVE_ADMIN_ONLY_CREATE=true"))
}

// The hook subprocess re-reads its config from the environment, and git
// inherits the server's own environment, so the flag is always emitted
// explicitly. An off flag must read as "=false", never as a value inherited
// from elsewhere.
func TestAdminOnlyCreateFalseInEnviron(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.AdminOnlyCreate = false
	envs := cfg.Environ()
	is.True(slices.Contains(envs, "SOFT_SERVE_ADMIN_ONLY_CREATE=false"))
	is.True(!slices.Contains(envs, "SOFT_SERVE_ADMIN_ONLY_CREATE=true"))
}

func TestAnonAccessEnvUnsetByDefault(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	// nil is the "no override" sentinel.
	is.True(cfg.AnonAccess == nil)
}

func TestParseAnonAccessEnv(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_ANON_ACCESS", access.AdminAccess.String()))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_ANON_ACCESS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.True(cfg.AnonAccess != nil)
	is.Equal(*cfg.AnonAccess, access.AdminAccess)
}

// An invalid anon-access level is rejected at parse time by AccessLevel's
// TextUnmarshaler, so it never reaches Validate as a bad value.
func TestParseRejectsInvalidAnonAccess(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_ANON_ACCESS", "not-a-real-access-level"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_ANON_ACCESS"))
	})
	cfg := DefaultConfig()
	err := cfg.ParseEnv()
	is.True(err != nil)
}

func TestAllowKeylessEnvUnsetByDefault(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	// nil is the "no override" sentinel — distinct from an explicit false.
	is.True(cfg.AllowKeyless == nil)
}

func TestParseAllowKeylessEnvTrue(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_ALLOW_KEYLESS", "true"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_ALLOW_KEYLESS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.True(cfg.AllowKeyless != nil)
	is.Equal(*cfg.AllowKeyless, true)
}

func TestParseAllowKeylessEnvFalse(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_ALLOW_KEYLESS", "false"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_ALLOW_KEYLESS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	// Explicit false must survive as a real override, not collapse back to
	// "unset" — that's the whole reason this field is a *bool.
	is.True(cfg.AllowKeyless != nil)
	is.Equal(*cfg.AllowKeyless, false)
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
	if len(cfg.SSH.AllowedHosts()) != 0 {
		t.Fatal("allowlist populated before Validate")
	}

	cfg.SSH.ClientAllowedHosts = []string{"git.internal.example", "", "10.0.0.0/8:2222", " "}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.SSH.ClientAllowedHosts; len(got) != 2 {
		t.Errorf("blank entries kept: %q", got)
	}
	if got := cfg.SSH.AllowedHosts(); len(got) != 2 {
		t.Errorf("parsed %d entries, want 2", len(got))
	}
	if err := cfg.SSH.AllowedHosts().Match("git.internal.example", ""); err != nil {
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

// TestParseEnvAdminKeysTwice checks that applying the environment again, as
// cmd/soft does after Parse, keeps the merged file and env keys.
func TestParseEnvAdminKeysTwice(t *testing.T) {
	is := is.New(t)
	t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "testdata/k1.pub")
	cfg := &Config{
		DataPath:         t.TempDir(),
		InitialAdminKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8 a@b"},
	}
	want := []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8",
	}
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.InitialAdminKeys, want)
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.InitialAdminKeys, want)
}

// An authorized_keys file can hold several keys, one per line. Every key is
// an admin key, and a line that is not a key is counted as ignored.
func TestAdminKeyFileWithSeveralKeys(t *testing.T) {
	is := is.New(t)
	k1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH"
	k2 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8"
	file := filepath.Join(t.TempDir(), "authorized_keys")
	is.NoErr(os.WriteFile(file, []byte("# ops\n"+k1+" a@b\r\n\nssh-ed25519 AAAAtypo\n"+k2+" c@d\n"), 0o600))
	cfg := &Config{
		DataPath:         t.TempDir(),
		InitialAdminKeys: []string{file},
	}
	is.Equal(len(cfg.AdminKeys()), 2)
	is.Equal(cfg.IgnoredAdminKeys(), 1)
	is.Equal(cfg.UnreadableAdminKeyFiles(), []string(nil))

	is.NoErr(cfg.Validate())
	is.Equal(cfg.InitialAdminKeys, []string{k1, k2})
	is.Equal(cfg.IgnoredAdminKeys(), 1)
	is.Equal(cfg.UnreadableAdminKeyFiles(), []string(nil))
}

// A key listed inline and again in a key file, there twice with different
// comments and spacing, is one admin key.
func TestAdminKeyFileRepeatsKey(t *testing.T) {
	is := is.New(t)
	k1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH"
	file := filepath.Join(t.TempDir(), "authorized_keys")
	is.NoErr(os.WriteFile(file, []byte(k1+" a@b\n"+k1+"  c@d\n"), 0o600))
	cfg := &Config{
		DataPath:         t.TempDir(),
		InitialAdminKeys: []string{k1 + " x@y", file},
	}
	is.Equal(len(cfg.AdminKeys()), 1)
	is.Equal(cfg.IgnoredAdminKeys(), 0)
	is.Equal(len(cfg.UnreadableAdminKeyFiles()), 0)
}

func TestMaxReposPerUser(t *testing.T) {
	is := is.New(t)
	// Ignore any value exported in the environment running the tests.
	t.Setenv("SOFT_SERVE_MAX_REPOS_PER_USER", "")
	is.NoErr(os.Unsetenv("SOFT_SERVE_MAX_REPOS_PER_USER"))
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.MaxReposPerUser, 0) // no limit by default

	t.Setenv("SOFT_SERVE_MAX_REPOS_PER_USER", "3")
	cfg = DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.MaxReposPerUser, 3)
	// Hooks re-read the config from the environment.
	is.True(slices.Contains(cfg.Environ(), "SOFT_SERVE_MAX_REPOS_PER_USER=3"))

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	is.NoErr(os.WriteFile(path, []byte("max_repos_per_user: 5\n"), 0o600))
	cfg = DefaultConfig()
	is.NoErr(parseFile(cfg, path))
	is.Equal(cfg.MaxReposPerUser, 5)

	t.Setenv("SOFT_SERVE_MAX_REPOS_PER_USER", "-1")
	cfg = DefaultConfig()
	is.True(cfg.ParseEnv() != nil)
}

func TestDefaultPrivate(t *testing.T) {
	is := is.New(t)
	// Ignore any value exported in the environment running the tests.
	t.Setenv("SOFT_SERVE_DEFAULT_PRIVATE", "")
	is.NoErr(os.Unsetenv("SOFT_SERVE_DEFAULT_PRIVATE"))
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.True(!cfg.DefaultPrivate) // public by default, as upstream
	is.True(slices.Contains(cfg.Environ(), "SOFT_SERVE_DEFAULT_PRIVATE=false"))

	t.Setenv("SOFT_SERVE_DEFAULT_PRIVATE", "true")
	cfg = DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.True(cfg.DefaultPrivate)
	// Hooks re-read the config from the environment.
	is.True(slices.Contains(cfg.Environ(), "SOFT_SERVE_DEFAULT_PRIVATE=true"))

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	is.NoErr(os.WriteFile(path, []byte("default_private: true\n"), 0o600))
	cfg = DefaultConfig()
	is.NoErr(parseFile(cfg, path))
	is.True(cfg.DefaultPrivate)
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
			is := is.New(t)
			t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", tc.env)
			t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
			cfg := DefaultConfig()
			is.NoErr(cfg.ParseEnv())
			is.Equal(cfg.InitialAdminKeys, tc.want)
			is.Equal(cfg.IgnoredAdminKeys(), 0)
		})
	}

	// Unmatched or inner quotes are not stripped.
	is := is.New(t)
	cfg := &Config{InitialAdminKeys: []string{"'" + k2 + `"`, `"` + k2}}
	is.Equal(len(cfg.AdminKeys()), 0)
	is.Equal(cfg.IgnoredAdminKeys(), 2)
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
			is := is.New(t)
			t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", tc.env)
			t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
			cfg := DefaultConfig()
			is.NoErr(cfg.ParseEnv())
			is.Equal(cfg.InitialAdminKeys, tc.want)
			is.Equal(cfg.IgnoredAdminKeys(), 0)
		})
	}

	// The outer quotes are stripped before the config file's keys are
	// merged in.
	t.Run("with config file keys", func(t *testing.T) {
		is := is.New(t)
		t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "'"+k1+"\n"+k2+"'")
		t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
		cfg := DefaultConfig()
		cfg.InitialAdminKeys = []string{k2}
		is.NoErr(cfg.ParseEnv())
		is.Equal(cfg.InitialAdminKeys, []string{k1, k2})
		is.Equal(cfg.IgnoredAdminKeys(), 0)
	})

	// A variable that is set but blank, as in the Docker image, leaves the
	// config file's keys in place, and their quotes are not stripped as if
	// they were the environment's.
	t.Run("blank variable with config file keys", func(t *testing.T) {
		is := is.New(t)
		t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "")
		t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
		cfg := DefaultConfig()
		cfg.InitialAdminKeys = []string{"'" + k1, k2 + "'"}
		is.NoErr(cfg.ParseEnv())
		is.Equal(len(cfg.InitialAdminKeys), 0)
		is.Equal(cfg.IgnoredAdminKeys(), 2)
	})

	// Mismatched outer quotes are not stripped.
	t.Run("mismatched quotes", func(t *testing.T) {
		is := is.New(t)
		t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "'"+k1+"\n"+k2+`"`)
		t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
		cfg := DefaultConfig()
		is.NoErr(cfg.ParseEnv())
		is.Equal(len(cfg.InitialAdminKeys), 0)
		is.Equal(cfg.IgnoredAdminKeys(), 2)
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
	if err := cfg.ParseEnv(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Webhook.AllowedHosts, []string{"127.0.0.1", "hooks.internal:9000"}) {
		t.Fatalf("AllowedHosts = %q", cfg.Webhook.AllowedHosts)
	}
	if len(cfg.Webhook.ParsedAllowedHosts()) != 2 {
		t.Fatal("env allowlist not parsed")
	}

	// The written config file round-trips the list.
	if err := cfg.WriteConfig(); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("SOFT_SERVE_WEBHOOK_ALLOWED_HOSTS") //nolint:errcheck // restored by t.Setenv above
	t.Setenv("SOFT_SERVE_CONFIG_LOCATION", cfg.ConfigPath())
	fromFile := &Config{DataPath: cfg.DataPath}
	if err := fromFile.ParseFile(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fromFile.Webhook.AllowedHosts, []string{"127.0.0.1", "hooks.internal:9000"}) {
		t.Fatalf("AllowedHosts from file = %q", fromFile.Webhook.AllowedHosts)
	}
}

// A misspelt key is ignored, so it is reported with its dotted path and line
// for the server to warn about.
func TestUnknownConfigKeys(t *testing.T) {
	is := is.New(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	is.NoErr(os.WriteFile(path, []byte(`name: "x"
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
`), 0o600))
	cfg := DefaultConfig()
	is.NoErr(parseFile(cfg, path))
	is.True(!cfg.AdminOnlyCreate)
	is.Equal(cfg.UnknownKeys(), []UnknownKey{
		{Key: "admin_only_creat", Line: 2},
		{Key: "log_format", Line: 3},
		{Key: "ssh.listen_adr", Line: 9},
		{Key: "http.cors.allowed_origin", Line: 12},
		// The merged ssh keys land in git, which only knows listen_addr.
		{Key: "git.listen_adr", Line: 9},
	})

	// A config file without unknown keys reports none.
	is.NoErr(os.WriteFile(path, []byte("admin_only_create: true\n"), 0o600))
	cfg = DefaultConfig()
	is.NoErr(parseFile(cfg, path))
	is.True(cfg.AdminOnlyCreate)
	is.Equal(len(cfg.UnknownKeys()), 0)
}

// The generated config file and the README's sample use only known keys.
func TestConfigSamplesHaveNoUnknownKeys(t *testing.T) {
	is := is.New(t)
	var doc yaml.Node
	file, err := newConfigFile(DefaultConfig())
	is.NoErr(err)
	is.NoErr(yaml.Unmarshal([]byte(file), &doc))
	is.Equal(findUnknownKeys(&doc, reflect.TypeFor[Config](), ""), []UnknownKey(nil))

	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	is.NoErr(err)
	s := strings.ReplaceAll(string(readme), "\r\n", "\n")
	const start = "```yaml\n# Soft Serve Plus Server configurations\n"
	i := strings.Index(s, start)
	is.True(i >= 0) // README has the sample config
	sample := s[i+len("```yaml\n"):]
	sample = sample[:strings.Index(sample, "\n```")]
	doc = yaml.Node{}
	is.NoErr(yaml.Unmarshal([]byte(sample), &doc))
	is.Equal(findUnknownKeys(&doc, reflect.TypeFor[Config](), ""), []UnknownKey(nil))
}

// WriteConfig keeps the config, which can hold a database password, and a
// data directory it creates private to the server's user.
func TestWriteConfigIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	cfg := DefaultConfig()
	cfg.DataPath = filepath.Join(t.TempDir(), "data")
	if err := cfg.WriteConfig(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		cfg.DataPath:     0o700,
		cfg.ConfigPath(): 0o600,
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
