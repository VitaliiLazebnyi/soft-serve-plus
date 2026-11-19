package backend

import (
	"context"
	"testing"

	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

// BaseAccessLevelForUser takes the user an SSH session already resolved and
// gives the level for a name that does not exist: admin for an admin or an
// admin key on the context, even one no account owns; read-write for a
// registered user, read-only under admin_only_create; the anonymous level
// otherwise.
func TestBaseAccessLevelForUser(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	userKey := newTestPublicKey(t)
	adminKey := newTestPublicKey(t)
	listedKey := newTestPublicKey(t)
	setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, sshutils.MarshalAuthorizedKey(listedKey)))

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{userKey}})
	is.NoErr(err)
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{adminKey}})
	is.NoErr(err)

	for _, anon := range []access.AccessLevel{access.NoAccess, access.ReadOnlyAccess} {
		is.NoErr(be.SetAnonAccess(ctx, anon))
		for _, adminOnly := range []bool{false, true} {
			cfg.AdminOnlyCreate = adminOnly
			registered := access.ReadWriteAccess
			if adminOnly {
				registered = access.ReadOnlyAccess
			}
			for _, tc := range []struct {
				name string
				pk   ssh.PublicKey
				user proto.User
				want access.AccessLevel
			}{
				{"anonymous", nil, nil, anon},
				{"unknown key", newTestPublicKey(t), nil, anon},
				{"user", userKey, user, registered},
				{"admin", adminKey, admin, access.AdminAccess},
				{"unowned admin key", listedKey, nil, access.AdminAccess},
			} {
				keyCtx := ctx
				if tc.pk != nil {
					keyCtx = context.WithValue(ctx, charmssh.ContextKeyPublicKey, tc.pk)
				}
				want := tc.want
				got := baseLevel(t, be, keyCtx, tc.user)
				if got != want {
					t.Errorf("anon=%s adminOnly=%t %s: got %s, want %s", anon, adminOnly, tc.name, got, want)
				}
			}
		}
	}

	// The unowned admin key is recognised from the context alone.
	keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, listedKey)
	is.Equal(baseLevel(t, be, keyCtx, nil), access.AdminAccess)
}
