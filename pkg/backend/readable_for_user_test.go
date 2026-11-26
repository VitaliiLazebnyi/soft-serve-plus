package backend

import (
	"context"
	"errors"
	"testing"

	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// ReadableForUser takes the user an SSH session already resolved and gives
// each caller its level: admin for an admin or an admin key on the context,
// even one no account owns, and for the owner; the collaborator's level on
// the private repository; read-only for other registered users on the public
// one; the anonymous level for callers without an account; nothing on the
// private repository for anyone else. A missing name gives the level for a
// new name. The repository is returned when it exists and the level reads.
func TestReadableForUser(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	ownerKey := newTestPublicKey(t)
	collabKey := newTestPublicKey(t)
	otherKey := newTestPublicKey(t)
	adminKey := newTestPublicKey(t)
	listedKey := newTestPublicKey(t)
	setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(listedKey)))

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []ssh.PublicKey{ownerKey}})
	if err != nil {
		t.Fatal(err)
	}
	collab, err := be.CreateUser(ctx, "collab", proto.UserOptions{PublicKeys: []ssh.PublicKey{collabKey}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := be.CreateUser(ctx, "other", proto.UserOptions{PublicKeys: []ssh.PublicKey{otherKey}})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{adminKey}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(ctx, "public", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	// Straight to the store: AddCollaborator also sends a webhook event,
	// which needs a database on ctx.
	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "private", access.ReadWriteAccess)
	}); err != nil {
		t.Fatal(err)
	}

	for _, anon := range []access.Level{access.NoAccess, access.ReadOnlyAccess} {
		if err := be.SetAnonAccess(ctx, anon); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"public", "private", "missing"} {
			for _, tc := range []struct {
				name string
				pk   ssh.PublicKey
				user proto.User
			}{
				{"anonymous", nil, nil},
				{"unknown key", newTestPublicKey(t), nil},
				{"owner", ownerKey, owner},
				{"collaborator", collabKey, collab},
				{"other user", otherKey, other},
				{"admin", adminKey, admin},
				{"unowned admin key", listedKey, nil},
			} {
				keyCtx := ctx
				if tc.pk != nil {
					keyCtx = context.WithValue(ctx, charmssh.ContextKeyPublicKey, tc.pk)
				}
				wantLevel := expectReadable(name, tc.name, anon)
				wantRepo := name != "missing" && wantLevel >= access.ReadOnlyAccess
				gotRepo, gotLevel := noLookupError(t)(be.ReadableForUser(keyCtx, name, tc.user))
				if gotLevel != wantLevel || (gotRepo != nil) != wantRepo {
					t.Errorf("anon=%s repo=%s %s: got (%v, %s), want (%v, %s)",
						anon, name, tc.name, gotRepo != nil, gotLevel, wantRepo, wantLevel)
				}
			}
		}
	}

	// The unowned admin key opens a private repository from the context alone.
	keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, listedKey)
	r, level := noLookupError(t)(be.ReadableForUser(keyCtx, "private", nil))
	if r == nil {
		t.Fatal("not true: r != nil")
	}
	if got := level; got != access.AdminAccess {
		t.Fatalf("level = %v, want %v", got, access.AdminAccess)
	}
}

// expectReadable is the level TestReadableForUser expects caller to hold on
// repo, with anon at most read-only and admin_only_create off.
func expectReadable(repo, caller string, anon access.Level) access.Level {
	switch {
	case caller == "admin" || caller == "unowned admin key":
		return access.AdminAccess
	case caller == "anonymous" || caller == "unknown key":
		if repo == "private" {
			return access.NoAccess
		}
		return anon
	case repo == "missing":
		return access.ReadWriteAccess
	case caller == "owner":
		return access.AdminAccess
	case repo == "private" && caller == "collaborator":
		return access.ReadWriteAccess
	case repo == "private":
		return access.NoAccess
	}
	return access.ReadOnlyAccess
}

// ReadableForUser uses the user it is given and does not resolve the key
// again: once the owner's key no longer maps to the account, a by-key
// lookup loses the private repository, but the session user keeps it.
func TestReadableForUserDoesNotLookUpKey(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	pk := newTestPublicKey(t)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []ssh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}

	sessCtx := proto.WithUserContext(context.WithValue(ctx, charmssh.ContextKeyPublicKey, pk), owner)
	if err := be.RemovePublicKey(ctx, "owner", pk); err != nil {
		t.Fatal(err)
	}

	_, err = be.UserByPublicKey(ctx, pk)
	if !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
	} // the key no longer resolves to the owner

	r, level := noLookupError(t)(be.ReadableForUser(sessCtx, "private", proto.UserFromContext(sessCtx)))
	if r == nil {
		t.Fatal("not true: r != nil")
	}
	if got := level; got != access.AdminAccess {
		t.Fatalf("level = %v, want %v", got, access.AdminAccess)
	}
}
