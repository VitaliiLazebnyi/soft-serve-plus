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

// Transport is the server a git service runs for. It decides which request
// variables Env sets.
type Transport int

// The transports.
const (
	// TransportSSH is the SSH server.
	TransportSSH Transport = iota + 1
	// TransportHTTP is the HTTP server.
	TransportHTTP
	// TransportDaemon is the git:// daemon.
	TransportDaemon
)

// Request is what Env needs to know about the request a git service runs
// for.
type Request struct {
	// Transport is the server the request came to.
	Transport Transport
	// Repo is the repository name, sanitized.
	Repo string
	// User is the account making the request, or nil.
	User proto.User
	// PublicKey is the key an SSH session authenticated with, or nil.
	PublicKey gossh.PublicKey
	// Host is the host a git:// client named in its request.
	Host string
	// Protocol is the GIT_PROTOCOL value to pass to git, or "".
	Protocol string
}

// Env returns the environment a git service and the hooks it runs get for
// req, on top of the server's own (see pkg/git's serviceEnv):
//
//   - SOFT_SERVE_REPO_NAME, SOFT_SERVE_REPO_PATH and SOFT_SERVE_LOG_PATH;
//   - SOFT_SERVE_PUBLIC_KEY over SSH, empty for a keyless session;
//   - SOFT_SERVE_HOST over git://;
//   - SOFT_SERVE_USERNAME when there is a user;
//   - the server config, from cfg.Environ;
//   - GIT_PROTOCOL when req has one.
func Env(cfg *config.Config, req Request) []string {
	env := []string{
		"SOFT_SERVE_REPO_NAME=" + req.Repo,
		"SOFT_SERVE_REPO_PATH=" + RepoPath(cfg, req.Repo),
		"SOFT_SERVE_LOG_PATH=" + LogPath(cfg),
	}
	switch req.Transport {
	case TransportSSH:
		env = append(env, "SOFT_SERVE_PUBLIC_KEY="+sshutils.MarshalAuthorizedKey(req.PublicKey))
	case TransportDaemon:
		env = append(env, "SOFT_SERVE_HOST="+req.Host)
	}
	if req.User != nil {
		env = append(env, "SOFT_SERVE_USERNAME="+req.User.Username())
	}
	env = append(env, cfg.Environ()...)
	if req.Protocol != "" {
		env = append(env, "GIT_PROTOCOL="+req.Protocol)
	}
	return env
}
