package cmd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	gossh "golang.org/x/crypto/ssh"
)

// Admin-only creation: only server admins may create repositories.

// With the flag off, registered users may create repositories as upstream
// does, and the admin-only restriction does not apply.
func TestUsersCreateRepositoriesWhenFlagOff(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContextWith(t, func(c *config.Config) { c.AdminOnlyCreate = false })
	userCtx := withUser(t, ctx, be, "worker", false)

	if err := runRepo(t, userCtx, "create", "mine", "-p"); err != nil {
		t.Fatal(err)
	}
	_, err := be.Repository(ctx, "mine")
	if err != nil {
		t.Fatal(err)
	}
}

// With the flag off, a registered owner may rename their repository to a new
// name, as upstream allows.
func TestUsersRenameToNewNameWhenFlagOff(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContextWith(t, func(c *config.Config) { c.AdminOnlyCreate = false })
	userCtx := withUser(t, ctx, be, "worker", false)

	if err := runRepo(t, userCtx, "create", "mine"); err != nil {
		t.Fatal(err)
	}
	if err := runRepo(t, userCtx, "rename", "mine", "renamed"); err != nil {
		t.Fatal(err)
	}
	_, err := be.Repository(ctx, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "mine")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

func TestOnlyAdminsCreateRepositories(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContextWith(t, func(c *config.Config) { c.AdminOnlyCreate = true })
	adminCtx := withUser(t, ctx, be, "boss", true)
	userCtx := withUser(t, ctx, be, "worker", false)

	// Non-admin: create and import of a new name are refused, nothing is made.
	if !errors.Is(runRepo(t, userCtx, "create", "junk"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(runRepo(t, userCtx, \"create\", \"junk\"), proto.ErrUnauthorized)")
	}
	if !errors.Is(runRepo(t, userCtx, "create", "junk2", "-p"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(runRepo(t, userCtx, \"create\", \"junk2\", \"-p\"), proto.ErrUnauthorized)")
	}
	if !errors.Is(runRepo(t, userCtx, "import", "junk3", "https://example.invalid/x.git"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(runRepo(t, userCtx, \"import\", \"junk3\", \"https://example.invalid/x.git\"), proto.ErrUnauthorized)")
	}
	for _, n := range []string{"junk", "junk2", "junk3"} {
		_, err := be.Repository(ctx, n)
		if err == nil {
			t.Fatal("not true: err != nil")
		}
	}

	// Admin: create works.
	if err := runRepo(t, adminCtx, "create", "real", "-p"); err != nil {
		t.Fatal(err)
	}
	_, err := be.Repository(ctx, "real")
	if err != nil {
		t.Fatal(err)
	}

	// A read-write collaborator still works on an existing repo, but cannot rename it to a new name.
	if err := runRepo(t, adminCtx, "collab", "add", "real", "worker", "read-write"); err != nil {
		t.Fatal(err)
	}
	if err := runRepo(t, userCtx, "description", "real", "hello"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(runRepo(t, userCtx, "rename", "real", "elsewhere"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(runRepo(t, userCtx, \"rename\", \"real\", \"elsewhere\"), proto.ErrUnauthorized)")
	}
}

// A key added to initial_admin_keys after the first migration never reaches
// the database, so its user is not an admin there. It is still a server
// admin, and with the flag on it may create, import and rename to a new name
// end to end, not just pass the SSH gate.
func TestAdminKeyCreatesRepositoriesWhenFlagOn(t *testing.T) {
	t.Parallel()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pub := signer.PublicKey()

	ctx, be := newAuthTestContextWith(t, func(c *config.Config) {
		c.AdminOnlyCreate = true
		c.InitialAdminKeys = append(c.InitialAdminKeys, proto.MarshalAuthorizedKey(pub))
	})
	userCtx := withUser(t, ctx, be, "keyholder", false)
	keyCtx := context.WithValue(userCtx, ssh.ContextKeyPublicKey, pub)

	if !errors.Is(runRepo(t, userCtx, "create", "nokey"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(runRepo(t, userCtx, \"create\", \"nokey\"), proto.ErrUnauthorized)")
	}

	if err := runRepo(t, keyCtx, "create", "made"); err != nil {
		t.Fatal(err)
	}
	if err := runRepo(t, keyCtx, "rename", "made", "made-again"); err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "made-again")
	if err != nil {
		t.Fatal(err)
	}
	if errors.Is(runRepo(t, keyCtx, "import", "imported", "https://example.invalid/x.git"), proto.ErrUnauthorized) {
		t.Fatal("not true: !errors.Is(runRepo(t, keyCtx, \"import\", \"imported\", \"https://example.invalid/x.git\"), proto.ErrUnauthorized)")
	}
}

// A non-admin may not attach a key listed in initial_admin_keys to their
// own account, even with proof of holding it. The refusal says the key is
// listed, so a new admin knows to ask an admin. A key another account owns
// is still taken. A server admin may attach a listed key, and ordinary keys
// are unaffected.
func TestPubkeyAddRefusesAdminKeyForNonAdmin(t *testing.T) {
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
	adminPub := newKey()
	ctx, be := newAuthTestContext(t)
	// Added after the first migration, so no account owns it yet.
	cfg := cmdsOf(ctx).cfg
	cfg.InitialAdminKeys = append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(adminPub))
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	session := func(name string, admin bool) context.Context {
		pk := newKey()
		u, err := be.CreateUser(ctx, name, proto.UserOptions{Admin: admin, PublicKeys: []gossh.PublicKey{pk}})
		if err != nil {
			t.Fatal(err)
		}
		return context.WithValue(proto.WithUserContext(ctx, u), ssh.ContextKeyPublicKey, pk)
	}
	runPubkey := func(ctx context.Context, args ...string) error {
		c := cmdsOf(ctx).PubkeyCommand()
		c.SilenceUsage = true
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		c.SetArgs(args)
		return c.ExecuteContext(ctx)
	}

	workerCtx := session("worker", false)
	if !errors.Is(runPubkey(workerCtx, "add", proto.MarshalAuthorizedKey(adminPub)), errListedAdminKey) {
		t.Fatal("not true: errors.Is(runPubkey(workerCtx, \"add\", proto.MarshalAuthorizedKey(adminPub)), errListedAdminKey)")
	}
	taken := newEd25519Signer(t)
	_, err := be.CreateUser(ctx, "other", proto.UserOptions{PublicKeys: []gossh.PublicKey{taken.PublicKey()}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runPubkeyIn(workerCtx, proofFor(t, taken, "worker"), "add", proto.MarshalAuthorizedKey(taken.PublicKey()))
	if !errors.Is(err, errPubkeyTaken) {
		t.Fatal("not true: errors.Is(err, errPubkeyTaken)")
	}
	worker, err := be.User(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	keys := worker.PublicKeys()
	if got := len(keys); got != 1 {
		t.Fatalf("len(keys) = %v, want %v", got, 1)
	}
	fresh := newEd25519Signer(t)
	_, err = runPubkeyIn(workerCtx, proofFor(t, fresh, "worker"), "add", proto.MarshalAuthorizedKey(fresh.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}

	if err := runPubkey(session("boss", true), "add", proto.MarshalAuthorizedKey(adminPub)); err != nil {
		t.Fatal(err)
	}
}

// anonFailsAfterStore answers the first ok reads of the anon-access setting
// and fails every one after, as a database that goes down between the
// command's access gate and the create would.
type anonFailsAfterStore struct {
	store.Store
	ok    int32
	reads *atomic.Int32
}

func (s anonFailsAfterStore) GetAnonAccess(ctx context.Context, h db.Handler) (access.Level, error) {
	if s.reads.Add(1) > s.ok {
		return access.NoAccess, errors.New("SQL logic error: no such table: settings")
	}
	return s.Store.GetAnonAccess(ctx, h)
}

// A create, import, rename onto a new name or push-to-create whose creation
// check cannot read the settings tells the client the lookup failed, keeps
// the database's message from it, and logs the cause once at Error level.
func TestCreateLookupFailureAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(ctx context.Context) error
		want error
	}{
		{"repo create", func(ctx context.Context) error { return runRepo(t, ctx, "create", "new") }, errLookupFailed},
		{"repo import", func(ctx context.Context) error {
			return runRepo(t, ctx, "import", "new", "https://127.0.0.1/x.git")
		}, errLookupFailed},
		{"repo rename", func(ctx context.Context) error { return runRepo(t, ctx, "rename", "mine", "new") }, errLookupFailed},
		{"push-to-create", func(ctx context.Context) error { return runGit(ctx, (*Commands).GitReceivePackCommand, "new") }, gitserve.ErrSystemMalfunction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := &atomic.Int32{}
			// Fail from the second read on: the first is the command's own
			// access gate, the second the backend's creation check.
			ok := int32(1)
			if tc.name == "repo rename" {
				// rename reads it for the repository renamed too.
				ok = 2
			}
			ctx, be, logs := newLookupTestContextWith(t, func(s store.Store) store.Store {
				return anonFailsAfterStore{Store: s, ok: ok, reads: reads}
			})
			userCtx := withUser(t, ctx, be, "alice", false)
			_, err := be.CreateRepository(userCtx, "mine", proto.UserFromContext(userCtx), proto.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			reads.Store(0)

			err = tc.run(userCtx)
			if !errors.Is(err, tc.want) {
				t.Fatal("not true: errors.Is(err, tc.want)")
			}
			if strings.Contains(err.Error(), "settings") {
				t.Fatal("not true: !strings.Contains(err.Error(), \"settings\")")
			}
			if got := logs.count("ERRO"); got != 1 {
				t.Fatalf("logs.count(\"ERRO\") = %v, want %v", got, 1)
			}
			if !strings.Contains(logs.String(), "settings") {
				t.Fatal("not true: strings.Contains(logs.String(), \"settings\")")
			}
			_, err = be.Repository(ctx, "new")
			if !errors.Is(err, proto.ErrRepoNotFound) {
				t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
			}
		})
	}
}
