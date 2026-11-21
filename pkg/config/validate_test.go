package config

import (
	"strings"
	"testing"
)

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
