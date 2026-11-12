package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

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
// that is not in the data directory is still read from the working directory,
// where earlier versions looked.
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

// TestNewConfigFileAnonAccessAllowKeylessCommented checks that the written
// config documents anon_access and allow_keyless but leaves them commented
// out, even when the running config sets them: they override the database
// for as long as they are set, so a generated file must not persist them.
func TestNewConfigFileAnonAccessAllowKeylessCommented(t *testing.T) {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal([]byte("anon_access: admin-access\nallow_keyless: false\n"), cfg); err != nil {
		t.Fatal(err)
	}
	s, err := newConfigFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
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
		written, err := newConfigFile(cfg)
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
