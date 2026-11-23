package backend

import (
	"context"
	"errors"
	"testing"

	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
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
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	ownerKey := newTestPublicKey(t)
	collabKey := newTestPublicKey(t)
	otherKey := newTestPublicKey(t)
	adminKey := newTestPublicKey(t)
	listedKey := newTestPublicKey(t)
	setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(listedKey)))

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []ssh.PublicKey{ownerKey}})
	is.NoErr(err)
	collab, err := be.CreateUser(ctx, "collab", proto.UserOptions{PublicKeys: []ssh.PublicKey{collabKey}})
	is.NoErr(err)
	other, err := be.CreateUser(ctx, "other", proto.UserOptions{PublicKeys: []ssh.PublicKey{otherKey}})
	is.NoErr(err)
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{adminKey}})
	is.NoErr(err)

	_, err = be.CreateRepository(ctx, "public", owner, proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	// Straight to the store: AddCollaborator also sends a webhook event,
	// which needs a database on ctx.
	is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "private", access.ReadWriteAccess)
	}))

	for _, anon := range []access.AccessLevel{access.NoAccess, access.ReadOnlyAccess} {
		is.NoErr(be.SetAnonAccess(ctx, anon))
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
	is.True(r != nil)
	is.Equal(level, access.AdminAccess)
}

// expectReadable is the level TestReadableForUser expects caller to hold on
// repo, with anon at most read-only and admin_only_create off.
func expectReadable(repo, caller string, anon access.AccessLevel) access.AccessLevel {
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
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	pk := newTestPublicKey(t)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []ssh.PublicKey{pk}})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)

	sessCtx := proto.WithUserContext(context.WithValue(ctx, charmssh.ContextKeyPublicKey, pk), owner)
	is.NoErr(be.RemovePublicKey(ctx, "owner", pk))

	_, err = be.UserByPublicKey(ctx, pk)
	is.True(errors.Is(err, proto.ErrUserNotFound)) // the key no longer resolves to the owner

	r, level := noLookupError(t)(be.ReadableForUser(sessCtx, "private", proto.UserFromContext(sessCtx)))
	is.True(r != nil)
	is.Equal(level, access.AdminAccess)
}
