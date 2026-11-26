package backend

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"golang.org/x/crypto/ssh"
)

// TestCreateRepositoryAnonymousOwner verifies that a repository created with
// a nil user (an anon-access/allow-keyless override) is owned by the
// lowest-ID admin rather than failing the NOT NULL repos.user_id constraint.
func TestCreateRepositoryAnonymousOwner(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Anonymous callers may create only where anon-access allows it.
	_, err = be.CreateRepository(ctx, "refused", nil, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	if err := be.SetAnonAccess(ctx, access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}

	repo, err := be.CreateRepository(ctx, "anon-repo", nil, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := repo.UserID(), admin.ID(); got != want {
		t.Fatalf("repo.UserID() = %v, want %v", got, want)
	}
}

// TestDefaultAdminUserIDNoAdmin verifies that defaultAdminUserID surfaces an
// error rather than silently returning a zero user ID (which would violate
// the NOT NULL repos.user_id constraint) when the database has no admin.
func TestDefaultAdminUserIDNoAdmin(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET admin = false"); err != nil {
			return err
		}

		_, err := be.defaultAdminUserID(ctx, tx)
		return err
	})
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// TestValidateImportRemote verifies that import remotes pointing at private,
// internal, or non-network destinations are rejected, that the ssh policy
// maps to the right user-facing error, and that accepted remotes carry the
// git environment that keeps the validation honest.
func TestValidateImportRemote(t *testing.T) {
	t.Parallel()
	allowed, err := ssrf.ParseAllowedHosts([]string{"git.internal.example", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	nobody := ssrf.GitRemotePolicy{}
	admin := ssrf.GitRemotePolicy{AllowAnySSHHost: true}
	listed := ssrf.GitRemotePolicy{AllowedSSHHosts: allowed}

	tests := []struct {
		name    string
		remote  string
		policy  ssrf.GitRemotePolicy
		wantErr error
	}{
		{"public https", "https://1.1.1.1/x.git", nobody, nil},
		{"public git", "git://1.1.1.1/x.git", nobody, nil},
		// With no allowlist, ssh is for admins only; the user sees an
		// authorization error, not a remote one.
		{"ssh admin", "ssh://git@10.0.0.1/x.git", admin, nil},
		{"ssh user", "ssh://git@10.0.0.1/x.git", nobody, proto.ErrUnauthorized},
		{"ssh allowed by name", "ssh://git@git.internal.example/x.git", listed, nil},
		{"ssh allowed by cidr", "git@10.1.1.1:x.git", listed, nil},
		{"ssh not on allowlist", "ssh://git@evil.example/x.git", listed, proto.ErrInvalidRemote},
		{"scp no user", "github.com:charmbracelet/soft-serve.git", admin, nil},
		{"scp ipv6", "git@[::1]:x.git", admin, nil},

		{"loopback", "http://127.0.0.1/x.git", nobody, proto.ErrInvalidRemote},
		{"localhost", "http://localhost:8080/x.git", nobody, proto.ErrInvalidRemote},
		{"private network", "http://10.0.0.1/x.git", nobody, proto.ErrInvalidRemote},
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/", nobody, proto.ErrInvalidRemote},
		{"git scheme private", "git://10.0.0.1/x.git", nobody, proto.ErrInvalidRemote},
		{"octal loopback", "http://0177.0.0.1/x.git", nobody, proto.ErrInvalidRemote},
		{"local path", "/data/repos/secret.git", nobody, proto.ErrInvalidRemote},
		{"file scheme", "file:///etc/passwd", nobody, proto.ErrInvalidRemote},
		{"empty", "", nobody, proto.ErrInvalidRemote},
		// A URL Go cannot parse but curl can is not classified as SSH,
		// which would skip validation entirely.
		{"unparseable http to loopback", "http://a{b@127.0.0.1:8080/", nobody, proto.ErrInvalidRemote},
	}

	d := &Backend{validateGitRemote: ssrf.ValidateGitRemote}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v, err := d.validateImportRemote(context.Background(), tt.remote, tt.policy)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatal("not true: errors.Is(err, tt.wantErr)")
				}
				return
			}

			if err != nil {
				t.Fatal(err)
			}
			env := ssrf.GitEnv(v)
			// Redirect following must be off, or a public remote can hand
			// off to an internal one after validation has passed.
			if !slices.Contains(env, "GIT_CONFIG_VALUE_0=false") {
				t.Fatal("not true: slices.Contains(env, \"GIT_CONFIG_VALUE_0=false\")")
			}
			if !slices.Contains(env, "GIT_CONFIG_KEY_0=http.followRedirects") {
				t.Fatal("not true: slices.Contains(env, \"GIT_CONFIG_KEY_0=http.followRedirects\")")
			}
		})
	}
}

// TestImportRepositoryCloneFailure verifies that a failed clone returns to the
// caller promptly with a fixed error and leaves nothing behind, and no
// goroutine.
func TestImportRepositoryCloneFailure(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not available")
	}

	be, _ := newTestBackend(t)
	ctx := context.Background()

	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// An ssh remote passes validation, and nothing listens on port 1 of
	// loopback, so the clone is refused immediately without touching the
	// network.
	type result struct {
		repo proto.Repository
		err  error
	}
	resc := make(chan result, 1)
	go func() {
		r, err := be.ImportRepository(ctx, "broken", admin, "ssh://127.0.0.1:1/x.git", proto.RepositoryOptions{})
		resc <- result{r, err}
	}()

	select {
	case res := <-resc:
		if res.repo != nil {
			t.Fatal("not true: res.repo == nil")
		}
		if !errors.Is(res.err, proto.ErrImportFailed) {
			t.Fatal("not true: errors.Is(res.err, proto.ErrImportFailed)")
		}
		// The clone's own message must not reach the caller.
		if got, want := res.err.Error(), proto.ErrImportFailed.Error(); got != want {
			t.Fatalf("res.err.Error() = %v, want %v", got, want)
		}
	case <-time.After(time.Minute):
		t.Fatal("ImportRepository did not return after a failed clone")
	}

	_, err = be.Repository(ctx, "broken")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	_, err = os.Stat(be.repoPath("broken"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("not true: errors.Is(err, os.ErrNotExist)")
	}
}

func TestRedactRemote(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"https://user:secret@example.com/x.git": "https://example.com/x.git",
		"https://TOKEN@example.com/x.git":       "https://example.com/x.git",
		"https://example.com/x.git?t=TOKEN":     "https://example.com/x.git",
		"https://example.com/x.git":             "https://example.com/x.git",
		"git@github.com:x/y.git":                "git@github.com:x/y.git",
		"ssh://git@10.0.0.1/x.git":              "ssh://10.0.0.1/x.git",
	}
	for in, want := range tests {
		if got := redactRemote(in); got != want {
			t.Errorf("redactRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestImportRepositorySSHRequiresAdmin verifies that with no ssh allowlist
// configured, only a server admin may import over ssh. The clone would
// present the server's client key to whatever host the remote names.
func TestImportRepositorySSHRequiresAdmin(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	if got := len(cfg.SSH.ParsedClientAllowedHosts()); got != 0 {
		t.Fatalf("len(cfg.SSH.ParsedClientAllowedHosts()) = %v, want %v", got, 0)
	}

	user, err := be.CreateUser(ctx, "tenant", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, remote := range []string{
		"ssh://git@10.0.0.1/x.git",
		"git+ssh://git@10.0.0.1/x.git",
		"git@10.0.0.1:x.git",
		"[git@10.0.0.1:2222]:x.git",
	} {
		_, err = be.ImportRepository(ctx, "tenant-ssh", user, remote, proto.RepositoryOptions{})
		if !errors.Is(err, proto.ErrUnauthorized) {
			t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
		} // remote
	}

	_, err = be.ImportRepository(ctx, "tenant-ssh", nil, "ssh://git@10.0.0.1/x.git", proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}

	// An ssh LFS endpoint is subject to the same policy as the remote.
	_, err = be.ImportRepository(ctx, "tenant-ssh", user, "https://1.1.1.1/x.git", proto.RepositoryOptions{LFSEndpoint: "ssh://git@10.0.0.1/x.git"})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}

	// An allowlist entry that does not cover the host is refused for
	// everyone, admin included, with the remote error rather than an
	// authorization one.
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	cfg.SSH.ClientAllowedHosts = []string{"git.internal.example"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	_, err = be.ImportRepository(ctx, "admin-ssh", admin, "ssh://git@10.0.0.1/x.git", proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrInvalidRemote) {
		t.Fatal("not true: errors.Is(err, proto.ErrInvalidRemote)")
	}
	if !errors.Is(err, ssrf.ErrSSHHostNotAllowed) {
		t.Fatal("not true: errors.Is(err, ssrf.ErrSSHHostNotAllowed)")
	}
	_, err = be.ImportRepository(ctx, "tenant-ssh", user, "ssh://git@10.0.0.1/x.git", proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrInvalidRemote) {
		t.Fatal("not true: errors.Is(err, proto.ErrInvalidRemote)")
	}

	// Nothing was created along the way.
	_, err = be.Repository(ctx, "tenant-ssh")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	_, err = be.Repository(ctx, "admin-ssh")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
}

// TestImportRepositorySSHAdminKey verifies that a caller whose session key is
// listed in initial_admin_keys may import over ssh with no allowlist, whether
// it has no account or a non-admin one, as IsServerAdmin treats it. The same caller without the key on the context, or with any
// other key, is still refused.
func TestImportRepositorySSHAdminKey(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not available")
	}

	for _, registered := range []bool{false, true} {
		name := "anonymous"
		if registered {
			name = "non-admin user"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			be, cfg := newTestBackend(t)
			ctx := backendTestContext(be, cfg)
			if got := len(cfg.SSH.ParsedClientAllowedHosts()); got != 0 {
				t.Fatalf("len(cfg.SSH.ParsedClientAllowedHosts()) = %v, want %v", got, 0)
			}

			pub := newTestPublicKey(t)
			setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(pub)))

			var user proto.User
			if registered {
				var err error
				user, err = be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{pub}})
				if err != nil {
					t.Fatal(err)
				}
				if user.IsAdmin() {
					t.Fatal("not true: !user.IsAdmin()")
				}
			}

			// Nothing listens on loopback port 1, so a remote that passes
			// policy fails at the clone without touching the network.
			remote := "ssh://git@127.0.0.1:1/x.git"
			opts := proto.RepositoryOptions{}

			_, err := be.ImportRepository(ctx, "nokey", user, remote, opts)
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
			} // no key on the context

			otherCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, newTestPublicKey(t))
			_, err = be.ImportRepository(otherCtx, "otherkey", user, remote, opts)
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
			} // not an admin key

			keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, pub)
			_, err = be.ImportRepository(keyCtx, "adminkey", user, remote, opts)
			if !errors.Is(err, proto.ErrImportFailed) {
				t.Fatal("not true: errors.Is(err, proto.ErrImportFailed)")
			} // admin key passes policy

			// An ssh LFS endpoint is checked, before the clone, against the
			// same policy, so the admin key covers it as well.
			lfsOpts := proto.RepositoryOptions{LFSEndpoint: remote}
			_, err = be.ImportRepository(otherCtx, "otherkey-lfs", user, remote, lfsOpts)
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
			}
			_, err = be.ImportRepository(keyCtx, "adminkey-lfs", user, remote, lfsOpts)
			// It passes policy, and is then refused as lfs objects are
			// not fetched over ssh.
			if !errors.Is(err, errLFSEndpointUnsupported) {
				t.Fatal("not true: errors.Is(err, errLFSEndpointUnsupported)")
			}

			// A mirror is synced later by the mirror job with its owner's
			// stored privileges. An anonymous caller's mirror is owned by
			// the default admin, so the key suffices. A non-admin account's
			// mirror would never sync, so it is refused with the reason.
			mirrorOpts := proto.RepositoryOptions{Mirror: true}
			_, err = be.ImportRepository(keyCtx, "adminkey-mirror", user, remote, mirrorOpts)
			if registered {
				if !errors.Is(err, proto.ErrUnauthorized) {
					t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
				}
				if !errors.Is(err, errMirrorImportOwnerNotAdmin) {
					t.Fatal("not true: errors.Is(err, errMirrorImportOwnerNotAdmin)")
				}
				if !errors.Is(err, errMirrorOwnerNotAdmin) {
					t.Fatal("not true: errors.Is(err, errMirrorOwnerNotAdmin)")
				}
			} else {
				if !errors.Is(err, proto.ErrImportFailed) {
					t.Fatal("not true: errors.Is(err, proto.ErrImportFailed)")
				}
			}
			mirrorOpts.LFSEndpoint = remote
			_, err = be.ImportRepository(keyCtx, "adminkey-mirror-lfs", user, remote, mirrorOpts)
			if registered {
				if !errors.Is(err, errMirrorOwnerNotAdmin) {
					t.Fatal("not true: errors.Is(err, errMirrorOwnerNotAdmin)")
				}
			} else {
				if !errors.Is(err, errLFSEndpointUnsupported) {
					t.Fatal("not true: errors.Is(err, errLFSEndpointUnsupported)")
				}
			}

			// A configured allowlist binds the admin key too. A host it
			// names is open to the mirror job as well, so a non-admin
			// account may mirror from it.
			cfg.SSH.ClientAllowedHosts = []string{"git.internal.example"}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			_, err = be.ImportRepository(keyCtx, "adminkey-allowlist", user, remote, opts)
			if !errors.Is(err, proto.ErrInvalidRemote) {
				t.Fatal("not true: errors.Is(err, proto.ErrInvalidRemote)")
			}
			if !errors.Is(err, ssrf.ErrSSHHostNotAllowed) {
				t.Fatal("not true: errors.Is(err, ssrf.ErrSSHHostNotAllowed)")
			}
			cfg.SSH.ClientAllowedHosts = []string{"127.0.0.1:1"}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			_, err = be.ImportRepository(keyCtx, "adminkey-allowed-mirror", user, remote, proto.RepositoryOptions{Mirror: true})
			if !errors.Is(err, proto.ErrImportFailed) {
				t.Fatal("not true: errors.Is(err, proto.ErrImportFailed)")
			}
		})
	}
}

// TestSetMirrorChecksOwnerPolicy verifies that switching mirroring on is
// refused when the mirror job, which validates every remote with
// RemotePolicy for the owner, would skip the repository on every run, and
// that an LFS endpoint the job would skip is not a refusal. Such a repository arises when a non-admin account holding an
// admin key imports from ssh without --mirror.
func TestSetMirrorChecksOwnerPolicy(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	if got := len(cfg.SSH.ParsedClientAllowedHosts()); got != 0 {
		t.Fatalf("len(cfg.SSH.ParsedClientAllowedHosts()) = %v, want %v", got, 0)
	}

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if alice.IsAdmin() {
		t.Fatal("not true: !alice.IsAdmin()")
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// newRepo creates a repository owned by owner whose git config holds
	// the given remote and, if non-empty, LFS endpoint, as an import does.
	newRepo := func(name string, owner proto.User, remote, lfsURL string) {
		t.Helper()
		_, err := seedCreate(be, ctx, name, owner, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		r, err := git.Open(t.Context(), be.repoPath(name))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.ConfigSet("remote.origin.url", remote); err != nil {
			t.Fatal(err)
		}
		if lfsURL != "" {
			if err := r.ConfigSet("lfs.url", lfsURL); err != nil {
				t.Fatal(err)
			}
		}
	}
	isMirror := func(name string) bool {
		t.Helper()
		r, err := be.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return r.IsMirror()
	}

	const sshRemote = "ssh://git@127.0.0.1:1/x.git"
	const httpsRemote = "https://1.1.1.1/x.git"

	// A non-admin owner's ssh remote would never sync, so it is refused,
	// and the flag and git config are left alone.
	newRepo("alice-ssh", alice, sshRemote, "")
	err = be.SetMirror(ctx, "alice-ssh", true)
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	if !errors.Is(err, errMirrorOwnerNotAdmin) {
		t.Fatal("not true: errors.Is(err, errMirrorOwnerNotAdmin)")
	}
	if isMirror("alice-ssh") {
		t.Fatal("not true: !isMirror(\"alice-ssh\")")
	}
	r, err := git.Open(t.Context(), be.repoPath("alice-ssh"))
	if err != nil {
		t.Fatal(err)
	}
	_, set, err := r.ConfigGet("remote.origin.mirror")
	if err != nil {
		t.Fatal(err)
	}
	if set {
		t.Fatal("not true: !set")
	}

	// An LFS endpoint that fails validation does not refuse the switch,
	// with LFS on or off: the job syncs the git data regardless and skips
	// only the LFS objects.
	newRepo("alice-lfs", alice, httpsRemote, sshRemote)
	if !cfg.LFS.Enabled {
		t.Fatal("not true: cfg.LFS.Enabled")
	}
	if err := be.SetMirror(ctx, "alice-lfs", true); err != nil {
		t.Fatal(err)
	}
	if !isMirror("alice-lfs") {
		t.Fatal("not true: isMirror(\"alice-lfs\")")
	}
	if err := be.SetMirror(ctx, "alice-lfs", false); err != nil {
		t.Fatal(err)
	}
	cfg.LFS.Enabled = false
	if err := be.SetMirror(ctx, "alice-lfs", true); err != nil {
		t.Fatal(err)
	}
	if !isMirror("alice-lfs") {
		t.Fatal("not true: isMirror(\"alice-lfs\")")
	}
	cfg.LFS.Enabled = true

	// Switching mirroring off is never refused.
	if err := be.SetMirror(ctx, "alice-ssh", false); err != nil {
		t.Fatal(err)
	}

	// A public https remote, and an admin owner's ssh remote, pass.
	newRepo("alice-https", alice, httpsRemote, "")
	if err := be.SetMirror(ctx, "alice-https", true); err != nil {
		t.Fatal(err)
	}
	if !isMirror("alice-https") {
		t.Fatal("not true: isMirror(\"alice-https\")")
	}
	newRepo("admin-ssh", admin, sshRemote, sshRemote)
	if err := be.SetMirror(ctx, "admin-ssh", true); err != nil {
		t.Fatal(err)
	}
	if !isMirror("admin-ssh") {
		t.Fatal("not true: isMirror(\"admin-ssh\")")
	}

	// An allowlist that names the host opens it to the job, and so to the
	// flag, for a non-admin owner. One that does not is refused with the
	// remote error, for an admin owner too.
	cfg.SSH.ClientAllowedHosts = []string{"127.0.0.1:1"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := be.SetMirror(ctx, "alice-ssh", true); err != nil {
		t.Fatal(err)
	}
	if !isMirror("alice-ssh") {
		t.Fatal("not true: isMirror(\"alice-ssh\")")
	}
	newRepo("alice-other", alice, "ssh://git@10.0.0.1/x.git", "")
	err = be.SetMirror(ctx, "alice-other", true)
	if !errors.Is(err, proto.ErrInvalidRemote) {
		t.Fatal("not true: errors.Is(err, proto.ErrInvalidRemote)")
	}
	if !errors.Is(err, ssrf.ErrSSHHostNotAllowed) {
		t.Fatal("not true: errors.Is(err, ssrf.ErrSSHHostNotAllowed)")
	}
	if isMirror("alice-other") {
		t.Fatal("not true: !isMirror(\"alice-other\")")
	}
	if err := be.SetMirror(ctx, "admin-ssh", false); err != nil {
		t.Fatal(err)
	}
	cfg.SSH.ClientAllowedHosts = []string{"git.internal.example"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	err = be.SetMirror(ctx, "admin-ssh", true)
	if !errors.Is(err, ssrf.ErrSSHHostNotAllowed) {
		t.Fatal("not true: errors.Is(err, ssrf.ErrSSHHostNotAllowed)")
	}
	if isMirror("admin-ssh") {
		t.Fatal("not true: !isMirror(\"admin-ssh\")")
	}
}

// TestSetMirrorValidatesOutsideTransaction verifies that SetMirror resolves
// remotes with a deadline and before it opens a transaction, so a slow
// resolver cannot hold the database lock and stall other writers.
func TestSetMirrorValidatesOutsideTransaction(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = seedCreate(be, ctx, "slow", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := git.Open(t.Context(), be.repoPath("slow"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ConfigSet("remote.origin.url", "https://1.1.1.1/x.git"); err != nil {
		t.Fatal(err)
	}

	entered := make(chan bool, 1)
	release := make(chan struct{})
	prev := be.validateGitRemote
	be.validateGitRemote = func(ctx context.Context, remote string, policy ssrf.GitRemotePolicy) (ssrf.ValidatedGitRemote, error) {
		_, hasDeadline := ctx.Deadline()
		entered <- hasDeadline
		<-release
		return prev(ctx, remote, policy)
	}

	done := make(chan error, 1)
	go func() { done <- be.SetMirror(ctx, "slow", true) }()
	if !(<-entered) {
		t.Fatal("not true: <-entered")
	} // the lookup is bounded

	// While validation is stalled, another writer still commits.
	_, err = be.CreateUser(ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	slow, err := be.Repository(ctx, "slow")
	if err != nil {
		t.Fatal(err)
	}
	if !slow.IsMirror() {
		t.Fatal("not true: slow.IsMirror()")
	}
}

// TestImportRepositoryDisabled verifies that when the installed git is too
// old to honour the remote guards, or its version cannot be read, imports are
// refused before any remote is touched, and that a new enough git leaves the
// gate open.
func TestImportRepositoryDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		version  git.Version
		err      error
		disabled bool
	}{
		{"too old", git.Version{Major: 2, Minor: 30, Patch: 2}, nil, true},
		// 2.33 to 2.36 understand GIT_CONFIG_* but not http.curloptResolve,
		// so they would fetch unpinned.
		{"no curloptResolve", git.Version{Major: 2, Minor: 34, Patch: 1}, nil, true},
		{"just below", git.Version{Major: 2, Minor: 36, Patch: 9}, nil, true},
		{"minimum", git.Version{Major: 2, Minor: 37}, nil, false},
		{"current", git.Version{Major: 2, Minor: 45}, nil, false},
		{"unreadable", git.Version{}, errors.New("exec: git: not found"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			be, _ := newTestBackend(t)
			be.binVersion = func(context.Context) (git.Version, error) { return tt.version, tt.err }
			ctx := context.Background()
			admin, err := be.User(ctx, "admin")
			if err != nil {
				t.Fatal(err)
			}

			reason := be.ImportsDisabledReason(context.Background())
			if got := reason != ""; got != tt.disabled {
				t.Fatalf("reason != \"\" = %v, want %v", got, tt.disabled)
			}
			if !tt.disabled {
				return
			}

			_, err = be.ImportRepository(ctx, "gated", admin, "https://1.1.1.1/x.git", proto.RepositoryOptions{})
			if !errors.Is(err, proto.ErrImportsDisabled) {
				t.Fatal("not true: errors.Is(err, proto.ErrImportsDisabled)")
			}
			// The reason is for the operator, not the user.
			if got, want := err.Error(), proto.ErrImportsDisabled.Error(); got != want {
				t.Fatalf("err.Error() = %v, want %v", got, want)
			}
			_, err = be.Repository(ctx, "gated")
			if !errors.Is(err, proto.ErrRepoNotFound) {
				t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
			}
		})
	}
}

// TestImportGateChecksOnce verifies that git's version is read once per
// backend, whatever the outcome: a version that could not be read keeps
// imports off until restart.
func TestImportGateChecksOnce(t *testing.T) {
	t.Parallel()
	var calls int
	err := errors.New("fork/exec git: resource temporarily unavailable")
	be, _ := newTestBackend(t)
	be.binVersion = func(context.Context) (git.Version, error) {
		calls++
		return git.Version{Major: 2, Minor: 45}, err
	}
	if be.ImportsDisabledReason(context.Background()) == "" {
		t.Fatal("not true: be.ImportsDisabledReason(context.Background()) != \"\"")
	}
	err = nil
	if be.ImportsDisabledReason(context.Background()) == "" {
		t.Fatal("not true: be.ImportsDisabledReason(context.Background()) != \"\"")
	}
	if got := calls; got != 1 {
		t.Fatalf("calls = %v, want %v", got, 1)
	}
}

// TestImportGateRechecksAfterCanceledCheck verifies that a check of git's
// version that its caller's context cut short is not kept: it says nothing
// about git, so it must not close imports until restart. The next check
// runs, and its answer is kept.
func TestImportGateRechecksAfterCanceledCheck(t *testing.T) {
	t.Parallel()
	var calls int
	be, _ := newTestBackend(t)
	be.binVersion = func(ctx context.Context) (git.Version, error) {
		calls++
		if err := ctx.Err(); err != nil {
			return git.Version{}, err
		}
		return git.Version{Major: 2, Minor: 45}, nil
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if be.ImportsDisabledReason(canceled) == "" {
		t.Fatal("not true: be.ImportsDisabledReason(canceled) != \"\"")
	}
	if got := be.ImportsDisabledReason(context.Background()); got != "" {
		t.Fatalf("be.ImportsDisabledReason(context.Background()) = %v, want %v", got, "")
	}
	if got := be.ImportsDisabledReason(canceled); got != "" {
		t.Fatalf("be.ImportsDisabledReason(canceled) = %v, want %v", got, "")
	}
	if got := calls; got != 2 {
		t.Fatalf("calls = %v, want %v", got, 2)
	}
}

// TestImportRefusesSSHLFSEndpoint verifies that an import asking for LFS
// objects from an ssh endpoint, given or derived from an ssh or scp-style
// remote, is refused with a reason before anything is cloned, rather than
// imported without its LFS objects and without a word.
func TestImportRefusesSSHLFSEndpoint(t *testing.T) {
	be, _ := newTestBackendWithLog(t, io.Discard)
	ctx := context.Background()
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, remote string
		opts         proto.RepositoryOptions
	}{
		{"ssh-remote", "ssh://TOKEN@127.0.0.1:1/x.git", proto.RepositoryOptions{LFS: true}},
		{"scp-remote", "git@127.0.0.1:x.git", proto.RepositoryOptions{LFS: true}},
		{"ssh-endpoint", "ssh://127.0.0.1:1/x.git", proto.RepositoryOptions{LFSEndpoint: "ssh://TOKEN@127.0.0.1:1/x.git"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := be.ImportRepository(ctx, tc.name, admin, tc.remote, tc.opts)
			if !errors.Is(err, errLFSEndpointUnsupported) {
				t.Fatalf("ImportRepository = %v, want %v", err, errLFSEndpointUnsupported)
			}
			if strings.Contains(err.Error(), "TOKEN") {
				t.Errorf("error names the credentials: %v", err)
			}
			if _, err := os.Stat(be.repoPath(tc.name)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the refused import left %s behind: %v", be.repoPath(tc.name), err)
			}
		})
	}
}
