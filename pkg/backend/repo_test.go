package backend

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
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
// caller within a minute with the fixed ErrImportFailed, not git's message, and
// leaves neither a repository row nor a directory behind.
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

// AboutRepo is reserved for server admins whatever AdminOnlyCreate says: a
// registered user and an anonymous caller can neither create nor import it,
// nor rename a repository to it.
func TestAboutRepoReservedForAdmins(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	if cfg.AdminOnlyCreate {
		t.Fatal("not true: !cfg.AdminOnlyCreate")
	}
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	userCtx := proto.WithUserContext(ctx, user)

	_, err = be.CreateRepository(userCtx, AboutRepo, user, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.CreateRepository(userCtx, "/"+AboutRepo+".git", user, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.CreateRepository(ctx, AboutRepo, nil, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	// A case variant takes the same directory on a case-insensitive
	// filesystem, so it is reserved too.
	_, err = be.CreateRepository(userCtx, ".Soft-Serve", user, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.CreateRepository(ctx, ".SOFT-SERVE", nil, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.ImportRepository(userCtx, ".Soft-Serve", user, offlineRemote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.ImportRepository(userCtx, AboutRepo, user, offlineRemote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}

	_, err = be.CreateRepository(userCtx, "mine", user, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(be.RenameRepository(userCtx, "mine", AboutRepo), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(be.RenameRepository(userCtx, \"mine\", AboutRepo), proto.ErrUnauthorized)")
	}
	if !errors.Is(be.RenameRepository(userCtx, "mine", ".Soft-Serve"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(be.RenameRepository(userCtx, \"mine\", \".Soft-Serve\"), proto.ErrUnauthorized)")
	}

	_, err = be.Repository(ctx, AboutRepo)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = be.Repository(ctx, "mine")
	if err != nil {
		t.Fatal(err)
	}
}

// A server admin may create AboutRepo, and rename a repository to it.
func TestAboutRepoAllowedForAdmins(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := proto.WithUserContext(ctx, admin)

	_, err = be.CreateRepository(adminCtx, AboutRepo, admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteRepository(adminCtx, AboutRepo); err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(adminCtx, "draft", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.RenameRepository(adminCtx, "draft", AboutRepo); err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, AboutRepo)
	if err != nil {
		t.Fatal(err)
	}
}

// AboutRepoOwnedByAdmin holds only for an AboutRepo an admin created and
// still owns. One an anonymous caller created, which is recorded with the
// default admin as owner, is refused: such a repository can predate the
// reservation of the name for admins.
func TestAboutRepoOwnedByAdmin(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := proto.WithUserContext(ctx, admin)

	aboutRepo := func() proto.Repository {
		t.Helper()
		repos, err := be.Repositories(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range repos {
			if r.Name() == AboutRepo {
				return r
			}
		}
		t.Fatalf("%s not found", AboutRepo)
		return nil
	}

	// Created by an admin: shown, until the owner is no longer an admin.
	_, err = be.CreateRepository(adminCtx, AboutRepo, admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !be.AboutRepoOwnedByAdmin(ctx, aboutRepo()) {
		t.Fatal("not true: be.AboutRepoOwnedByAdmin(ctx, aboutRepo())")
	}
	if err := be.SetAdmin(ctx, "root", false); err != nil {
		t.Fatal(err)
	}
	if be.AboutRepoOwnedByAdmin(ctx, aboutRepo()) {
		t.Fatal("not true: !be.AboutRepoOwnedByAdmin(ctx, aboutRepo())")
	}
	if err := be.SetAdmin(ctx, "root", true); err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteRepository(adminCtx, AboutRepo); err != nil {
		t.Fatal(err)
	}

	// Created anonymously before the name was reserved: the row names an
	// admin as owner, but it is not shown.
	if _, err := be.store.CreateRepo(ctx, be.db, models.Repo{
		Name:      AboutRepo,
		UserID:    admin.ID(),
		Anonymous: true,
	}); err != nil {
		t.Fatal(err)
	}
	r := aboutRepo()
	if got, want := r.UserID(), admin.ID(); got != want {
		t.Fatalf("r.UserID() = %v, want %v", got, want)
	}
	if be.AboutRepoOwnedByAdmin(ctx, r) {
		t.Fatal("not true: !be.AboutRepoOwnedByAdmin(ctx, r)")
	}
}

// DefaultPrivate is the visibility of a repository created without an
// explicit one. With default_private set, it is private for users and
// server admins, and refused for anonymous callers, who could not reach a
// private repository.
func TestDefaultPrivate(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Off (the default): public for everyone, anonymous callers included.
	for _, u := range []proto.User{alice, admin, nil} {
		private, err := be.DefaultPrivate(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if private {
			t.Fatal("not true: !private")
		}
	}

	cfg.DefaultPrivate = true
	for _, u := range []proto.User{alice, admin} {
		private, err := be.DefaultPrivate(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if !private {
			t.Fatal("not true: private")
		}
	}

	_, err = be.DefaultPrivate(ctx, nil)
	if !errors.Is(err, proto.ErrAnonymousPrivate) {
		t.Fatal("not true: errors.Is(err, proto.ErrAnonymousPrivate)")
	}

	// A key in initial_admin_keys that no account holds is a server admin
	// and has access to every repository, so it gets the private default.
	pub := newTestPublicKey(t)
	setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(pub)))
	keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, pub)
	private, err := be.DefaultPrivate(keyCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !private {
		t.Fatal("not true: private")
	}
}

// Once an --lfs import has registered its repository, the owner can delete
// it and create another under the name while the LFS objects are fetched.
// A failed fetch then removes the import's own repository only: the one now
// under the name is left alone: the cleanup does not delete by name.
func TestFailedImportDeletesByIdentity(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx, cancel := context.WithCancel(asAdmin(t, be, backendTestContext(be, cfg)))
	defer cancel()
	admin := defaultAdmin(t, be)

	src := t.TempDir()
	if out, err := exec.CommandContext(context.Background(), "git", "init", "--bare", "--quiet", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	// Right after the import registered its repository: replace it, and
	// stop the import, so its LFS fetch fails.
	var replacement proto.Repository
	var origID int64
	be.afterRegister = func() {
		be.afterRegister = nil
		other := backendTestContext(be, cfg)
		orig, err := be.Repository(other, "imported")
		if err != nil {
			t.Fatal(err)
		}
		origID = orig.ID()
		if err := be.DeleteRepository(other, "imported"); err != nil {
			t.Fatal(err)
		}
		// What the import's LFS fetch had stored after the delete removed
		// the repository's LFS storage.
		if err := os.MkdirAll(filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(origID, 10), "objects"), 0o755); err != nil {
			t.Fatal(err)
		}
		replacement, err = be.CreateRepository(other, "imported", admin, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
	}

	imp := importSpec{
		name:        "imported",
		path:        be.repoPath("imported"),
		remote:      src,
		logRemote:   src,
		lfsEndpoint: "http://127.0.0.1:1/x.git",
		lfs:         &fakeLFSClient{},
	}
	if err := claimRepoPath(imp.path); err != nil {
		t.Fatal(err)
	}
	_, err := be.importRepository(ctx, admin, imp)
	if err == nil {
		t.Fatal("not true: err != nil")
	} // the fetch failed

	r, err := be.Repository(context.Background(), "imported")
	if err != nil {
		t.Fatal(err)
	} // the repository now under the name is kept
	if got, want := r.ID(), replacement.ID(); got != want {
		t.Fatalf("r.ID() = %v, want %v", got, want)
	}

	// The import set its LFS endpoint in its own clone only, not in the
	// config of the repository now under the name.
	gr, err := r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	lfsURL, _, err := gr.ConfigGet("lfs.url")
	if err != nil {
		t.Fatal(err)
	}
	if got := lfsURL; got != "" {
		t.Fatalf("lfsURL = %v, want %v", got, "")
	}

	// The LFS storage of the import's own repository, gone with its row,
	// is removed.
	_, err = os.Stat(filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(origID, 10)))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// longAgo is the date of the commits these tests make, far from the time
// any row is written.
var longAgo = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

// commitLongAgo points refs/heads/main of the repository at dir at a new
// empty commit dated longAgo, without running any hook.
func commitLongAgo(t *testing.T, dir string) {
	t.Helper()
	tree, err := exec.CommandContext(t.Context(), "git", "-C", dir, "mktree").Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "commit-tree", strings.TrimSpace(string(tree)), "-m", "old")
	date := longAgo.Format(time.RFC3339)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b", "GIT_COMMITTER_DATE="+date)
	commit, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.CommandContext(t.Context(), "git", "-C", dir, "update-ref", "refs/heads/main", strings.TrimSpace(string(commit))).Run(); err != nil {
		t.Fatal(err)
	}
}

// UpdatedAt runs no git: without info/last-modified it is the time the row
// last changed, not the latest commit's, so a TUI listing runs no git for
// a repository nothing has been pushed to.
func TestUpdatedAtFallsBackToRow(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commitLongAgo(t, be.repoPath("repo"))

	r, err := be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if r.UpdatedAt().IsZero() {
		t.Fatal("not true: !r.UpdatedAt().IsZero()")
	}
	if !r.UpdatedAt().After(longAgo.Add(24 * time.Hour)) {
		t.Fatal("not true: r.UpdatedAt().After(longAgo.Add(24 * time.Hour))")
	} // the row's time, not the commit's
}

// An import and a mirror sync run no hooks, so they record the time of the
// latest commit themselves.
func TestImportAndMirrorSyncRecordLastModified(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	src := t.TempDir()
	if out, err := exec.CommandContext(context.Background(), "git", "init", "--bare", "--quiet", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	commitLongAgo(t, src)

	imp := importSpec{name: "imported", path: be.repoPath("imported"), remote: src, logRemote: src}
	if err := claimRepoPath(imp.path); err != nil {
		t.Fatal(err)
	}
	r, err := be.importRepository(ctx, defaultAdmin(t, be), imp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(imp.path + "/info/last-modified")
	if err != nil {
		t.Fatal(err)
	}
	if !r.UpdatedAt().Equal(longAgo) {
		t.Fatal("not true: r.UpdatedAt().Equal(longAgo)")
	}

	// A sync that fetched a newer history records it.
	if err := os.Remove(imp.path + "/info/last-modified"); err != nil {
		t.Fatal(err)
	}
	if err := be.RecordMirrorSync(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(imp.path + "/info/last-modified")
	if err != nil {
		t.Fatal(err)
	}
	r, err = be.Repository(ctx, "imported")
	if err != nil {
		t.Fatal(err)
	}
	if !r.UpdatedAt().Equal(longAgo) {
		t.Fatal("not true: r.UpdatedAt().Equal(longAgo)")
	}
}

// An empty import, or a sync of an empty mirror, has no commit to record:
// it is not a failure to warn about.
func TestEmptyImportRecordsNoLastModified(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	src := t.TempDir()
	if out, err := exec.CommandContext(context.Background(), "git", "init", "--bare", "--quiet", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	imp := importSpec{name: "empty", path: be.repoPath("empty"), remote: src, logRemote: src}
	if err := claimRepoPath(imp.path); err != nil {
		t.Fatal(err)
	}
	r, err := be.importRepository(ctx, defaultAdmin(t, be), imp)
	if err != nil {
		t.Fatal(err)
	}
	if err := be.RecordMirrorSync(ctx, r, nil); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(logs.String(), "failed to record the last change") {
		t.Fatal("not true: !strings.Contains(logs.String(), \"failed to record the last change\")")
	}
	_, err = os.Stat(imp.path + "/info/last-modified")
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// info/last-modified is a data file: it is written readable and writable as
// the umask allows, like the files git writes, and never executable.
func TestLastModifiedIsNotExecutable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no execute bits")
	}
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commitLongAgo(t, be.repoPath("repo"))

	if err := be.populateLastModified(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(be.repoPath("repo"), "info", "last-modified"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&^0o666 != 0 {
		t.Fatalf("info/last-modified has mode %v, want no bits beyond 0666", mode)
	}
}

// TestSetMirrorConfig pins what SetMirror writes to a repository's git
// config: mirror = true on the remote with a URL, removed again when
// mirroring is switched off, and nothing else touched.
func TestSetMirrorConfig(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = seedCreate(be, ctx, "m", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rp := be.repoPath("m")

	cp := filepath.Join(rp, "config")
	f, err := os.OpenFile(cp, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A remote named with a dot and capitals, with two urls (the last is
	// the one read) and a section split in two, one without a url and one
	// whose url is empty.
	_, err = f.WriteString(`[remote "Up.Per"]
	url = https://1.1.1.1/a.git
	url = https://1.1.1.1/b.git
	fetch = +refs/*:refs/*
[remote "nourl"]
	fetch = +refs/*:refs/*
[remote "empty"]
	url =
[custom]
	keep = "me ; # too"
[remote "Up.Per"]
	pushurl = https://1.1.1.1/p.git
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	get := func(key string) string {
		t.Helper()
		out, err := gittestConfig(t, cp, key)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	if err := be.SetMirror(ctx, "m", true); err != nil {
		t.Fatal(err)
	}
	if got := get("remote.Up.Per.mirror"); got != "true" {
		t.Fatalf("get(\"remote.Up.Per.mirror\") = %v, want %v", got, "true")
	}
	if got := get("remote.nourl.mirror"); got != "" {
		t.Fatalf("get(\"remote.nourl.mirror\") = %v, want %v", got, "")
	}
	if got := get("remote.empty.mirror"); got != "" {
		t.Fatalf("get(\"remote.empty.mirror\") = %v, want %v", got, "")
	}
	if got := get("custom.keep"); got != "me ; # too" {
		t.Fatalf("get(\"custom.keep\") = %v, want %v", got, "me ; # too")
	}
	if got := get("remote.Up.Per.url"); got != "https://1.1.1.1/b.git" {
		t.Fatalf("get(\"remote.Up.Per.url\") = %v, want %v", got, "https://1.1.1.1/b.git")
	}
	if got := get("remote.Up.Per.pushurl"); got != "https://1.1.1.1/p.git" {
		t.Fatalf("get(\"remote.Up.Per.pushurl\") = %v, want %v", got, "https://1.1.1.1/p.git")
	}

	if err := be.SetMirror(ctx, "m", false); err != nil {
		t.Fatal(err)
	}
	if got := get("remote.Up.Per.mirror"); got != "" {
		t.Fatalf("get(\"remote.Up.Per.mirror\") = %v, want %v", got, "")
	}
	if got := get("custom.keep"); got != "me ; # too" {
		t.Fatalf("get(\"custom.keep\") = %v, want %v", got, "me ; # too")
	}
	if got := get("remote.Up.Per.url"); got != "https://1.1.1.1/b.git" {
		t.Fatalf("get(\"remote.Up.Per.url\") = %v, want %v", got, "https://1.1.1.1/b.git")
	}
}

// A repository with two remotes can't be made a mirror: the mirror job
// fetches every ref of a remote with --prune, so each would delete and
// overwrite the other's refs. Nothing is written.
func TestSetMirrorRefusesTwoRemotes(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = seedCreate(be, ctx, "m", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(be.repoPath("m"), "config")
	f, err := os.OpenFile(cp, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`[remote "origin"]
	url = https://1.1.1.1/a.git
[remote "backup"]
	url = https://1.1.1.1/b.git
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	err = be.SetMirror(ctx, "m", true)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if !strings.Contains(err.Error(), "more than one remote") {
		t.Fatal("not true: strings.Contains(err.Error(), \"more than one remote\")")
	}
	out, err := gittestConfig(t, cp, "remote.origin.mirror")
	if err != nil {
		t.Fatal(err)
	}
	if got := out; got != "" {
		t.Fatalf("out = %v, want %v", got, "")
	}
	r, err := be.Repository(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if r.IsMirror() {
		t.Fatal("not true: !r.IsMirror()")
	}
}

// gittestConfig returns the last value of key in the config file at path,
// or "" when it is not set.
func gittestConfig(t *testing.T, path, key string) (string, error) {
	t.Helper()
	out := gittest.Git(t, filepath.Dir(path), nil, "config", "--file", path, "--get", "--default", "", key)
	return strings.TrimSpace(out), nil
}

// pausingMirrorStore runs paused inside SetMirror's transaction, after its
// checks and before the flag is written.
type pausingMirrorStore struct {
	store.Store
	paused func()
}

func (s *pausingMirrorStore) SetRepoIsMirrorByName(ctx context.Context, h db.Handler, name string, isMirror bool) error {
	s.paused()
	return s.Store.SetRepoIsMirrorByName(ctx, h, name, isMirror)
}

// On Postgres SetMirror holds the row it checked until it commits, so a
// rename of the repository waits for it rather than slip in between the
// check and the write.
func TestSetMirrorLocksCheckedRowPostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	r, err := git.Open(t.Context(), e.be.repoPath("repo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ConfigSet("remote.origin.url", "https://1.1.1.1/x.git"); err != nil {
		t.Fatal(err)
	}
	e.be.validateGitRemote = func(context.Context, string, ssrf.GitRemotePolicy) (ssrf.ValidatedGitRemote, error) {
		return ssrf.ValidatedGitRemote{}, nil
	}

	checked := make(chan struct{})
	release := make(chan struct{})
	e.be.store = &pausingMirrorStore{Store: e.be.store, paused: func() {
		close(checked)
		<-release
	}}
	mirrored := make(chan error, 1)
	go func() { mirrored <- e.be.SetMirror(e.ctx, "repo", true) }()
	<-checked

	renamed := make(chan error, 1)
	go func() { renamed <- e.be.RenameRepository(e.ctx, "repo", "moved") }()
	waitForLockWaiter(t, e, renamed, "RenameRepository")
	close(release)

	if err := <-mirrored; err != nil {
		t.Fatal(err)
	}
	if err := <-renamed; err != nil {
		t.Fatal(err)
	}
	moved, err := e.be.Repository(e.ctx, "moved")
	if err != nil {
		t.Fatal(err)
	}
	if !moved.IsMirror() {
		t.Fatal("not true: moved.IsMirror()")
	}
}

// On Postgres the re-check after a failed rename waits for a transaction
// that holds the row's lock, as a COMMIT still in flight on the lost
// connection does, and sees the row as it leaves it. Here the rename's own
// transaction fails, and another transaction, standing in for the COMMIT
// in flight, renames the row to the new name and commits only once the
// re-check waits for it. A plain read would see the old name, move the
// directory back, and leave it away from its row.
func TestRenameRepositoryRecheckWaitsForRowLockPostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	_, err := e.be.db.ExecContext(e.ctx, `CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	e.be.store = unpinnableRenameStore{Store: e.be.store}

	committed := make(chan error, 1)
	e.be.renameTxDone = func(err error) error {
		// The rename's transaction has failed and rolled back. Hold the
		// row's lock with the rename that, from here, is in flight.
		tx, berr := beginRawTx(t, e.ctx, e.be.db)
		if berr != nil {
			committed <- berr
			return err
		}
		if _, xerr := tx.ExecContext(e.ctx, `UPDATE repos SET name = 'new' WHERE id = $1`, e.repo.ID()); xerr != nil {
			_ = tx.Rollback()
			committed <- xerr
			return err
		}
		go func() {
			// Commit once the re-check waits for the lock.
			deadline := time.Now().Add(time.Minute)
			for {
				var waiting bool
				if qerr := e.be.db.GetContext(context.Background(), &waiting, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock');`); qerr != nil {
					_ = tx.Rollback()
					committed <- qerr
					return
				}
				if waiting {
					committed <- tx.Commit()
					return
				}
				if time.Now().After(deadline) {
					_ = tx.Rollback()
					committed <- errors.New("the re-check did not wait for the row's lock")
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		return err
	}

	if err := e.be.RenameRepository(e.ctx, "repo", "new"); err != nil {
		t.Fatal(err)
	}
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	r, err := e.be.Repository(e.ctx, "new")
	if err != nil {
		t.Fatal(err)
	} // the row and the directory are under the new name
	_, err = os.Stat(filepath.Join(e.be.repoPath("new"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

// On Postgres too, when the outcome of a rename cannot be read either, the
// directory stays where it is rather than risk moving it away from its row.
// The re-check's locked lookup fails here; newTestBackend is SQLite only
// and never reaches it.
func TestRenameRepositoryCommitOutcomeUnknownLeavesDirectoryPostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	e.be.store = failingByIDStore{Store: e.be.store}
	e.be.renameTxDone = func(err error) error {
		if err == nil {
			return errors.New("driver: bad connection")
		}
		return err
	}

	if e.be.RenameRepository(e.ctx, "repo", "new") == nil {
		t.Fatal("not true: e.be.RenameRepository(e.ctx, \"repo\", \"new\") != nil")
	}
	_, err := os.Stat(filepath.Join(e.be.repoPath("new"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	} // where the committed row puts it
	_, err = os.Lstat(e.be.repoPath("repo"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// createCaseVariantFixture creates repository "secret" and reports whether
// the filesystem finds its directory under the case variant "SECRET" too.
func createCaseVariantFixture(t *testing.T, be *Backend) bool {
	t.Helper()
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "secret", owner, proto.RepositoryOptions{Private: true}); err != nil {
		t.Fatal(err)
	}
	return be.repoPathExists("SECRET")
}

// assertSecretIntact checks that repository "secret" still has its row and
// its directory.
func assertSecretIntact(t *testing.T, be *Backend) {
	t.Helper()
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))
	r, err := be.Repository(ctx, "secret")
	if err != nil {
		t.Fatalf("repository row gone: %v", err)
	}
	if !r.IsPrivate() {
		t.Error("repository is no longer private")
	}
	if _, err := os.Stat(filepath.Join(be.repoPath("secret"), "HEAD")); err != nil {
		t.Errorf("repository directory damaged: %v", err)
	}
}

// On a case-insensitive filesystem the path of "SECRET" is the directory of
// "secret". Deleting the case variant, which has no row, must not treat
// that directory as an orphan and remove it, which would leave the real
// repository's row without a repository.
func TestDeleteRepositoryCaseVariantKeepsRepository(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	if !createCaseVariantFixture(t, be) {
		t.Skip("filesystem is case-sensitive: SECRET is a different repository")
	}
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	if err := be.DeleteRepository(ctx, "SECRET"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("DeleteRepository(SECRET) => %v, want ErrRepoNotFound", err)
	}
	assertSecretIntact(t, be)
}

// On a case-insensitive filesystem renaming the case variant must not move
// the directory of "secret" while no row is renamed, which would strand the
// repository.
func TestRenameRepositoryCaseVariantKeepsRepository(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	if !createCaseVariantFixture(t, be) {
		t.Skip("filesystem is case-sensitive: SECRET is a different repository")
	}
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	if err := be.RenameRepository(ctx, "SECRET", "other"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("RenameRepository(SECRET) => %v, want ErrRepoNotFound", err)
	}
	assertSecretIntact(t, be)
	if be.repoPathExists("other") {
		t.Error("directory moved to the new name")
	}
	if _, err := be.Repository(ctx, "other"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("Repository(other) => %v, want ErrRepoNotFound", err)
	}
}

// A rename needs the row: a directory with no row is left where it is, and
// the rename does not report success for a repository that does not exist.
func TestRenameRepositoryWithoutRowLeavesDirectory(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	orphan := be.repoPath("orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	if !errors.Is(be.RenameRepository(ctx, "orphan", "other"), proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(be.RenameRepository(ctx, \"orphan\", \"other\"), proto.ErrRepoNotFound)")
	}
	_, err := os.Stat(orphan)
	if err != nil {
		t.Fatal(err)
	}
	if be.repoPathExists("other") {
		t.Fatal("not true: !be.repoPathExists(\"other\")")
	}
}

// A delete needs the row: a directory with no row is left in place and the
// name reported as not found. It is reported when the server starts (see
// OrphanRepositoryDirs).
func TestDeleteRepositoryLeavesOrphanDirectory(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	orphan := be.repoPath("orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	if !errors.Is(be.DeleteRepository(ctx, "orphan"), proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(be.DeleteRepository(ctx, \"orphan\"), proto.ErrRepoNotFound)")
	}
	_, err := os.Stat(orphan)
	if err != nil {
		t.Fatal(err)
	}
}

// APFS ignores Unicode normalization as well as case, so the decomposed form
// of a repository's name leads to the directory of the precomposed one. Names
// like these are not case variants of each other, so the delete must compare
// the directory against every repository, not only names that fold alike.
func TestDeleteRepositoryNormalizationVariantKeepsRepository(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	const (
		precomposed = "\uD55C"             // 한
		decomposed  = "\u1112\u1161\u11AB" // 한 as jamo
	)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, precomposed, owner, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if !be.repoPathExists(decomposed) {
		t.Skip("filesystem keeps Unicode normalization forms apart")
	}

	if err := be.DeleteRepository(ctx, decomposed); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("DeleteRepository(decomposed) => %v, want ErrRepoNotFound", err)
	}
	if _, err := be.Repository(ctx, precomposed); err != nil {
		t.Fatalf("repository row gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(be.repoPath(precomposed), "HEAD")); err != nil {
		t.Errorf("repository directory damaged: %v", err)
	}
}

// On a case-insensitive filesystem a repository can be named "Secret" while
// its directory is still spelled secret.git. Deleting "secret", which has no
// row and whose directory is spelled exactly so on disk, must not treat that
// directory as an orphan: it is the directory of "Secret".
func TestDeleteRepositoryExactSpellingOfAnotherRepositoryKeepsIt(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	if !createCaseVariantFixture(t, be) {
		t.Skip("filesystem is case-sensitive: Secret has its own directory")
	}
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	if err := be.store.SetRepoNameByName(ctx, be.db, "secret", "Secret"); err != nil {
		t.Fatal(err)
	}

	if err := be.DeleteRepository(ctx, "secret"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("DeleteRepository(secret) => %v, want ErrRepoNotFound", err)
	}
	if _, err := be.Repository(ctx, "Secret"); err != nil {
		t.Fatalf("repository row gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(be.repoPath("Secret"), "HEAD")); err != nil {
		t.Errorf("repository directory damaged: %v", err)
	}
}

// RemoveCollaborator takes the name in any case, like AddCollaborator, and
// its event carries the stored, lowercase name.
func TestRemoveCollaboratorEventNamesStoredUsername(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	id := e.seedHook(t, webhook.EventCollaborator)

	if err := e.be.RemoveCollaborator(e.ctx, "repo", "BOB"); err != nil {
		t.Fatal(err)
	}
	e.awaitDeliveries(t, id, webhook.EventCollaborator)

	var body string
	if err := e.be.db.GetContext(e.ctx, &body, e.be.db.Rebind("SELECT request_body FROM webhook_deliveries WHERE webhook_id = ?"), id); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"username":"bob"`) {
		t.Fatal("not true: strings.Contains(body, `\"username\":\"bob\"`)")
	}
	if strings.Contains(body, "BOB") {
		t.Fatal("not true: !strings.Contains(body, \"BOB\")")
	}
}

// refuseRepoUpdate makes every update of column on repos fail.
func refuseRepoUpdate(t *testing.T, e whEnv, column string) {
	t.Helper()
	refuseTrigger(t, e.ctx, e.be.db, "refuse_update", "BEFORE", "UPDATE OF "+column, "repos")
}

// A description the database refuses leaves the stored one as it was.
func TestSetDescriptionDBFailureKeepsDescription(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	if err := e.be.SetDescription(e.ctx, "repo", "before"); err != nil {
		t.Fatal(err)
	}
	refuseRepoUpdate(t, e, "description")

	if e.be.SetDescription(e.ctx, "repo", "after") == nil {
		t.Fatal("not true: e.be.SetDescription(e.ctx, \"repo\", \"after\") != nil")
	}
	r, err := e.be.Repository(e.ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Description(); got != "before" {
		t.Fatalf("r.Description() = %v, want %v", got, "before")
	}
}

// A visibility change the database refuses leaves the repository as it was.
func TestSetPrivateDBFailureKeepsVisibility(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	refuseRepoUpdate(t, e, "private")

	if e.be.SetPrivate(e.ctx, "repo", true) == nil {
		t.Fatal("not true: e.be.SetPrivate(e.ctx, \"repo\", true) != nil")
	}
	r, err := e.be.Repository(e.ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if r.IsPrivate() {
		t.Fatal("not true: !r.IsPrivate()")
	}
}

// createDeleteFixture creates repository "delrepo" and returns its directory.
func createDeleteFixture(t *testing.T, be *Backend) string {
	t.Helper()
	ctx := backendTestContext(be, be.cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "delrepo", owner, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	return be.repoPath("delrepo")
}

// A delete whose commit fails leaves the repository, so its directory must
// stay too: it is removed after the commit, so a failed commit cannot leave
// a row with no repository behind it.
func TestDeleteRepositoryCommitFailureKeepsDirectory(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	rp := createDeleteFixture(t, be)
	ctx := backendTestContext(be, cfg)

	// A deferred foreign key is checked at commit, so the delete statements
	// succeed and the commit fails.
	for _, q := range []string{
		`CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO pin (repo_id) SELECT id FROM repos WHERE name = 'delrepo'`,
	} {
		if _, err := be.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	if err := be.DeleteRepository(ctx, "delrepo"); err == nil {
		t.Fatal("DeleteRepository succeeded although the commit was blocked")
	}
	if _, err := be.store.GetRepoByName(ctx, be.db, "delrepo"); err != nil {
		t.Fatalf("repository row gone after a failed delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rp, "HEAD")); err != nil {
		t.Errorf("repository directory damaged by a failed delete: %v", err)
	}
}

// Once the row is gone the delete has happened: a directory that cannot be
// removed is logged rather than reported as a failed delete, and nothing can
// take the leftover directory over. It is an orphan, reported at start-up.
func TestDeleteRepositoryDirectoryRemovalFailure(t *testing.T) {
	t.Parallel()
	// As in os/removeall_test.go: on Windows a mode only sets the read-only
	// attribute, which does not stop removal, and root ignores the mode.
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not prevent removal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can remove the directory regardless of its mode")
	}
	var logs bytes.Buffer
	be, cfg := newTestBackendWithLog(t, &logs)
	rp := createDeleteFixture(t, be)
	ctx := backendTestContext(be, cfg)

	// A directory without write permission keeps its entries.
	objects := filepath.Join(rp, "objects")
	if err := os.Chmod(objects, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(objects, 0o755) }) //nolint:errcheck

	if err := be.DeleteRepository(ctx, "delrepo"); err != nil {
		t.Fatalf("DeleteRepository => %v, want nil once the row is gone", err)
	}
	if _, err := be.store.GetRepoByName(ctx, be.db, "delrepo"); err == nil {
		t.Error("repository row still exists")
	}
	if !strings.Contains(logs.String(), "failed to delete repository directory") {
		t.Errorf("removal failure not logged; logs:\n%s", logs.String())
	}
	if _, err := os.Stat(rp); err != nil {
		t.Fatalf("expected the leftover directory to remain: %v", err)
	}

	// The leftover directory is not free for the taking.
	owner, err := be.User(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "delrepo", owner, proto.RepositoryOptions{}); !errors.Is(err, proto.ErrRepoExist) {
		t.Errorf("CreateRepository over the leftover directory => %v, want ErrRepoExist", err)
	}

	// A delete needs the row, so deleting again leaves the directory; it
	// is reported at start-up as an orphan.
	if err := os.Chmod(objects, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteRepository(ctx, "delrepo"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatalf("second DeleteRepository => %v, want ErrRepoNotFound", err)
	}
	if _, err := os.Stat(rp); err != nil {
		t.Errorf("leftover directory removed by a delete without a row: %v", err)
	}
	orphans, err := be.OrphanRepositoryDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != rp {
		t.Errorf("OrphanRepositoryDirs => %q, want [%q]", orphans, rp)
	}
}

// swapAfterLookupStore runs swap once, right after the first lookup of name
// by GetRepoByName, outside any transaction: what a rename and a create by
// another session in that moment would do.
type swapAfterLookupStore struct {
	store.Store
	name    string
	swapped atomic.Bool
	swap    func()
}

func (s *swapAfterLookupStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	m, err := s.Store.GetRepoByName(ctx, h, name)
	if name == s.name && s.swapped.CompareAndSwap(false, true) {
		s.swap()
	}
	return m, err
}

// swapFixture creates repository "victim", and returns a backend whose next
// lookup of it is followed by renaming it to "moved" and creating another
// "victim". It returns the ID of the original repository and a function
// that returns the ID of its replacement once the swap ran.
func swapFixture(t *testing.T) (be *Backend, ctx context.Context, orig proto.Repository, replacement func() int64) {
	t.Helper()
	be, cfg := newTestBackend(t)
	ctx = asAdmin(t, be, backendTestContext(be, cfg))
	admin := defaultAdmin(t, be)
	orig, err := be.CreateRepository(ctx, "victim", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var replacementID int64
	st := &swapAfterLookupStore{Store: be.store, name: "victim"}
	st.swap = func() {
		if err := be.RenameRepository(ctx, "victim", "moved"); err != nil {
			t.Fatal(err)
		}
		r, err := be.CreateRepository(ctx, "victim", admin, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		replacementID = r.ID()
	}
	be.store = st
	return be, ctx, orig, func() int64 { return replacementID }
}

// A delete acts on the repository it looked up: if another repository took
// the name between that lookup and the delete, the delete reports the name
// as not found and leaves both alone.
func TestDeleteRepositoryActsOnTheRepositoryItLookedUp(t *testing.T) {
	t.Parallel()
	be, ctx, orig, replacement := swapFixture(t)

	err := be.DeleteRepository(ctx, "victim")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	r, err := be.Repository(ctx, "victim")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.ID(), replacement(); got != want {
		t.Fatalf("r.ID() = %v, want %v", got, want)
	}
	moved, err := be.Repository(ctx, "moved")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := moved.ID(), orig.ID(); got != want {
		t.Fatalf("moved.ID() = %v, want %v", got, want)
	}
}

// DeleteResolvedRepository deletes the repository a caller resolved, and
// checked access to, before; never another one that took its name since.
func TestDeleteResolvedRepository(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	admin := defaultAdmin(t, be)
	r, err := be.CreateRepository(ctx, "repo", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := be.RenameRepository(ctx, "repo", "moved"); err != nil {
		t.Fatal(err)
	}
	other, err := be.CreateRepository(ctx, "repo", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(be.DeleteResolvedRepository(ctx, r), proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(be.DeleteResolvedRepository(ctx, r), proto.ErrRepoNotFound)")
	}
	cur, err := be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cur.ID(), other.ID(); got != want {
		t.Fatalf("cur.ID() = %v, want %v", got, want)
	}

	if err := be.DeleteResolvedRepository(ctx, other); err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "repo")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
}

// lfsDeleteFixture creates repository "lfsrepo" with a recorded LFS object,
// an object file with no row, and the temporary file of an unfinished
// upload, and returns the repository's LFS directory and those three paths.
func lfsDeleteFixture(t *testing.T, be *Backend) (dir string, files []string) {
	t.Helper()
	ctx := backendTestContext(be, be.cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := be.CreateRepository(ctx, "lfsrepo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	dir = filepath.Join(be.cfg.DataPath, "lfs", strconv.FormatInt(repo.ID(), 10))
	recorded := strings.Repeat("a", 64)
	unrecorded := strings.Repeat("b", 64)
	if err := be.store.CreateLFSObject(ctx, be.db, repo.ID(), recorded, 4); err != nil {
		t.Fatal(err)
	}
	files = []string{
		filepath.Join(dir, "objects", "aa", "aa", recorded),
		filepath.Join(dir, "objects", "bb", "bb", unrecorded),
		filepath.Join(dir, "incomplete", unrecorded+"0123456789abcdef01234567"),
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, files
}

// Deleting a repository removes its whole LFS directory, so objects without
// a row and the temporary files of unfinished uploads do not outlive the
// repository.
func TestDeleteRepositoryRemovesLFSDirectory(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	dir, _ := lfsDeleteFixture(t, be)

	if err := be.DeleteRepository(backendTestContext(be, cfg), "lfsrepo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		var left []string
		filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error { //nolint:errcheck
			if d != nil && !d.IsDir() {
				left = append(left, p)
			}
			return nil
		})
		t.Errorf("LFS directory %s still exists (stat err: %v), files left: %v", dir, err, left)
	}
}

// A delete that fails in the database leaves the repository, so its LFS
// objects must stay too: they are removed after the row's transaction
// commits.
func TestDeleteRepositoryFailureKeepsLFSObjects(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	_, files := lfsDeleteFixture(t, be)
	ctx := backendTestContext(be, cfg)

	refuseTrigger(t, ctx, be.db, "block_repo_delete", "BEFORE", "DELETE", "repos")
	if err := be.DeleteRepository(ctx, "lfsrepo"); err == nil {
		t.Fatal("DeleteRepository succeeded although the row delete was blocked")
	}
	if _, err := be.store.GetRepoByName(ctx, be.db, "lfsrepo"); err != nil {
		t.Fatalf("repository row gone after a failed delete: %v", err)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s removed by a failed delete: %v", f, err)
		}
	}
}

// Once the row is gone the delete has happened, so a directory that cannot be
// removed is logged rather than reported as a failed delete.
func TestDeleteRepositoryLogsLFSRemovalFailure(t *testing.T) {
	t.Parallel()
	// As in os/removeall_test.go: on Windows a mode only sets the read-only
	// attribute, which does not stop removal, and root ignores the mode.
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not prevent removal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can remove the directory regardless of its mode")
	}
	var logs bytes.Buffer
	be, cfg := newTestBackendWithLog(t, &logs)
	dir, _ := lfsDeleteFixture(t, be)

	// A directory without write permission keeps its entries.
	objects := filepath.Join(dir, "objects")
	if err := os.Chmod(objects, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(objects, 0o755) }) //nolint:errcheck

	if err := be.DeleteRepository(backendTestContext(be, cfg), "lfsrepo"); err != nil {
		t.Fatalf("DeleteRepository => %v, want nil once the row is gone", err)
	}
	if _, err := be.store.GetRepoByName(backendTestContext(be, cfg), be.db, "lfsrepo"); err == nil {
		t.Error("repository row still exists")
	}
	if !strings.Contains(logs.String(), "failed to delete lfs objects") {
		t.Errorf("removal failure not logged; logs:\n%s", logs.String())
	}
}

// failingLookupStore counts its GetRepoByName lookups and fails only the
// failAt-th one (counting from 1; 0 fails none).
type failingLookupStore struct {
	store.Store
	failAt int32
	calls  atomic.Int32
}

var errLookup = errors.New("lookup failed")

func (s *failingLookupStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	if s.calls.Add(1) == s.failAt {
		return models.Repo{}, errLookup
	}
	return s.Store.GetRepoByName(ctx, h, name)
}

// GetRepoByNameForUpdate counts and fails as GetRepoByName does.
func (s *failingLookupStore) GetRepoByNameForUpdate(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	if s.calls.Add(1) == s.failAt {
		return models.Repo{}, errLookup
	}
	return s.Store.GetRepoByNameForUpdate(ctx, h, name)
}

// A lookup that fails for any reason other than a missing row says nothing
// about whether the repository exists, so its directory must stay, not be
// removed as an orphan with the row kept. Only the delete's own
// lookup fails: a failure that is gone by the next lookup must still be
// returned, not taken for an orphan and reported as not found.
func TestDeleteRepositoryLookupErrorKeepsDirectory(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	rp := createDeleteFixture(t, be)
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))

	// Repository, which builds the webhook event, looks the row up first;
	// the second lookup is the delete's own.
	st := &failingLookupStore{Store: be.store, failAt: 2}
	be.store = st

	if err := be.DeleteRepository(ctx, "delrepo"); !errors.Is(err, errLookup) {
		t.Fatalf("DeleteRepository => %v, want the lookup error", err)
	}
	if n := st.calls.Load(); n != 2 {
		t.Fatalf("DeleteRepository made %d lookups, want 2", n)
	}

	if _, err := os.Stat(filepath.Join(rp, "HEAD")); err != nil {
		t.Errorf("repository directory removed after a failed lookup: %v", err)
	}
	if _, err := be.Repository(ctx, "delrepo"); err != nil {
		t.Errorf("repository gone after a failed lookup: %v", err)
	}
}

// Deleting a case variant of a repository's name must not remove that
// repository's directory, which a case-insensitive filesystem resolves the
// variant to.
func TestDeleteRepositoryCaseVariantKeepsDirectory(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	rp := createDeleteFixture(t, be)
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))

	if err := be.DeleteRepository(ctx, "DelRepo"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatalf("DeleteRepository of a case variant => %v, want ErrRepoNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(rp, "HEAD")); err != nil {
		t.Errorf("repository directory removed by a case variant delete: %v", err)
	}
	if _, err := be.Repository(ctx, "delrepo"); err != nil {
		t.Errorf("repository gone after a case variant delete: %v", err)
	}
}

// A nested name with a directory and no row is not found either, whether
// spelled as on disk or as a case variant, and its directory stays.
func TestDeleteRepositoryLeavesNestedOrphan(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	rp := be.repoPath("org/orphan")
	if err := os.MkdirAll(rp, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Org/orphan", "org/orphan"} {
		if err := be.DeleteRepository(ctx, name); !errors.Is(err, proto.ErrRepoNotFound) {
			t.Errorf("DeleteRepository(%s) => %v, want ErrRepoNotFound", name, err)
		}
		if _, err := os.Stat(rp); err != nil {
			t.Fatalf("orphan removed through %s: %v", name, err)
		}
	}
}

// The description and the visibility live in the database only. The
// repository's description and git-daemon-export-ok files, which nothing in
// the server reads, are not written: creating a repository or changing
// either setting does no file I/O inside the transaction.
func TestRepositorySettingsWriteNoFiles(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	_, err := be.CreateRepository(ctx, "described", defaultAdmin(t, be), proto.RepositoryOptions{Description: "first words"})
	if err != nil {
		t.Fatal(err)
	}
	dir := be.repoPath("described")
	template, err := os.ReadFile(filepath.Join(dir, "description"))
	if err != nil {
		t.Fatal(err)
	} // git init's own template
	if string(template) == "first words" {
		t.Fatal("not true: string(template) != \"first words\"")
	}
	_, err = os.Stat(filepath.Join(dir, "git-daemon-export-ok"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}

	if err := be.SetDescription(ctx, "described", "second words"); err != nil {
		t.Fatal(err)
	}
	if err := be.SetPrivate(ctx, "described", true); err != nil {
		t.Fatal(err)
	}
	if err := be.SetPrivate(ctx, "described", false); err != nil {
		t.Fatal(err)
	}
	r, err := be.Repository(ctx, "described")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Description(); got != "second words" {
		t.Fatalf("r.Description() = %v, want %v", got, "second words")
	}
	if r.IsPrivate() {
		t.Fatal("not true: !r.IsPrivate()")
	}

	got, err := os.ReadFile(filepath.Join(dir, "description"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(got), string(template); got != want {
		t.Fatalf("string(got) = %v, want %v", got, want)
	}
	_, err = os.Stat(filepath.Join(dir, "git-daemon-export-ok"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// nestedReposFixture returns a backend, a context whose user is an admin, and
// the repos directory.
func nestedReposFixture(t *testing.T) (*Backend, func(string), string) {
	t.Helper()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	create := func(name string) {
		t.Helper()
		if _, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	return be, create, filepath.Join(cfg.DataPath, "repos")
}

func assertGone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s still exists (err %v)", p, err)
	}
}

func assertDir(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil || !fi.IsDir() {
		t.Errorf("%s is not a directory: %v", p, err)
	}
}

// Deleting the last repository of a group removes the group directories it
// leaves empty, but not the repos directory or a group that still has
// repositories.
func TestDeleteRepositoryRemovesEmptyParents(t *testing.T) {
	t.Parallel()
	be, create, root := nestedReposFixture(t)
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))
	create("a/b/c")
	create("a/d")

	if err := be.DeleteRepository(ctx, "a/b/c"); err != nil {
		t.Fatal(err)
	}
	assertGone(t, filepath.Join(root, "a", "b"))
	assertDir(t, filepath.Join(root, "a"))

	if err := be.DeleteRepository(ctx, "a/d"); err != nil {
		t.Fatal(err)
	}
	assertGone(t, filepath.Join(root, "a"))
	assertDir(t, root)

	// A top-level repository has no group to remove.
	create("top")
	if err := be.DeleteRepository(ctx, "top"); err != nil {
		t.Fatal(err)
	}
	assertDir(t, root)
}

// A group directory holding something other than repositories is kept.
func TestDeleteRepositoryKeepsNonEmptyParent(t *testing.T) {
	t.Parallel()
	be, create, root := nestedReposFixture(t)
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))
	create("a/b")
	note := filepath.Join(root, "a", "README")
	if err := os.WriteFile(note, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := be.DeleteRepository(ctx, "a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(note); err != nil {
		t.Errorf("file in group directory removed: %v", err)
	}
}

// A group directory that is a symlink is left alone, even when what it
// points to is empty.
func TestDeleteRepositoryKeepsSymlinkedParent(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
	be, create, root := nestedReposFixture(t)
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))
	target := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	create("linked/b")

	if err := be.DeleteRepository(ctx, "linked/b"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "linked")); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("symlinked group directory removed or replaced: %v", err)
	}
	assertDir(t, target)
}

// Renaming a repository out of a group removes the group directories the old
// name leaves empty, and renaming within a group keeps the group.
func TestRenameRepositoryRemovesEmptyParents(t *testing.T) {
	t.Parallel()
	be, create, root := nestedReposFixture(t)
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))
	create("a/b/c")

	if err := be.RenameRepository(ctx, "a/b/c", "a/b/d"); err != nil {
		t.Fatal(err)
	}
	assertDir(t, filepath.Join(root, "a", "b"))

	if err := be.RenameRepository(ctx, "a/b/d", "e/f"); err != nil {
		t.Fatal(err)
	}
	assertGone(t, filepath.Join(root, "a"))
	if _, err := os.Stat(filepath.Join(be.repoPath("e/f"), "HEAD")); err != nil {
		t.Errorf("renamed repository missing: %v", err)
	}

	// Renaming into a subgroup of the old group: the old group is not
	// empty afterwards, so it stays.
	if err := be.RenameRepository(ctx, "e/f", "e/g/h"); err != nil {
		t.Fatal(err)
	}
	assertDir(t, filepath.Join(root, "e", "g"))
}

// A create that fails after claiming its directory removes the group
// directories it made.
func TestFailedCreateRemovesEmptyParents(t *testing.T) {
	t.Parallel()
	be, create, root := nestedReposFixture(t)
	create("g/h/r")
	// Leave the row but take the directory away, so a second create
	// claims the path and then fails on the duplicate row.
	if err := os.RemoveAll(filepath.Join(root, "g")); err != nil {
		t.Fatal(err)
	}
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))
	if _, err := be.CreateRepository(ctx, "g/h/r", defaultAdmin(t, be), proto.RepositoryOptions{}); !errors.Is(err, proto.ErrRepoExist) {
		t.Fatalf("create error = %v, want ErrRepoExist", err)
	}
	assertGone(t, filepath.Join(root, "g"))
}

// withRepoParent recreates a parent removed between its creation and the
// placement, as a concurrent delete of the group's last repository does.
func TestWithRepoParentRecreatesRemovedParent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	p := filepath.Join(root, "a", "b", "r.git")
	removed := false
	err := withRepoParent(p, func() error {
		if !removed {
			removed = true
			if err := os.RemoveAll(filepath.Join(root, "a")); err != nil {
				t.Fatal(err)
			}
		}
		return os.Mkdir(p, 0o755)
	})
	if err != nil {
		t.Fatalf("withRepoParent: %v", err)
	}
	assertDir(t, p)
}

// A rename whose move fails removes the group directories it made for the new
// name. The move is made to fail by taking write permission away from the old
// name's group directory, which os.Rename needs to unlink the old entry.
func TestFailedRenameRemovesEmptyParents(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block a rename on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	be, create, root := nestedReposFixture(t)
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))
	create("p/a")

	group := filepath.Join(root, "p")
	if err := os.Chmod(group, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(group, 0o755) })

	if err := be.RenameRepository(ctx, "p/a", "x/y/z"); !errors.Is(err, errRepoUpdate) {
		t.Fatalf("rename error = %v, want errRepoUpdate", err)
	}
	assertGone(t, filepath.Join(root, "x"))
	assertDir(t, be.repoPath("p/a"))
	if _, err := be.Repository(ctx, "p/a"); err != nil {
		t.Errorf("old name lost after failed rename: %v", err)
	}
}

// With max_repos_per_user set, a non-admin is refused once they have created
// that many repositories, by create and by import, and keeps the ones they
// have. Deleting one frees a slot.
func TestMaxReposPerUser(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.MaxReposPerUser = 2

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := be.CreateUser(ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(ctx, "a1", alice, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "a2", alice, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(ctx, "a3", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoLimitReached)")
	}
	// Refused before the remote is touched, with the reason rather than
	// the fixed import failure.
	_, err = be.ImportRepository(ctx, "a4", alice, offlineRemote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoLimitReached)")
	}
	for _, name := range []string{"a3", "a4"} {
		_, err = be.Repository(ctx, name)
		if !errors.Is(err, proto.ErrRepoNotFound) {
			t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
		}
		if be.repoPathExists(name) {
			t.Fatal("not true: !be.repoPathExists(name)")
		}
	}

	// The existing repositories are kept.
	for _, name := range []string{"a1", "a2"} {
		_, err = be.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
	}

	// The limit is per user.
	_, err = be.CreateRepository(ctx, "b1", bob, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := be.DeleteRepository(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "a3", alice, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// 0 means no limit.
	cfg.MaxReposPerUser = 0
	_, err = be.CreateRepository(ctx, "a5", alice, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

// At the limit, create refuses before touching the disk: a nested name
// leaves no parent directory behind, and a name that is already taken gets
// the limit as its answer, as import does.
func TestMaxReposPerUserRefusesBeforeCreating(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.MaxReposPerUser = 1

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "a1", alice, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(ctx, "group/a2", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoLimitReached)")
	}
	_, err = os.Stat(filepath.Join(cfg.DataPath, "repos", "group"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("not true: errors.Is(err, fs.ErrNotExist)")
	}

	_, err = be.CreateRepository(ctx, "a1", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoLimitReached)")
	}

	// An invalid name is reported as such even at the limit, as for import.
	_, err = be.CreateRepository(ctx, "bad name!", alice, proto.RepositoryOptions{})
	if err == nil || errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: err != nil && !errors.Is(err, proto.ErrRepoLimitReached)")
	}
	_, err = be.ImportRepository(ctx, "bad name!", alice, "https://example.com/x.git", proto.RepositoryOptions{})
	if err == nil || errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: err != nil && !errors.Is(err, proto.ErrRepoLimitReached)")
	}
}

// Server admins are exempt: admin accounts, and the holder of a key in
// initial_admin_keys whatever account the key is on. So is the server's own
// bootstrap create.
func TestMaxReposPerUserAdminsExempt(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.MaxReposPerUser = 1

	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"x1", "x2", "x3"} {
		_, err = be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}

	pub := newTestPublicKey(t)
	setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(pub)))
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{pub}})
	if err != nil {
		t.Fatal(err)
	}
	keyCtx := context.WithValue(proto.WithUserContext(ctx, alice), charmssh.ContextKeyPublicKey, pub)
	for _, name := range []string{"k1", "k2"} {
		_, err = be.CreateRepository(keyCtx, name, alice, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Without the key on the context, alice is an ordinary user at the limit.
	_, err = be.CreateRepository(ctx, "k3", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoLimitReached)")
	}

	bob, err := be.CreateUser(ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"s1", "s2"} {
		_, err = be.CreateSystemRepository(ctx, name, bob, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Repositories created without an account all count together as one user.
// They are owned by the default admin but are counted apart from the
// admin's own repositories.
func TestMaxReposPerUserAnonymous(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.MaxReposPerUser = 2
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw

	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"x1", "x2", "x3"} {
		_, err = be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = be.CreateRepository(ctx, "anon1", nil, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "anon2", nil, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "anon3", nil, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoLimitReached)")
	}
	_, err = be.ImportRepository(ctx, "anon4", nil, offlineRemote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoLimitReached)")
	}

	r, err := be.Repository(ctx, "anon1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.UserID(), admin.ID(); got != want {
		t.Fatalf("r.UserID() = %v, want %v", got, want)
	}
}

// Concurrent creates by one user cannot together go past the limit. On
// Postgres (SOFT_SERVE_DB_DRIVER=postgres) this relies on LockUserRepos:
// in READ COMMITTED each create would otherwise count without seeing the
// others' uncommitted rows.
func TestMaxReposPerUserConcurrent(t *testing.T) {
	t.Parallel()
	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()
		be, cfg := newTestBackend(t)
		testMaxReposPerUserConcurrent(t, be, cfg, backendTestContext(be, cfg))
	})
	t.Run("postgres", func(t *testing.T) {
		t.Parallel()
		e := newPostgresWebhookEnv(t)
		testMaxReposPerUserConcurrent(t, e.be, e.cfg, e.ctx)
	})
}

func testMaxReposPerUserConcurrent(t *testing.T, be *Backend, cfg *config.Config, ctx context.Context) {
	cfg.MaxReposPerUser = 3
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, user := range []proto.User{alice, nil} {
		const n = 10
		var wg sync.WaitGroup
		errs := make([]error, n)
		prefix := "anon"
		if user != nil {
			prefix = user.Username()
		}
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = be.CreateRepository(ctx, fmt.Sprintf("%s-%d", prefix, i), user, proto.RepositoryOptions{})
			}()
		}
		wg.Wait()

		var ok int
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, proto.ErrRepoLimitReached):
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if got := ok; got != 3 {
			t.Fatalf("ok = %v, want %v", got, 3)
		}
		got, err := be.countRepos(ctx, be.db, user)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := got, int64(3); got != want {
			t.Fatalf("got = %v, want %v", got, want)
		}
	}
}

// TestMissingRepositoryAccessors verifies every per-repository getter and
// setter reports a missing repository as proto.ErrRepoNotFound rather than
// leaking a database or filesystem error, or silently succeeding.
func TestMissingRepositoryAccessors(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	tests := map[string]func() error{
		"Repository": func() error {
			_, err := be.Repository(ctx, "ghost")
			return err
		},
		"SetHidden":      func() error { return be.SetHidden(ctx, "ghost", true) },
		"SetMirror":      func() error { return be.SetMirror(ctx, "ghost", true) },
		"SetDescription": func() error { return be.SetDescription(ctx, "ghost", "desc") },
		"SetPrivate":     func() error { return be.SetPrivate(ctx, "ghost", true) },
		"SetProjectName": func() error { return be.SetProjectName(ctx, "ghost", "name") },
		"Collaborators": func() error {
			_, err := be.Collaborators(ctx, "ghost")
			return err
		},
		"AddCollaborator":    func() error { return be.AddCollaborator(ctx, "ghost", "admin", access.ReadOnlyAccess) },
		"RemoveCollaborator": func() error { return be.RemoveCollaborator(ctx, "ghost", "admin") },
	}

	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := fn()
			if !errors.Is(err, proto.ErrRepoNotFound) {
				t.Fatalf("expected ErrRepoNotFound, got %v", err)
			}
			if strings.Contains(err.Error(), cfg.DataPath) {
				t.Fatalf("error leaks the server data path: %v", err)
			}
		})
	}

	// Nothing should have been created along the way.
	if _, err := os.Stat(be.repoPath("ghost")); err == nil {
		t.Fatal("a missing repository was created by a setter")
	}
}

// TestRepositoryInvalidNameNotLogged verifies a name the filesystem cannot
// hold, which comes straight from a request, is reported as a missing
// repository without an error log line.
func TestRepositoryInvalidNameNotLogged(t *testing.T) {
	var logs bytes.Buffer
	be, _ := newTestBackendWithLog(t, &logs)
	ctx := context.Background()

	for name, repo := range map[string]string{
		"nul":            "a\x00b",
		"long segment":   strings.Repeat("x", 300),
		"long path":      strings.Repeat("a/", 3000),
		"through a file": "repo/sub",
	} {
		t.Run(name, func(t *testing.T) {
			logs.Reset()
			if repo == "repo/sub" {
				// A file where the parent directory of the repository should be.
				dir := filepath.Dir(be.repoPath("repo"))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "repo"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := be.Repository(ctx, repo)
			if !errors.Is(err, proto.ErrRepoNotFound) {
				t.Fatalf("expected ErrRepoNotFound, got %v", err)
			}
			if strings.Contains(logs.String(), "ERRO") {
				t.Fatalf("invalid name logged an error: %s", logs.String())
			}
		})
	}
}

// TestOpenErrorLoggedNotLeaked checks that when git refuses a repository (here
// for "dubious ownership"), clients get the plain ErrNotAGitRepository with no
// server path, and git's reason is logged on the server.
func TestOpenErrorLoggedNotLeaked(t *testing.T) {
	logs := &syncBuffer{}
	be, cfg := newTestBackendWithLog(t, logs)
	ctx := context.Background()

	if _, err := be.CreateRepository(ctx, "existing", defaultAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}

	// Makes git treat repositories as owned by someone else, as it would on
	// a volume owned by another user. System and global config are not read,
	// so a machine-wide safe.directory cannot accept the repositories.
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	// check verifies err, and that the logs written since logStart contain
	// git's reason.
	check := func(t *testing.T, err error, logStart int) {
		t.Helper()
		if !errors.Is(err, git.ErrNotAGitRepository) {
			t.Fatalf("error = %v, want ErrNotAGitRepository", err)
		}
		if err.Error() != git.ErrNotAGitRepository.Error() || strings.Contains(err.Error(), cfg.DataPath) {
			t.Fatalf("client error = %q, want only %q", err, git.ErrNotAGitRepository)
		}
		if newLogs := logs.String()[logStart:]; !strings.Contains(newLogs, "dubious ownership") {
			t.Fatalf("git's reason was not logged; logs:\n%s", newLogs)
		}
	}

	t.Run("Open", func(t *testing.T) {
		r, err := be.Repository(ctx, "existing")
		if err != nil {
			t.Fatal(err)
		}
		logStart := len(logs.String())
		_, err = r.Open(t.Context())
		check(t, err, logStart)
	})

	t.Run("CreateRepository", func(t *testing.T) {
		logStart := len(logs.String())
		_, err := be.CreateRepository(ctx, "new", defaultAdmin(t, be), proto.RepositoryOptions{})
		check(t, err, logStart)
	})
}

// A repository whose owner was deleted meanwhile fails on the owner's
// foreign key, which is not the repository already existing.
func TestCreateRepositoryOwnerGone(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateSystemRepository(ctx, "orphan", user, proto.RepositoryOptions{})
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if errors.Is(err, proto.ErrRepoExist) {
		t.Fatal("not true: !errors.Is(err, proto.ErrRepoExist)")
	}
	if !errors.Is(err, db.ErrForeignKey) {
		t.Fatal("not true: errors.Is(err, db.ErrForeignKey)")
	}
	_, err = be.Repository(ctx, "orphan")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// TestOrphanRepoDirectoryIsNotClaimable verifies that a repository directory
// with no database row is treated as taken rather than missing. Otherwise a
// user who can create repositories could claim it, and on a case-insensitive
// filesystem "claim it" means becoming the owner of somebody else's
// repository.
func TestOrphanRepoDirectoryIsNotClaimable(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	orphan := filepath.Join(cfg.DataPath, "repos", "orphan.git")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := levelOf(t, be, ctx, "orphan", user); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"orphan\", user) = %v, want %v", got, access.NoAccess)
	}
	if got := levelOf(t, be, ctx, "orphan", nil); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"orphan\", nil) = %v, want %v", got, access.NoAccess)
	}

	_, err = be.CreateRepository(ctx, "orphan", user, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoExist) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoExist)")
	}

	// A genuinely new name is still creatable, unless admin-only creation is on.
	if got := levelOf(t, be, ctx, "fresh", user); got != access.ReadWriteAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.ReadWriteAccess)
	}
	cfg.AdminOnlyCreate = true
	if got := levelOf(t, be, ctx, "fresh", user); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.ReadOnlyAccess)
	}
	if got := levelOf(t, be, ctx, "orphan", user); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"orphan\", user) = %v, want %v", got, access.NoAccess)
	}
}

// TestCaseVariantOfExistingRepo verifies that a name differing only by case
// from an existing private repository cannot be used to create or gain
// access to it. On a case-sensitive filesystem the two names are distinct
// repositories and the variant is simply creatable; on a case-insensitive
// one they share a directory and the variant must be refused.
func TestCaseVariantOfExistingRepo(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := be.CreateUser(ctx, "attacker", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(ctx, "secret", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}

	_, statErr := os.Stat(filepath.Join(cfg.DataPath, "repos", "SECRET.git"))
	caseInsensitive := statErr == nil

	level := levelOf(t, be, ctx, "SECRET", attacker)
	_, createErr := be.CreateRepository(ctx, "SECRET", attacker, proto.RepositoryOptions{})

	if caseInsensitive {
		if got := level; got != access.NoAccess {
			t.Fatalf("level = %v, want %v", got, access.NoAccess)
		}
		if !errors.Is(createErr, proto.ErrRepoExist) {
			t.Fatal("not true: errors.Is(createErr, proto.ErrRepoExist)")
		}
	} else {
		if got := level; got != access.ReadWriteAccess {
			t.Fatalf("level = %v, want %v", got, access.ReadWriteAccess)
		}
		if createErr != nil {
			t.Fatal(createErr)
		}
	}

	// Whatever the filesystem did, the original is untouched and still private.
	r, err := be.Repository(ctx, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsPrivate() {
		t.Fatal("not true: r.IsPrivate()")
	}
	if got, want := r.UserID(), owner.ID(); got != want {
		t.Fatalf("r.UserID() = %v, want %v", got, want)
	}
	if got := levelOf(t, be, ctx, "secret", attacker); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"secret\", attacker) = %v, want %v", got, access.NoAccess)
	}

	// With admin-only creation on, no case variant is creatable by the
	// attacker, whatever the filesystem.
	cfg.AdminOnlyCreate = true
	level = levelOf(t, be, ctx, "Secret", attacker)
	_, createErr = be.CreateRepository(ctx, "Secret", attacker, proto.RepositoryOptions{})
	if !errors.Is(createErr, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(createErr, proto.ErrUnauthorized)")
	}
	if caseInsensitive {
		if got := level; got != access.NoAccess {
			t.Fatalf("level = %v, want %v", got, access.NoAccess)
		}
	} else {
		if got := level; got != access.ReadOnlyAccess {
			t.Fatalf("level = %v, want %v", got, access.ReadOnlyAccess)
		}
	}
}

// TestConcurrentCreateSameName verifies that racing creates of one name yield
// exactly one repository; the directory claim is what arbitrates.
func TestConcurrentCreateSameName(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := be.CreateRepository(ctx, "raced", defaultAdmin(t, be), proto.RepositoryOptions{})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	var created, existed int
	for err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, proto.ErrRepoExist):
			existed++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := created; got != 1 {
		t.Fatalf("created = %v, want %v", got, 1)
	}
	if got, want := existed, workers-1; got != want {
		t.Fatalf("existed = %v, want %v", got, want)
	}

	repos, err := be.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(repos); got != 1 {
		t.Fatalf("len(repos) = %v, want %v", got, 1)
	}
	_, err = os.Stat(filepath.Join(cfg.DataPath, "repos", "raced.git", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
}

// TestRegisterRepositoryAdoptsClaimedPath covers the import flow: the
// directory is claimed and filled before the database row exists, and
// registering it must not refuse its own directory.
func TestRegisterRepositoryAdoptsClaimedPath(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "importer", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	rp := filepath.Join(cfg.DataPath, "repos", "imported.git")
	if err := claimRepoPath(rp); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(claimRepoPath(rp), proto.ErrRepoExist) {
		t.Fatal("not true: errors.Is(claimRepoPath(rp), proto.ErrRepoExist)")
	}
	_, err = git.Init(t.Context(), rp, true)
	if err != nil {
		t.Fatal(err)
	}

	r, err := be.registerRepository(ctx, "imported", owner, proto.RepositoryOptions{Private: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.UserID(), owner.ID(); got != want {
		t.Fatalf("r.UserID() = %v, want %v", got, want)
	}
	if !r.IsPrivate() {
		t.Fatal("not true: r.IsPrivate()")
	}
	if got := levelOf(t, be, ctx, "imported", owner); got != access.AdminAccess {
		t.Fatalf("levelOf(t, be, ctx, \"imported\", owner) = %v, want %v", got, access.AdminAccess)
	}

	// The name is now taken on both axes.
	_, err = be.CreateRepository(ctx, "imported", owner, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoExist) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoExist)")
	}
	_, err = be.Repository(ctx, "imported")
	if err != nil {
		t.Fatal(err)
	}
}

// pausingRenameStore runs renamed inside a rename's transaction, right after
// the row is renamed and so locked.
type pausingRenameStore struct {
	store.Store
	renamed func(newName string)
}

func (s *pausingRenameStore) SetRepoNameByName(ctx context.Context, h db.Handler, name string, newName string) error {
	if err := s.Store.SetRepoNameByName(ctx, h, name, newName); err != nil {
		return err
	}
	s.renamed(newName)
	return nil
}

// raceRename starts renaming e's repository "repo" to "moved" and returns
// once the rename holds the row's lock, inside its transaction. Calling the
// returned function lets the rename commit and waits for it.
func raceRename(t *testing.T, e whEnv) (commit func()) {
	t.Helper()
	moved := make(chan struct{})
	release := make(chan struct{})
	e.be.store = &pausingRenameStore{Store: e.be.store, renamed: func(name string) {
		if name == "moved" {
			close(moved)
			<-release
		}
	}}
	done := make(chan error, 1)
	go func() { done <- e.be.RenameRepository(e.ctx, "repo", "moved") }()
	<-moved
	return func() {
		t.Helper()
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("RenameRepository() => %v", err)
		}
	}
}

// waitForLockWaiter waits until a backend of the test database waits for a
// row lock. It fails the test if op returns first, since op was to wait.
func waitForLockWaiter(t *testing.T, e whEnv, op <-chan error, name string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		select {
		case err := <-op:
			t.Fatalf("%s returned without waiting for the rename: %v", name, err)
		default:
		}
		var waiting bool
		if err := e.be.db.GetContext(e.ctx, &waiting, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock');`); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not wait for the rename's row lock", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// On Postgres a delete that waited for a rename of the same repository to
// commit finds no repository under the old name. It must say so, and leave
// the renamed repository's webhooks and LFS objects alone.
func TestDeleteRepositoryRacingRenamePostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	e.createHook(t, whURL, webhook.EventPush)
	object := filepath.Join(e.cfg.DataPath, "lfs", strconv.FormatInt(e.repo.ID(), 10), "objects", "o")
	if err := os.MkdirAll(filepath.Dir(object), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	commit := raceRename(t, e)
	deleted := make(chan error, 1)
	go func() { deleted <- e.be.DeleteRepository(e.ctx, "repo") }()
	waitForLockWaiter(t, e, deleted, "DeleteRepository")
	commit()

	deleteErr := <-deleted
	r, err := e.be.Repository(e.ctx, "moved")
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := e.be.ListWebhooks(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 1 {
		t.Fatalf("len(hooks) = %v, want %v", got, 1)
	} // the renamed repository's webhook is kept
	_, err = os.Stat(object)
	if err != nil {
		t.Fatal(err)
	} // and its LFS object
	if !errors.Is(deleteErr, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(deleteErr, proto.ErrRepoNotFound)")
	} // the delete reported a miss
}

// On Postgres a setter that waited for a rename of the same repository to
// commit finds no repository under the old name and must say so, rather
// than update no row and report success: "repo private" would then leave
// the renamed repository public.
func TestRepoSettersRacingRenamePostgres(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(e whEnv) error{
		"SetPrivate":     func(e whEnv) error { return e.be.SetPrivate(e.ctx, "repo", true) },
		"SetHidden":      func(e whEnv) error { return e.be.SetHidden(e.ctx, "repo", true) },
		"SetDescription": func(e whEnv) error { return e.be.SetDescription(e.ctx, "repo", "d") },
		"SetProjectName": func(e whEnv) error { return e.be.SetProjectName(e.ctx, "repo", "p") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newPostgresWebhookEnv(t)

			commit := raceRename(t, e)
			done := make(chan error, 1)
			go func() { done <- set(e) }()
			waitForLockWaiter(t, e, done, name)
			commit()

			err := <-done
			if !errors.Is(err, proto.ErrRepoNotFound) {
				t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
			}
			r, err := e.be.Repository(e.ctx, "moved")
			if err != nil {
				t.Fatal(err)
			}
			if r.IsPrivate() || r.IsHidden() {
				t.Fatal("not true: !r.IsPrivate() && !r.IsHidden()")
			}
			if got := r.Description(); got != "" {
				t.Fatalf("r.Description() = %v, want %v", got, "")
			}
			if got := r.ProjectName(); got != "" {
				t.Fatalf("r.ProjectName() = %v, want %v", got, "")
			}
		})
	}
}

// renameHookStore runs onRename when a rename is recorded, in the middle of
// RenameRepository, so a test can act as a concurrent request there.
type renameHookStore struct {
	store.Store
	onRename func()
}

func (s renameHookStore) SetRepoNameByName(ctx context.Context, h db.Handler, name string, newName string) error {
	s.onRename()
	return s.Store.SetRepoNameByName(ctx, h, name, newName)
}

// A create or import that claims the new name while a rename is under way
// fails, instead of having its empty directory replaced by the renamed
// repository and its cleanup then deleting that repository.
func TestRenameRepositoryClaimsNewPath(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	_, err := be.CreateRepository(ctx, "a", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var claimErr error
	be.store = renameHookStore{Store: be.store, onRename: func() {
		claimErr = claimRepoPath(be.repoPath("c"))
	}}
	if err := be.RenameRepository(ctx, "a", "c"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(claimErr, proto.ErrRepoExist) {
		t.Fatal("not true: errors.Is(claimErr, proto.ErrRepoExist)")
	} // the concurrent claim failed

	r, err := be.Repository(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

// A rename that fails after claiming the new path gives the claim up and
// leaves the repository where it was.
func TestRenameRepositoryFailureReleasesClaim(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	_, err := be.CreateRepository(ctx, "a", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	failed := errors.New("store failure")
	be.store = failingRenameStore{Store: be.store, err: failed}
	if !errors.Is(be.RenameRepository(ctx, "a", "c"), proto.ErrServerFault) {
		t.Fatal("not true: errors.Is(be.RenameRepository(ctx, \"a\", \"c\"), proto.ErrServerFault)")
	} // the cause is logged, not returned

	_, err = os.Lstat(be.repoPath("c"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("not true: errors.Is(err, os.ErrNotExist)")
	} // the claim was given up
	r, err := be.Repository(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

type failingRenameStore struct {
	store.Store
	err error
}

func (s failingRenameStore) SetRepoNameByName(context.Context, db.Handler, string, string) error {
	return s.err
}

// unpinnableRenameStore renames the row and then records a row that breaks
// a deferred foreign key, so the rename's statements succeed and its commit
// fails, as a lost connection or a full disk would make it.
type unpinnableRenameStore struct {
	store.Store
}

func (s unpinnableRenameStore) SetRepoNameByName(ctx context.Context, h db.Handler, name string, newName string) error {
	if err := s.Store.SetRepoNameByName(ctx, h, name, newName); err != nil {
		return err
	}
	_, err := h.ExecContext(ctx, `INSERT INTO pin (repo_id) VALUES (999999)`)
	return err
}

// A rename whose transaction fails after the directory moved moves it back,
// so the row and the directory agree again.
func TestRenameRepositoryCommitFailureMovesDirectoryBack(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateRepository(ctx, "team/old", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.db.ExecContext(ctx, `CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	be.store = unpinnableRenameStore{Store: be.store}

	if be.RenameRepository(ctx, "team/old", "other/new") == nil {
		t.Fatal("not true: be.RenameRepository(ctx, \"team/old\", \"other/new\") != nil")
	}

	r, err := be.Repository(ctx, "team/old")
	if err != nil {
		t.Fatal(err)
	} // the row and the directory are under the old name
	_, err = os.Stat(filepath.Join(be.repoPath("team/old"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Lstat(be.repoPath("other/new"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
	_, err = os.Lstat(filepath.Join(cfg.DataPath, "repos", "other"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	} // no empty group left behind
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

// A COMMIT whose answer is lost, as when the connection drops during it,
// reports an error although the rename committed. The row then names the
// new name, so the directory stays there and the rename succeeds.
func TestRenameRepositoryCommitOutcomeLostKeepsDirectoryWithRow(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateRepository(ctx, "old", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	be.renameTxDone = func(err error) error {
		if err == nil {
			return errors.New("driver: bad connection")
		}
		return err
	}

	if err := be.RenameRepository(ctx, "old", "new"); err != nil {
		t.Fatal(err)
	}
	r, err := be.Repository(ctx, "new")
	if err != nil {
		t.Fatal(err)
	} // the row and the directory are under the new name
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Lstat(be.repoPath("old"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// failingByIDStore fails every lookup by ID, as a database that went away
// with the connection would.
type failingByIDStore struct {
	store.Store
}

func (failingByIDStore) GetRepoByIDForUpdate(context.Context, db.Handler, int64) (models.Repo, error) {
	return models.Repo{}, errors.New("driver: bad connection")
}

func (failingByIDStore) GetRepoByID(context.Context, db.Handler, int64) (models.Repo, error) {
	return models.Repo{}, errors.New("driver: bad connection")
}

// When the outcome cannot be read either, the directory stays where it is
// rather than risk moving it away from its row.
func TestRenameRepositoryCommitOutcomeUnknownLeavesDirectory(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateRepository(ctx, "old", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	be.store = failingByIDStore{Store: be.store}
	be.renameTxDone = func(err error) error {
		if err == nil {
			return errors.New("driver: bad connection")
		}
		return err
	}

	if be.RenameRepository(ctx, "old", "new") == nil {
		t.Fatal("not true: be.RenameRepository(ctx, \"old\", \"new\") != nil")
	}
	_, err = os.Stat(filepath.Join(be.repoPath("new"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	} // where the committed row puts it
}

// After a failed rename, the directory is moved back only if the row still
// has the old name. A row renamed by another session since has a name this
// rename does not know of, so the directory stays where it is.
func TestRenameRepositoryFailureRenamedAgainLeavesDirectory(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	r, err := be.CreateRepository(ctx, "old", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.db.ExecContext(ctx, `CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	be.store = unpinnableRenameStore{Store: be.store}
	be.renameTxDone = func(err error) error {
		// Another session renames the row once this one's transaction
		// has failed.
		_, xerr := be.db.ExecContext(ctx, be.db.Rebind(`UPDATE repos SET name = 'third' WHERE id = ?`), r.ID())
		if xerr != nil {
			t.Fatal(xerr)
		}
		return err
	}

	if be.RenameRepository(ctx, "old", "new") == nil {
		t.Fatal("not true: be.RenameRepository(ctx, \"old\", \"new\") != nil")
	}
	_, err = os.Stat(filepath.Join(be.repoPath("new"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	} // left where it is
	_, err = os.Lstat(be.repoPath("old"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// On SQLite the re-check after a failed rename does not wait for the
// database's write lock: another writer holding it does not keep the
// directory from going back to its row, as it would once busy_timeout
// passed.
func TestRenameRepositoryRecheckSQLiteIgnoresWriteLock(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	if be.db.DriverName() != "sqlite" {
		t.Skip("SQLite only")
	}
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateRepository(ctx, "old", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.db.ExecContext(ctx, `CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	be.store = unpinnableRenameStore{Store: be.store}

	// Another writer takes the write lock once the rename's transaction
	// has failed, and keeps it until the rename returns.
	var holder *sql.Tx
	be.renameTxDone = func(err error) error {
		var berr error
		holder, berr = beginRawTx(t, ctx, be.db)
		if berr != nil {
			t.Fatal(berr)
		}
		_, berr = holder.ExecContext(ctx, `CREATE TABLE held (n INTEGER)`)
		if berr != nil {
			t.Fatal(berr)
		}
		return err
	}
	renameErr := be.RenameRepository(ctx, "old", "new")
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if renameErr == nil {
		t.Fatal("not true: renameErr != nil")
	}
	_, err = os.Stat(filepath.Join(be.repoPath("old"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	} // moved back to its row
	_, err = os.Lstat(be.repoPath("new"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// A description or project name given at creation reaches every user who
// lists the repository, so it is stripped of escape sequences and control
// characters just as when it is set later.
func TestCreateRepositorySanitisesDescriptionAndProjectName(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	r, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{
		Description: "a\x1bP1|x\x1b\\b\x07\nc",
		ProjectName: "p\x1b]52;c;Zm9v\x07q",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Description(); got != "ab\nc" {
		t.Fatalf("r.Description() = %v, want %v", got, "ab\nc")
	}
	if got := r.ProjectName(); got != "pq" {
		t.Fatalf("r.ProjectName() = %v, want %v", got, "pq")
	}
}

// TestRepositoryReflectsDatabase verifies that access checks see a
// visibility change as soon as it is committed, whoever committed it
// (GHSA-f3fj-9625-rv3m).
func TestRepositoryReflectsDatabase(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := levelOf(t, be, ctx, "repo", nil); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"repo\", nil) = %v, want %v", got, access.ReadOnlyAccess)
	}

	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.SetRepoIsPrivateByName(ctx, tx, "repo", true)
	}); err != nil {
		t.Fatal(err)
	}

	r, err := be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsPrivate() {
		t.Fatal("not true: r.IsPrivate()")
	}
	if got := levelOf(t, be, ctx, "repo", nil); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"repo\", nil) = %v, want %v", got, access.NoAccess)
	}
}

// TestSetPrivateWebhookOnlyOnChange verifies that the visibility change
// webhook fires when visibility changes and not when it is set to the
// value it already has.
func TestSetPrivateWebhookOnlyOnChange(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)

	ctx := context.Background()
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx = proto.WithUserContext(ctx, admin)

	repo, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Deliveries to this address fail fast, but are still recorded.
	var hookID int64
	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		hookID, err = be.store.CreateWebhook(ctx, tx, models.Webhook{RepoID: repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeJSON), Active: true})
		if err != nil {
			return err
		}
		return be.store.CreateWebhookEvents(ctx, tx, hookID, []int{int(webhook.EventRepositoryVisibilityChange)})
	}); err != nil {
		t.Fatal(err)
	}

	deliveries := func() int {
		ds, err := be.store.ListWebhookDeliveriesByWebhookID(ctx, be.db, hookID)
		if err != nil {
			t.Fatal(err)
		}
		return len(ds)
	}

	// Deliveries run in the background. The negative checks first wait until
	// the queue is idle, so a delivery still on its way cannot be missed.
	idle := func() {
		_, finished := be.deliveries.pending()
		<-finished
	}

	if err := be.SetPrivate(ctx, "repo", false); err != nil {
		t.Fatal(err)
	}
	idle()
	if got := deliveries(); got != 0 {
		t.Fatalf("deliveries() = %v, want %v", got, 0)
	}

	if err := be.SetPrivate(ctx, "repo", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first visibility delivery", func() bool { return deliveries() == 1 })

	if err := be.SetPrivate(ctx, "repo", true); err != nil {
		t.Fatal(err)
	}
	idle()
	if got := deliveries(); got != 1 {
		t.Fatalf("deliveries() = %v, want %v", got, 1)
	}

	if err := be.SetPrivate(ctx, "repo", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second visibility delivery", func() bool { return deliveries() == 2 })
}

// swapBeforeLookupStore runs swap once, right before the first lookup of
// name by GetRepoByName, outside any transaction.
type swapBeforeLookupStore struct {
	store.Store
	name    string
	swapped atomic.Bool
	swap    func()
}

func (s *swapBeforeLookupStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	if name == s.name && s.swapped.CompareAndSwap(false, true) {
		s.swap()
	}
	return s.Store.GetRepoByName(ctx, h, name)
}

// swapRepo renames the repository name to moved and creates another under
// name, as another session can between two steps of a change, and returns
// the new one.
func swapRepo(t *testing.T, be *Backend, ctx context.Context, name, moved string) proto.Repository {
	t.Helper()
	if err := be.RenameRepository(ctx, name, moved); err != nil {
		t.Fatal(err)
	}
	r, err := be.CreateRepository(ctx, name, defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// collabNames returns the collaborators of the repository name.
func collabNames(t *testing.T, be *Backend, ctx context.Context, name string) []string {
	t.Helper()
	cs, err := be.Collaborators(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cs {
		names = append(names, c.Username)
	}
	return names
}

// A collaborator added while another repository takes the name between the
// lookup and the write is not added to that other repository: the change
// is refused as the repository is gone.
func TestAddCollaboratorActsOnTheRepositoryItLookedUp(t *testing.T) {
	t.Parallel()
	be, ctx, _, replacement := swapFixture(t)
	_, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	err = be.AddCollaborator(ctx, "victim", "alice", access.ReadWriteAccess)
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	if replacement() == 0 {
		t.Fatal("not true: replacement() != 0")
	}
	if got := len(collabNames(t, be, ctx, "victim")); got != 0 {
		t.Fatalf("len(collabNames(t, be, ctx, \"victim\")) = %v, want %v", got, 0)
	}
}

// The same for a removal: the collaborator of the repository now under the
// name stays.
func TestRemoveCollaboratorActsOnTheRepositoryItLookedUp(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "victim", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.AddCollaborator(ctx, "victim", "alice", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	be.store = &swapAfterLookupStore{Store: be.store, name: "victim", swap: func() {
		swapRepo(t, be, ctx, "victim", "moved")
		if err := be.AddCollaborator(ctx, "victim", "alice", access.ReadOnlyAccess); err != nil {
			t.Fatal(err)
		}
	}}

	err = be.RemoveCollaborator(ctx, "victim", "alice")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	if got, want := collabNames(t, be, ctx, "victim"), ([]string{"alice"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("collabNames(t, be, ctx, \"victim\") = %v, want %v", got, want)
	}
	if got, want := collabNames(t, be, ctx, "moved"), ([]string{"alice"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("collabNames(t, be, ctx, \"moved\") = %v, want %v", got, want)
	}
}

// The resolved setters act on the repository the caller resolved, and
// refuse when another has taken its name since.
func TestResolvedSetters(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	r, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := be.SetResolvedHidden(ctx, r, true); err != nil {
		t.Fatal(err)
	}
	if err := be.SetResolvedDescription(ctx, r, "d"); err != nil {
		t.Fatal(err)
	}
	if err := be.SetResolvedProjectName(ctx, r, "p"); err != nil {
		t.Fatal(err)
	}
	if err := be.SetResolvedPrivate(ctx, r, true); err != nil {
		t.Fatal(err)
	}
	if err := be.AddResolvedCollaborator(ctx, r, "alice", access.ReadOnlyAccess); err != nil {
		t.Fatal(err)
	}
	if err := be.RemoveResolvedCollaborator(ctx, r, "alice"); err != nil {
		t.Fatal(err)
	}
	cur, err := be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !cur.IsHidden() || !cur.IsPrivate() {
		t.Fatal("not true: cur.IsHidden() && cur.IsPrivate()")
	}
	if got := cur.Description(); got != "d" {
		t.Fatalf("cur.Description() = %v, want %v", got, "d")
	}
	if got := cur.ProjectName(); got != "p" {
		t.Fatalf("cur.ProjectName() = %v, want %v", got, "p")
	}

	other := swapRepo(t, be, ctx, "repo", "moved")
	for name, err := range map[string]error{
		"SetResolvedHidden":          be.SetResolvedHidden(ctx, r, false),
		"SetResolvedDescription":     be.SetResolvedDescription(ctx, r, "x"),
		"SetResolvedProjectName":     be.SetResolvedProjectName(ctx, r, "x"),
		"SetResolvedPrivate":         be.SetResolvedPrivate(ctx, r, false),
		"AddResolvedCollaborator":    be.AddResolvedCollaborator(ctx, r, "alice", access.ReadOnlyAccess),
		"RemoveResolvedCollaborator": be.RemoveResolvedCollaborator(ctx, r, "alice"),
	} {
		if !errors.Is(err, proto.ErrRepoNotFound) {
			t.Errorf("%s on a repository whose name another has taken => %v, want ErrRepoNotFound", name, err)
		}
	}
	cur, err = be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cur.ID(), other.ID(); got != want {
		t.Fatalf("cur.ID() = %v, want %v", got, want)
	}
	if cur.IsHidden() || cur.IsPrivate() {
		t.Fatal("not true: !cur.IsHidden() && !cur.IsPrivate()")
	}
	if got := cur.Description(); got != "" {
		t.Fatalf("cur.Description() = %v, want %v", got, "")
	}
	if got := cur.ProjectName(); got != "" {
		t.Fatalf("cur.ProjectName() = %v, want %v", got, "")
	}
	if got := len(collabNames(t, be, ctx, "repo")); got != 0 {
		t.Fatalf("len(collabNames(t, be, ctx, \"repo\")) = %v, want %v", got, 0)
	}
}

// A mirror sync is recorded on the mirror's row, wherever it is now, and a
// mirror deleted meanwhile is not found.
func TestRecordMirrorSyncByID(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	m, err := seedCreate(be, ctx, "m", defaultAdmin(t, be), proto.RepositoryOptions{Mirror: true})
	if err != nil {
		t.Fatal(err)
	}
	swapRepo(t, be, ctx, "m", "moved")

	if err := be.RecordMirrorSync(ctx, m, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	st, err := be.MirrorSyncStatus(ctx, "moved")
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Error; got != "boom" {
		t.Fatalf("st.Error = %v, want %v", got, "boom")
	}
	st, err = be.MirrorSyncStatus(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Error; got != "" {
		t.Fatalf("st.Error = %v, want %v", got, "")
	}

	if err := be.DeleteRepository(ctx, "moved"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(be.RecordMirrorSync(ctx, m, nil), proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(be.RecordMirrorSync(ctx, m, nil), proto.ErrRepoNotFound)")
	}
}

// The events of a visibility change and a rename name the repository the
// change was made to, even if another takes its name before the event is
// built: they go to that repository's webhooks.
func TestChangeEventsNameTheChangedRepository(t *testing.T) {
	t.Parallel()
	t.Run("SetPrivate", func(t *testing.T) {
		t.Parallel()
		e := newWebhookEnv(t)
		id := e.seedHook(t, webhook.EventRepositoryVisibilityChange)
		e.be.store = &swapBeforeLookupStore{Store: e.be.store, name: "repo", swap: func() {
			swapRepo(t, e.be, e.ctx, "repo", "moved")
		}}
		if err := e.be.SetPrivate(e.ctx, "repo", true); err != nil {
			t.Fatal(err)
		}
		e.awaitDeliveries(t, id, webhook.EventRepositoryVisibilityChange)
	})
	t.Run("RenameRepository", func(t *testing.T) {
		t.Parallel()
		e := newWebhookEnv(t)
		id := e.seedHook(t, webhook.EventRepository)
		e.be.store = &swapBeforeLookupStore{Store: e.be.store, name: "renamed", swap: func() {
			swapRepo(t, e.be, e.ctx, "renamed", "moved")
		}}
		if err := e.be.RenameRepository(e.ctx, "repo", "renamed"); err != nil {
			t.Fatal(err)
		}
		// The first delivery is this rename's. Built from a second lookup,
		// it went to the new repository, which has no webhook, and the only
		// delivery was the swap's own rename.
		waitFor(t, "the rename's delivery", func() bool {
			list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
			return err == nil && len(list) > 0
		})
		list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
		if err != nil {
			t.Fatal(err)
		}
		d, err := e.be.WebhookDelivery(e.ctx, e.repo, id, list[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(d.RequestBody, `"changes":{"name":{"from":"repo"}}`) {
			t.Fatal("not true: strings.Contains(d.RequestBody, `\"changes\":{\"name\":{\"from\":\"repo\"}}`)")
		}
	})
}

// Unknown webhook and delivery IDs are reported with their own not-found
// errors, not the driver's "sql: no rows in result set".
func TestWebhookNotFoundSentinels(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	const missing = 424242

	_, err := e.be.Webhook(e.ctx, e.repo, missing)
	if !errors.Is(err, proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrWebhookNotFound)")
	}
	if !errors.Is(e.be.UpdateWebhook(e.ctx, e.repo, missing, WebhookInput{URL: whURL, ContentType: 1, Active: true}), proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(e.be.UpdateWebhook(e.ctx, e.repo, missing, WebhookInput{URL: whURL, ContentType: 1, Active: true}), proto.ErrWebhookNotFound)")
	}
	if !errors.Is(e.be.DeleteWebhook(e.ctx, e.repo, missing), proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(e.be.DeleteWebhook(e.ctx, e.repo, missing), proto.ErrWebhookNotFound)")
	}
	_, err = e.be.ListWebhookDeliveries(e.ctx, e.repo, missing)
	if !errors.Is(err, proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrWebhookNotFound)")
	}
	_, err = e.be.WebhookDelivery(e.ctx, e.repo, missing, uuid.New())
	if !errors.Is(err, proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrWebhookNotFound)")
	}
	if !errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, missing, uuid.New()), proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, missing, uuid.New()), proto.ErrWebhookNotFound)")
	}

	id := e.seedHook(t)
	_, err = e.be.WebhookDelivery(e.ctx, e.repo, id, uuid.New())
	if !errors.Is(err, proto.ErrDeliveryNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrDeliveryNotFound)")
	}
	if !errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, uuid.New()), proto.ErrDeliveryNotFound) {
		t.Fatal("not true: errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, uuid.New()), proto.ErrDeliveryNotFound)")
	}

	// A mistyped ID is the caller's mistake, not a server error.
	if strings.Contains(e.logs.String(), "error getting webhook") {
		t.Fatal("not true: !strings.Contains(e.logs.String(), \"error getting webhook\")")
	}
}

// Renaming onto a name whose row exists without a directory says the name
// is taken, as creating it does, not the driver's constraint error.
func TestRenameOntoRowWithoutDirectory(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	admin := defaultAdmin(t, be)
	for _, name := range []string{"a", "taken"} {
		_, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(be.repoPath("taken")); err != nil {
		t.Fatal(err)
	}

	if !errors.Is(be.RenameRepository(ctx, "a", "taken"), proto.ErrRepoExist) {
		t.Fatal("not true: errors.Is(be.RenameRepository(ctx, \"a\", \"taken\"), proto.ErrRepoExist)")
	}
	r, err := be.Repository(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	} // the directory moved back with the claim given up
}

// failingCreateStore fails every repository insert and repository count
// with err, as a lost database connection would.
type failingCreateStore struct {
	store.Store
	err error
}

func (s failingCreateStore) CreateRepo(context.Context, db.Handler, models.Repo) (models.Repo, error) {
	return models.Repo{}, s.err
}

func (s failingCreateStore) CountUserRepos(context.Context, db.Handler, int64) (int64, error) {
	return 0, s.err
}

// A create, import or rename that fails for a reason other than a refusal
// tells the client only that the server could not do it; the cause is
// logged. The driver's error does not reach the SSH client.
func TestRepositoryChangesHideServerFaults(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	cfg.MaxReposPerUser = 5
	ctx := backendTestContext(be, cfg)
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("pq: connection to 10.0.0.7:5432 reset")
	be.store = failingCreateStore{Store: be.store, err: cause}

	_, err = be.CreateRepository(ctx, "new", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrServerFault) {
		t.Fatal("not true: errors.Is(err, proto.ErrServerFault)")
	}
	_, err = be.ImportRepository(ctx, "imported", alice, "https://1.1.1.1/x.git", proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrServerFault) {
		t.Fatal("not true: errors.Is(err, proto.ErrServerFault)")
	}
	if !strings.Contains(logs.String(), "10.0.0.7") {
		t.Fatal("not true: strings.Contains(logs.String(), \"10.0.0.7\")")
	} // the cause is logged

	// Refusals are returned as they are, with their own text.
	_, err = be.CreateRepository(ctx, "-bad", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrInvalidRepo) {
		t.Fatal("not true: errors.Is(err, proto.ErrInvalidRepo)")
	}
	if got := err.Error(); got != "repo path segments cannot start with a hyphen" {
		t.Fatalf("err.Error() = %v, want %v", got, "repo path segments cannot start with a hyphen")
	}
	if !errors.Is(be.RenameRepository(asAdmin(t, be, ctx), "missing", "other"), proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(be.RenameRepository(asAdmin(t, be, ctx), \"missing\", \"other\"), proto.ErrRepoNotFound)")
	}
}

// cancellingCreateStore fails the repository insert with err after ending
// the caller's context, as a statement the client's disconnect cancelled
// does.
type cancellingCreateStore struct {
	store.Store
	cancel context.CancelFunc
	err    error
}

func (s cancellingCreateStore) CreateRepo(context.Context, db.Handler, models.Repo) (models.Repo, error) {
	s.cancel()
	return models.Repo{}, s.err
}

// A fault that came with the caller's context ending is the client going
// away, not a server fault: it is logged at Debug, not Error.
func TestRepositoryChangesCancelledNotLoggedAsFault(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx, cancel := context.WithCancel(backendTestContext(be, cfg))
	defer cancel()
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	be.store = cancellingCreateStore{Store: be.store, cancel: cancel, err: errors.New("pq: canceling statement due to user request")}

	_, err = be.CreateRepository(ctx, "new", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrServerFault) {
		t.Fatal("not true: errors.Is(err, proto.ErrServerFault)")
	}
	if strings.Contains(logs.String(), "failed to create repository") {
		t.Fatal("not true: !strings.Contains(logs.String(), \"failed to create repository\")")
	}
}

// The cause of a failed create is logged once.
func TestCreateRepositoryFaultLoggedOnce(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	be.logger.SetLevel(log.DebugLevel)
	ctx := backendTestContext(be, cfg)
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	be.store = failingCreateStore{Store: be.store, err: errors.New("pq: connection to 10.0.0.7:5432 reset")}

	_, err = be.CreateRepository(ctx, "new", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrServerFault) {
		t.Fatal("not true: errors.Is(err, proto.ErrServerFault)")
	}
	if got := strings.Count(logs.String(), "10.0.0.7"); got != 1 {
		t.Fatalf("strings.Count(logs.String(), \"10.0.0.7\") = %v, want %v", got, 1)
	}
}

// txCheckStore fails the test when one of the reads below runs inside a
// transaction.
type txCheckStore struct {
	store.Store
	t *testing.T
}

func (s txCheckStore) check(name string, h db.Handler) {
	s.t.Helper()
	if _, ok := h.(*db.Tx); ok {
		s.t.Errorf("%s ran in a transaction", name)
	}
}

func (s txCheckStore) GetAnonAccess(ctx context.Context, h db.Handler) (access.Level, error) {
	s.check("GetAnonAccess", h)
	return s.Store.GetAnonAccess(ctx, h)
}

func (s txCheckStore) GetAllowKeylessAccess(ctx context.Context, h db.Handler) (bool, error) {
	s.check("GetAllowKeylessAccess", h)
	return s.Store.GetAllowKeylessAccess(ctx, h)
}

func (s txCheckStore) GetCollabByUserIDAndRepoID(ctx context.Context, h db.Handler, userID, repoID int64) (models.Collab, error) {
	s.check("GetCollabByUserIDAndRepoID", h)
	return s.Store.GetCollabByUserIDAndRepoID(ctx, h, userID, repoID)
}

func (s txCheckStore) ListCollabsByUserID(ctx context.Context, h db.Handler, userID int64) ([]models.Collab, error) {
	s.check("ListCollabsByUserID", h)
	return s.Store.ListCollabsByUserID(ctx, h, userID)
}

func (s txCheckStore) GetAllUsers(ctx context.Context, h db.Handler) ([]models.User, error) {
	s.check("GetAllUsers", h)
	return s.Store.GetAllUsers(ctx, h)
}

func (s txCheckStore) GetAllRepos(ctx context.Context, h db.Handler) ([]models.Repo, error) {
	s.check("GetAllRepos", h)
	return s.Store.GetAllRepos(ctx, h)
}

func (s txCheckStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	s.check("GetRepoByName", h)
	return s.Store.GetRepoByName(ctx, h, name)
}

// A read of one statement runs on the pool, not in a transaction: a read
// transaction takes three round trips to Postgres instead of one, and
// AnonAccess, AllowKeyless, Repository and IsCollaborator are on the access
// check of every request.
func TestSingleReadsRunWithoutTransaction(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	r, err := be.CreateRepository(ctx, "r", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	be.store = txCheckStore{Store: be.store, t: t}

	be.AnonAccess(ctx)
	be.AllowKeyless(ctx)
	if _, _, err := be.IsCollaborator(ctx, r, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Users(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Repositories(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Repository(ctx, "r"); err != nil {
		t.Fatal(err)
	}
}

func TestRenameRepositorySendsRepositoryEvent(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)

	if err := e.be.RenameRepository(e.ctx, "repo", "renamed"); err != nil {
		t.Fatal(err)
	}

	e.awaitDeliveries(t, id, webhook.EventRepository)

	// The event names the repository by its new name and says what it was
	// called before.
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.be.WebhookDelivery(e.ctx, e.repo, id, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.RequestBody, `"name":"renamed"`) {
		t.Fatal("not true: strings.Contains(d.RequestBody, `\"name\":\"renamed\"`)")
	}
	if !strings.Contains(d.RequestBody, `"changes":{"name":{"from":"repo"}}`) {
		t.Fatal("not true: strings.Contains(d.RequestBody, `\"changes\":{\"name\":{\"from\":\"repo\"}}`)")
	}
}

// A failed event does not undo a committed rename, and the caller sees success.
func TestRenameRepositoryKeepsChangeWhenEventFails(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventRepository)
	_, err := e.be.db.ExecContext(e.ctx, "DROP TABLE webhook_deliveries")
	if err != nil {
		t.Fatal(err)
	}

	if err := e.be.RenameRepository(e.ctx, "repo", "renamed"); err != nil {
		t.Fatal(err)
	}
	_, err = e.be.Repository(e.ctx, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.be.Repository(e.ctx, "repo")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	e.awaitLog(t, "webhook event not delivered")
}

// A failed event does not undo a committed visibility change.
func TestSetPrivateKeepsChangeWhenEventFails(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventRepositoryVisibilityChange)
	_, err := e.be.db.ExecContext(e.ctx, "DROP TABLE webhook_deliveries")
	if err != nil {
		t.Fatal(err)
	}

	if err := e.be.SetPrivate(e.ctx, "repo", true); err != nil {
		t.Fatal(err)
	}
	repo, err := e.be.Repository(e.ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !repo.IsPrivate() {
		t.Fatal("not true: repo.IsPrivate()")
	}
	e.awaitLog(t, "webhook event not delivered")
}

// The repository-deleted event goes to the subscribers that were read before
// the rows were removed. The seeded receiver is a loopback address that the
// SSRF-protected client refuses, so the attempt shows up as a logged failure.
func TestDeleteRepositorySendsRepositoryEvent(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventRepository)

	if err := e.be.DeleteRepository(e.ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	_, err := e.be.Repository(e.ctx, "repo")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}

	e.awaitLog(t, "webhook event not delivered")
}

// Deleting a repository removes its webhooks with their events and
// deliveries. No delivery row is written for the repository event, since its
// webhook is gone; the attempt is sent unrecorded, so its refusal is logged
// instead.
func TestDeleteRepositoryRemovesWebhookRows(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventRepository)
	e.afterPush(t, e.admin, []hooks.HookArg{{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	}})
	if got, want := e.deliveredEvents(t, id), ([]webhook.Event{webhook.EventBranchTagCreate}); !reflect.DeepEqual(got, want) {
		t.Fatalf("e.deliveredEvents(t, id) = %v, want %v", got, want)
	}

	if err := e.be.DeleteRepository(e.ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	e.be.WaitForDeliveries(e.ctx)
	if !strings.Contains(e.logs.String(), "webhook event not delivered") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"webhook event not delivered\")")
	}

	for _, q := range []string{
		"SELECT COUNT(*) FROM webhooks WHERE id = ?",
		"SELECT COUNT(*) FROM webhook_events WHERE webhook_id = ?",
		"SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = ?",
	} {
		var n int
		if err := e.be.db.GetContext(e.ctx, &n, e.be.db.Rebind(q), id); err != nil {
			t.Fatal(err)
		}
		if got := n; got != 0 {
			t.Fatalf("n = %v, want %v", got, 0)
		}
	}
}

// Building an event looks up the repository owner. Foreign keys stop the owner
// row being removed normally, so corruptOwner switches them off for the delete.
// A rename whose event cannot be built is still committed.
func TestRenameRepositoryKeepsChangeWhenEventBuildFails(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	corruptOwner(t, e)

	if err := e.be.RenameRepository(e.ctx, "repo", "renamed"); err != nil {
		t.Fatal(err)
	}
	_, err := e.be.Repository(e.ctx, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.logs.String(), "rename committed but webhook event skipped") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"rename committed but webhook event skipped\")")
	}
}

// A delete whose event cannot be built still removes the repository; the
// skipped event is logged.
func TestDeleteRepositoryKeepsGoingWhenEventBuildFails(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	corruptOwner(t, e)

	if err := e.be.DeleteRepository(e.ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	_, err := e.be.Repository(e.ctx, "repo")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	_, err = os.Stat(e.be.repoPath("repo"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("not true: errors.Is(err, os.ErrNotExist)")
	}
	if !strings.Contains(e.logs.String(), "deleting repository without webhook event") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"deleting repository without webhook event\")")
	}
}

// Changing visibility builds the event first; a failed owner lookup returns
// the error.
func TestSetPrivateReturnsEventBuildFailure(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	_, err := e.be.db.ExecContext(e.ctx, "DELETE FROM users WHERE username = 'admin'")
	if err != nil {
		t.Fatal(err)
	}

	if e.be.SetPrivate(e.ctx, "repo", true) == nil {
		t.Fatal("not true: e.be.SetPrivate(e.ctx, \"repo\", true) != nil")
	}
}

// A visibility change whose event cannot be built still commits and returns nil.
func TestSetPrivateKeepsChangeWhenEventBuildFails(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	corruptOwner(t, e)

	if err := e.be.SetPrivate(e.ctx, "repo", true); err != nil {
		t.Fatal(err)
	}
	repo, err := e.be.Repository(e.ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !repo.IsPrivate() {
		t.Fatal("not true: repo.IsPrivate()")
	}
	if !strings.Contains(e.logs.String(), "visibility change committed but webhook event skipped") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"visibility change committed but webhook event skipped\")")
	}
}

// With AdminOnlyCreate on, a non-admin's create and rename to a new name are
// refused before any change. The rename half is the meaningful one: the
// non-admin owns "mine", which has a repository hook, and a policy check
// that ran after the commit would leave "renamed" behind and deliver an
// event to that hook. (CreateRepository sends no event either way.)
// TestRenameRepositorySendsRepositoryEvent shows the same hook does receive
// the event when a rename really happens.
func TestRefusedCreateAndRenameSendNoRepositoryEvent(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	e.cfg.AdminOnlyCreate = true

	alice, err := e.be.CreateUser(e.ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	aliceCtx := proto.WithUserContext(e.ctx, alice)
	mine, err := seedCreate(e.be, e.ctx, "mine", alice, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mineHook := e.seedHookOn(t, mine, webhook.EventRepository)

	before := e.logs.String()

	if !errors.Is(func() error {
		_, err := e.be.CreateRepository(aliceCtx, "junk", alice, proto.RepositoryOptions{})
		return err
	}(), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(CreateRepository(aliceCtx, \"junk\", alice, …), proto.ErrUnauthorized)")
	}
	if !errors.Is(e.be.RenameRepository(aliceCtx, "mine", "renamed"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(e.be.RenameRepository(aliceCtx, \"mine\", \"renamed\"), proto.ErrUnauthorized)")
	}
	e.be.WaitForDeliveries(e.ctx)

	// Nothing was created or renamed.
	_, err = e.be.Repository(e.ctx, "junk")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = e.be.Repository(e.ctx, "mine")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.be.Repository(e.ctx, "renamed")
	if err == nil {
		t.Fatal("not true: err != nil")
	}

	// No delivery was recorded on the hook, and no webhook event was logged.
	mineDeliveries, err := e.be.ListWebhookDeliveries(e.ctx, mine, mineHook)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(mineDeliveries); got != 0 {
		t.Fatalf("len(mineDeliveries) = %v, want %v", got, 0)
	}
	if strings.Contains(e.logs.String()[len(before):], "webhook event") {
		t.Fatal("not true: !strings.Contains(e.logs.String()[len(before):], \"webhook event\")")
	}
}

// A failed owner lookup keeps the about repository's readme hidden and is
// logged.
func TestAboutRepoOwnerLookupFailureLogged(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be := newLookupBackend(t, &logs)
	ctx := context.Background()
	r, err := be.CreateRepository(ctx, AboutRepo, defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !be.AboutRepoOwnedByAdmin(ctx, r) {
		t.Fatal("not true: be.AboutRepoOwnedByAdmin(ctx, r)")
	}

	breakTable(t, be, "users")
	if be.AboutRepoOwnedByAdmin(ctx, r) {
		t.Fatal("not true: !be.AboutRepoOwnedByAdmin(ctx, r)")
	}
	if !strings.Contains(logs.String(), "about repository's owner") {
		t.Fatal("not true: strings.Contains(logs.String(), \"about repository's owner\")")
	}
}
