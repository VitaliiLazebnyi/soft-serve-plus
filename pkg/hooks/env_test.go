package hooks

import (
	"crypto/ed25519"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	gossh "golang.org/x/crypto/ssh"
)

// testUser is a proto.User with only a name.
type testUser struct{ proto.User }

func (testUser) Username() string { return "alice" }

func TestRepoPath(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = filepath.Join(t.TempDir(), "data")
	want := filepath.Join(cfg.DataPath, "repos", "group", "repo.git")
	for _, name := range []string{"group/repo", "group/repo.git", "/group/repo", "group//repo.git/"} {
		if got := RepoPath(cfg, name); got != want {
			t.Errorf("RepoPath(%q) = %q, want %q", name, got, want)
		}
	}
	if got, want := ReposPath(cfg), filepath.Join(cfg.DataPath, "repos"); got != want {
		t.Errorf("ReposPath = %q, want %q", got, want)
	}
	if got, want := LogPath(cfg), filepath.Join(cfg.DataPath, "log", "hooks.log"); got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
}

// withoutServerConfig returns env without the entries of cfg.Environ, and
// fails unless env has every one of them but the database data source, which
// it must not have.
func withoutServerConfig(t *testing.T, cfg *config.Config, env []string) []string {
	t.Helper()
	server := cfg.Environ()
	for _, kv := range server {
		dataSource := strings.HasPrefix(kv, "SOFT_SERVE_DB_DATA_SOURCE=")
		if has := slices.Contains(env, kv); has == dataSource {
			t.Errorf("env has %q: %v, want %v", kv, has, !dataSource)
		}
	}
	var out []string
	for _, kv := range env {
		if !slices.Contains(server, kv) {
			out = append(out, kv)
		}
	}
	return out
}

// TestEnv pins the variables each transport passes to git and its hooks,
// beyond the server config.
func TestEnv(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = filepath.Join(t.TempDir(), "data")
	repoPath := "SOFT_SERVE_REPO_PATH=" + filepath.Join(cfg.DataPath, "repos", "repo.git")
	logPath := "SOFT_SERVE_LOG_PATH=" + filepath.Join(cfg.DataPath, "log", "hooks.log")

	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	key := sshutils.MarshalAuthorizedKey(pk)

	for _, tc := range []struct {
		name string
		req  Request
		want []string
	}{{
		name: "ssh",
		req:  Request{Transport: TransportSSH, Repo: "repo", User: testUser{}, PublicKey: pk, Protocol: "version=2"},
		want: []string{"SOFT_SERVE_REPO_NAME=repo", repoPath, logPath, "SOFT_SERVE_PUBLIC_KEY=" + key, "SOFT_SERVE_USERNAME=alice", "GIT_PROTOCOL=version=2"},
	}, {
		name: "ssh keyless anonymous",
		req:  Request{Transport: TransportSSH, Repo: "repo"},
		want: []string{"SOFT_SERVE_REPO_NAME=repo", repoPath, logPath, "SOFT_SERVE_PUBLIC_KEY="},
	}, {
		name: "http",
		req:  Request{Transport: TransportHTTP, Repo: "repo", User: testUser{}, Protocol: "version=2:foo"},
		want: []string{"SOFT_SERVE_REPO_NAME=repo", repoPath, logPath, "SOFT_SERVE_USERNAME=alice", "GIT_PROTOCOL=version=2:foo"},
	}, {
		name: "http anonymous",
		req:  Request{Transport: TransportHTTP, Repo: "repo"},
		want: []string{"SOFT_SERVE_REPO_NAME=repo", repoPath, logPath},
	}, {
		name: "refs",
		req:  Request{Transport: TransportHTTP, Repo: "repo", Refs: &PushRefs{path: "/tmp/refs"}},
		want: []string{"SOFT_SERVE_REPO_NAME=repo", repoPath, logPath, "SOFT_SERVE_PUSH_REFS=/tmp/refs"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := withoutServerConfig(t, cfg, Env(cfg, tc.req))
			if !slices.Equal(got, tc.want) {
				t.Errorf("Env request variables\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestProtocolEnv: a service that runs no hooks gets GIT_PROTOCOL and
// nothing else.
func TestProtocolEnv(t *testing.T) {
	if got, want := ProtocolEnv("version=2"), []string{"GIT_PROTOCOL=version=2"}; !slices.Equal(got, want) {
		t.Errorf("ProtocolEnv(version=2) = %q, want %q", got, want)
	}
	if got := ProtocolEnv(""); len(got) != 0 {
		t.Errorf("ProtocolEnv(\"\") = %q, want none", got)
	}
}
