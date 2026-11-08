package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestNewConfigFile(t *testing.T) {
	for _, cfg := range []*Config{
		DefaultConfig(),
		{},
	} {
		if s, err := newConfigFile(cfg); err != nil || s == "" {
			t.Errorf("newConfigFile(%+v) => %q, %v, want non-empty string", cfg, s, err)
		}
	}

	// A template error is returned rather than a file cut short where the
	// template stopped.
	if s, err := newConfigFile(nil); err == nil {
		t.Errorf("newConfigFile(nil) => %q, nil, want an error", s)
	}
	if err := writeConfig(nil, filepath.Join(t.TempDir(), "config.yaml")); err == nil {
		t.Error("writeConfig(nil) => nil, want an error")
	}
}

// TestNewConfigFileRoundTrip checks that every string the config file
// template writes reads back unchanged, on every platform. Windows paths are
// the common case: in a double-quoted YAML scalar, C:\a\x1 would otherwise
// read as the escapes \a and \x1.
func TestNewConfigFileRoundTrip(t *testing.T) {
	const winPath = `D:\a\soft-serve\x1\data\soft-serve.db?_pragma=busy_timeout(5000)`
	odd := "quote \" backslash \\ hash # colon: tab\t bell\a nel\u0085 ls\u2028 nonchar\ufffe\uffff é"

	cfg := DefaultConfig()
	cfg.Name = odd
	cfg.Log.Format = odd
	cfg.Log.TimeFormat = odd
	cfg.SSH.ListenAddr = odd
	cfg.SSH.PublicURL = odd
	cfg.SSH.KeyPath = `C:\Users\me\ssh\soft_serve_host_ed25519`
	cfg.SSH.ClientKeyPath = `\\server\share\#key: x`
	cfg.SSH.ClientStrictHostKeyChecking = odd
	cfg.SSH.ClientAllowedHosts = []string{odd, `C:\x`}
	cfg.SSH.KexAlgorithms = []string{odd, `C:\k`}
	cfg.SSH.Ciphers = []string{odd, `C:\c`}
	cfg.SSH.MACs = []string{odd, `C:\m`}
	cfg.Git.ListenAddr = odd
	cfg.Git.PublicURL = odd
	cfg.HTTP.ListenAddr = odd
	cfg.HTTP.TLSKeyPath = `C:\tls\key.pem`
	cfg.HTTP.TLSCertPath = `"C:\tls\cert.pem"`
	cfg.HTTP.PublicURL = odd
	cfg.Stats.ListenAddr = odd
	cfg.DB.Driver = odd
	cfg.DB.DataSource = winPath
	cfg.Jobs.MirrorPull = odd
	cfg.Jobs.LFSSweep = odd
	cfg.Webhook.AllowedHosts = []string{odd, `C:\y`}

	cfg.HTTP.CORS.AllowedOrigins = []string{odd, "https://example.com"}

	file, err := newConfigFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := yaml.Unmarshal([]byte(file), &got); err != nil {
		t.Fatalf("decode written config: %v", err)
	}

	for _, c := range []struct{ name, got, want string }{
		{"name", got.Name, cfg.Name},
		{"log.format", got.Log.Format, cfg.Log.Format},
		{"log.time_format", got.Log.TimeFormat, cfg.Log.TimeFormat},
		{"ssh.listen_addr", got.SSH.ListenAddr, cfg.SSH.ListenAddr},
		{"ssh.public_url", got.SSH.PublicURL, cfg.SSH.PublicURL},
		{"ssh.key_path", got.SSH.KeyPath, cfg.SSH.KeyPath},
		{"ssh.client_key_path", got.SSH.ClientKeyPath, cfg.SSH.ClientKeyPath},
		{"ssh.client_strict_host_key_checking", got.SSH.ClientStrictHostKeyChecking, cfg.SSH.ClientStrictHostKeyChecking},
		{"git.listen_addr", got.Git.ListenAddr, cfg.Git.ListenAddr},
		{"git.public_url", got.Git.PublicURL, cfg.Git.PublicURL},
		{"http.listen_addr", got.HTTP.ListenAddr, cfg.HTTP.ListenAddr},
		{"http.tls_key_path", got.HTTP.TLSKeyPath, cfg.HTTP.TLSKeyPath},
		{"http.tls_cert_path", got.HTTP.TLSCertPath, cfg.HTTP.TLSCertPath},
		{"http.public_url", got.HTTP.PublicURL, cfg.HTTP.PublicURL},
		{"stats.listen_addr", got.Stats.ListenAddr, cfg.Stats.ListenAddr},
		{"db.driver", got.DB.Driver, cfg.DB.Driver},
		{"db.data_source", got.DB.DataSource, cfg.DB.DataSource},
		{"jobs.mirror_pull", got.Jobs.MirrorPull, cfg.Jobs.MirrorPull},
		{"jobs.lfs_sweep", got.Jobs.LFSSweep, cfg.Jobs.LFSSweep},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"ssh.client_allowed_hosts", got.SSH.ClientAllowedHosts, cfg.SSH.ClientAllowedHosts},
		{"webhook.allowed_hosts", got.Webhook.AllowedHosts, cfg.Webhook.AllowedHosts},
		{"ssh.kex_algorithms", got.SSH.KexAlgorithms, cfg.SSH.KexAlgorithms},
		{"ssh.ciphers", got.SSH.Ciphers, cfg.SSH.Ciphers},
		{"ssh.macs", got.SSH.MACs, cfg.SSH.MACs},
		{"http.cors.allowed_origins", got.HTTP.CORS.AllowedOrigins, cfg.HTTP.CORS.AllowedOrigins},
	} {
		if len(c.got) != len(c.want) {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
			continue
		}
		for i := range c.got {
			if c.got[i] != c.want[i] {
				t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
				break
			}
		}
	}
}

// TestReadmeConfigMatchesGenerated keeps the sample config.yaml in the
// README identical to the file the server writes on first start.
func TestReadmeConfigMatchesGenerated(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Git may check README.md out with CRLF line endings (the windows CI
	// runner does).
	doc := strings.ReplaceAll(string(readme), "\r\n", "\n")
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

	// The default key paths are joined with the OS separator; the README
	// shows them as written on Unix.
	cfg := DefaultConfig()
	cfg.SSH.KeyPath = filepath.ToSlash(cfg.SSH.KeyPath)
	cfg.SSH.ClientKeyPath = filepath.ToSlash(cfg.SSH.ClientKeyPath)
	want := newConfigFile(cfg)
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
				t.Fatalf("README sample config differs from the generated file at line %d:\nREADME:    %q\ngenerated: %q\nRegenerate it from newConfigFile(DefaultConfig()).", n+1, g, w)
			}
		}
	}
}
