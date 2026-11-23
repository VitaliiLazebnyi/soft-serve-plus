package backend

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// With AdminOnlyCreate on, a missing name is read-only for non-admins and
// anonymous users, so nothing can be created there.
func TestAdminOnlyCreateFlagOnCapsBaseAccess(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	cfg.AdminOnlyCreate = true

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}

	if got := levelOf(t, be, ctx, "fresh", user); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.ReadOnlyAccess)
	}
	if got := levelOf(t, be, ctx, "fresh", admin); got != access.AdminAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", admin) = %v, want %v", got, access.AdminAccess)
	}
	// Anonymous access is capped too: even when the server allows read-write
	// for anonymous users, a missing name is read-only.
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw
	if got := levelOf(t, be, ctx, "fresh", nil); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", nil) = %v, want %v", got, access.ReadOnlyAccess)
	}
}

// With AdminOnlyCreate off (the default), the backend keeps upstream
// behaviour: a registered user gets read-write on a missing name, so can
// create it.
func TestAdminOnlyCreateFlagOffKeepsUpstreamBaseAccess(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	if cfg.AdminOnlyCreate {
		t.Fatal("not true: !cfg.AdminOnlyCreate")
	}
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if got := levelOf(t, be, ctx, "fresh", user); got != access.ReadWriteAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.ReadWriteAccess)
	}
	if got := levelOf(t, be, ctx, "fresh", nil); got != access.ReadWriteAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", nil) = %v, want %v", got, access.ReadWriteAccess)
	}
}

// A database admin keeps admin access on a missing name whatever the flag.
func TestDatabaseAdminBaseAccessIgnoresFlag(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	pub := newTestPublicKey(t)
	root, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{pub}})
	if err != nil {
		t.Fatal(err)
	}

	cfg.AdminOnlyCreate = true
	if got := baseLevel(t, be, ctx, root); got != access.AdminAccess {
		t.Fatalf("baseLevel(t, be, ctx, root) = %v, want %v", got, access.AdminAccess)
	}
	cfg.AdminOnlyCreate = false
	if got := baseLevel(t, be, ctx, root); got != access.AdminAccess {
		t.Fatalf("baseLevel(t, be, ctx, root) = %v, want %v", got, access.AdminAccess)
	}
}

// With the flag off, a registered user on a missing name gets the anonymous
// level when that is higher than read-write (upstream behaviour), and the
// flag on caps it at read-only.
func TestAnonAdminLevelIsCappedOnlyWhenFlagOn(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	anonAdmin := access.AdminAccess
	cfg.AnonAccess = &anonAdmin

	cfg.AdminOnlyCreate = false
	if got := levelOf(t, be, ctx, "fresh", user); got != access.AdminAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.AdminAccess)
	}

	cfg.AdminOnlyCreate = true
	if got := levelOf(t, be, ctx, "fresh", user); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.ReadOnlyAccess)
	}
}

// backendTestContext wires the config, database and store a backend call
// reads from its context (webhook delivery and rename need them).
func backendTestContext(be *Backend, cfg *config.Config) context.Context {
	ctx := context.Background()
	return ctx
}

// defaultAdmin returns the admin account the first migration creates. Tests
// pass it as the creator of fixture repositories, or put it on the context
// of a rename: an anonymous caller may not create under the default
// anon-access, and the backend refuses a create that the caller may not
// make (see checkCanCreate).
func defaultAdmin(t testing.TB, be *Backend) proto.User {
	t.Helper()
	u, err := be.User(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// asAdmin returns ctx acting as defaultAdmin.
func asAdmin(t testing.TB, be *Backend, ctx context.Context) context.Context {
	t.Helper()
	return proto.WithUserContext(ctx, defaultAdmin(t, be))
}

// seedCreate creates a fixture repository while admin-only creation is off,
// so tests can set up owners without the create policy in the way. Tests that
// exercise the policy call be.CreateRepository directly.
func seedCreate(be *Backend, ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions) (proto.Repository, error) {
	prev := be.cfg.AdminOnlyCreate
	be.cfg.AdminOnlyCreate = false
	defer func() { be.cfg.AdminOnlyCreate = prev }()
	return be.CreateRepository(ctx, name, user, opts)
}

// offlineRemote is the remote the import checks below use. The tests only
// care whether the create policy lets the import through; a loopback literal
// is then refused by the remote check without a DNS lookup or a dial, so
// they run without network access.
const offlineRemote = "https://127.0.0.1/x.git"

// The backend's own creation guard is the whole rule, not only its
// admin_only_create half: with that setting off, an anonymous caller may not
// create, import or rename onto a new name unless anon-access is read-write
// or above, as the SSH and HTTP handlers already require. An entry point that
// skips the handlers' checks cannot let anonymous users create.
func TestBackendRefusesAnonymousCreatesUnderReadOnlyAnonAccess(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	if cfg.AdminOnlyCreate {
		t.Fatal("not true: !cfg.AdminOnlyCreate")
	}
	if got := be.AnonAccess(ctx); got != access.ReadOnlyAccess {
		t.Fatalf("be.AnonAccess(ctx) = %v, want %v", got, access.ReadOnlyAccess)
	}

	_, err := be.CreateRepository(ctx, "a", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(ctx, "anon", nil, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.ImportRepository(ctx, "anon", nil, offlineRemote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	if !errors.Is(be.RenameRepository(ctx, "a", "anon"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(be.RenameRepository(ctx, \"a\", \"anon\"), proto.ErrUnauthorized)")
	}
	_, err = be.Repository(ctx, "anon")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	_, err = be.Repository(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}

	// With anon-access read-write, anonymous callers may create.
	if err := be.SetAnonAccess(ctx, access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "anon", nil, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

// With AdminOnlyCreate on, the backend itself refuses a non-admin, and an
// anonymous caller, creating, importing or renaming to a new name, whichever
// caller reaches it. Nothing is created.
func TestBackendRefusesNonAdminCreatesWhenFlagOn(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	userCtx := proto.WithUserContext(ctx, user)

	_, err = be.CreateRepository(userCtx, "junk", user, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.CreateRepository(ctx, "anon-junk", nil, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.ImportRepository(userCtx, "junk-import", user, offlineRemote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}

	_, err = be.Repository(ctx, "junk")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = be.Repository(ctx, "anon-junk")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = be.Repository(ctx, "junk-import")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// With AdminOnlyCreate on, a non-admin's rename of an owned repository to a
// new name is refused and the repository stays where it was.
func TestBackendRefusesNonAdminRenameToNewNameWhenFlagOn(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = seedCreate(be, ctx, "mine", user, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	userCtx := proto.WithUserContext(ctx, user)
	if !errors.Is(be.RenameRepository(userCtx, "mine", "renamed"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(be.RenameRepository(userCtx, \"mine\", \"renamed\"), proto.ErrUnauthorized)")
	}
	_, err = be.Repository(ctx, "mine")
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "renamed")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// With AdminOnlyCreate on, a database admin may create, import and rename to
// a new name. The import may still fail for an unrelated reason (for example,
// a git version gate), so it only has to pass the policy.
func TestBackendAllowsAdminCreatesWhenFlagOn(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := proto.WithUserContext(ctx, admin)

	_, err = be.CreateRepository(adminCtx, "made", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.ImportRepository(adminCtx, "imported", admin, offlineRemote, proto.RepositoryOptions{})
	if errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: !errors.Is(err, proto.ErrUnauthorized)")
	}
	if err := be.RenameRepository(adminCtx, "made", "made-again"); err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "made-again")
	if err != nil {
		t.Fatal(err)
	}
}

// With AdminOnlyCreate off, the same non-admin calls are not refused by the
// policy: create and rename to a new name succeed.
func TestBackendAllowsNonAdminCreatesWhenFlagOff(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = false

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	userCtx := proto.WithUserContext(ctx, user)

	_, err = be.CreateRepository(userCtx, "mine", user, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.RenameRepository(userCtx, "mine", "renamed"); err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.ImportRepository(userCtx, "imported", user, offlineRemote, proto.RepositoryOptions{})
	if errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: !errors.Is(err, proto.ErrUnauthorized)")
	}
}

func newTestPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// A key listed in initial_admin_keys is a server admin even when it was added
// after the first migration, so it never reached the database: the caller is
// anonymous or a non-admin account. With AdminOnlyCreate on, the backend must
// still let it create, import and rename to a new name: IsServerAdmin honours
// the key.
func TestBackendAllowsAdminKeyCreatesWhenFlagOn(t *testing.T) {
	t.Parallel()
	for _, registered := range []bool{false, true} {
		name := "anonymous"
		if registered {
			name = "non-admin user"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			be, cfg := newTestBackend(t)
			ctx := backendTestContext(be, cfg)
			cfg.AdminOnlyCreate = true

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
				ctx = proto.WithUserContext(ctx, user)
			}
			keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, pub)

			// Without the key on the context, the same user is refused.
			_, err := be.CreateRepository(ctx, "nokey", user, proto.RepositoryOptions{})
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
			}

			// ReadableForUser honours the key; Readable, for the account
			// alone, does not, on a missing name as on an existing one.
			_, level := noLookupError(t)(be.ReadableForUser(keyCtx, "fresh", user))
			if got := level; got != access.AdminAccess {
				t.Fatalf("level = %v, want %v", got, access.AdminAccess)
			}
			if levelOf(t, be, keyCtx, "fresh", user) >= access.ReadWriteAccess {
				t.Fatal("not true: levelOf(t, be, keyCtx, \"fresh\", user) < access.ReadWriteAccess")
			}

			_, err = be.CreateRepository(keyCtx, "made", user, proto.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = be.ImportRepository(keyCtx, "imported", user, offlineRemote, proto.RepositoryOptions{})
			if errors.Is(err, proto.ErrUnauthorized) {
				t.Fatal("not true: !errors.Is(err, proto.ErrUnauthorized)")
			}
			if !registered {
				// The rename webhook needs a sender, so rename is only
				// exercised with a registered user.
				return
			}
			if err := be.RenameRepository(keyCtx, "made", "made-again"); err != nil {
				t.Fatal(err)
			}
			_, err = be.Repository(ctx, "made-again")
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Over HTTP (token, password or JWT) there is no public key on the context,
// so the only server-admin signal is the account's admin flag. Owning a key
// listed in initial_admin_keys must not count: `pubkey add` attaches a key
// without proof of holding its private half, so a user could claim a key
// before an operator lists it and become an admin over HTTP.
func TestHTTPAdminOnlyByAdminFlag(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	claimed := newTestPublicKey(t)
	_, err := be.CreateUser(ctx, "mallory", proto.UserOptions{PublicKeys: []ssh.PublicKey{claimed}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "boss", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{newTestPublicKey(t)}})
	if err != nil {
		t.Fatal(err)
	}

	// The operator lists the key after mallory attached it.
	setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(claimed)))

	// Load the users the way the HTTP layer does: by name (password, JWT)
	// and by access token.
	load := func(name string) []proto.User {
		u, err := be.User(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := be.CreateAccessToken(ctx, u, "t", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		byToken, err := be.UserByAccessToken(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		return []proto.User{u, byToken}
	}

	for i, u := range load("mallory") {
		if u.IsAdmin() {
			t.Fatal("not true: !u.IsAdmin()")
		}
		if got := levelOf(t, be, ctx, "fresh", u); got != access.ReadOnlyAccess {
			t.Fatalf("levelOf(t, be, ctx, \"fresh\", u) = %v, want %v", got, access.ReadOnlyAccess)
		}
		_, err := be.CreateRepository(ctx, fmt.Sprintf("mallory-%d", i), u, proto.RepositoryOptions{})
		if !errors.Is(err, proto.ErrUnauthorized) {
			t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
		}
	}
	for i, u := range load("boss") {
		if got := levelOf(t, be, ctx, "fresh", u); got != access.AdminAccess {
			t.Fatalf("levelOf(t, be, ctx, \"fresh\", u) = %v, want %v", got, access.AdminAccess)
		}
		_, err := be.CreateRepository(ctx, fmt.Sprintf("boss-%d", i), u, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A key listed in initial_admin_keys that a non-admin account owns does not
// resolve to that account: the owner may have claimed it with `pubkey add`
// before it was listed. The lookup returns AdminKeyNonAdminOwnerError naming
// the account (the SSH server refuses the session with it), and the warning
// is logged on every such lookup. A listed key owned by an admin account
// resolves to it, and so does the owner once promoted or once the key is
// unlisted.
func TestListedAdminKeyOwnedByNonAdminHasNoAccount(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx := backendTestContext(be, cfg)

	claimed := newTestPublicKey(t)
	_, err := be.CreateUser(ctx, "mallory", proto.UserOptions{PublicKeys: []ssh.PublicKey{claimed}})
	if err != nil {
		t.Fatal(err)
	}
	bossKey := newTestPublicKey(t)
	_, err = be.CreateUser(ctx, "boss", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{bossKey}})
	if err != nil {
		t.Fatal(err)
	}

	u, err := be.UserByPublicKey(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "mallory" {
		t.Fatalf("u.Username() = %v, want %v", got, "mallory")
	}

	orig := cfg.InitialAdminKeys
	setAdminKeys(t, cfg, append(append([]string{}, orig...),
		proto.MarshalAuthorizedKey(claimed), proto.MarshalAuthorizedKey(bossKey)))

	for range 3 {
		_, err = be.UserByPublicKey(ctx, claimed)
		if !errors.Is(err, proto.ErrUserNotFound) {
			t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
		}
		var owned *AdminKeyNonAdminOwnerError
		if !errors.As(err, &owned) {
			t.Fatal("not true: errors.As(err, &owned)")
		}
		if got := owned.Username; got != "mallory" {
			t.Fatalf("owned.Username = %v, want %v", got, "mallory")
		}
		if !strings.Contains(err.Error(), "user set-admin mallory true") {
			t.Fatal("not true: strings.Contains(err.Error(), \"user set-admin mallory true\")")
		}
		if !strings.Contains(err.Error(), "user remove-pubkey mallory KEY") {
			t.Fatal("not true: strings.Contains(err.Error(), \"user remove-pubkey mallory KEY\")")
		}
	}
	if got := strings.Count(logs.String(), "admin key is owned by a non-admin account"); got != 3 {
		t.Fatalf("strings.Count(logs.String(), \"admin key is owned by a non-admin account\") = %v, want %v", got, 3)
	}
	if got := baseLevel(t, be, context.WithValue(ctx, charmssh.ContextKeyPublicKey, claimed), nil); got != access.AdminAccess {
		t.Fatalf("baseLevel(t, be, context.WithValue(ctx, charmssh.ContextKeyPublicKey, claimed), nil) = %v, want %v", got, access.AdminAccess)
	}

	// A private repository made with the key goes to the default admin,
	// and mallory gets nothing on it.
	r, err := be.CreateRepository(context.WithValue(ctx, charmssh.ContextKeyPublicKey, claimed), "secret", nil, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	mallory, err := be.User(ctx, "mallory")
	if err != nil {
		t.Fatal(err)
	}
	if r.UserID() == mallory.ID() {
		t.Fatal("not true: r.UserID() != mallory.ID()")
	}
	if got := levelOf(t, be, ctx, "secret", mallory); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"secret\", mallory) = %v, want %v", got, access.NoAccess)
	}

	u, err = be.UserByPublicKey(ctx, bossKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "boss" {
		t.Fatalf("u.Username() = %v, want %v", got, "boss")
	}

	setAdminKeys(t, cfg, orig)
	u, err = be.UserByPublicKey(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "mallory" {
		t.Fatalf("u.Username() = %v, want %v", got, "mallory")
	}

	// Promoting the owner (the fix when the key is theirs) resolves the
	// listed key to them again, keeping their account.
	setAdminKeys(t, cfg, append(append([]string{}, orig...), proto.MarshalAuthorizedKey(claimed)))
	if err := be.SetAdmin(ctx, "mallory", true); err != nil {
		t.Fatal(err)
	}
	u, err = be.UserByPublicKey(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "mallory" {
		t.Fatalf("u.Username() = %v, want %v", got, "mallory")
	}
	if !u.IsAdmin() {
		t.Fatal("not true: u.IsAdmin()")
	}
}

// The owned-admin-key warning is logged on every refused lookup, naming the
// current owner, and not once the conflict is resolved: the owner promoted,
// the key gone from the account, or the key no longer listed.
func TestOwnedAdminKeyWarningFollowsTheConflict(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx := backendTestContext(be, cfg)
	warnings := func(owner string) int {
		n := 0
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "admin key is owned by a non-admin account") && strings.Contains(line, owner) {
				n++
			}
		}
		return n
	}

	key := newTestPublicKey(t)
	_, err := be.CreateUser(ctx, "mallory", proto.UserOptions{PublicKeys: []ssh.PublicKey{key}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "eve", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	orig := cfg.InitialAdminKeys
	listed := append(append([]string{}, orig...), proto.MarshalAuthorizedKey(key))
	setAdminKeys(t, cfg, listed)

	lookup := func(owner string) {
		t.Helper()
		for range 2 {
			_, err := be.UserByPublicKey(ctx, key)
			var owned *AdminKeyNonAdminOwnerError
			if !errors.As(err, &owned) {
				t.Fatal("not true: errors.As(err, &owned)")
			}
			if got := owned.Username; got != owner {
				t.Fatalf("owned.Username = %v, want %v", got, owner)
			}
		}
	}

	lookup("mallory")
	if got := warnings("mallory"); got != 2 {
		t.Fatalf("warnings(\"mallory\") = %v, want %v", got, 2)
	}

	// Promoting the owner resolves it; demoting them again brings it back.
	if err := be.SetAdmin(ctx, "mallory", true); err != nil {
		t.Fatal(err)
	}
	u, err := be.UserByPublicKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "mallory" {
		t.Fatalf("u.Username() = %v, want %v", got, "mallory")
	}
	if got := warnings("mallory"); got != 2 {
		t.Fatalf("warnings(\"mallory\") = %v, want %v", got, 2)
	}
	if err := be.SetAdmin(ctx, "mallory", false); err != nil {
		t.Fatal(err)
	}
	lookup("mallory")
	if got := warnings("mallory"); got != 4 {
		t.Fatalf("warnings(\"mallory\") = %v, want %v", got, 4)
	}

	// The key leaving the account resolves it; another non-admin claiming
	// it is logged, naming the new owner.
	if err := be.RemovePublicKey(ctx, "mallory", key); err != nil {
		t.Fatal(err)
	}
	_, err = be.UserByPublicKey(ctx, key)
	if !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
	}
	if err := be.AddPublicKey(ctx, "eve", key); err != nil {
		t.Fatal(err)
	}
	lookup("eve")
	if got := warnings("eve"); got != 2 {
		t.Fatalf("warnings(\"eve\") = %v, want %v", got, 2)
	}

	// Unlisting the key resolves it too.
	setAdminKeys(t, cfg, orig)
	u, err = be.UserByPublicKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "eve" {
		t.Fatalf("u.Username() = %v, want %v", got, "eve")
	}
	if got := warnings("eve"); got != 2 {
		t.Fatalf("warnings(\"eve\") = %v, want %v", got, 2)
	}
	if got := warnings("mallory"); got != 4 {
		t.Fatalf("warnings(\"mallory\") = %v, want %v", got, 4)
	}
}
