package cmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
)

// newLookupTestContext is newAuthTestContext on the test database dbtest
// opens (Postgres when SOFT_SERVE_DB_DRIVER is "postgres", SQLite
// otherwise), with the log written to the returned buffer at Debug level.
// The lookup tests break its tables.
func newLookupTestContext(t *testing.T) (context.Context, *backend.Backend, *syncLog) {
	t.Helper()
	return newLookupTestContextWith(t, nil)
}

// newLookupTestContextWith is newLookupTestContext with the backend's store
// wrapped by wrap, when it is not nil.
func newLookupTestContextWith(t *testing.T, wrap func(store.Store) store.Store) (context.Context, *backend.Backend, *syncLog) {
	t.Helper()
	logs := &syncLog{}
	logger := log.New(logs)
	logger.SetLevel(log.DebugLevel)
	ctx, be := newTestCommands(t, nil, wrap, logger)
	return ctx, be, logs
}

// syncLog is a log destination that is safe to read while git's goroutines
// may still write to it.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// reset empties the log.
func (l *syncLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

// count returns how many lines of the log hold level.
func (l *syncLog) count(level string) int {
	n := 0
	for line := range strings.Lines(l.String()) {
		if strings.Contains(line, level) {
			n++
		}
	}
	return n
}

// breakTable renames table so every query on it fails, as a database
// fault would.
func breakTable(t *testing.T, ctx context.Context, table string) {
	t.Helper()
	dbx := cmdsOf(ctx).db
	if _, err := dbx.ExecContext(ctx, "ALTER TABLE "+table+" RENAME TO "+table+"_gone"); err != nil {
		t.Fatal(err)
	}
}

// `repo list` lists the repositories of the session's user, as the session
// resolved it. A lookup of the session's key that fails does not turn the
// list into what an anonymous caller sees.
func TestRepoListUsesSessionUser(t *testing.T) {
	t.Parallel()
	ctx, be, _ := newLookupTestContext(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pk := signer.PublicKey()
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "mine", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	sessCtx := context.WithValue(proto.WithUserContext(ctx, owner), ssh.ContextKeyPublicKey, pk)

	// Key lookups now fail; the session's user was resolved before.
	breakTable(t, ctx, "public_keys")

	out, _, err := runRepoOutput(t, sessCtx, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mine") {
		t.Fatal("not true: strings.Contains(out, \"mine\")")
	}
}

// A repository lookup that fails is answered as a server fault, not as a
// missing repository; the cause goes to the server log, not the client.
func TestRepoCommandLookupFailure(t *testing.T) {
	t.Parallel()
	ctx, be, logs := newLookupTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	strangerCtx := withUser(t, ctx, be, "stranger", false)

	if !errors.Is(runRepo(t, strangerCtx, "info", "repo"), proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(runRepo(t, strangerCtx, \"info\", \"repo\"), proto.ErrRepoNotFound)")
	}

	// Without the collaborators table, the stranger's level cannot be
	// worked out.
	breakTable(t, ctx, "collabs")
	err = runRepo(t, strangerCtx, "info", "repo")
	if !errors.Is(err, errLookupFailed) {
		t.Fatal("not true: errors.Is(err, errLookupFailed)")
	}
	if strings.Contains(err.Error(), "collabs") {
		t.Fatal("not true: !strings.Contains(err.Error(), \"collabs\")")
	}
	if !strings.Contains(logs.String(), "lookup failed") {
		t.Fatal("not true: strings.Contains(logs.String(), \"lookup failed\")")
	}
	if !strings.Contains(logs.String(), "collabs") {
		t.Fatal("not true: strings.Contains(logs.String(), \"collabs\")")
	}

	// With the repository table gone, the lookup fails for everyone, and
	// so does the list.
	breakTable(t, ctx, "repos")
	if !errors.Is(runRepo(t, ownerCtx, "info", "repo"), errLookupFailed) {
		t.Fatal("not true: errors.Is(runRepo(t, ownerCtx, \"info\", \"repo\"), errLookupFailed)")
	}
	if !errors.Is(runRepo(t, ownerCtx, "list"), errLookupFailed) {
		t.Fatal("not true: errors.Is(runRepo(t, ownerCtx, \"list\"), errLookupFailed)")
	}
}

// During an outage a missing name and an existing private one get the same
// answer, for anonymous and registered callers: otherwise anyone could tell
// which private names exist.
func TestRepoCommandOutageSameAnswer(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"repos", "settings"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			ctx, be, _ := newLookupTestContext(t)
			ownerCtx := withUser(t, ctx, be, "owner", false)
			_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Private: true})
			if err != nil {
				t.Fatal(err)
			}
			strangerCtx := withUser(t, ctx, be, "stranger", false)

			breakTable(t, ctx, table)
			for _, c := range []context.Context{ctx, strangerCtx} {
				for _, name := range []string{"repo", "ghost"} {
					if !errors.Is(runRepo(t, c, "info", name), errLookupFailed) {
						t.Fatal("not true: errors.Is(runRepo(t, c, \"info\", name), errLookupFailed)")
					}
				}
			}
		})
	}
}

// runGit runs the git service command c for args as ctx's caller, with no
// input, and returns its error.
func runGit(ctx context.Context, c func(*Commands) *cobra.Command, args ...string) error {
	cmd := c(cmdsOf(ctx))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}

// A failed lookup in a git service over SSH, clone, push or
// git-lfs-authenticate, is answered with the fixed malfunction message and
// logged once at Error level.
func TestGitServiceLookupFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cmd  func(*Commands) *cobra.Command
		args []string
	}{
		{"upload-pack", (*Commands).GitUploadPackCommand, []string{"repo"}},
		{"receive-pack", (*Commands).GitReceivePackCommand, []string{"repo"}},
		{"lfs-authenticate", (*Commands).GitLFSAuthenticateCommand, []string{"repo", "download"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, be, logs := newLookupTestContext(t)
			ownerCtx := withUser(t, ctx, be, "owner", false)
			_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Private: true})
			if err != nil {
				t.Fatal(err)
			}
			strangerCtx := withUser(t, ctx, be, "stranger", false)

			callerCtx := strangerCtx
			if tc.name == "lfs-authenticate" {
				// git-lfs-authenticate looks the repository up twice: as
				// the session, then for the account alone. An admin key
				// on a non-admin account passes the first without a
				// collaborator lookup, so the second is the one that fails.
				pk, ak := slashFreeKey(t)
				cfg := cmdsOf(ctx).cfg
				cfg.InitialAdminKeys = []string{ak}
				if err := cfg.Validate(); err != nil {
					t.Fatal(err)
				}
				callerCtx = context.WithValue(strangerCtx, ssh.ContextKeyPublicKey, pk)
			}

			breakTable(t, ctx, "collabs")
			err = runGit(callerCtx, tc.cmd, tc.args...)
			if !errors.Is(err, gitserve.ErrSystemMalfunction) {
				t.Fatal("not true: errors.Is(err, gitserve.ErrSystemMalfunction)")
			}
			if got := logs.count("ERRO"); got != 1 {
				t.Fatalf("logs.count(\"ERRO\") = %v, want %v", got, 1)
			}
			if !strings.Contains(logs.String(), "failed to look up repository") {
				t.Fatal("not true: strings.Contains(logs.String(), \"failed to look up repository\")")
			}
		})
	}
}

// During an outage a git service gives a missing name and an existing
// private one the same answer.
func TestGitServiceOutageSameAnswer(t *testing.T) {
	t.Parallel()
	ctx, be, _ := newLookupTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	strangerCtx := withUser(t, ctx, be, "stranger", false)

	breakTable(t, ctx, "repos")
	for _, c := range []context.Context{ctx, strangerCtx} {
		for _, name := range []string{"repo", "ghost"} {
			if !errors.Is(runGit(c, (*Commands).GitUploadPackCommand, name), gitserve.ErrSystemMalfunction) {
				t.Fatal("not true: errors.Is(runGit(c, (*Commands).GitUploadPackCommand, name), gitserve.ErrSystemMalfunction)")
			}
		}
	}
}

// A lookup the client canceled by going away is logged at Debug, not as a
// server error, and still answered as a failed lookup.
func TestLookupFailedCanceledLoggedAtDebug(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if !errors.Is(lookupFailed(ctx, logger, "look up repository", context.Canceled), errLookupFailed) {
		t.Fatal("not true: errors.Is(lookupFailed(ctx, logger, \"look up repository\", context.Canceled), errLookupFailed)")
	}
	if !errors.Is(gitLookupFailed(ctx, logger, "repo", context.Canceled), gitserve.ErrSystemMalfunction) {
		t.Fatal("not true: errors.Is(gitLookupFailed(ctx, logger, \"repo\", context.Canceled), gitserve.ErrSystemMalfunction)")
	}
	if !strings.Contains(buf.String(), "DEBU") {
		t.Fatal("not true: strings.Contains(buf.String(), \"DEBU\")")
	}
	if strings.Contains(buf.String(), "ERRO") {
		t.Fatal("not true: !strings.Contains(buf.String(), \"ERRO\")")
	}

	buf.Reset()
	if !errors.Is(lookupFailed(context.Background(), logger, "look up repository", errors.New("db down")), errLookupFailed) {
		t.Fatal("not true: errors.Is(lookupFailed(context.Background(), logger, \"look up repository\", errors.New(\"db down\")), errLookupFailed)")
	}
	if !errors.Is(gitLookupFailed(context.Background(), logger, "repo", errors.New("db down")), gitserve.ErrSystemMalfunction) {
		t.Fatal("not true: errors.Is(gitLookupFailed(context.Background(), logger, \"repo\", errors.New(\"db down\")), gitserve.ErrSystemMalfunction)")
	}
	if got := strings.Count(buf.String(), "ERRO"); got != 2 {
		t.Fatalf("strings.Count(buf.String(), \"ERRO\") = %v, want %v", got, 2)
	}
}

// While the settings cannot be read, the admin's settings getters say the
// lookup failed rather than print the fail-closed default as if it were the
// stored value.
func TestSettingsGettersLookupFailure(t *testing.T) {
	t.Parallel()
	ctx, be, _ := newLookupTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	breakTable(t, ctx, "settings")
	for _, setting := range []string{"allow-keyless", "anon-access"} {
		c := cmdsOf(adminCtx).SettingsCommand()
		c.SilenceUsage = true
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(io.Discard)
		c.SetArgs([]string{setting})
		if !errors.Is(c.ExecuteContext(adminCtx), errLookupFailed) {
			t.Fatal("not true: errors.Is(c.ExecuteContext(adminCtx), errLookupFailed)")
		}
		if got := out.String(); got != "" {
			t.Fatalf("out.String() = %v, want %v", got, "")
		}
	}
}

// slashFreeKey returns a public key whose authorized-key text has no "/",
// so the text can also be a file name.
func slashFreeKey(t *testing.T) (gossh.PublicKey, string) {
	t.Helper()
	for {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := gossh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		ak := strings.TrimSpace(proto.MarshalAuthorizedKey(signer.PublicKey()))
		if !strings.Contains(ak, "/") {
			return signer.PublicKey(), ak
		}
	}
}

// The admin keys are parsed once, when the config is validated, instead of
// parsing initial_admin_keys again, trying each entry as a file first, on
// every check. A file that appears later in the working directory, named
// like a listed key, does not replace that key.
func TestAdminKeysParsedOnce(t *testing.T) {
	pub, ak := slashFreeKey(t)
	ctx, be := newAuthTestContextWith(t, func(c *config.Config) {
		c.InitialAdminKeys = []string{ak}
	})

	t.Chdir(t.TempDir())
	if !cmdsOf(ctx).isServerAdmin(context.WithValue(ctx, ssh.ContextKeyPublicKey, pub)) {
		t.Fatal("not true: cmdsOf(ctx).isServerAdmin(context.WithValue(ctx, ssh.ContextKeyPublicKey, pub))")
	}

	other, otherAK := slashFreeKey(t)
	if err := os.WriteFile(ak, []byte(otherAK), 0o600); err != nil {
		t.Fatal(err)
	}

	if !be.IsAdminKey(pub) {
		t.Fatal("not true: be.IsAdminKey(pub)")
	} // still the listed key
	if be.IsAdminKey(other) {
		t.Fatal("not true: !be.IsAdminKey(other)")
	} // the file's key is not an admin
	if cmdsOf(ctx).isServerAdmin(context.WithValue(ctx, ssh.ContextKeyPublicKey, other)) {
		t.Fatal("not true: !cmdsOf(ctx).isServerAdmin(context.WithValue(ctx, ssh.ContextKeyPublicKey, other))")
	}
}

// TestGlobalCommandsIgnoreRepositoryAccess checks that a non-admin cannot run
// global admin commands by creating a repository named after the command's
// first argument: `user set-admin victim true` is not authorized by the
// caller's admin access to a repository they own called "victim".
func TestGlobalCommandsIgnoreRepositoryAccess(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	attackerCtx := withUser(t, ctx, be, "attacker", false)

	// The attacker owns a repository named "victim", so they have
	// AdminAccess *to that repository*.
	attacker := proto.UserFromContext(attackerCtx)
	_, err := be.CreateRepository(attackerCtx, "victim", attacker, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := levelOf(t, be, attackerCtx, "victim", attacker); got != access.AdminAccess {
		t.Fatalf("levelOf(t, be, attackerCtx, \"victim\", attacker) = %v, want %v", got, access.AdminAccess)
	}

	// Repository admin access must not authorize global user commands.
	for _, args := range [][]string{
		{"set-admin", "victim", "true"},
		{"create", "victim2"},
		{"delete", "victim"},
		{"list"},
		{"info", "victim"},
		{"set-username", "victim", "victim3"},
	} {
		if err := runUser(t, attackerCtx, args...); !errors.Is(err, proto.ErrUnauthorized) {
			t.Errorf("user %v: expected ErrUnauthorized, got %v", args, err)
		}
	}

	// The attacker must not have become an admin.
	reloaded, err := be.User(ctx, "attacker")
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.IsAdmin(); got != false {
		t.Fatalf("reloaded.IsAdmin() = %v, want %v", got, false)
	}
}

// TestGlobalCommandsIgnoreAnonAdminAccess covers the variant that needs no
// repository at all: with anon-access set to admin-access,
// Readable gives AdminAccess to any authenticated user for a
// nonexistent repository name. That only holds with admin_only_create off
// (the default), so the flag is forced off here; with it on the missing name
// is capped at read-only and set-admin must still be refused.
func TestGlobalCommandsIgnoreAnonAdminAccess(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		adminOnly bool
		missing   access.Level
	}{
		{adminOnly: false, missing: access.AdminAccess},
		{adminOnly: true, missing: access.ReadOnlyAccess},
	} {
		t.Run(fmt.Sprintf("admin_only_create=%t", tt.adminOnly), func(t *testing.T) {
			t.Parallel()
			ctx, be := newAuthTestContextWith(t, func(c *config.Config) { c.AdminOnlyCreate = tt.adminOnly })
			if err := be.SetAnonAccess(ctx, access.AdminAccess); err != nil {
				t.Fatal(err)
			}

			attackerCtx := withUser(t, ctx, be, "attacker", false)
			attacker := proto.UserFromContext(attackerCtx)
			if got := levelOf(t, be, attackerCtx, "nonexistent", attacker); got != tt.missing {
				t.Fatalf("levelOf(t, be, attackerCtx, \"nonexistent\", attacker) = %v, want %v", got, tt.missing)
			}

			err := runUser(t, attackerCtx, "set-admin", "attacker", "true")
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Fatalf("expected ErrUnauthorized, got %v", err)
			}

			reloaded, err := be.User(ctx, "attacker")
			if err != nil {
				t.Fatal(err)
			}
			if got := reloaded.IsAdmin(); got != false {
				t.Fatalf("reloaded.IsAdmin() = %v, want %v", got, false)
			}
		})
	}
}

// TestUserSubcommandsAreGatedByParent verifies the gate lives on the `user`
// parent command, so subcommands cannot be left unprotected by omission.
func TestUserSubcommandsAreGatedByParent(t *testing.T) {
	t.Parallel()
	ctx, _ := newAuthTestContext(t)
	c := cmdsOf(ctx).UserCommand()
	if c.PersistentPreRunE == nil {
		t.Fatal("not true: c.PersistentPreRunE != nil")
	}

	for _, sub := range c.Commands() {
		if sub.PersistentPreRunE != nil {
			t.Errorf("subcommand %q sets its own PersistentPreRunE; the parent gate is authoritative", sub.Name())
		}
	}
}

// TestSettingsSubcommandsAreGatedByParent is the settings counterpart.
func TestSettingsSubcommandsAreGatedByParent(t *testing.T) {
	t.Parallel()
	ctx, _ := newAuthTestContext(t)
	c := cmdsOf(ctx).SettingsCommand()
	if c.PersistentPreRunE == nil {
		t.Fatal("not true: c.PersistentPreRunE != nil")
	}

	for _, sub := range c.Commands() {
		if sub.PersistentPreRunE != nil {
			t.Errorf("subcommand %q sets its own PersistentPreRunE; the parent gate is authoritative", sub.Name())
		}
	}
}

// TestCollabManagementRequiresAdmin verifies a read-write collaborator cannot
// add, remove or demote collaborators. Before, they could remove or demote
// peers and, with anon-access set to no-access, lock any registered user out
// of a public repository by adding them as a no-access collaborator.
func TestCollabManagementRequiresAdmin(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	if err := be.SetAnonAccess(ctx, access.NoAccess); err != nil {
		t.Fatal(err)
	}

	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	writerCtx := withUser(t, ctx, be, "writer", false)
	victimCtx := withUser(t, ctx, be, "victim", false)
	_ = withUser(t, ctx, be, "peer", false)
	if err := be.AddCollaborator(ownerCtx, "repo", "writer", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	if err := be.AddCollaborator(ownerCtx, "repo", "peer", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	victim := proto.UserFromContext(victimCtx)
	if got := levelOf(t, be, victimCtx, "repo", victim); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, victimCtx, \"repo\", victim) = %v, want %v", got, access.ReadOnlyAccess)
	}

	for _, args := range [][]string{
		{"collab", "add", "repo", "victim", "no-access"},
		{"collab", "add", "repo", "victim", "read-only"},
		{"collab", "add", "repo", "victim"},
		{"collab", "remove", "repo", "peer"},
		{"collab", "add", "repo", "peer", "read-only"},
	} {
		if err := runRepo(t, writerCtx, args...); !errors.Is(err, proto.ErrUnauthorized) {
			t.Errorf("%v: expected ErrUnauthorized, got %v", args, err)
		}
	}

	// Nothing changed: the victim can still read and the peer keeps write.
	_, isCollab, _ := collabOf(t, be, ctx, "repo", "victim")
	if got := isCollab; got != false {
		t.Fatalf("isCollab = %v, want %v", got, false)
	}
	if got := levelOf(t, be, victimCtx, "repo", victim); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, victimCtx, \"repo\", victim) = %v, want %v", got, access.ReadOnlyAccess)
	}
	level, isCollab, err := collabOf(t, be, ctx, "repo", "peer")
	if err != nil {
		t.Fatal(err)
	}
	if !isCollab {
		t.Fatal("not true: isCollab")
	}
	if got := level; got != access.ReadWriteAccess {
		t.Fatalf("level = %v, want %v", got, access.ReadWriteAccess)
	}

	// A read-write collaborator can still list collaborators.
	if err := runRepo(t, writerCtx, "collab", "list", "repo"); err != nil {
		t.Fatal(err)
	}
}

// TestCollabManagementByRepoAdmin verifies an admin-access collaborator, who
// is not the owner or a server admin, can add, remove and re-add
// collaborators at any level.
func TestCollabManagementByRepoAdmin(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)

	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	adminCtx := withUser(t, ctx, be, "maintainer", false)
	_ = withUser(t, ctx, be, "peer", false)
	if err := be.AddCollaborator(ownerCtx, "repo", "maintainer", access.AdminAccess); err != nil {
		t.Fatal(err)
	}

	if err := runRepo(t, adminCtx, "collab", "add", "repo", "peer", "admin-access"); err != nil {
		t.Fatal(err)
	}
	level, isCollab, err := collabOf(t, be, ctx, "repo", "peer")
	if err != nil {
		t.Fatal(err)
	}
	if !isCollab {
		t.Fatal("not true: isCollab")
	}
	if got := level; got != access.AdminAccess {
		t.Fatalf("level = %v, want %v", got, access.AdminAccess)
	}

	if err := runRepo(t, adminCtx, "collab", "remove", "repo", "peer"); err != nil {
		t.Fatal(err)
	}
	if err := runRepo(t, adminCtx, "collab", "add", "repo", "peer", "no-access"); err != nil {
		t.Fatal(err)
	}
	level, isCollab, err = collabOf(t, be, ctx, "repo", "peer")
	if err != nil {
		t.Fatal(err)
	}
	if !isCollab {
		t.Fatal("not true: isCollab")
	}
	if got := level; got != access.NoAccess {
		t.Fatalf("level = %v, want %v", got, access.NoAccess)
	}
}

// TestCollabManagementRefusesAnonymous verifies that anon-access set to
// admin-access does not let an anonymous caller manage collaborators.
func TestCollabManagementRefusesAnonymous(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)

	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = withUser(t, ctx, be, "peer", false)
	if err := be.AddCollaborator(ownerCtx, "repo", "peer", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	if err := be.SetAnonAccess(ctx, access.AdminAccess); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"collab", "add", "repo", "stranger"},
		{"collab", "remove", "repo", "peer"},
	} {
		if err := runRepo(t, ctx, args...); !errors.Is(err, proto.ErrUnauthorized) {
			t.Errorf("%v: expected ErrUnauthorized, got %v", args, err)
		}
	}
}

// TestCollabListShowsLevels verifies `collab list` prints each
// collaborator's access level next to their name.
func TestCollabListShowsLevels(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)

	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = withUser(t, ctx, be, "alice", false)
	_ = withUser(t, ctx, be, "bob", false)
	if err := be.AddCollaborator(ownerCtx, "repo", "alice", access.AdminAccess); err != nil {
		t.Fatal(err)
	}
	if err := be.AddCollaborator(ownerCtx, "repo", "bob", access.NoAccess); err != nil {
		t.Fatal(err)
	}

	out, _, err := runRepoOutput(t, ownerCtx, "collab", "list", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alice\tadmin-access\n") {
		t.Fatal("not true: strings.Contains(out, \"alice\\tadmin-access\\n\")")
	}
	if !strings.Contains(out, "bob\tno-access\n") {
		t.Fatal("not true: strings.Contains(out, \"bob\\tno-access\\n\")")
	}
}

// TestRepoPropertyLevels checks the level each property command needs to
// change its property: read-write for the text properties, admin for the
// boolean ones, whose access check comes before the value is parsed. Any
// reader may print every property.
func TestRepoPropertyLevels(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	writerCtx := withUser(t, ctx, be, "writer", false)
	_, err := be.CreateRepository(ownerCtx, "r", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.AddCollaborator(ownerCtx, "r", "writer", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}

	for _, prop := range []string{"description", "project-name"} {
		if err := runRepo(t, writerCtx, prop, "r", "a", "b"); err != nil {
			t.Fatal(err)
		}
		out, _, err := runRepoOutput(t, writerCtx, prop, "r")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(out); got != "a b" {
			t.Fatalf("strings.TrimSpace(out) = %v, want %v", got, "a b")
		}
	}
	for _, prop := range []string{"hidden", "mirror", "private"} {
		for _, v := range []string{"true", "maybe"} {
			if err := runRepo(t, writerCtx, prop, "r", v); !errors.Is(err, proto.ErrUnauthorized) {
				t.Errorf("repo %s r %s as a writer: err = %v, want %v", prop, v, err, proto.ErrUnauthorized)
			}
		}
		out, _, err := runRepoOutput(t, writerCtx, prop, "r")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(out); got != "false" {
			t.Fatalf("strings.TrimSpace(out) = %v, want %v", got, "false")
		}
	}
}

func TestParseBool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
		ok   bool
	}{
		{"true", true, true},
		{"TRUE", true, true},
		{"True", true, true},
		{"1", true, true},
		{"false", false, true},
		{"FALSE", false, true},
		{"0", false, true},
		{"yes", false, false},
		{"ture", false, false},
		{"", false, false},
	} {
		got, err := parseBool(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("parseBool(%q) error = %v, want ok=%t", tc.in, err, tc.ok)
			continue
		}
		if got != tc.want {
			t.Errorf("parseBool(%q) = %t, want %t", tc.in, got, tc.want)
		}
	}
}

// wantInvalidBool accepts only the shared parseBool error, which every SSH
// command that takes a boolean argument returns.
func wantInvalidBool(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "invalid boolean value") || !strings.Contains(err.Error(), "true or false") {
		t.Fatalf("expected invalid boolean error, got %v", err)
	}
}

// TestRepoBoolArgs checks that repository boolean properties do not treat
// anything other than the exact string "true" as false: an uppercase TRUE
// turns the property on and a typo is refused.
func TestRepoBoolArgs(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	_, err := be.CreateRepository(adminCtx, "r", proto.UserFromContext(adminCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	get := map[string]func() bool{
		"hidden": func() bool {
			r, err := be.Repository(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			return r.IsHidden()
		},
		"private": func() bool {
			r, err := be.Repository(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			return r.IsPrivate()
		},
	}
	for _, prop := range []string{"hidden", "private"} {
		if err := runRepo(t, adminCtx, prop, "r", "TRUE"); err != nil {
			t.Fatal(err)
		}
		if !get[prop]() {
			t.Errorf("repo %s r TRUE: property not set", prop)
		}
		wantInvalidBool(t, runRepo(t, adminCtx, prop, "r", "yes"))
		if !get[prop]() {
			t.Errorf("repo %s r yes: property changed despite the error", prop)
		}
		if err := runRepo(t, adminCtx, prop, "r", "False"); err != nil {
			t.Fatal(err)
		}
		if get[prop]() {
			t.Errorf("repo %s r False: property not cleared", prop)
		}
	}

	// Mirroring cannot be turned on without a remote, so TRUE must reach
	// the backend and be refused there rather than being read as false and
	// succeeding as a no-op.
	if err := runRepo(t, adminCtx, "mirror", "r", "TRUE"); err == nil || !strings.Contains(err.Error(), "no remote URL") {
		t.Errorf("repo mirror r TRUE: expected the no-remote error, got %v", err)
	}
	wantInvalidBool(t, runRepo(t, adminCtx, "mirror", "r", "yes"))
	if err := runRepo(t, adminCtx, "mirror", "r", "FALSE"); err != nil {
		t.Fatal(err)
	}

	// hidden refuses extra arguments rather than ignore them.
	if err := runRepo(t, adminCtx, "hidden", "r", "true", "extra"); err == nil {
		t.Error("repo hidden r true extra: expected an argument count error")
	}
	if get["hidden"]() {
		t.Error("repo hidden r true extra: property changed")
	}
}

// TestUserSetAdminBoolArg checks that `user set-admin alice True` promotes
// alice instead of demoting her, and that a typo is rejected.
func TestUserSetAdminBoolArg(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	_, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	isAdmin := func() bool {
		u, err := be.User(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		return u.IsAdmin()
	}

	if err := runUser(t, adminCtx, "set-admin", "alice", "True"); err != nil {
		t.Fatal(err)
	}
	if !isAdmin() {
		t.Fatal("not true: isAdmin()")
	}
	wantInvalidBool(t, runUser(t, adminCtx, "set-admin", "alice", "no"))
	if !isAdmin() {
		t.Fatal("not true: isAdmin()")
	}
	if err := runUser(t, adminCtx, "set-admin", "alice", "false"); err != nil {
		t.Fatal(err)
	}
	if isAdmin() {
		t.Fatal("not true: !isAdmin()")
	}
}

// TestSettingsAllowKeylessBoolArg checks that an unparsable value is rejected
// instead of disabling keyless access server-wide.
func TestSettingsAllowKeylessBoolArg(t *testing.T) {
	t.Parallel()
	ctx, _ := newSettingsTestContext(t)

	_, _, err := runSettings(t, ctx, "allow-keyless", "true")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = runSettings(t, ctx, "allow-keyless", "yes")
	wantInvalidBool(t, err)
	out, _, err := runSettings(t, ctx, "allow-keyless")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != "true" {
		t.Fatalf("strings.TrimSpace(out) = %v, want %v", got, "true")
	}
}

// newMemServer starts an httptest server for h on testNet and closes it when
// the test ends. Its URL is an http URL on 127.0.0.1, which a webhook may
// use once webhook.allowed_hosts covers it.
func newMemServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	// NewUnstartedServer has already opened a loopback listener; it is
	// replaced before anything can connect to it.
	_ = srv.Listener.Close()
	srv.Listener = testNet.Listen()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// senderEvent is what a test receiver keeps of a webhook request.
type senderEvent struct {
	event    string
	id       int64
	username string
}

// TestEventSenderIsCurrentAccountPerCommand is TestEventSenderIsCurrentAccount
// for the other commands that send an event: the session's account is
// renamed after the session resolved it, and each event names the account
// as it is now.
func TestEventSenderIsCurrentAccountPerCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		args  []string
		event webhook.Event
	}{
		{"branch delete", []string{"branch", "delete", "hooks", "feature"}, webhook.EventBranchTagDelete},
		{"tag delete", []string{"tag", "delete", "hooks", "v1"}, webhook.EventBranchTagDelete},
		{"branch default", []string{"branch", "default", "hooks", "feature"}, webhook.EventRepository},
		{"repo delete", []string{"delete", "hooks"}, webhook.EventRepository},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, be := newAuthTestContextWith(t, func(cfg *config.Config) {
				cfg.Webhook.AllowedHosts = []string{"127.0.0.0/8"}
			})
			ownerCtx := whRepo(t, ctx, be) // the session resolved "owner"
			r, err := be.Repository(ctx, "hooks")
			if err != nil {
				t.Fatal(err)
			}
			gitRefs(t, filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "hooks.git"))

			got := make(chan senderEvent, 8)
			srv := newMemServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, _ := io.ReadAll(req.Body)
				var payload struct {
					Sender struct {
						ID       int64  `json:"id"`
						Username string `json:"username"`
					} `json:"sender"`
				}
				_ = json.Unmarshal(body, &payload)
				got <- senderEvent{req.Header.Get("X-SoftServe-Event"), payload.Sender.ID, payload.Sender.Username}
				w.WriteHeader(http.StatusOK)
			}))
			if err := be.CreateWebhook(ownerCtx, r, backend.WebhookInput{URL: srv.URL + "/hook", ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventBranchTagDelete, webhook.EventRepository}, Active: true}); err != nil {
				t.Fatal(err)
			}

			if err := be.SetUsername(ctx, "owner", "renamed"); err != nil {
				t.Fatal(err)
			}
			if err := runRepo(t, ownerCtx, tc.args...); err != nil {
				t.Fatal(err)
			}

			select {
			case ev := <-got:
				if got, want := ev.event, tc.event.String(); got != want {
					t.Fatalf("ev.event = %v, want %v", got, want)
				}
				if got, want := ev.id, proto.UserFromContext(ownerCtx).ID(); got != want {
					t.Fatalf("ev.id = %v, want %v", got, want)
				}
				if ev.username != "renamed" {
					t.Errorf("%s event: sender %q, want %q", tc.name, ev.username, "renamed")
				}
			case <-time.After(time.Minute):
				t.Fatalf("%s: no event delivered", tc.name)
			}
			be.WaitForDeliveries(context.Background())
		})
	}
}

// gitRefs gives the bare repository dir the branches main, its HEAD, and
// feature, and the tag v1, all on one empty commit.
func gitRefs(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	commit := run("commit-tree", run("mktree"), "-m", "refs")
	run("update-ref", "refs/heads/main", commit)
	run("update-ref", "refs/heads/feature", commit)
	run("update-ref", "refs/tags/v1", commit)
	run("symbolic-ref", "HEAD", "refs/heads/main")
}

// gitFailureRepo is a repository "r" owned by the session's user, with the
// files "f" and "d/g" on branch main (HEAD), a branch "other" and a tag
// "v1", all loose: every object and ref is a file of its own. It holds the
// session context, the backend, the server log, the repository's directory
// and the IDs of main's commit, its root tree, the blob of "f" and the tree
// of "d".
type gitFailureRepo struct {
	ctx                         context.Context
	be                          *backend.Backend
	logs                        *syncLog
	dir                         string
	commit, tree, blob, subtree string
}

func newGitFailureRepo(t *testing.T, wrap func(store.Store) store.Store) gitFailureRepo {
	t.Helper()
	ctx, be, logs := newLookupTestContextWith(t, wrap)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	if _, err := be.CreateRepository(ownerCtx, "r", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "r.git")
	blob := gittest.Git(t, dir, []byte("root\n"), "hash-object", "-w", "--stdin")
	subtree := gittest.Tree(t, dir, "g", "sub\n")
	tree := gittest.Git(t, dir, fmt.Appendf(nil, "100644 blob %s\tf\n040000 tree %s\td\n", blob, subtree), "mktree")
	commit := gittest.Commit(t, dir, gittest.Valid, tree, "root")
	gittest.Git(t, dir, nil, "update-ref", "refs/heads/main", commit)
	gittest.Git(t, dir, nil, "update-ref", "refs/heads/other", commit)
	gittest.Git(t, dir, nil, "update-ref", "refs/tags/v1", commit)
	gittest.Git(t, dir, nil, "symbolic-ref", "HEAD", "refs/heads/main")
	return gitFailureRepo{ownerCtx, be, logs, dir, commit, tree, blob, subtree}
}

// looseObject returns the path of the loose object id in the repository dir.
func looseObject(dir, id string) string {
	return filepath.Join(dir, "objects", id[:2], id[2:])
}

// wantGitFailed runs the repo command args and checks that the client gets
// only errGitFailed, with none of leaks in what it sees, and that git's
// failure is in the server log once, at Error, holding want.
func (g gitFailureRepo) wantGitFailed(t *testing.T, want string, leaks []string, args ...string) {
	t.Helper()
	out, stderr, err := runRepoOutput(t, g.ctx, args...)
	if !errors.Is(err, errGitFailed) {
		t.Errorf("%v: err = %v, output %q; want %v", args, err, out, errGitFailed)
	}
	client := out + stderr + fmt.Sprint(err)
	for _, leak := range leaks {
		if strings.Contains(client, leak) {
			t.Errorf("%v: %q reached the client: %q", args, leak, client)
		}
	}
	if n := g.logs.count("ERRO"); n != 1 || !strings.Contains(g.logs.String(), want) {
		t.Errorf("%v: want one Error line holding %q, got %d, log:\n%s", args, want, n, g.logs.String())
	}
}

// A ref git cannot lock, as when another git holds it, fails the change on
// the server: the client must not see git's message, which names the
// repository's path on the server.
func TestLockedRefIsServerFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		lock string
		args []string
	}{
		{"HEAD.lock", []string{"branch", "default", "r", "other"}},
		{"refs/heads/other.lock", []string{"branch", "delete", "r", "other"}},
		{"refs/tags/v1.lock", []string{"tag", "delete", "r", "v1"}},
	} {
		t.Run(tc.lock, func(t *testing.T) {
			t.Parallel()
			g := newGitFailureRepo(t, nil)
			if err := os.WriteFile(filepath.Join(g.dir, filepath.FromSlash(tc.lock)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			g.wantGitFailed(t, ".lock", []string{".lock", g.dir, "fatal", "error:"}, tc.args...)
		})
	}
}

// An object that is missing from the repository, while the refs that lead
// to it still resolve, fails the read on the server.
func TestMissingObjectIsServerFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		missing func(g gitFailureRepo) string
		args    []string
	}{
		{"blob", func(g gitFailureRepo) string { return g.blob }, []string{"blob", "r", "f"}},
		{"blob raw", func(g gitFailureRepo) string { return g.blob }, []string{"blob", "-r", "r", "f"}},
		{"commit blob", func(g gitFailureRepo) string { return g.blob }, []string{"commit", "r", "main"}},
		{"commit patch", func(g gitFailureRepo) string { return g.blob }, []string{"commit", "-p", "r", "main"}},
		{"tree", func(g gitFailureRepo) string { return g.tree }, []string{"tree", "r"}},
		{"tree path", func(g gitFailureRepo) string { return g.tree }, []string{"tree", "r", "f"}},
		{"blob tree", func(g gitFailureRepo) string { return g.tree }, []string{"blob", "r", "f"}},
		{"commit tree", func(g gitFailureRepo) string { return g.tree }, []string{"commit", "r", "main"}},
		{"tree subtree", func(g gitFailureRepo) string { return g.subtree }, []string{"tree", "r", "d"}},
		{"tree in subtree", func(g gitFailureRepo) string { return g.subtree }, []string{"tree", "r", "d/g"}},
		{"blob in subtree", func(g gitFailureRepo) string { return g.subtree }, []string{"blob", "r", "d/g"}},
		// The branch still resolves to its name; its commit object is gone.
		{"commit of branch", func(g gitFailureRepo) string { return g.commit }, []string{"commit", "r", "main"}},
		{"commit patch of branch", func(g gitFailureRepo) string { return g.commit }, []string{"commit", "-p", "r", "main"}},
		{"tree of branch", func(g gitFailureRepo) string { return g.commit }, []string{"tree", "r", "main", "f"}},
		{"blob of branch", func(g gitFailureRepo) string { return g.commit }, []string{"blob", "r", "main", "f"}},
		{"blob raw of branch", func(g gitFailureRepo) string { return g.commit }, []string{"blob", "-r", "r", "main", "f"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newGitFailureRepo(t, nil)
			id := tc.missing(g)
			if err := os.Remove(looseObject(g.dir, id)); err != nil {
				t.Fatal(err)
			}
			g.wantGitFailed(t, "git failed", []string{id, "fatal", "bad file", "unable to read", "Needed"}, tc.args...)
			// git's message is in the log: it names the missing object,
			// or, resolving a tree it cannot find, is a fatal error.
			if log := g.logs.String(); !strings.Contains(log, id) && !strings.Contains(log, "fatal") {
				t.Errorf("%v: git's message is not in the log:\n%s", tc.args, log)
			}
		})
	}
}

// A repository directory git does not accept, or one that is gone, fails
// every command that opens it on the server.
func TestOpenFailureIsServerFailure(t *testing.T) {
	t.Parallel()
	t.Run("not a repository", func(t *testing.T) {
		t.Parallel()
		g := newGitFailureRepo(t, nil)
		if err := os.Remove(filepath.Join(g.dir, "HEAD")); err != nil {
			t.Fatal(err)
		}
		// The backend logs why git does not accept the directory; the
		// command logs nothing more.
		g.wantGitFailed(t, "git does not accept the repository", []string{g.dir, "not a git repository"}, "branch", "list", "r")
	})
	t.Run("not a directory", func(t *testing.T) {
		t.Parallel()
		g := newGitFailureRepo(t, nil)
		if err := os.RemoveAll(g.dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(g.dir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// One command of each file that opens the repository.
		for _, args := range [][]string{
			{"branch", "list", "r"},
			{"tag", "list", "r"},
			{"tree", "r"},
			{"commit", "r", "main"},
			{"blob", "r", "f"},
			{"info", "r"},
			{"branch", "default", "r"},
			{"branch", "delete", "r", "other"},
			{"tag", "delete", "r", "v1"},
		} {
			g.logs.reset()
			g.wantGitFailed(t, "open repository", []string{g.dir, "does not exist"}, args...)
		}
	})
}

// failRepoByNameStore fails GetRepoByName while armed.
type failRepoByNameStore struct {
	store.Store
	armed *atomic.Bool
}

var errInjectedRepoRead = errors.New("injected repos read failure: /server/path")

func (s failRepoByNameStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	if s.armed.Load() {
		return models.Repo{}, errInjectedRepoRead
	}
	return s.Store.GetRepoByName(ctx, h, name)
}

// `repo info` on a mirror reads its sync status from the database; a failed
// read is logged, and the client gets the generic lookup error.
func TestInfoMirrorSyncStatusLookupFailure(t *testing.T) {
	t.Parallel()
	armed := new(atomic.Bool)
	g := newGitFailureRepo(t, func(s store.Store) store.Store { return failRepoByNameStore{Store: s, armed: armed} })
	if err := cmdsOf(g.ctx).store.SetRepoIsMirrorByName(g.ctx, cmdsOf(g.ctx).db, "r", true); err != nil {
		t.Fatal(err)
	}
	afterGate(g.ctx, func() { armed.Store(true) })

	out, stderr, err := runRepoOutput(t, g.ctx, "info", "r")
	if !errors.Is(err, errLookupFailed) {
		t.Fatalf("err = %v, output %q, want %v", err, out+stderr, errLookupFailed)
	}
	if client := out + stderr + err.Error(); strings.Contains(client, "/server/path") {
		t.Errorf("the database error reached the client: %q", client)
	}
	if n := g.logs.count("ERRO"); n != 1 || !strings.Contains(g.logs.String(), "/server/path") {
		t.Errorf("want the failure logged once at Error, got %d:\n%s", n, g.logs.String())
	}
}

// A tree this server will not list, here one with a name longer than any
// file system takes, is the repository content's: the client gets
// git.ErrMalformedTree, from the listing and from a lookup of that name
// alike.
func TestMalformedTreeIsReported(t *testing.T) {
	t.Parallel()
	g := newGitFailureRepo(t, nil)
	blob := gittest.Git(t, g.dir, []byte("x\n"), "hash-object", "-w", "--stdin")
	long := strings.Repeat("n", 5000)
	tree := gittest.Git(t, g.dir, fmt.Appendf(nil, "100644 blob %s\tf\n100644 blob %s\t%s\n", blob, blob, long), "mktree")
	commit := gittest.Commit(t, g.dir, gittest.Valid, tree, "long name")
	gittest.Git(t, g.dir, nil, "update-ref", "refs/heads/main", commit)

	// A lookup lists only the path asked for, so it meets the long name
	// only when asked for it.
	for _, args := range [][]string{
		{"tree", "r"},
		{"tree", "r", long},
		{"blob", "r", long},
	} {
		_, _, err := runRepoOutput(t, g.ctx, args...)
		if !errors.Is(err, git.ErrMalformedTree) {
			t.Errorf("%v: err = %v, want %v", args, err, git.ErrMalformedTree)
		}
	}
	if n := g.logs.count("ERRO"); n != 0 {
		t.Errorf("a malformed tree was logged as a server failure:\n%s", g.logs.String())
	}
}

// With a detached HEAD, reading the default branch before the change, which
// only feeds the event, fails; the change still goes on and the failure is
// logged at Warn.
func TestBranchDefaultFromDetachedHEAD(t *testing.T) {
	t.Parallel()
	g := newGitFailureRepo(t, nil)
	if err := os.WriteFile(filepath.Join(g.dir, "HEAD"), []byte(g.commit+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runRepoOutput(t, g.ctx, "branch", "default", "r", "other"); err != nil {
		t.Fatalf("branch default from a detached HEAD: %v", err)
	}
	if head := gittest.Git(t, g.dir, nil, "symbolic-ref", "HEAD"); head != "refs/heads/other" {
		t.Errorf("HEAD = %q, want refs/heads/other", head)
	}
	if n := g.logs.count("WARN"); n != 1 || g.logs.count("ERRO") != 0 {
		t.Errorf("want the old default branch read logged once at Warn, log:\n%s", g.logs.String())
	}
}

// A tag on a blob, lightweight or annotated, names no tree or commit: asking
// for one there is the client's mistake, answered "revision does not
// exist" and not logged as a server failure.
func TestTagOnBlobIsRevisionNotExist(t *testing.T) {
	t.Parallel()
	g := newGitFailureRepo(t, nil)
	gittest.Git(t, g.dir, nil, "update-ref", "refs/tags/blobtag", g.blob)
	gittest.Git(t, g.dir, nil, "tag", "-a", "-m", "on a blob", "annblob", g.blob)

	for _, args := range [][]string{
		{"tree", "r", "blobtag", "f"},
		{"blob", "r", "blobtag", "f"},
		{"tree", "r", "annblob", "f"},
		{"blob", "-r", "r", "annblob", "f"},
		// Guards only: git log on a tag on a blob exits 0 with no
		// commits, which is a missing revision without the peel check.
		{"commit", "r", "blobtag"},
		{"commit", "r", "annblob"},
	} {
		_, _, err := runRepoOutput(t, g.ctx, args...)
		if !errors.Is(err, git.ErrRevisionNotExist) {
			t.Errorf("%v: err = %v, want %v", args, err, git.ErrRevisionNotExist)
		}
	}
	if n := g.logs.count("ERRO"); n != 0 {
		t.Errorf("a tag on a blob was logged as a server failure:\n%s", g.logs.String())
	}
}

// A detached HEAD whose commit object is gone is the repository failing,
// as a branch without its commit is.
func TestDetachedHEADWithoutCommitIsServerFailure(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"commit", "r", "HEAD"},
		{"tree", "r", "HEAD", "f"},
	} {
		g := newGitFailureRepo(t, nil)
		gittest.Git(t, g.dir, nil, "update-ref", "--no-deref", "HEAD", g.commit)
		if err := os.Remove(looseObject(g.dir, g.commit)); err != nil {
			t.Fatal(err)
		}
		g.wantGitFailed(t, "git failed", []string{g.commit, "fatal", "bad object", "unable to read"}, args...)
	}
}

// A session refused by a server admin gate gets an error that names the
// key's fingerprint and how to make ssh offer only the key passed with -i,
// instead of a bare "unauthorized". The gate does not depend on repository
// state, so the extra detail reveals nothing about repositories.
func TestNotServerAdminErrorNamesFingerprint(t *testing.T) {
	ctx, be := newAuthTestContext(t)
	kp, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	pk := kp.PublicKey()
	fp := gossh.FingerprintSHA256(pk)

	// Both a key that belongs to no user and a non-admin user's key.
	unknownCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, pk)
	userCtx := context.WithValue(withUser(t, ctx, be, "bob", false), ssh.ContextKeyPublicKey, pk)

	for _, tc := range []struct {
		name string
		ctx  context.Context
		cmd  func(*Commands) *cobra.Command
		args []string
	}{
		{"user list, unknown key", unknownCtx, (*Commands).UserCommand, []string{"list"}},
		{"user list, non-admin", userCtx, (*Commands).UserCommand, []string{"list"}},
		{"settings, non-admin", userCtx, (*Commands).SettingsCommand, []string{"anon-access"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cmd(cmdsOf(tc.ctx))
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			c.SetArgs(tc.args)
			err := c.ExecuteContext(tc.ctx)
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Fatalf("got %v, want ErrUnauthorized", err)
			}
			for _, want := range []string{fp, "IdentitiesOnly=yes"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}

	// Without a public key, such as a keyboard-interactive session, there
	// is no key to name.
	if err := errNotServerAdmin(ctx); err.Error() != proto.ErrUnauthorized.Error() {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}

// repoScopedCommands lists every repo subcommand that names an existing
// repository, with a placeholder for the repository name.
var repoScopedCommands = [][]string{
	{"info", "REPO"},
	{"tree", "REPO"},
	{"blob", "REPO", "README.md"},
	{"commit", "REPO", "HEAD"},
	{"branch", "list", "REPO"},
	{"branch", "default", "REPO"},
	{"branch", "default", "REPO", "main"},
	{"branch", "delete", "REPO", "main"},
	{"tag", "list", "REPO"},
	{"tag", "delete", "REPO", "v1"},
	{"delete", "REPO"},
	{"rename", "REPO", "other"},
	{"collab", "list", "REPO"},
	{"collab", "add", "REPO", "someone"},
	{"collab", "add", "REPO", "someone", "admin-access"},
	{"collab", "remove", "REPO", "someone"},
	{"description", "REPO"},
	{"description", "REPO", "new"},
	{"project-name", "REPO"},
	{"project-name", "REPO", "new"},
	{"private", "REPO"},
	{"private", "REPO", "true"},
	{"hidden", "REPO"},
	{"hidden", "REPO", "true"},
	{"mirror", "REPO"},
	{"mirror", "REPO", "true"},
	{"webhook", "list", "REPO"},
	{"webhook", "create", "REPO", "https://93.184.216.34", "-e", "push"},
	{"webhook", "delete", "REPO", "1"},
	{"webhook", "update", "REPO", "1"},
	{"webhook", "deliveries", "list", "REPO", "1"},
	{"webhook", "deliveries", "redeliver", "REPO", "1", "1"},
	{"webhook", "deliveries", "get", "REPO", "1", "1"},
}

func withRepoName(args []string, name string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "REPO" {
			a = name
		}
		out[i] = a
	}
	return out
}

// assertSameAnswer runs every repo-scoped command against a private
// repository the caller cannot read and against a missing one, and checks
// the caller gets want for both. Any difference would reveal which names
// exist.
func assertSameAnswer(t *testing.T, ctx context.Context, want error) {
	t.Helper()
	for _, args := range repoScopedCommands {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			privOut, privErrOut, privErr := runRepoOutput(t, ctx, withRepoName(args, "secret")...)
			missOut, missErrOut, missErr := runRepoOutput(t, ctx, withRepoName(args, "ghost")...)
			if !errors.Is(privErr, want) {
				t.Errorf("private: expected %v, got %v", want, privErr)
			}
			if !errors.Is(missErr, want) {
				t.Errorf("missing: expected %v, got %v", want, missErr)
			}
			if got := privOut; got != "" {
				t.Fatalf("privOut = %v, want %v", got, "")
			}
			if got := missOut; got != privOut {
				t.Fatalf("missOut = %v, want %v", got, privOut)
			}
			if got := missErrOut; got != privErrOut {
				t.Fatalf("missErrOut = %v, want %v", got, privErrOut)
			}
		})
	}
}

func setupSecretRepo(t *testing.T) (context.Context, context.Context, *backend.Backend) {
	t.Helper()
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "secret", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = withUser(t, ctx, be, "someone", false)
	return ctx, ownerCtx, be
}

// TestAnonymousCannotTellPrivateFromMissing covers callers with no account:
// both a private repository and a missing one answer ErrUnauthorized.
func TestAnonymousCannotTellPrivateFromMissing(t *testing.T) {
	ctx, _, _ := setupSecretRepo(t)
	assertSameAnswer(t, ctx, proto.ErrUnauthorized)
}

// TestRegisteredUserCannotTellPrivateFromMissing covers signed-in users
// without access: both answer ErrRepoNotFound, even though a missing name
// would otherwise grant them read-write access so a push can create it.
func TestRegisteredUserCannotTellPrivateFromMissing(t *testing.T) {
	ctx, _, be := setupSecretRepo(t)
	userCtx := withUser(t, ctx, be, "stranger", false)
	assertSameAnswer(t, userCtx, proto.ErrRepoNotFound)
}

// TestNoAccessCollaboratorCannotTellPrivateFromMissing covers a user who was
// explicitly granted no-access on the private repository.
func TestNoAccessCollaboratorCannotTellPrivateFromMissing(t *testing.T) {
	ctx, ownerCtx, be := setupSecretRepo(t)
	userCtx := withUser(t, ctx, be, "blocked", false)
	if err := be.AddCollaborator(ownerCtx, "secret", "blocked", 0); err != nil {
		t.Fatal(err)
	}
	assertSameAnswer(t, userCtx, proto.ErrRepoNotFound)
}

// TestReaderGetsUnauthorizedForWrites verifies that a caller who can read a
// repository but not write to it is still told "unauthorized" for writes, as
// existence is not a secret from someone who can read it.
func TestReaderGetsUnauthorizedForWrites(t *testing.T) {
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "public", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	userCtx := withUser(t, ctx, be, "reader", false)
	for _, args := range [][]string{
		{"delete", "public"},
		{"rename", "public", "other"},
		{"collab", "list", "public"},
		{"description", "public", "new"},
		{"private", "public", "true"},
		{"hidden", "public", "true"},
		{"webhook", "list", "public"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			err := runRepo(t, userCtx, args...)
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Errorf("expected ErrUnauthorized, got %v", err)
			}
		})
	}

	// Reads still work.
	if err := runRepo(t, userCtx, "description", "public"); err != nil {
		t.Errorf("reader cannot read description: %v", err)
	}
}

// TestAdminCanDeleteOrphanedRepository verifies the read gate lets a server
// admin through to a repository whose directory is gone, so the leftover
// database row can still be deleted.
func TestAdminCanDeleteOrphanedRepository(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "root", true)
	_, err := be.CreateRepository(adminCtx, "orphan", proto.UserFromContext(adminCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	cfg := cmdsOf(ctx).cfg
	if err := os.RemoveAll(filepath.Join(cfg.DataPath, "repos", "orphan.git")); err != nil {
		t.Fatal(err)
	}

	// Reads still say not found, even to the admin.
	err = runRepo(t, adminCtx, "info", "orphan")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}

	if err := runRepo(t, adminCtx, "delete", "orphan"); err != nil {
		t.Fatal(err)
	}
	repos, err := be.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(repos); got != 0 {
		t.Fatalf("len(repos) = %v, want %v", got, 0)
	}

	// A second delete reports the name as missing.
	err = runRepo(t, adminCtx, "delete", "orphan")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
}

// TestRenameDestinationIsGatedLikeCreate verifies a writer cannot rename
// onto a name they could not create, and gets the same answer create gives.
func TestRenameDestinationIsGatedLikeCreate(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	otherCtx := withUser(t, ctx, be, "other", false)
	_, err := be.CreateRepository(ownerCtx, "mine", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(otherCtx, "theirs", proto.UserFromContext(otherCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	renameErr := runRepo(t, ownerCtx, "rename", "mine", "theirs")
	createErr := runRepo(t, ownerCtx, "create", "theirs")
	if !errors.Is(renameErr, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(renameErr, proto.ErrUnauthorized)")
	}
	if got, want := renameErr, createErr; !reflect.DeepEqual(got, want) {
		t.Fatalf("renameErr = %v, want %v", got, want)
	}

	// The gate for the source does not leak into the destination check.
	if err := runRepo(t, ownerCtx, "rename", "mine", "renamed"); err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "renamed")
	if err != nil {
		t.Fatal(err)
	}

	// With admin-only creation on, a new name is one the owner could not
	// create, so renaming onto it gets create's answer too.
	cmdsOf(ctx).cfg.AdminOnlyCreate = true
	renameErr = runRepo(t, ownerCtx, "rename", "renamed", "again")
	createErr = runRepo(t, ownerCtx, "create", "again")
	if !errors.Is(renameErr, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(renameErr, proto.ErrUnauthorized)")
	}
	if got, want := renameErr, createErr; !reflect.DeepEqual(got, want) {
		t.Fatalf("renameErr = %v, want %v", got, want)
	}
	_, err = be.Repository(ctx, "again")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = be.Repository(ctx, "renamed")
	if err != nil {
		t.Fatal(err)
	}
}

// afterGate runs fn once, after the next access check of ctx's commands and
// before the command acts.
func afterGate(ctx context.Context, fn func()) {
	ran := false
	cmdsOf(ctx).afterGate = func() {
		if !ran {
			ran = true
			fn()
		}
	}
}

// TestRepoCommandsActOnCheckedRepository runs each repository command that
// changes a repository's row, its collaborators or the repository itself,
// with the repository it checked renamed away, and another one created
// under its name by someone else, between the check and the action. The
// command must not act on the other repository: the caller was authorized
// for the first one only.
func TestRepoCommandsActOnCheckedRepository(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		// same reports whether other, the repository now named "r", is
		// as it was.
		same func(t *testing.T, ctx context.Context, be *backend.Backend, other proto.Repository) bool
	}{
		{"description", []string{"description", "r", "changed"}, func(_ *testing.T, _ context.Context, _ *backend.Backend, o proto.Repository) bool {
			return o.Description() == ""
		}},
		{"project-name", []string{"project-name", "r", "changed"}, func(_ *testing.T, _ context.Context, _ *backend.Backend, o proto.Repository) bool {
			return o.ProjectName() == ""
		}},
		{"hidden", []string{"hidden", "r", "true"}, func(_ *testing.T, _ context.Context, _ *backend.Backend, o proto.Repository) bool {
			return !o.IsHidden()
		}},
		{"private", []string{"private", "r", "true"}, func(_ *testing.T, _ context.Context, _ *backend.Backend, o proto.Repository) bool {
			return !o.IsPrivate()
		}},
		{"delete", []string{"delete", "r"}, func(_ *testing.T, _ context.Context, _ *backend.Backend, _ proto.Repository) bool {
			return true // it still exists
		}},
		{"collab add", []string{"collab", "add", "r", "carol"}, func(t *testing.T, ctx context.Context, be *backend.Backend, o proto.Repository) bool {
			return !isCollab(t, ctx, be, o, "carol")
		}},
		{"collab remove", []string{"collab", "remove", "r", "dave"}, func(t *testing.T, ctx context.Context, be *backend.Backend, o proto.Repository) bool {
			return isCollab(t, ctx, be, o, "dave")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, be := newAuthTestContext(t)
			aliceCtx := withUser(t, ctx, be, "alice", false)
			bobCtx := withUser(t, ctx, be, "bob", false)
			for _, u := range []string{"carol", "dave"} {
				_, err := be.CreateUser(ctx, u, proto.UserOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err := be.CreateRepository(aliceCtx, "r", proto.UserFromContext(aliceCtx), proto.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := be.AddCollaborator(aliceCtx, "r", "dave", access.ReadOnlyAccess); err != nil {
				t.Fatal(err)
			}

			afterGate(ctx, func() {
				if err := be.RenameRepository(aliceCtx, "r", "r-old"); err != nil {
					t.Fatal(err)
				}
				_, err := be.CreateRepository(bobCtx, "r", proto.UserFromContext(bobCtx), proto.RepositoryOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if err := be.AddCollaborator(bobCtx, "r", "dave", access.ReadOnlyAccess); err != nil {
					t.Fatal(err)
				}
			})
			err = runRepo(t, aliceCtx, tc.args...)
			if !errors.Is(err, proto.ErrRepoNotFound) {
				t.Errorf("err = %v, want %v", err, proto.ErrRepoNotFound)
			}

			other, err := be.Repository(ctx, "r")
			if err != nil {
				t.Fatal(err)
			} // bob's repository is still there
			if got, want := other.UserID(), proto.UserFromContext(bobCtx).ID(); got != want {
				t.Fatalf("other.UserID() = %v, want %v", got, want)
			}
			if !tc.same(t, ctx, be, other) {
				t.Errorf("%v changed bob's repository", tc.args)
			}
		})
	}
}

// isCollab reports whether username is a collaborator of r.
func isCollab(t *testing.T, ctx context.Context, be *backend.Backend, r proto.Repository, username string) bool {
	t.Helper()
	u, err := be.User(ctx, username)
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err := be.IsCollaborator(ctx, r, u)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestEventSenderIsCurrentAccount renames the session's account after the
// session resolved it. The events the session's commands send name the
// account as it is now, not the name it had when the session started.
func TestEventSenderIsCurrentAccount(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be) // the session resolved "owner"
	id := whCreate(t, ownerCtx, be, "-e", "collaborator")
	whPointAtLoopback(t, ownerCtx, "hooks", id)
	_, err := be.CreateUser(ctx, "carol", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := be.SetUsername(ctx, "owner", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := runRepo(t, ownerCtx, "collab", "add", "hooks", "carol"); err != nil {
		t.Fatal(err)
	}
	whAwaitDeliveries(t, ownerCtx, be, "hooks", id, 1)
	if err := runRepo(t, ownerCtx, "collab", "remove", "hooks", "carol"); err != nil {
		t.Fatal(err)
	}
	whAwaitDeliveries(t, ownerCtx, be, "hooks", id, 2)

	be.WaitForDeliveries(context.Background())
	r, err := be.Repository(ctx, "hooks")
	if err != nil {
		t.Fatal(err)
	}
	ds, err := be.ListWebhookDeliveries(ctx, r, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ds); got != 2 {
		t.Fatalf("len(ds) = %v, want %v", got, 2)
	}
	for _, d := range ds {
		full, err := be.WebhookDelivery(ctx, r, id, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Action string `json:"action"`
			Sender struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"sender"`
		}
		if err := json.Unmarshal([]byte(full.RequestBody), &payload); err != nil {
			t.Fatal(err)
		}
		if got, want := payload.Sender.ID, proto.UserFromContext(ownerCtx).ID(); got != want {
			t.Fatalf("payload.Sender.ID = %v, want %v", got, want)
		}
		if payload.Sender.Username != "renamed" {
			t.Errorf("collaborator %s event: sender %q, want %q", payload.Action, payload.Sender.Username, "renamed")
		}
	}
}

// An SSH session resolves its user once. If that account is renamed and
// another account takes the old name, the session's account commands still
// act on the session's own account: `pubkey add` must not give the other
// account the key (which would let the key's holder sign in as it), `pubkey
// remove` must not remove the other account's keys, and `set-username`
// must not rename the other account.
func TestAccountCommandsActOnSessionAccountAfterRename(t *testing.T) {
	t.Parallel()
	newKey := func() gossh.PublicKey {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := gossh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		return signer.PublicKey()
	}
	run := func(ctx context.Context, c *cobra.Command, args ...string) error {
		c.SilenceUsage = true
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		c.SetArgs(args)
		return c.ExecuteContext(ctx)
	}
	ctx, be := newAuthTestContext(t)
	keysOf := func(ctx context.Context, name string) []gossh.PublicKey {
		t.Helper()
		u, err := be.User(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return u.PublicKeys()
	}
	k1, k2signer := newKey(), newEd25519Signer(t)
	k2 := k2signer.PublicKey()
	session, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []gossh.PublicKey{k1}})
	if err != nil {
		t.Fatal(err)
	}
	sessCtx := context.WithValue(proto.WithUserContext(ctx, session), ssh.ContextKeyPublicKey, k1)

	// An admin renames the account; another account takes the old name.
	if err := be.SetUsername(ctx, "alice", "carol"); err != nil {
		t.Fatal(err)
	}
	// The other account has two keys, so removing one of them is not
	// refused as its last key: only the account decides.
	mk, mk2 := newKey(), newKey()
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []gossh.PublicKey{mk, mk2}})
	if err != nil {
		t.Fatal(err)
	}

	// The proof signs the account's name as it is now.
	_, err = runPubkeyIn(sessCtx, proofFor(t, k2signer, "carol"), "add", proto.MarshalAuthorizedKey(k2))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(keysOf(ctx, "carol")); got != 2 {
		t.Fatalf("len(keysOf(ctx, \"carol\")) = %v, want %v", got, 2)
	}
	if got := len(keysOf(ctx, "alice")); got != 2 {
		t.Fatalf("len(keysOf(ctx, \"alice\")) = %v, want %v", got, 2)
	}

	// The other account's key is not the session's to remove.
	err = run(sessCtx, cmdsOf(sessCtx).PubkeyCommand(), "remove", proto.MarshalAuthorizedKey(mk))
	if !errors.Is(err, proto.ErrPublicKeyNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrPublicKeyNotFound)")
	}
	if got := len(keysOf(ctx, "alice")); got != 2 {
		t.Fatalf("len(keysOf(ctx, \"alice\")) = %v, want %v", got, 2)
	}
	if err := run(sessCtx, cmdsOf(sessCtx).PubkeyCommand(), "remove", proto.MarshalAuthorizedKey(k2)); err != nil {
		t.Fatal(err)
	}
	if got := len(keysOf(ctx, "carol")); got != 1 {
		t.Fatalf("len(keysOf(ctx, \"carol\")) = %v, want %v", got, 1)
	}

	if err := run(sessCtx, cmdsOf(sessCtx).SetUsernameCommand(), "dave"); err != nil {
		t.Fatal(err)
	}
	_, err = be.User(ctx, "dave")
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.User(ctx, "carol")
	if !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
	}
	if got := len(keysOf(ctx, "alice")); got != 2 {
		t.Fatalf("len(keysOf(ctx, \"alice\")) = %v, want %v", got, 2)
	} // the other account kept its name and keys
}

// The SSH authentication middleware resolves the session's key to a user
// once and stores it in the context. Commands that act on the caller's
// account use that user instead of looking the key up again: with the
// user on the context they work even when the database cannot be reached.
func TestAccountCommandsUseSessionUser(t *testing.T) {
	ctx, be := newAuthTestContext(t)
	kp, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	pk := kp.PublicKey()
	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	sessCtx := proto.WithUserContext(context.WithValue(ctx, ssh.ContextKeyPublicKey, pk), user)

	// Any lookup from here on fails.
	if err := cmdsOf(ctx).db.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		cmd  func(*Commands) *cobra.Command
		args []string
		want string
	}{
		{"info", (*Commands).InfoCommand, nil, "Username: alice"},
		{"pubkey list", (*Commands).PubkeyCommand, []string{"list"}, strings.TrimSpace(string(kp.AuthorizedKey()))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cmd(cmdsOf(sessCtx))
			var out bytes.Buffer
			c.SetOut(&out)
			c.SetErr(&bytes.Buffer{})
			c.SetArgs(tc.args)
			if err := c.ExecuteContext(sessCtx); err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output %q does not contain %q", out.String(), tc.want)
			}
		})
	}
}

// A session whose key belongs to no user gets an error that names the
// key's fingerprint and how to make ssh offer only the key passed with -i,
// instead of a bare "user not found".
func TestUnknownKeyErrorNamesFingerprint(t *testing.T) {
	ctx, _ := newAuthTestContext(t)
	kp, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	pk := kp.PublicKey()
	keyCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, pk)
	fp := gossh.FingerprintSHA256(pk)

	for _, tc := range []struct {
		name string
		cmd  func(*Commands) *cobra.Command
		args []string
	}{
		{"info", (*Commands).InfoCommand, nil},
		{"pubkey list", (*Commands).PubkeyCommand, []string{"list"}},
		{"set-username", (*Commands).SetUsernameCommand, []string{"bob"}},
		{"token list", (*Commands).TokenCommand, []string{"list"}},
		{"jwt", (*Commands).JWTCommand, []string{"repo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cmd(cmdsOf(keyCtx))
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			c.SetArgs(tc.args)
			err := c.ExecuteContext(keyCtx)
			if !errors.Is(err, proto.ErrUserNotFound) {
				t.Fatalf("got %v, want ErrUserNotFound", err)
			}
			for _, want := range []string{fp, "IdentitiesOnly=yes"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}

	// Without a public key, such as a keyboard-interactive session, there
	// is no key to name.
	if err := errUnknownKey(ctx); err.Error() != proto.ErrUserNotFound.Error() {
		t.Errorf("got %v, want ErrUserNotFound", err)
	}
}
