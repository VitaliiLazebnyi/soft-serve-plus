// Package config loads and validates the server's configuration from
// config.yaml and the SOFT_SERVE_* environment variables.
package config

import (
	"bytes"
	_ "embed" // config.yaml
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/caarlos0/env/v11"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v4"
	"golang.org/x/crypto/ssh"
)

// binPath returns the path of the running soft binary, with forward
// slashes, or "soft" when it cannot be found. It is read once, when first
// needed, rather than in an init function.
var binPath = sync.OnceValue(func() string {
	if ex, err := os.Executable(); err == nil {
		return filepath.ToSlash(ex)
	}
	return "soft"
})

const (
	// StrictHostKeyCheckingAcceptNew records an unknown host key on first
	// contact and refuses a changed one afterwards.
	StrictHostKeyCheckingAcceptNew = "accept-new"
	// StrictHostKeyCheckingYes refuses any host not already in known_hosts.
	StrictHostKeyCheckingYes = "yes"
)

// SSHConfig is the configuration for the SSH server.
type SSHConfig struct {
	// Enabled toggles the SSH server on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the SSH server will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`

	// PublicURL is the public URL of the SSH server.
	PublicURL string `env:"PUBLIC_URL" yaml:"public_url"`

	// KeyPath is the path to the SSH server's private key.
	KeyPath string `env:"KEY_PATH" yaml:"key_path"`

	// ClientKeyPath is the path to the server's client private key.
	ClientKeyPath string `env:"CLIENT_KEY_PATH" yaml:"client_key_path"`

	// ClientStrictHostKeyChecking is the StrictHostKeyChecking value the
	// server passes to ssh when cloning or syncing from ssh remotes. Valid
	// values are "accept-new" (record a host key on first contact, refuse a
	// changed one) and "yes" (only hosts already in known_hosts).
	ClientStrictHostKeyChecking string `env:"CLIENT_STRICT_HOST_KEY_CHECKING" yaml:"client_strict_host_key_checking"`

	// ClientAllowedHosts lists the ssh hosts users may import from or mirror.
	// The client key is presented to every one of them. Entries are
	// hostnames, IP addresses, or CIDR ranges with an optional port. When
	// empty, only server admins may use ssh remotes; when not, it binds
	// admins too.
	ClientAllowedHosts []string `env:"CLIENT_ALLOWED_HOSTS" yaml:"client_allowed_hosts"`

	// MaxTimeout is the maximum number of seconds a connection can take.
	MaxTimeout int `env:"MAX_TIMEOUT" yaml:"max_timeout"`

	// IdleTimeout is the number of seconds a connection can be idle before it is closed.
	IdleTimeout int `env:"IDLE_TIMEOUT" yaml:"idle_timeout"`

	// MaxConnections is the maximum number of SSH connections open at once.
	// Zero means no limit.
	MaxConnections int `env:"MAX_CONNECTIONS" yaml:"max_connections"`

	// MaxConnectionsPerIP is the maximum number of SSH connections open at
	// once from one client address (one IPv4 address, or one IPv6 /64).
	// Zero means no limit.
	MaxConnectionsPerIP int `env:"MAX_CONNECTIONS_PER_IP" yaml:"max_connections_per_ip"`

	// PublicKeyAuthAlgorithms lists the signature algorithms the SSH server
	// accepts for client public key authentication. When empty,
	// DefaultSSHPublicKeyAuthAlgorithms is used.
	PublicKeyAuthAlgorithms []string `env:"PUBLIC_KEY_AUTH_ALGORITHMS" yaml:"public_key_auth_algorithms"`

	// allowedHosts is ClientAllowedHosts parsed by Validate.
	allowedHosts ssrf.AllowedHosts
}

// ParsedClientAllowedHosts returns the parsed ssh allowlist. It is empty
// until Validate has run. The slice is a copy, so a caller may change it.
func (c *SSHConfig) ParsedClientAllowedHosts() ssrf.AllowedHosts {
	return slices.Clone(c.allowedHosts)
}

// GitConfig is the Git daemon configuration for the server.
type GitConfig struct {
	// Enabled toggles the Git daemon on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the Git daemon will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`

	// PublicURL is the public URL of the Git daemon server.
	PublicURL string `env:"PUBLIC_URL" yaml:"public_url"`

	// MaxTimeout is the maximum number of seconds a connection can take.
	MaxTimeout int `env:"MAX_TIMEOUT" yaml:"max_timeout"`

	// IdleTimeout is the number of seconds a connection can be idle before it is closed.
	IdleTimeout int `env:"IDLE_TIMEOUT" yaml:"idle_timeout"`

	// MaxConnections is the maximum number of concurrent connections. It must
	// be at least 1 while the daemon is enabled.
	MaxConnections int `env:"MAX_CONNECTIONS" yaml:"max_connections"`

	// MaxConnectionsPerIP is the maximum number of concurrent connections
	// from one client address (one IPv4 address, or one IPv6 /64). 0 means
	// no limit. Loopback clients are not limited by it; behind a TCP proxy
	// that connects from a non-loopback address every client has the
	// proxy's address, so set it to 0 there and limit clients at the proxy.
	MaxConnectionsPerIP int `env:"MAX_CONNECTIONS_PER_IP" yaml:"max_connections_per_ip"`
}

// CORSConfig is the CORS configuration for the server.
type CORSConfig struct {
	AllowedHeaders []string `env:"ALLOWED_HEADERS" yaml:"allowed_headers"`

	AllowedOrigins []string `env:"ALLOWED_ORIGINS" yaml:"allowed_origins"`

	AllowedMethods []string `env:"ALLOWED_METHODS" yaml:"allowed_methods"`
}

// HTTPConfig is the HTTP configuration for the server.
type HTTPConfig struct {
	// Enabled toggles the HTTP server on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the HTTP server will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`

	// TLSKeyPath is the path to the TLS private key.
	TLSKeyPath string `env:"TLS_KEY_PATH" yaml:"tls_key_path"`

	// TLSCertPath is the path to the TLS certificate.
	TLSCertPath string `env:"TLS_CERT_PATH" yaml:"tls_cert_path"`

	// PublicURL is the public URL of the HTTP server.
	PublicURL string `env:"PUBLIC_URL" yaml:"public_url"`

	// MaxTimeout is the maximum number of seconds a request can take. 0
	// means no limit.
	MaxTimeout int `env:"MAX_TIMEOUT" yaml:"max_timeout"`

	// IdleTimeout is the number of seconds a request can go without reading
	// from or writing to the client before it is closed. 0 means no limit.
	IdleTimeout int `env:"IDLE_TIMEOUT" yaml:"idle_timeout"`

	// MaxConnections is the maximum number of git and LFS requests served at
	// once. 0 means no limit.
	MaxConnections int `env:"MAX_CONNECTIONS" yaml:"max_connections"`

	// MaxConnectionsPerIP is the maximum number of git and LFS requests
	// served at once from one client address (one IPv4 address, or one IPv6
	// /64). 0 means no limit. Loopback clients are not limited by it, so a
	// reverse proxy on the same host is not; behind a reverse proxy that
	// connects from a non-loopback address, such as another host or another
	// container, every client has the proxy's address, so set it to 0 there
	// and limit clients at the proxy.
	MaxConnectionsPerIP int `env:"MAX_CONNECTIONS_PER_IP" yaml:"max_connections_per_ip"`

	// CORS is the cross-origin configuration for the HTTP server.
	CORS CORSConfig `envPrefix:"CORS_" yaml:"cors"`
}

// StatsConfig is the configuration for the stats server.
type StatsConfig struct {
	// Enabled toggles the Stats server on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the stats server will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`
}

// LogConfig is the logger configuration.
type LogConfig struct {
	// Format is the format of the logs.
	// Valid values are "json", "logfmt", and "text".
	Format string `env:"FORMAT" yaml:"format"`

	// Time format for the log `time` field.
	// Format must be described in Golang's time format.
	TimeFormat string `env:"TIME_FORMAT" yaml:"time_format"`

	// Path to a file to write logs to.
	// If not set, logs will be written to stderr.
	Path string `env:"PATH" yaml:"path"`
}

// DBConfig is the database connection configuration.
type DBConfig struct {
	// Driver is the driver for the database.
	Driver string `env:"DRIVER" yaml:"driver"`

	// DataSource is the database data source name.
	DataSource string `env:"DATA_SOURCE" yaml:"data_source"`

	// MaxOpenConns is the maximum number of open connections to the
	// database. 0 (the default) means no limit. The pool keeps up to 2 idle
	// connections, database/sql's default, and fewer when this is lower.
	MaxOpenConns int `env:"MAX_OPEN_CONNS" yaml:"max_open_conns"`
}

// LFSConfig is the configuration for Git LFS.
type LFSConfig struct {
	// Enabled is whether or not Git LFS is enabled.
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// SSHEnabled is whether or not Git LFS over SSH is enabled.
	// This is only used if LFS is enabled.
	SSHEnabled bool `env:"SSH_ENABLED" yaml:"ssh_enabled"`
}

// JobsConfig is the configuration for cron jobs.
type JobsConfig struct {
	// MirrorPull is the cron spec for syncing pull mirrors. An empty value
	// in the config file disables mirror syncing. An empty environment
	// variable leaves the config file's value in place.
	MirrorPull string `env:"MIRROR_PULL" yaml:"mirror_pull"`
}

// WebhookConfig is the configuration for repository webhooks.
type WebhookConfig struct {
	// AllowedHosts lists the private and internal destinations webhooks
	// may be sent to, which are otherwise refused. Entries are hostnames,
	// IP addresses, or CIDR ranges with an optional port. An IP or CIDR
	// entry allows the addresses it covers for any hostname. A hostname
	// entry allows that name to resolve to loopback, RFC1918, CGNAT or IPv6
	// unique local addresses, whatever they are when a webhook is sent.
	// Link-local and cloud metadata addresses need an IP or CIDR entry
	// inside their own range. Empty (the default) allows public addresses
	// only.
	AllowedHosts []string `env:"ALLOWED_HOSTS" yaml:"allowed_hosts"`

	// allowedHosts is AllowedHosts parsed by Validate.
	allowedHosts ssrf.AllowedHosts
}

// ParsedAllowedHosts returns the parsed webhook allowlist. It is empty until
// Validate has run. The slice is a copy, so a caller may change it.
func (c *WebhookConfig) ParsedAllowedHosts() ssrf.AllowedHosts {
	return slices.Clone(c.allowedHosts)
}

// Config is the configuration for Soft Serve.
type Config struct {
	// Name is the name of the server.
	Name string `env:"NAME" yaml:"name"`

	// SSH is the configuration for the SSH server.
	SSH SSHConfig `envPrefix:"SSH_" yaml:"ssh"`

	// Git is the configuration for the Git daemon.
	Git GitConfig `envPrefix:"GIT_" yaml:"git"`

	// HTTP is the configuration for the HTTP server.
	HTTP HTTPConfig `envPrefix:"HTTP_" yaml:"http"`

	// Stats is the configuration for the stats server.
	Stats StatsConfig `envPrefix:"STATS_" yaml:"stats"`

	// Log is the logger configuration.
	Log LogConfig `envPrefix:"LOG_" yaml:"log"`

	// DB is the database configuration.
	DB DBConfig `envPrefix:"DB_" yaml:"db"`

	// LFS is the configuration for Git LFS.
	LFS LFSConfig `envPrefix:"LFS_" yaml:"lfs"`

	// Jobs is the configuration for cron jobs
	Jobs JobsConfig `envPrefix:"JOBS_" yaml:"jobs"`

	// Webhook is the configuration for repository webhooks.
	Webhook WebhookConfig `envPrefix:"WEBHOOK_" yaml:"webhook"`

	// InitialAdminKeys is a list of public keys that will be added to the list of admins.
	InitialAdminKeys []string `env:"INITIAL_ADMIN_KEYS" envSeparator:"\n" yaml:"initial_admin_keys"`

	// AnonAccess overrides the access level for anonymous users.
	//
	// If set (non-nil), this takes precedence over the "anon-access" value
	// stored in the database (see the `settings` command) on every read, for
	// as long as it remains set. It is not a one-time default: it behaves
	// like InitialAdminKeys, not like a seed value. Invalid values are
	// rejected at parse time by access.Level's TextUnmarshaler.
	//
	// This is intended for scripted, non-production bootstrapping only.
	AnonAccess *access.Level `env:"ANON_ACCESS" yaml:"anon_access"`

	// AllowKeyless overrides whether keyless (no public key) connections
	// are allowed.
	//
	// If set (non-nil), this takes precedence over the "allow-keyless"
	// value stored in the database on every read, same as AnonAccess above.
	// A nil value means "no override": fall back to the database.
	AllowKeyless *bool `env:"ALLOW_KEYLESS" yaml:"allow_keyless"`

	// AdminOnlyCreate restricts repository creation to server admins.
	//
	// When true, registered non-admins and anonymous users get read-only on
	// repository names that do not exist yet, so repo create, repo import,
	// push-to-create and rename-to-a-new-name are refused. Existing
	// repositories are not affected. When false (the default), access follows
	// upstream Soft Serve: registered users may create repositories.
	AdminOnlyCreate bool `env:"ADMIN_ONLY_CREATE" yaml:"admin_only_create"`

	// MaxReposPerUser caps how many repositories each user may create, for
	// every way of creating one: repo create, repo import and
	// push-to-create. Server admins (admin accounts or initial_admin_keys)
	// are exempt. Repositories created without an account all count
	// together as one user. A user at the limit is refused; the
	// repositories they already have are kept. Imports still running count
	// against the limit. 0 (the default) means no limit.
	MaxReposPerUser int `env:"MAX_REPOS_PER_USER" yaml:"max_repos_per_user"`

	// DefaultPrivate makes repositories created without an explicit
	// visibility private: push-to-create over SSH and HTTP, repo create and
	// repo import without --private, and default_repo on boot. An explicit
	// --private=false still creates a public repository. An anonymous
	// caller could not access a private repository, so with this set an
	// anonymous create that does not choose a visibility is refused (see
	// Backend.DefaultPrivate). false (the default) keeps upstream
	// behaviour: such repositories are public.
	DefaultPrivate bool `env:"DEFAULT_PRIVATE" yaml:"default_private"`

	// DefaultRepo is a repository name to create on boot if it does not
	// already exist. Leave empty to disable.
	DefaultRepo string `env:"DEFAULT_REPO" yaml:"default_repo"`

	// DataPath is the path to the directory where Soft Serve will store its data.
	DataPath string `env:"DATA_PATH" yaml:"-"`

	// Version is the version of the soft binary, which webhook requests
	// name in their User-Agent. It is not a setting: neither the config
	// file nor the environment sets it, and Environ leaves it out. The soft
	// command sets it once it has loaded the config.
	Version string `yaml:"-"`

	// adminKeys is InitialAdminKeys parsed by Validate. Validate is the
	// only place the entries are read as keys or key files, so admin
	// identity does not depend on the data directory's contents after
	// start-up.
	adminKeys []ssh.PublicKey

	// unreadableAdminKeyFiles and ignoredAdminKeys record the
	// InitialAdminKeys entries Validate dropped: key files that could not
	// be read or parsed, and entries that are neither a key nor a file path.
	// Validate rewrites InitialAdminKeys to the valid keys only, so these
	// are the only record of them. They are sets that each Validate adds
	// to: a config validated again, as a caller that changes a loaded
	// config does, still knows what the first Validate dropped.
	unreadableAdminKeyFiles map[string]struct{}
	ignoredAdminKeys        map[string]struct{}

	// filePath is the absolute path of the config file Load resolved.
	filePath string

	// unknownKeys records the config file keys no setting is read from,
	// and removed the removed settings the file or the environment sets.
	unknownKeys []UnknownKey
	removed     []RemovedSetting
}

// UnknownKey is a key in the config file that no setting is read from,
// such as a misspelt one. It is ignored.
type UnknownKey struct {
	// Key is the dotted path of the key, such as "ssh.listen_adr".
	Key string
	// Line is the line of the key in the config file.
	Line int
}

// Environ returns the config as a list of environment variables.
func (c *Config) Environ() []string {
	envs := []string{
		fmt.Sprintf("SOFT_SERVE_BIN_PATH=%s", binPath()),
	}

	// TestEnvironRoundTrip checks that the environment Load reads gives every field back, so
	// a new field must be added here. Log.Path is left out on purpose: the
	// server gives hooks their own log file.
	envs = append(envs, []string{
		fmt.Sprintf("SOFT_SERVE_CONFIG_LOCATION=%s", c.FilePath()),
		fmt.Sprintf("SOFT_SERVE_DATA_PATH=%s", c.DataPath),
		fmt.Sprintf("SOFT_SERVE_NAME=%s", c.Name),
		fmt.Sprintf("SOFT_SERVE_INITIAL_ADMIN_KEYS=%s", strings.Join(c.InitialAdminKeys, "\n")),
		fmt.Sprintf("SOFT_SERVE_DEFAULT_REPO=%s", c.DefaultRepo),
		fmt.Sprintf("SOFT_SERVE_SSH_ENABLED=%t", c.SSH.Enabled),
		fmt.Sprintf("SOFT_SERVE_SSH_LISTEN_ADDR=%s", c.SSH.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_SSH_PUBLIC_URL=%s", c.SSH.PublicURL),
		fmt.Sprintf("SOFT_SERVE_SSH_KEY_PATH=%s", c.SSH.KeyPath),
		fmt.Sprintf("SOFT_SERVE_SSH_CLIENT_KEY_PATH=%s", c.SSH.ClientKeyPath),
		fmt.Sprintf("SOFT_SERVE_SSH_CLIENT_STRICT_HOST_KEY_CHECKING=%s", c.SSH.ClientStrictHostKeyChecking),
		fmt.Sprintf("SOFT_SERVE_SSH_CLIENT_ALLOWED_HOSTS=%s", strings.Join(c.SSH.ClientAllowedHosts, ",")),
		fmt.Sprintf("SOFT_SERVE_SSH_MAX_TIMEOUT=%d", c.SSH.MaxTimeout),
		fmt.Sprintf("SOFT_SERVE_SSH_IDLE_TIMEOUT=%d", c.SSH.IdleTimeout),
		fmt.Sprintf("SOFT_SERVE_SSH_MAX_CONNECTIONS=%d", c.SSH.MaxConnections),
		fmt.Sprintf("SOFT_SERVE_SSH_MAX_CONNECTIONS_PER_IP=%d", c.SSH.MaxConnectionsPerIP),
		fmt.Sprintf("SOFT_SERVE_SSH_PUBLIC_KEY_AUTH_ALGORITHMS=%s", strings.Join(c.SSH.PublicKeyAuthAlgorithms, ",")),
		fmt.Sprintf("SOFT_SERVE_GIT_ENABLED=%t", c.Git.Enabled),
		fmt.Sprintf("SOFT_SERVE_GIT_LISTEN_ADDR=%s", c.Git.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_GIT_PUBLIC_URL=%s", c.Git.PublicURL),
		fmt.Sprintf("SOFT_SERVE_GIT_MAX_TIMEOUT=%d", c.Git.MaxTimeout),
		fmt.Sprintf("SOFT_SERVE_GIT_IDLE_TIMEOUT=%d", c.Git.IdleTimeout),
		fmt.Sprintf("SOFT_SERVE_GIT_MAX_CONNECTIONS=%d", c.Git.MaxConnections),
		fmt.Sprintf("SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP=%d", c.Git.MaxConnectionsPerIP),
		fmt.Sprintf("SOFT_SERVE_HTTP_ENABLED=%t", c.HTTP.Enabled),
		fmt.Sprintf("SOFT_SERVE_HTTP_LISTEN_ADDR=%s", c.HTTP.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_HTTP_TLS_KEY_PATH=%s", c.HTTP.TLSKeyPath),
		fmt.Sprintf("SOFT_SERVE_HTTP_TLS_CERT_PATH=%s", c.HTTP.TLSCertPath),
		fmt.Sprintf("SOFT_SERVE_HTTP_PUBLIC_URL=%s", c.HTTP.PublicURL),
		fmt.Sprintf("SOFT_SERVE_HTTP_MAX_TIMEOUT=%d", c.HTTP.MaxTimeout),
		fmt.Sprintf("SOFT_SERVE_HTTP_IDLE_TIMEOUT=%d", c.HTTP.IdleTimeout),
		fmt.Sprintf("SOFT_SERVE_HTTP_MAX_CONNECTIONS=%d", c.HTTP.MaxConnections),
		fmt.Sprintf("SOFT_SERVE_HTTP_MAX_CONNECTIONS_PER_IP=%d", c.HTTP.MaxConnectionsPerIP),
		fmt.Sprintf("SOFT_SERVE_HTTP_CORS_ALLOWED_HEADERS=%s", strings.Join(c.HTTP.CORS.AllowedHeaders, ",")),
		fmt.Sprintf("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS=%s", strings.Join(c.HTTP.CORS.AllowedOrigins, ",")),
		fmt.Sprintf("SOFT_SERVE_HTTP_CORS_ALLOWED_METHODS=%s", strings.Join(c.HTTP.CORS.AllowedMethods, ",")),
		fmt.Sprintf("SOFT_SERVE_STATS_ENABLED=%t", c.Stats.Enabled),
		fmt.Sprintf("SOFT_SERVE_STATS_LISTEN_ADDR=%s", c.Stats.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_LOG_FORMAT=%s", c.Log.Format),
		fmt.Sprintf("SOFT_SERVE_LOG_TIME_FORMAT=%s", c.Log.TimeFormat),
		fmt.Sprintf("SOFT_SERVE_DB_DRIVER=%s", c.DB.Driver),
		fmt.Sprintf("SOFT_SERVE_DB_DATA_SOURCE=%s", c.DB.DataSource),
		fmt.Sprintf("SOFT_SERVE_DB_MAX_OPEN_CONNS=%d", c.DB.MaxOpenConns),
		fmt.Sprintf("SOFT_SERVE_LFS_ENABLED=%t", c.LFS.Enabled),
		fmt.Sprintf("SOFT_SERVE_LFS_SSH_ENABLED=%t", c.LFS.SSHEnabled),
		fmt.Sprintf("SOFT_SERVE_JOBS_MIRROR_PULL=%s", c.Jobs.MirrorPull),
		fmt.Sprintf("SOFT_SERVE_WEBHOOK_ALLOWED_HOSTS=%s", strings.Join(c.Webhook.AllowedHosts, ",")),
	}...)

	// AnonAccess and AllowKeyless are tri-state overrides: only emit them
	// when explicitly set, so a subprocess parsing these envs sees the same
	// "unset" state (rather than an empty string coercing to a zero value).
	if c.AnonAccess != nil {
		envs = append(envs, fmt.Sprintf("SOFT_SERVE_ANON_ACCESS=%s", c.AnonAccess.String()))
	}

	if c.AllowKeyless != nil {
		envs = append(envs, fmt.Sprintf("SOFT_SERVE_ALLOW_KEYLESS=%t", *c.AllowKeyless))
	}

	envs = append(envs, fmt.Sprintf("SOFT_SERVE_ADMIN_ONLY_CREATE=%t", c.AdminOnlyCreate))
	envs = append(envs, fmt.Sprintf("SOFT_SERVE_MAX_REPOS_PER_USER=%d", c.MaxReposPerUser))
	envs = append(envs, fmt.Sprintf("SOFT_SERVE_DEFAULT_PRIVATE=%t", c.DefaultPrivate))

	return envs
}

// IsDebug returns true if the server is running in debug mode.
func IsDebug() bool {
	debug, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_DEBUG"))
	return debug
}

// IsVerbose returns true if the server is running in verbose mode.
// Verbose mode is only enabled if debug mode is enabled.
func IsVerbose() bool {
	verbose, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_VERBOSE"))
	return IsDebug() && verbose
}

// readFile decodes the config file at path into c, over the values c
// already holds, and records the keys no setting is read from. It does not
// validate: load does that once, after the environment.
func (c *Config) readFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// An empty or comment-only file has no YAML document and decodes to
	// io.EOF. Treat it like an empty document and keep the defaults.
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode config: %w", err)
	}

	// Unknown keys are reported, not rejected, so a config written for
	// another version still loads.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	c.unknownKeys = findUnknownKeys(&doc, reflect.TypeFor[Config](), "")
	c.takeRemovedKeys(&doc)
	return nil
}

// findUnknownKeys returns the keys of the mappings in n that no field of t,
// or of the structs nested in it, is decoded from. prefix is the dotted
// path of n.
func findUnknownKeys(n *yaml.Node, t reflect.Type, prefix string) []UnknownKey {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch n.Kind {
	case yaml.DocumentNode:
		var keys []UnknownKey
		for _, c := range n.Content {
			keys = append(keys, findUnknownKeys(c, t, prefix)...)
		}
		return keys
	case yaml.AliasNode:
		return findUnknownKeys(n.Alias, t, prefix)
	case yaml.MappingNode:
	default:
		return nil
	}
	if t.Kind() != reflect.Struct {
		return nil
	}

	fields := make(map[string]reflect.Type, t.NumField())
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = strings.ToLower(f.Name)
		}
		fields[name] = f.Type
	}

	var keys []UnknownKey
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Tag == "!!merge" {
			// The keys of a "<<" merge belong to this mapping.
			if v.Kind == yaml.SequenceNode {
				for _, m := range v.Content {
					keys = append(keys, findUnknownKeys(m, t, prefix)...)
				}
			} else {
				keys = append(keys, findUnknownKeys(v, t, prefix)...)
			}
			continue
		}
		ft, ok := fields[k.Value]
		if !ok {
			keys = append(keys, UnknownKey{Key: prefix + k.Value, Line: k.Line})
			continue
		}
		keys = append(keys, findUnknownKeys(v, ft, prefix+k.Value+".")...)
	}
	return keys
}

// UnknownKeys returns the keys of the parsed config file that no setting is
// read from, in file order. They are ignored. A removed setting is not
// among them; see RemovedSettings.
func (c *Config) UnknownKeys() []UnknownKey {
	return slices.Clone(c.unknownKeys)
}

// removedSetting is a setting the server does not read. A config that
// still sets one loads, and the server warns about it, saying what it does
// instead. A config file that holds the value older servers wrote there,
// oldDefault, gets no warning: the server does what that value said.
type removedSetting struct {
	key, env, now string
	// oldDefault is the value older servers wrote to config.yaml: a
	// scalar's text, or "[]" for an empty list. Silence is right only
	// while the server does what it says:
	// TestRemovedSettingsOldDefaults pins it to both, so a change of a fixed
	// value fails there rather than silence a config that still holds the
	// old one.
	oldDefault []string
}

// removedSettings are the settings replaced by fixed values, because no
// realistic operator tunes them.
var removedSettings = []removedSetting{
	{"ssh.kex_algorithms", "SOFT_SERVE_SSH_KEX_ALGORITHMS", "the server offers the key exchanges golang.org/x/crypto/ssh considers secure", []string{"[]"}},
	{"ssh.ciphers", "SOFT_SERVE_SSH_CIPHERS", "the server offers the ciphers golang.org/x/crypto/ssh considers secure", []string{"[]"}},
	{"ssh.macs", "SOFT_SERVE_SSH_MACS", "the server offers the MACs golang.org/x/crypto/ssh considers secure, except hmac-sha1", []string{"[]"}},
	{"ssh.max_sessions", "SOFT_SERVE_SSH_MAX_SESSIONS", fmt.Sprintf("a connection may have %d sessions open at once", SSHMaxSessions), []string{"10"}},
	{"ssh.handshake_timeout", "SOFT_SERVE_SSH_HANDSHAKE_TIMEOUT", fmt.Sprintf("a client has %s to finish the SSH handshake and authenticate", SSHHandshakeTimeout), []string{"120"}},
	{"db.max_idle_conns", "SOFT_SERVE_DB_MAX_IDLE_CONNS", "the pool keeps database/sql's default of 2 idle connections", []string{"0"}},
	{"db.conn_max_lifetime", "SOFT_SERVE_DB_CONN_MAX_LIFETIME", "database connections are reused without a time limit", []string{"0"}},
	{"db.conn_max_idle_time", "SOFT_SERVE_DB_CONN_MAX_IDLE_TIME", "idle database connections are kept without a time limit", []string{"0"}},
	{"jobs.lfs_sweep", "SOFT_SERVE_JOBS_LFS_SWEEP", "the LFS temporary file sweep runs hourly", []string{"@every 1h"}},
	{"max_blob_size", "SOFT_SERVE_MAX_BLOB_SIZE", fmt.Sprintf("files over %d bytes are not read into memory to display", MaxBlobSize), []string{"1048576"}},
}

// RemovedSetting is a setting the config file or the environment still
// sets that the server does not read. It is ignored.
type RemovedSetting struct {
	// Key is the setting's config file key, such as "ssh.macs".
	Key string
	// Source is where it is set: "line 12" of the config file, or the
	// environment variable.
	Source string
	// Now says what the server does instead.
	Now string
	// InFile is true for a key in the config file, which can be deleted,
	// and false for an environment variable, which can be unset.
	InFile bool
}

// RemovedSettings returns the removed settings the config file, in file
// order, and then the environment still set. They are ignored.
func (c *Config) RemovedSettings() []RemovedSetting {
	return slices.Clone(c.removed)
}

// takeRemovedKeys moves the removed settings out of the unknown keys of
// the config file doc. One that holds the value older servers wrote there
// is dropped without a record: every upgraded config.yaml has those, and
// the server does what they say.
func (c *Config) takeRemovedKeys(doc *yaml.Node) {
	c.unknownKeys = slices.DeleteFunc(c.unknownKeys, func(k UnknownKey) bool {
		for _, r := range removedSettings {
			if k.Key != r.key {
				continue
			}
			if !isOldDefault(lookupKey(doc, strings.Split(r.key, ".")), r.oldDefault) {
				c.removed = append(c.removed, RemovedSetting{Key: r.key, Source: fmt.Sprintf("line %d", k.Line), Now: r.now, InFile: true})
			}
			return true
		}
		return false
	})
}

// lookupKey returns the value of the key at path in the mappings of n, or
// nil. It follows aliases but not merges.
func lookupKey(n *yaml.Node, path []string) *yaml.Node {
	for n != nil && (n.Kind == yaml.DocumentNode || n.Kind == yaml.AliasNode) {
		if n.Kind == yaml.AliasNode {
			n = n.Alias
		} else if len(n.Content) > 0 {
			n = n.Content[0]
		} else {
			return nil
		}
	}
	if len(path) == 0 {
		return n
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == path[0] {
			return lookupKey(n.Content[i+1], path[1:])
		}
	}
	return nil
}

// isOldDefault reports whether v is one of defaults: a scalar's text, or
// "[]" for an empty list.
func isOldDefault(v *yaml.Node, defaults []string) bool {
	for v != nil && v.Kind == yaml.AliasNode {
		v = v.Alias
	}
	if v == nil {
		return false
	}
	for _, d := range defaults {
		switch {
		case d == "[]" && v.Kind == yaml.SequenceNode && len(v.Content) == 0:
			return true
		case v.Kind == yaml.ScalarNode && v.Value == d:
			return true
		}
	}
	return false
}

// readRemovedEnv records the removed settings the environment sets. An
// empty variable sets nothing, as for the other settings.
func (c *Config) readRemovedEnv() {
	for _, r := range removedSettings {
		if os.Getenv(r.env) != "" {
			c.removed = append(c.removed, RemovedSetting{Key: r.key, Source: r.env, Now: r.now})
		}
	}
}

// readEnv applies the SOFT_SERVE_* environment variables to c, over the
// values c already holds. Like readFile, it does not validate.
func (c *Config) readEnv() error {
	// Initial admin keys from the config file and the environment are
	// merged, not overridden. Keep the file keys to add them back below.
	fileAdminKeys := slices.Clone(c.InitialAdminKeys)

	if err := env.ParseWithOptions(c, env.Options{
		Prefix: "SOFT_SERVE_",
	}); err != nil {
		return fmt.Errorf("parse environment variables: %w", err)
	}
	c.clearEmptyEnvLists()
	// The env parser leaves the list alone when the variable is set but
	// blank, as in the Docker image, so it then still holds the file's keys,
	// whose quotes are not the environment's to strip.
	if v := os.Getenv("SOFT_SERVE_INITIAL_ADMIN_KEYS"); v != "" {
		c.InitialAdminKeys = trimOuterQuotes(c.InitialAdminKeys)
	}

	// The env parser leaves the list alone when the variable is unset and
	// replaces it otherwise. Either way, the env keys come first, followed
	// by each file key not already listed.
	c.InitialAdminKeys = mergeAdminKeys(c.InitialAdminKeys, fileAdminKeys)
	c.readRemovedEnv()
	return nil
}

// clearEmptyEnvLists empties each list setting whose variable is set to the
// empty string. The env parser leaves a field alone when its variable is
// empty, which suits a scalar but means a list from the config file could
// not be cleared from the environment, and an empty list is a policy of its
// own for some (ssh.client_allowed_hosts lets only admins import over ssh).
// Initial admin keys are left out: the environment's keys are merged with
// the file's, so a blank variable keeps the file's keys.
func (c *Config) clearEmptyEnvLists() {
	for name, list := range map[string]*[]string{
		"SOFT_SERVE_SSH_CLIENT_ALLOWED_HOSTS":       &c.SSH.ClientAllowedHosts,
		"SOFT_SERVE_SSH_PUBLIC_KEY_AUTH_ALGORITHMS": &c.SSH.PublicKeyAuthAlgorithms,
		"SOFT_SERVE_HTTP_CORS_ALLOWED_HEADERS":      &c.HTTP.CORS.AllowedHeaders,
		"SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS":      &c.HTTP.CORS.AllowedOrigins,
		"SOFT_SERVE_HTTP_CORS_ALLOWED_METHODS":      &c.HTTP.CORS.AllowedMethods,
		"SOFT_SERVE_WEBHOOK_ALLOWED_HOSTS":          &c.Webhook.AllowedHosts,
	} {
		if v, ok := os.LookupEnv(name); ok && v == "" {
			*list = nil
		}
	}
}

// mergeAdminKeys returns the entries of a followed by those of b, each
// listed once. Entries are compared after trimming surrounding space.
func mergeAdminKeys(a, b []string) []string {
	merged := make([]string, 0, len(a)+len(b))
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, k := range slices.Concat(a, b) {
		key := strings.TrimSpace(k)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, k)
	}
	return merged
}

// Load returns the server's config: the defaults, then the config file, if
// there is one, then the SOFT_SERVE_* environment variables, validated once
// on the result. So the environment can repair a value from the file, and
// Validate sees each value once.
func Load() (*Config, error) {
	cfg := DefaultConfig()
	if err := cfg.load(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// load is Load on c, which holds the defaults or values a test chose.
func (c *Config) load() error {
	path, err := c.locateFile()
	if err != nil {
		return err
	}
	c.filePath = path
	if exist(path) {
		if err := c.readFile(path); err != nil {
			return err
		}
	}
	if err := c.readEnv(); err != nil {
		return err
	}
	return c.Validate()
}

// defaultConfigFile is the file the server writes on its first start. It
// holds DefaultConfig's values, with a comment for each setting
// (TestDefaultConfigFileMatchesDefaults), and is the README's sample.
//
//go:embed config.yaml
var defaultConfigFile []byte

// WriteDefaultFile writes the default config file to FilePath, creating
// its directory if needed. It writes the defaults, not c, so environment
// overrides are not saved to the file.
func (c *Config) WriteDefaultFile() error {
	path := c.FilePath()
	// The config can hold secrets such as a database password, so only
	// the server's user may read it.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, defaultConfigFile, 0o600)
}

// DefaultDataPath returns the path to the data directory.
// It uses the SOFT_SERVE_DATA_PATH environment variable if set, otherwise it
// uses "data".
func DefaultDataPath() string {
	dp := os.Getenv("SOFT_SERVE_DATA_PATH")
	if dp == "" {
		dp = "data"
	}

	return dp
}

// FilePath returns the config file: for a config from Load, the absolute
// path Load resolved, whether or not the file exists; for any other,
// config.yaml in the data path.
func (c *Config) FilePath() string {
	if c.filePath != "" {
		return c.filePath
	}
	return filepath.Join(c.DataPath, "config.yaml")
}

// locateFile returns the absolute path of the config file:
// SOFT_SERVE_CONFIG_LOCATION, resolved against the working directory once
// here, or config.yaml in the data path. Environ hands the path to hooks,
// which start in a repository's directory, and a relative one would name
// another file there.
//
// It is an error for SOFT_SERVE_CONFIG_LOCATION to name a file that does
// not exist: the server would otherwise start on the default config, and
// write it to the data directory, after a typo in the variable or with a
// config file that is not mounted yet. Naming the default location in the
// data directory is allowed, since the server writes the default config
// there on its first start.
func (c *Config) locateFile() (string, error) {
	def, err := filepath.Abs(filepath.Join(c.DataPath, "config.yaml"))
	if err != nil {
		return "", err
	}
	loc := os.Getenv("SOFT_SERVE_CONFIG_LOCATION")
	if loc == "" {
		return def, nil
	}
	path, err := filepath.Abs(loc)
	if err != nil {
		return "", fmt.Errorf("config file set by SOFT_SERVE_CONFIG_LOCATION: %w", err)
	}
	if _, err := os.Stat(path); err != nil && path != def { //nolint:gosec // the operator names the config file
		return "", fmt.Errorf("config file set by SOFT_SERVE_CONFIG_LOCATION: %w", err)
	}
	return path, nil
}

func exist(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Exist returns true if the config file exists.
func (c *Config) Exist() bool {
	return exist(c.FilePath())
}

// DefaultMaxTimeout is the default max_timeout of the SSH server, the git
// daemon and the HTTP server, in seconds: 6 hours.
//
// The idle timeouts are re-armed by every byte, so without a total limit a
// client that trickles a byte now and then keeps its connection, and its
// place under max_connections, for ever. The limit must still let a large
// clone over a slow link finish: 6 hours carries about 27 GB at 10 Mbit/s.
// git daemon's --timeout and the timeouts of common reverse proxies (nginx's
// proxy_read_timeout, HAProxy's timeout client and server, a cloud load
// balancer's idle timeout) only bound inactivity, like idle_timeout here,
// and set no total limit, so they cut off no transfer that makes progress;
// neither does a limit this long in practice.
const DefaultMaxTimeout = 6 * 60 * 60

// UnboundedTimeouts returns the max_timeout settings, such as
// "http.max_timeout", of the enabled servers that set no total time limit.
// A config.yaml written before DefaultMaxTimeout keeps max_timeout: 0.
func (c *Config) UnboundedTimeouts() []string {
	var names []string
	for _, v := range []struct {
		name    string
		enabled bool
		value   int
	}{
		{"ssh.max_timeout", c.SSH.Enabled, c.SSH.MaxTimeout},
		{"git.max_timeout", c.Git.Enabled, c.Git.MaxTimeout},
		{"http.max_timeout", c.HTTP.Enabled, c.HTTP.MaxTimeout},
	} {
		if v.enabled && v.value == 0 {
			names = append(names, v.name)
		}
	}
	return names
}

// DefaultConfig returns the default Config: the values config.yaml holds,
// and the data path. The other paths are relative to the data directory and
// use forward slashes, as config.yaml does; Validate joins them to the data
// path with the OS separator.
func DefaultConfig() *Config {
	return &Config{
		Name: "Soft Serve Plus",
		// DefaultRepo: "",
		DataPath: DefaultDataPath(),
		SSH: SSHConfig{
			Enabled:                     true,
			ListenAddr:                  ":23231",
			PublicURL:                   "ssh://localhost:23231",
			KeyPath:                     "ssh/soft_serve_host_ed25519",
			ClientKeyPath:               "ssh/soft_serve_client_ed25519",
			ClientStrictHostKeyChecking: StrictHostKeyCheckingAcceptNew,
			MaxTimeout:                  DefaultMaxTimeout,
			IdleTimeout:                 10 * 60, // 10 minutes
			MaxConnections:              256,
			MaxConnectionsPerIP:         32,
		},
		Git: GitConfig{
			Enabled:             true,
			ListenAddr:          ":9418",
			PublicURL:           "git://localhost",
			MaxTimeout:          DefaultMaxTimeout,
			IdleTimeout:         3,
			MaxConnections:      32,
			MaxConnectionsPerIP: 8,
		},
		HTTP: HTTPConfig{
			Enabled:             true,
			ListenAddr:          ":23232",
			PublicURL:           "http://localhost:23232",
			MaxTimeout:          DefaultMaxTimeout,
			IdleTimeout:         60,
			MaxConnections:      64,
			MaxConnectionsPerIP: 16,
			CORS: CORSConfig{
				AllowedHeaders: []string{"Accept", "Accept-Language", "Content-Language", "Content-Type", "Origin", "X-Requested-With", "User-Agent", "Authorization", "Access-Control-Request-Method", "Access-Control-Allow-Origin"},
				AllowedMethods: []string{"GET", "HEAD", "POST", "PUT", "OPTIONS"},
			},
		},
		Stats: StatsConfig{
			Enabled:    true,
			ListenAddr: "localhost:23233",
		},
		Log: LogConfig{
			Format:     "text",
			TimeFormat: time.DateTime,
		},
		DB: DBConfig{
			Driver: "sqlite",
			DataSource: "soft-serve.db" +
				"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		},
		LFS: LFSConfig{
			Enabled:    true,
			SSHEnabled: false,
		},
		Jobs: JobsConfig{
			MirrorPull: "@every 10m",
		},
	}
}

// MaxBlobSize is the largest file, in bytes, that the TUI and the SSH
// "repo blob" command read into memory to display: 1 MiB. Larger files are
// refused ("repo blob --raw" streams them instead), and a larger README is
// not shown. The TUI renders a file synchronously, so this is also what
// bounds the time and memory one file view takes.
const MaxBlobSize = 1 << 20

// SSHMaxSessions is the largest number of sessions one SSH connection may
// have open at once, as OpenSSH's MaxSessions. Further sessions are refused
// until one ends.
const SSHMaxSessions = 10

// SSHHandshakeTimeout is how long an SSH client has to finish the handshake
// and authenticate, as OpenSSH's LoginGraceTime. Until then the connection
// holds a place under ssh.max_connections and ssh.max_connections_per_ip,
// so this bounds how long an idle, unauthenticated client keeps one.
const SSHHandshakeTimeout = 2 * time.Minute

// Validate checks the configuration and resolves it: relative paths become
// absolute under the data path, lists lose blank entries, and the
// allowlists and admin keys are parsed. Load runs it once, on what the file
// and the environment produced.
func (c *Config) Validate() error {
	if err := c.resolveDataPath(); err != nil {
		return err
	}
	c.resolvePaths()
	if err := c.check(); err != nil {
		return err
	}
	return c.parseLists()
}

// resolveDataPath checks the data path and makes it absolute.
func (c *Config) resolveDataPath() error {
	// The data path is handed to ssh inside a double-quoted option value,
	// where nothing escapes a quote or a newline.
	if strings.ContainsAny(c.DataPath, "\"\n\r") {
		return fmt.Errorf("data path must not contain quotes or newlines: %q", c.DataPath)
	}
	if !filepath.IsAbs(c.DataPath) {
		dp, err := filepath.Abs(c.DataPath)
		if err != nil {
			return err
		}
		c.DataPath = dp
	}
	return nil
}

// resolvePaths makes the relative paths absolute under the data path and
// drops trailing slashes from the public URLs.
func (c *Config) resolvePaths() {
	for _, p := range []*string{&c.SSH.KeyPath, &c.SSH.ClientKeyPath, &c.HTTP.TLSKeyPath, &c.HTTP.TLSCertPath, &c.Log.Path} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(c.DataPath, *p)
		}
	}
	if c.DB.Driver == "sqlite" && sqliteName(c.DB.DataSource) != "" {
		c.DB.DataSource = resolveSQLiteDataSource(c.DataPath, c.DB.DataSource)
	}

	c.SSH.PublicURL = strings.TrimRight(c.SSH.PublicURL, "/")
	c.Git.PublicURL = strings.TrimRight(c.Git.PublicURL, "/")
	c.HTTP.PublicURL = strings.TrimRight(c.HTTP.PublicURL, "/")
}

// check rejects values the server cannot use.
func (c *Config) check() error {
	// db.Open takes only these, so a mistyped driver fails here, with the
	// setting named, rather than when the database is opened.
	if c.DB.Driver != "sqlite" && c.DB.Driver != "postgres" {
		return fmt.Errorf("db.driver: %q is not sqlite or postgres", c.DB.Driver)
	}
	// A SQLite data source without a file name, empty or only options
	// ("?_pragma=..."), would name the data directory itself, or a file
	// named after the options. An empty Postgres one is valid: lib/pq then
	// reads the PG* environment variables.
	if c.DB.Driver == "sqlite" && sqliteName(c.DB.DataSource) == "" {
		return fmt.Errorf(`db.data_source: %q names no database file; with sqlite it is a file name, such as "soft-serve.db", with options after "?"`, c.DB.DataSource)
	}

	// The HTTP public URL is the JWT issuer and the base of LFS hrefs, so a
	// value without a scheme or a host would hand clients unusable links.
	if c.HTTP.PublicURL != "" {
		u, err := url.Parse(c.HTTP.PublicURL)
		if err != nil {
			return fmt.Errorf("http.public_url: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("http.public_url: %q must be an http:// or https:// URL with a host", c.HTTP.PublicURL)
		}
	}

	// Setting only one of the pair would silently serve plain HTTP.
	if (c.HTTP.TLSKeyPath == "") != (c.HTTP.TLSCertPath == "") {
		return fmt.Errorf("http.tls_key_path and http.tls_cert_path must be set together")
	}

	switch strings.ToLower(c.Log.Format) {
	case "", "json", "logfmt", "text":
	default:
		return fmt.Errorf("log.format: %q is not one of %q, %q, %q", c.Log.Format, "json", "logfmt", "text")
	}

	// The scheduler uses the standard parser; a spec it rejects would leave
	// mirrors never syncing behind a startup warning. An empty spec
	// disables mirror syncing.
	if spec := c.Jobs.MirrorPull; spec != "" {
		if _, err := cron.ParseStandard(spec); err != nil {
			return fmt.Errorf("jobs.mirror_pull: %q: %w", spec, err)
		}
	}

	if err := c.checkNotNegative(); err != nil {
		return err
	}

	if c.Git.Enabled && c.Git.MaxConnections < 1 {
		return fmt.Errorf("git.max_connections: %d must be at least 1", c.Git.MaxConnections)
	}

	switch c.SSH.ClientStrictHostKeyChecking {
	case "":
		c.SSH.ClientStrictHostKeyChecking = StrictHostKeyCheckingAcceptNew
	case StrictHostKeyCheckingAcceptNew, StrictHostKeyCheckingYes:
	default:
		// "no" and "off" are not offered: they accept any host key, which
		// hands the client key's authentication to whoever answers.
		return fmt.Errorf("ssh.client_strict_host_key_checking: %q is not one of %q, %q",
			c.SSH.ClientStrictHostKeyChecking, StrictHostKeyCheckingAcceptNew, StrictHostKeyCheckingYes)
	}
	return nil
}

// checkNotNegative rejects a negative count, size or number of seconds,
// telling the operator what the setting's 0 means.
func (c *Config) checkNotNegative() error {
	const (
		noLimit   = "use 0 for no limit"
		noTimeout = "use 0 for no timeout"
		dbDefault = "use 0 for the default"
	)
	for _, v := range []struct {
		name  string
		value int64
		zero  string
	}{
		{"db.max_open_conns", int64(c.DB.MaxOpenConns), dbDefault},
		{"ssh.max_timeout", int64(c.SSH.MaxTimeout), noTimeout},
		{"ssh.idle_timeout", int64(c.SSH.IdleTimeout), noTimeout},
		{"git.max_timeout", int64(c.Git.MaxTimeout), noTimeout},
		{"git.idle_timeout", int64(c.Git.IdleTimeout), noTimeout},
		{"git.max_connections_per_ip", int64(c.Git.MaxConnectionsPerIP), noLimit},
		{"http.max_timeout", int64(c.HTTP.MaxTimeout), noLimit},
		{"http.idle_timeout", int64(c.HTTP.IdleTimeout), noLimit},
		{"http.max_connections", int64(c.HTTP.MaxConnections), noLimit},
		{"http.max_connections_per_ip", int64(c.HTTP.MaxConnectionsPerIP), noLimit},
		{"max_repos_per_user", int64(c.MaxReposPerUser), noLimit},
		{"ssh.max_connections", int64(c.SSH.MaxConnections), noLimit},
		{"ssh.max_connections_per_ip", int64(c.SSH.MaxConnectionsPerIP), noLimit},
	} {
		if v.value < 0 {
			return fmt.Errorf("%s: %d is negative; %s", v.name, v.value, v.zero)
		}
	}
	return nil
}

// parseLists drops blank entries from the lists, so an unset environment
// variable does not read as one empty entry, and parses the allowlists, the
// SSH algorithm lists and the admin keys once, rejecting anything
// unreadable now rather than at first use.
func (c *Config) parseLists() error {
	notBlank := func(l []string) []string {
		return slices.DeleteFunc(slices.Clone(l), func(s string) bool {
			return strings.TrimSpace(s) == ""
		})
	}

	c.SSH.ClientAllowedHosts = notBlank(c.SSH.ClientAllowedHosts)
	allowed, err := ssrf.ParseAllowedHosts(c.SSH.ClientAllowedHosts)
	if err != nil {
		return fmt.Errorf("ssh.client_allowed_hosts: %w", err)
	}
	c.SSH.allowedHosts = allowed

	if err := c.SSH.validateAlgorithms(); err != nil {
		return err
	}

	c.Webhook.AllowedHosts = notBlank(c.Webhook.AllowedHosts)
	webhookAllowed, err := ssrf.ParseAllowedHosts(c.Webhook.AllowedHosts)
	if err != nil {
		return fmt.Errorf("webhook.allowed_hosts: %w", err)
	}
	c.Webhook.allowedHosts = webhookAllowed

	keys := parseAuthKeys(c.InitialAdminKeys, c.DataPath)
	pks := make([]string, 0, len(keys.pks))
	for _, key := range keys.pks {
		pks = append(pks, proto.MarshalAuthorizedKey(key))
	}
	c.InitialAdminKeys = pks
	c.adminKeys = keys.pks
	c.unreadableAdminKeyFiles = addToSet(c.unreadableAdminKeyFiles, keys.unreadable)
	c.ignoredAdminKeys = addToSet(c.ignoredAdminKeys, keys.ignored)

	// The public URL is allowed by CORSAllowedOrigins, not added here, so an
	// environment override of it does not leave the file's URL allowed too.
	c.HTTP.CORS.AllowedOrigins = notBlank(c.HTTP.CORS.AllowedOrigins)
	return nil
}

// CORSAllowedOrigins returns the origins allowed to make cross-origin
// requests: the HTTP public URL, which is always allowed, followed by the
// configured allowed origins. The list is empty when neither is set, which
// means no origin is allowed, not every one.
func (c *Config) CORSAllowedOrigins() []string {
	origins := slices.Clone(c.HTTP.CORS.AllowedOrigins)
	if c.HTTP.PublicURL != "" && !slices.Contains(origins, c.HTTP.PublicURL) {
		origins = append([]string{c.HTTP.PublicURL}, origins...)
	}
	return origins
}

// authKeys is the result of parseAuthKeys.
type authKeys struct {
	pks []ssh.PublicKey
	// unreadable holds the entries that name a key file that could not be
	// read or holds no key, such as a secret that is not mounted yet. Such
	// an entry may stand for a key that is still wanted.
	unreadable []string
	// ignored holds the entries that are neither a key nor a file path,
	// such as a mistyped key. They cannot stand for a wanted key.
	ignored []string
}

// parseAuthKeys parses authorized keys from either file paths or string
// authorized_keys. Blank entries and entries starting with "#" are comments
// and are skipped. A relative key file path is read from dataPath, like the
// other relative paths in the config; when no file of that name is there,
// it is read from the working directory (see readKeyFile).
func parseAuthKeys(aks []string, dataPath string) authKeys {
	var r authKeys
	exist := make(map[string]struct{}, 0)
	r.pks = make([]ssh.PublicKey, 0)
	for _, entry := range aks {
		key := trimQuotes(strings.TrimSpace(entry))
		if key == "" || strings.HasPrefix(key, "#") {
			continue
		}
		if bts, err := readKeyFile(key, dataPath); err == nil {
			// key is a file in authorized_keys format: one key per line.
			var found bool
			var bad []string
			for _, line := range strings.Split(string(bts), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				pk, _, err := proto.ParseAuthorizedKey(line)
				if err != nil {
					bad = append(bad, line)
					continue
				}
				found = true
				id := string(pk.Marshal())
				if _, ok := exist[id]; !ok {
					r.pks = append(r.pks, pk)
					exist[id] = struct{}{}
				}
			}
			if !found {
				r.unreadable = append(r.unreadable, key)
				continue
			}
			// A file that holds keys was read; its other lines are
			// mistyped keys.
			r.ignored = append(r.ignored, bad...)
			continue
		}

		pk, _, err := proto.ParseAuthorizedKey(key)
		switch {
		case err == nil:
			// Compare the key itself, not its text: a key file, a comment
			// or extra spacing must not list the same key twice.
			id := string(pk.Marshal())
			if _, ok := exist[id]; !ok {
				r.pks = append(r.pks, pk)
				exist[id] = struct{}{}
			}
		case looksLikePath(key):
			r.unreadable = append(r.unreadable, key)
		default:
			r.ignored = append(r.ignored, key)
		}
	}
	return r
}

// trimQuotes removes one pair of matching single or double quotes around
// s. Docker Compose's list syntax, "- SOFT_SERVE_INITIAL_ADMIN_KEYS='ssh-ed25519
// AAAA...'", keeps the quotes in the value, and a quoted key would otherwise
// be ignored as malformed.
func trimQuotes(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// trimOuterQuotes removes a pair of quotes that wraps the whole
// SOFT_SERVE_INITIAL_ADMIN_KEYS value rather than one entry. The value is
// split on newlines before trimQuotes sees the entries, so several keys in
// one pair of quotes, as in "- SOFT_SERVE_INITIAL_ADMIN_KEYS='key1\nkey2'",
// leave an unmatched quote at the start of the first entry and at the end of
// the last one, and both keys would be ignored or misread. It strips them
// only when both are unmatched and are the same quote, so entries that are
// each quoted are left to trimQuotes. It runs before the config file's keys
// are merged in, so the last entry is the environment's.
func trimOuterQuotes(aks []string) []string {
	first, last := -1, -1
	for i, entry := range aks {
		if strings.TrimSpace(entry) != "" {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 || first == last {
		return aks
	}
	head := strings.TrimSpace(aks[first])
	tail := strings.TrimSpace(aks[last])
	q := head[0]
	if q != '"' && q != '\'' {
		return aks
	}
	if len(head) >= 2 && head[len(head)-1] == q {
		return aks // the first entry is quoted on its own
	}
	if tail[len(tail)-1] != q || (len(tail) >= 2 && tail[0] == q) {
		return aks // the last entry is not closed, or is quoted on its own
	}
	out := slices.Clone(aks)
	out[first] = head[1:]
	out[last] = tail[:len(tail)-1]
	return out
}

// looksLikePath reports whether an entry that is not a key names a file. An
// authorized key always has a space between its type and its data, so an
// entry with a space is a malformed key; an entry without one is taken for a
// path, even one with no directory or extension such as "admin_key", so that
// a key file that is missing keeps the keys it may stand for.
func looksLikePath(s string) bool {
	return !strings.ContainsFunc(s, unicode.IsSpace)
}

// readKeyFile reads the admin key file named by path. A relative path is
// looked up in dataPath first, then, if no such file exists there, in the
// working directory, so a config written for a server that read key files
// from there keeps its admin keys.
func readKeyFile(path, dataPath string) ([]byte, error) {
	if dataPath != "" && !filepath.IsAbs(path) {
		bts, err := os.ReadFile(filepath.Join(dataPath, path))
		if !errors.Is(err, fs.ErrNotExist) {
			return bts, err
		}
	}
	return os.ReadFile(path)
}

// sqliteName returns the database name of a SQLite data source: the part
// before its options ("?..."), without a "file:" scheme.
func sqliteName(dsn string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(dsn), "?")
	return strings.TrimSpace(strings.TrimPrefix(name, "file:"))
}

// resolveSQLiteDataSource makes a relative SQLite database path in dsn
// relative to dataPath. An in-memory database (":memory:", or a "file:" URI
// naming ":memory:" or using mode=memory) is left alone, as is a "file:" URI
// that already holds an absolute path or an authority ("file://..."). A
// relative "file:" URI keeps its form and query, with the data path put in
// front of its path.
func resolveSQLiteDataSource(dataPath, dsn string) string {
	name, query, hasQuery := strings.Cut(dsn, "?")
	rest, isURI := strings.CutPrefix(name, "file:")
	if !isURI {
		if name == ":memory:" || filepath.IsAbs(dsn) {
			return dsn
		}
		return filepath.Join(dataPath, dsn)
	}

	if rest == "" || rest == ":memory:" || strings.HasPrefix(rest, "/") || filepath.IsAbs(rest) {
		return dsn
	}
	if hasQuery {
		if q, err := url.ParseQuery(query); err == nil && q.Get("mode") == "memory" {
			return dsn
		}
	}

	// The data path goes into a URI, so escape the characters SQLite would
	// read as the start of the query or fragment, or as an escape. The
	// path from the URI is already in URI form and is kept as written.
	base := (&url.URL{Path: filepath.ToSlash(dataPath)}).EscapedPath()
	switch {
	case strings.HasPrefix(base, "//"):
		// A UNC path such as //server/share/data. As file://server/...
		// SQLite would read "server" as the URI authority and refuse it, so
		// keep the authority empty: file:////server/share/data.
		base = "//" + base
	case !strings.HasPrefix(base, "/"):
		// A Windows path such as C:/data: SQLite wants file:///C:/data.
		base = "///" + base
	}
	out := "file:" + strings.TrimSuffix(base, "/") + "/" + rest
	if hasQuery {
		out += "?" + query
	}
	return out
}

// addToSet adds items to set, creating it if needed.
func addToSet(set map[string]struct{}, items []string) map[string]struct{} {
	if len(items) == 0 {
		return set
	}
	if set == nil {
		set = make(map[string]struct{}, len(items))
	}
	for _, it := range items {
		set[it] = struct{}{}
	}
	return set
}

// AdminKeys returns the server admin keys, as parsed by the last Validate.
// A config that was never validated has none. The slice is a copy, so a
// caller may sort or change it: concurrent boots share one config.
func (c *Config) AdminKeys() []ssh.PublicKey {
	return slices.Clone(c.adminKeys)
}

// UnreadableAdminKeyFiles returns, sorted and without duplicates, the
// initial_admin_keys entries Validate found to name a key file that could
// not be read or holds no key, such as a secret that is not mounted yet.
// Such an entry may stand for a key that is still wanted. Paths are not key
// material.
func (c *Config) UnreadableAdminKeyFiles() []string {
	return slices.Sorted(maps.Keys(c.unreadableAdminKeyFiles))
}

// IgnoredAdminKeys returns how many distinct initial_admin_keys entries
// Validate ignored because they are neither a key nor a file path, such as a
// mistyped key. The entries are not returned, as they may hold key material.
func (c *Config) IgnoredAdminKeys() int {
	return len(c.ignoredAdminKeys)
}
