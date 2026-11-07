package jobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
			remotes, err := validateMirrorRemotes(t.Context(), r, tt.policy)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("validateMirrorRemotes() error = %v, want %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("validateMirrorRemotes() unexpected error: %v", err)
			}
			if len(remotes.names) != len(tt.remotes) {
				t.Errorf("validateMirrorRemotes() names = %v, want one per remote in %v", remotes.names, tt.remotes)
			}
			env := ssrf.GitEnv(remotes.validated...)
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
	if _, err := validateMirrorRemotes(t.Context(), r, ssrf.GitRemotePolicy{}); err == nil {
		t.Error("validateMirrorRemotes() accepted a repo with no remote")
	}
}

// TestValidateMirrorRemotesReadsLikeGit verifies remotes are read the way git
// fetch reads them, not the way a config parser does: the first of several
// urls, urls rewritten by insteadOf, and the legacy [remote.name] section all
// reach validation.
func TestValidateMirrorRemotesReadsLikeGit(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{
			// git fetches from the first url; a parser that keeps the
			// last one validates a different remote.
			name: "first of several urls",
			config: `[remote "origin"]
	url = http://10.0.0.1/x.git
	url = https://1.1.1.1/x.git
`,
		},
		{
			// The rewrite applies when git fetches, after the stored url
			// was validated.
			name: "insteadOf rewrite",
			config: `[url "http://169.254.169.254/"]
	insteadOf = https://1.1.1.1/
[remote "origin"]
	url = https://1.1.1.1/x.git
`,
		},
		{
			// The legacy section syntax is a remote to git, but not a
			// subsection to a config parser.
			name: "legacy section syntax",
			config: `[remote "origin"]
	url = https://1.1.1.1/x.git
[remote.backup]
	url = http://127.0.0.1/x.git
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepoWithRemotes(t, nil)
			appendConfig(t, r, tt.config)

			if _, err := validateMirrorRemotes(t.Context(), r, ssrf.GitRemotePolicy{}); !errors.Is(err, ssrf.ErrPrivateIP) {
				t.Fatalf("validateMirrorRemotes() error = %v, want %v", err, ssrf.ErrPrivateIP)
			}
		})
	}
}

// appendConfig appends raw text to the repository's config file.
func appendConfig(t *testing.T, r *git.Repository, config string) {
	t.Helper()

	f, err := os.OpenFile(filepath.Join(r.Path, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(config); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestValidateMirrorRemotesRefusesUnlistedFetchSources verifies a mirror is
// refused when git could be led to fetch from something validation does not
// see, and that a remote git would fetch from as a local path fails.
func TestValidateMirrorRemotesRefusesUnlistedFetchSources(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr error
	}{
		{
			// `git remote update` fetches the remotes.default group, which
			// can name a legacy branches/ file remote `git remote` does not
			// list.
			name: "remotes.default group",
			config: `[remotes]
	default = foo
`,
			wantErr: errRemoteGroup,
		},
		{
			// A group named like a remote is fetched in its place even when
			// the remote is fetched by name.
			name: "group named like a remote",
			config: `[remotes]
	origin = foo bar
`,
			wantErr: errRemoteGroup,
		},
		{
			// git reports a remote without a url by its name and would
			// fetch from that name as a local path.
			name: "remote without url",
			config: `[remote "x"]
	fetch = +refs/heads/*:refs/remotes/x/*
`,
			wantErr: ssrf.ErrUnsupportedRemoteScheme,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepoWithRemotes(t, map[string]string{"origin": "https://1.1.1.1/x.git"})
			appendConfig(t, r, tt.config)

			if _, err := validateMirrorRemotes(t.Context(), r, ssrf.GitRemotePolicy{}); !errors.Is(err, tt.wantErr) {
				t.Fatalf("validateMirrorRemotes() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// runGit runs git in dir with a fixed identity and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestSyncMirrorFetchesOnlyNamedRemotes verifies the sync fetches the named
// remotes and nothing git would pick on its own: neither the url in
// branch.<HEAD>.remote, which a bare `git fetch` uses, nor the legacy file
// remote in the remotes.default group, which `git remote update` uses.
func TestSyncMirrorFetchesOnlyNamedRemotes(t *testing.T) {
	dir := t.TempDir()

	// Each upstream holds one commit of its own on main, which its HEAD
	// points at, so a bare fetch of the upstream and the legacy branches/
	// refspec (refs/heads/main) both find something to fetch. Whether the
	// mirror then has that commit shows whether the upstream was fetched.
	upstream := func(name string) (path, commit string) {
		path = filepath.Join(dir, name+".git")
		runGit(t, dir, "init", "--bare", path)
		runGit(t, path, "symbolic-ref", "HEAD", "refs/heads/main")
		tree := runGit(t, path, "mktree")
		commit = runGit(t, path, "commit-tree", "-m", name, tree)
		runGit(t, path, "update-ref", "refs/heads/main", commit)
		return path, commit
	}
	origin, originCommit := upstream("origin")
	viaBranch, viaBranchCommit := upstream("viabranch")
	viaGroup, viaGroupCommit := upstream("viagroup")

	mirror := filepath.Join(dir, "mirror.git")
	runGit(t, dir, "init", "--bare", mirror)
	runGit(t, mirror, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, mirror, "remote", "add", "--mirror=fetch", "origin", origin)
	runGit(t, mirror, "config", "branch.main.remote", viaBranch)
	runGit(t, mirror, "config", "remotes.default", "foo")
	if err := os.MkdirAll(filepath.Join(mirror, "branches"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "branches", "foo"), []byte(viaGroup+"#main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := syncMirror(t.Context(), mirror, []string{"origin"}, nil); err != nil {
		t.Fatalf("syncMirror() error = %v", err)
	}

	hasObject := func(commit string) bool {
		cmd := exec.CommandContext(t.Context(), "git", "cat-file", "-e", commit+"^{commit}")
		cmd.Dir = mirror
		return cmd.Run() == nil
	}
	if !hasObject(originCommit) {
		t.Errorf("origin commit %s was not fetched", originCommit)
	}

	fetchHead, err := os.ReadFile(filepath.Join(mirror, "FETCH_HEAD"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, other := range []struct{ name, path, commit string }{
		{"viabranch", viaBranch, viaBranchCommit},
		{"viagroup", viaGroup, viaGroupCommit},
	} {
		if hasObject(other.commit) {
			t.Errorf("%s commit %s is in the mirror, though %s was not named", other.name, other.commit, other.name)
		}
		if strings.Contains(string(fetchHead), other.commit) || strings.Contains(string(fetchHead), filepath.Base(other.path)) {
			t.Errorf("FETCH_HEAD mentions %s, which was not named:\n%s", other.name, fetchHead)
		}
	}
}
