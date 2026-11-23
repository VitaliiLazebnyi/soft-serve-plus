package hooks

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	gossh "golang.org/x/crypto/ssh"
)

// RepoPath returns the directory of the repository name under cfg's data
// path. Bare repositories end in ".git"; see
// https://git-scm.com/docs/gitrepository-layout. The name is sanitized
// first, so "a/b.git", "/a/b" and "a/b" share one directory.
func RepoPath(cfg *config.Config, name string) string {
	name = utils.SanitizeRepo(name)
	return filepath.Join(ReposPath(cfg), strings.ReplaceAll(name, "/", string(os.PathSeparator))+".git")
}

// ReposPath returns the directory the repositories are in.
func ReposPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataPath, "repos")
}

// LogPath returns the log file of the hook processes.
func LogPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataPath, "log", "hooks.log")
}

// Transport is the server a push came to. It decides which request
// variables Env sets.
type Transport int

// The transports a push can come over.
const (
	// TransportSSH is the SSH server.
	TransportSSH Transport = iota + 1
	// TransportHTTP is the HTTP server.
	TransportHTTP
)

// Request is what Env needs to know about the push a receive-pack runs for.
type Request struct {
	// Transport is the server the push came to.
	Transport Transport
	// Repo is the repository name, sanitized.
	Repo string
	// User is the account pushing, or nil.
	User proto.User
	// PublicKey is the key an SSH session authenticated with, or nil.
	PublicKey gossh.PublicKey
	// Protocol is the client's GIT_PROTOCOL value, or "". git gets only
	// the version it asks for (see ProtocolEnv).
	Protocol string
	// Refs, if not nil, is where post-receive writes the refs the push
	// updated.
	Refs *PushRefs
}

// dataSourceEnv is the server config variable that holds the database data
// source, with its password, if any. No hook opens the database, so it is
// left out of their environment.
const dataSourceEnv = "SOFT_SERVE_DB_DATA_SOURCE="

// Env returns the environment git receive-pack and the hooks it runs get
// for req, on top of the server's own (see pkg/gitserve's serviceEnv):
//
//   - SOFT_SERVE_REPO_NAME, SOFT_SERVE_REPO_PATH and SOFT_SERVE_LOG_PATH;
//   - SOFT_SERVE_PUBLIC_KEY over SSH, empty for a keyless session;
//   - SOFT_SERVE_USERNAME when there is a user;
//   - SOFT_SERVE_PUSH_REFS when req has Refs;
//   - SOFT_SERVE_HOOK_CONFIG, the server's settings for `soft hook`
//     (see WriteConfig);
//   - the server config, from cfg.Environ, without the database data
//     source;
//   - GIT_PROTOCOL with the version req.Protocol asks for, if any (see
//     ProtocolEnv).
//
// Only receive-pack runs hooks. Every other git service gets ProtocolEnv,
// so a fetch, including an anonymous one, never carries the server config.
func Env(cfg *config.Config, req Request) []string {
	env := []string{
		"SOFT_SERVE_REPO_NAME=" + req.Repo,
		"SOFT_SERVE_REPO_PATH=" + RepoPath(cfg, req.Repo),
		"SOFT_SERVE_LOG_PATH=" + LogPath(cfg),
	}
	if req.Transport == TransportSSH {
		env = append(env, "SOFT_SERVE_PUBLIC_KEY="+sshutils.MarshalAuthorizedKey(req.PublicKey))
	}
	if req.User != nil {
		env = append(env, "SOFT_SERVE_USERNAME="+req.User.Username())
	}
	if req.Refs != nil {
		env = append(env, req.Refs.Env())
	}
	env = append(env, ConfigEnv+"="+ConfigPath(cfg))
	for _, kv := range cfg.Environ() {
		if !strings.HasPrefix(kv, dataSourceEnv) {
			env = append(env, kv)
		}
	}
	return append(env, ProtocolEnv(req.Protocol)...)
}

// ProtocolEnv returns the environment of a git service that runs no hooks:
// GIT_PROTOCOL with the version protocol, the client's GIT_PROTOCOL value,
// asks for (see ProtocolVersion), and nothing else. Every transport passes
// what its client sent through it, so git gets "version=N" and none of the
// client's other parameters. It returns nil when protocol asks for no
// version git knows.
func ProtocolEnv(protocol string) []string {
	v, ok := ProtocolVersion(protocol)
	if !ok {
		return nil
	}
	return []string{"GIT_PROTOCOL=version=" + strconv.Itoa(v)}
}

// maxProtocolVersion is the highest protocol version git knows.
const maxProtocolVersion = 2

// ProtocolVersion returns the protocol version git serves for protocol, a
// GIT_PROTOCOL value: parameters separated by colons, of which git reads
// only "version=N". Like git, it takes the highest version it knows, 0, 1 or
// 2, and ignores every other parameter. ok is false when protocol asks for
// none of them.
func ProtocolVersion(protocol string) (version int, ok bool) {
	version = -1
	for p := range strings.SplitSeq(protocol, ":") {
		v, found := strings.CutPrefix(p, "version=")
		// git compares the value with "0", "1" and "2" as strings, so
		// "02" or "+2" is no version at all.
		if !found || len(v) != 1 || v[0] < '0' || v[0] > '0'+maxProtocolVersion {
			continue
		}
		version = max(version, int(v[0]-'0'))
	}
	return version, version >= 0
}
