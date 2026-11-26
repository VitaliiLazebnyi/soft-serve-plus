package backend

import (
	"context"
	"errors"
	"testing"

	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

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
