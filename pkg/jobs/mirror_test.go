package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

// newRepoWithRemotes creates a bare repo with the given name=url remotes.
func newRepoWithRemotes(t *testing.T, remotes map[string]string) *git.Repository {
	t.Helper()

	path := filepath.Join(t.TempDir(), "repo.git")
	if _, err := git.Init(path, true); err != nil {
		t.Fatalf("init: %v", err)
	}

	for name, url := range remotes {
		cmd := exec.CommandContext(t.Context(), "git", "remote", "add", name, url)
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("adding remote %s: %v: %s", name, err, out)
		}
	}

	r, err := git.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return r
}

func TestValidateMirrorRemotes(t *testing.T) {
	allowed, err := ssrf.ParseAllowedHosts([]string{"git.internal.example"})
	if err != nil {
		t.Fatal(err)
	}
	allow := ssrf.GitRemotePolicy{AllowedSSHHosts: allowed}
	admin := ssrf.GitRemotePolicy{AllowAnySSHHost: true}

	tests := []struct {
		name    string
		remotes map[string]string
		policy  ssrf.GitRemotePolicy
		wantErr error
	}{
		{
			// With an allowlist, every ssh remote must be on it. A second
			// remote added out of band is fetched by `remote update` too.
			name: "ssh remote on allowlist",
			remotes: map[string]string{
				"origin": "ssh://git@git.internal.example/x.git",
			},
			policy: allow,
		},
		{
			name: "ssh remote off allowlist",
			remotes: map[string]string{
				"origin": "ssh://git@git.internal.example/x.git",
				"backup": "git@10.0.0.1:x.git",
			},
			policy:  allow,
			wantErr: ssrf.ErrSSHHostNotAllowed,
		},
		{
			name:    "public origin",
			remotes: map[string]string{"origin": "https://1.1.1.1/x.git"},
		},
		{
			// With no allowlist, an ssh remote syncs only for an admin owner.
			name:    "ssh origin, admin owner",
			remotes: map[string]string{"origin": "ssh://git@10.0.0.1/x.git"},
			policy:  admin,
		},
		{
			name:    "ssh origin, unprivileged owner",
			remotes: map[string]string{"origin": "ssh://git@10.0.0.1/x.git"},
			wantErr: ssrf.ErrSSHRequiresAdmin,
		},
		{
			name:    "private origin",
			remotes: map[string]string{"origin": "http://127.0.0.1:8080/x.git"},
			wantErr: ssrf.ErrPrivateIP,
		},
		{
			name:    "metadata origin",
			remotes: map[string]string{"origin": "http://169.254.169.254/x.git"},
			wantErr: ssrf.ErrPrivateIP,
		},
		{
			// `git remote update` fetches from every remote, not just
			// origin. A guard that only reads origin misses this entirely.
			name: "private non-origin remote",
			remotes: map[string]string{
				"origin": "https://1.1.1.1/x.git",
				"backup": "http://192.168.1.1/x.git",
			},
			wantErr: ssrf.ErrPrivateIP,
		},
		{
			// git:// is a raw TCP connect and is just as usable for SSRF.
			name:    "private git scheme",
			remotes: map[string]string{"origin": "git://10.0.0.1/x.git"},
			wantErr: ssrf.ErrPrivateIP,
		},
		{
			// Go and libcurl disagree on non-canonical IPv4 literals.
			name:    "octal loopback",
			remotes: map[string]string{"origin": "http://0177.0.0.1/x.git"},
			wantErr: ssrf.ErrAmbiguousHost,
		},
		{
			// A remote that cannot be parsed must not be fetched from.
			// Failing open here was how unvalidated remotes kept firing.
			name:    "unparseable remote",
			remotes: map[string]string{"origin": "http://[::1/x.git"},
			wantErr: ssrf.ErrInvalidURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepoWithRemotes(t, tt.remotes)
			remotes, err := validateMirrorRemotes(r, tt.policy)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("validateMirrorRemotes() error = %v, want %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("validateMirrorRemotes() unexpected error: %v", err)
			}
			env := ssrf.GitEnv(remotes...)
			if !slices.Contains(env, "GIT_CONFIG_KEY_0=http.followRedirects") ||
				!slices.Contains(env, "GIT_CONFIG_VALUE_0=false") {
				t.Errorf("sync env did not disable redirects: %v", env)
			}
		})
	}
}

// TestValidateMirrorRemotesNoRemote verifies a mirror with no remote URL is
// skipped rather than treated as valid.
func TestValidateMirrorRemotesNoRemote(t *testing.T) {
	r := newRepoWithRemotes(t, nil)
	if _, err := validateMirrorRemotes(r, ssrf.GitRemotePolicy{}); err == nil {
		t.Error("validateMirrorRemotes() accepted a repo with no remote")
	}
}

// stubMirrorGit replaces runMirrorGit for the test and records the commands
// it was asked to run.
func stubMirrorGit(t *testing.T, fn func(args []string) error) *[][]string {
	t.Helper()
	var calls [][]string
	orig := runMirrorGit
	runMirrorGit = func(_ context.Context, _ string, _ []string, args ...string) error {
		calls = append(calls, args)
		return fn(args)
	}
	t.Cleanup(func() { runMirrorGit = orig })
	return &calls
}

func TestFetchMirrorRunsAllCommands(t *testing.T) {
	calls := stubMirrorGit(t, func([]string) error { return nil })
	var logs bytes.Buffer

	if !fetchMirror(t.Context(), log.New(&logs), "repo", t.TempDir(), nil) {
		t.Fatal("fetchMirror stopped without being cancelled")
	}
	if len(*calls) != len(mirrorSyncCmds) {
		t.Fatalf("ran %v, want %v", *calls, mirrorSyncCmds)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected logs: %s", logs.String())
	}
}

// A shutdown during the fetch lets it finish, then starts nothing else: not
// the next git command and not the lfs sync. It logs nothing either way.
func TestFetchMirrorStopsAfterCancel(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"fetch finished", nil},
		{"fetch failed", errors.New("exit status 128")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := stubMirrorGit(t, func([]string) error {
				cancel()
				return tc.err
			})
			var logs bytes.Buffer

			if fetchMirror(ctx, log.New(&logs), "repo", t.TempDir(), nil) {
				t.Fatal("fetchMirror went on after it was cancelled")
			}
			if len(*calls) != 1 {
				t.Fatalf("ran %v, want only the fetch", *calls)
			}
			if logs.Len() != 0 {
				t.Fatalf("a cancelled sync logged: %s", logs.String())
			}
		})
	}
}

func TestFetchMirrorCancelledStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := stubMirrorGit(t, func([]string) error { return nil })

	if fetchMirror(ctx, log.New(io.Discard), "repo", t.TempDir(), nil) {
		t.Fatal("fetchMirror went on after it was cancelled")
	}
	if len(*calls) != 0 {
		t.Fatalf("ran %v after cancellation", *calls)
	}
}

// The git command does not take the cancellation: killing git would leave
// its ref locks behind.
func TestRunMirrorGitIgnoresCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := runMirrorGit(ctx, t.TempDir(), nil, "version"); err != nil {
		t.Fatalf("git did not run under a cancelled context: %v", err)
	}
}
