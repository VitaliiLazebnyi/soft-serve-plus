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

// BaseAccessLevelForUser takes the user an SSH session already resolved
// and must give the same level BaseAccessLevelByPublicKey gives for that
// user's key, including for an unowned admin key on the context.
func TestBaseAccessLevelForUserMatchesByPublicKey(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	userKey := newTestPublicKey(t)
	adminKey := newTestPublicKey(t)
	listedKey := newTestPublicKey(t)
	cfg.InitialAdminKeys = append(cfg.InitialAdminKeys, sshutils.MarshalAuthorizedKey(listedKey))

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{userKey}})
	is.NoErr(err)
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{adminKey}})
	is.NoErr(err)

	for _, anon := range []access.AccessLevel{access.NoAccess, access.ReadOnlyAccess} {
		is.NoErr(be.SetAnonAccess(ctx, anon))
		for _, adminOnly := range []bool{false, true} {
			cfg.AdminOnlyCreate = adminOnly
			for _, tc := range []struct {
				name string
				pk   ssh.PublicKey
				user proto.User
			}{
				{"anonymous", nil, nil},
				{"unknown key", newTestPublicKey(t), nil},
				{"user", userKey, user},
				{"admin", adminKey, admin},
				{"unowned admin key", listedKey, nil},
			} {
				keyCtx := ctx
				if tc.pk != nil {
					keyCtx = context.WithValue(ctx, charmssh.ContextKeyPublicKey, tc.pk)
				}
				want := be.BaseAccessLevelByPublicKey(keyCtx, tc.pk)
				got := be.BaseAccessLevelForUser(keyCtx, tc.user)
				if got != want {
					t.Errorf("anon=%s adminOnly=%t %s: got %s, want %s", anon, adminOnly, tc.name, got, want)
				}
			}
		}
	}

	// The unowned admin key is recognised from the context alone.
	keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, listedKey)
	is.Equal(be.BaseAccessLevelForUser(keyCtx, nil), access.AdminAccess)
}
