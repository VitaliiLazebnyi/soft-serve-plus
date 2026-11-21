package hooks

import (
	"os"
	"path/filepath"
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
	// Protocol is the GIT_PROTOCOL value to pass to git, or "".
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
// for req, on top of the server's own (see pkg/git's serviceEnv):
//
//   - SOFT_SERVE_REPO_NAME, SOFT_SERVE_REPO_PATH and SOFT_SERVE_LOG_PATH;
//   - SOFT_SERVE_PUBLIC_KEY over SSH, empty for a keyless session;
//   - SOFT_SERVE_USERNAME when there is a user;
//   - SOFT_SERVE_PUSH_REFS when req has Refs;
//   - the server config, from cfg.Environ, without the database data
//     source;
//   - GIT_PROTOCOL when req has one.
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
	for _, kv := range cfg.Environ() {
		if !strings.HasPrefix(kv, dataSourceEnv) {
			env = append(env, kv)
		}
	}
	return append(env, ProtocolEnv(req.Protocol)...)
}

// ProtocolEnv returns the environment of a git service that runs no hooks:
// GIT_PROTOCOL, when protocol is not empty, and nothing else.
func ProtocolEnv(protocol string) []string {
	if protocol == "" {
		return nil
	}
	return []string{"GIT_PROTOCOL=" + protocol}
}
