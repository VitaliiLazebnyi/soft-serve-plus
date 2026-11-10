package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestGlobalCommandsIgnoreRepositoryAccess is the regression test for the
// privilege escalation where a non-admin could run global admin commands by
// creating a repository named after the command's first argument. `user
// set-admin victim true` used to be authorized by the caller's admin access
// to a repository they happened to own called "victim".
func TestGlobalCommandsIgnoreRepositoryAccess(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	attackerCtx := withUser(t, ctx, be, "attacker", false)

	// The attacker owns a repository named "victim", so they have
	// AdminAccess *to that repository*.
	attacker := proto.UserFromContext(attackerCtx)
	_, err := be.CreateRepository(attackerCtx, "victim", attacker, proto.RepositoryOptions{})
	is.NoErr(err)
	is.Equal(be.AccessLevelForUser(attackerCtx, "victim", attacker), access.AdminAccess)

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
	is.NoErr(err)
	is.Equal(reloaded.IsAdmin(), false)
}

// TestGlobalCommandsIgnoreAnonAdminAccess covers the variant that needs no
// repository at all: with anon-access set to admin-access,
// AccessLevelForUser returns AdminAccess to any authenticated user for a
// nonexistent repository name. That only holds with admin_only_create off
// (the default), so the flag is forced off here; with it on the missing name
// is capped at read-only and set-admin must still be refused.
func TestGlobalCommandsIgnoreAnonAdminAccess(t *testing.T) {
	for _, tt := range []struct {
		adminOnly bool
		missing   access.AccessLevel
	}{
		{adminOnly: false, missing: access.AdminAccess},
		{adminOnly: true, missing: access.ReadOnlyAccess},
	} {
		t.Run(fmt.Sprintf("admin_only_create=%t", tt.adminOnly), func(t *testing.T) {
			is := is.New(t)
			ctx, be := newAuthTestContextWith(t, func(c *config.Config) { c.AdminOnlyCreate = tt.adminOnly })
			is.NoErr(be.SetAnonAccess(ctx, access.AdminAccess))

			attackerCtx := withUser(t, ctx, be, "attacker", false)
			attacker := proto.UserFromContext(attackerCtx)
			is.Equal(be.AccessLevelForUser(attackerCtx, "nonexistent", attacker), tt.missing)

			err := runUser(t, attackerCtx, "set-admin", "attacker", "true")
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Fatalf("expected ErrUnauthorized, got %v", err)
			}

			reloaded, err := be.User(ctx, "attacker")
			is.NoErr(err)
			is.Equal(reloaded.IsAdmin(), false)
		})
	}
}

// TestUserSubcommandsAreGatedByParent verifies the gate lives on the `user`
// parent command, so subcommands cannot be left unprotected by omission.
func TestUserSubcommandsAreGatedByParent(t *testing.T) {
	is := is.New(t)
	c := UserCommand()
	is.True(c.PersistentPreRunE != nil)

	for _, sub := range c.Commands() {
		if sub.PersistentPreRunE != nil {
			t.Errorf("subcommand %q sets its own PersistentPreRunE; the parent gate is authoritative", sub.Name())
		}
	}
}

// TestSettingsSubcommandsAreGatedByParent is the settings counterpart.
func TestSettingsSubcommandsAreGatedByParent(t *testing.T) {
	is := is.New(t)
	c := SettingsCommand()
	is.True(c.PersistentPreRunE != nil)

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
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	is.NoErr(be.SetAnonAccess(ctx, access.NoAccess))

	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)

	writerCtx := withUser(t, ctx, be, "writer", false)
	victimCtx := withUser(t, ctx, be, "victim", false)
	_ = withUser(t, ctx, be, "peer", false)
	is.NoErr(be.AddCollaborator(ownerCtx, "repo", "writer", access.ReadWriteAccess))
	is.NoErr(be.AddCollaborator(ownerCtx, "repo", "peer", access.ReadWriteAccess))
	victim := proto.UserFromContext(victimCtx)
	is.Equal(be.AccessLevelForUser(victimCtx, "repo", victim), access.ReadOnlyAccess)

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
	_, isCollab, _ := be.IsCollaborator(ctx, "repo", "victim")
	is.Equal(isCollab, false)
	is.Equal(be.AccessLevelForUser(victimCtx, "repo", victim), access.ReadOnlyAccess)
	level, isCollab, err := be.IsCollaborator(ctx, "repo", "peer")
	is.NoErr(err)
	is.True(isCollab)
	is.Equal(level, access.ReadWriteAccess)

	// A read-write collaborator can still list collaborators.
	is.NoErr(runRepo(t, writerCtx, "collab", "list", "repo"))
}

// TestCollabManagementByRepoAdmin verifies an admin-access collaborator, who
// is not the owner or a server admin, can add, remove and re-add
// collaborators at any level.
func TestCollabManagementByRepoAdmin(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)

	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)

	adminCtx := withUser(t, ctx, be, "maintainer", false)
	_ = withUser(t, ctx, be, "peer", false)
	is.NoErr(be.AddCollaborator(ownerCtx, "repo", "maintainer", access.AdminAccess))

	is.NoErr(runRepo(t, adminCtx, "collab", "add", "repo", "peer", "admin-access"))
	level, isCollab, err := be.IsCollaborator(ctx, "repo", "peer")
	is.NoErr(err)
	is.True(isCollab)
	is.Equal(level, access.AdminAccess)

	is.NoErr(runRepo(t, adminCtx, "collab", "remove", "repo", "peer"))
	is.NoErr(runRepo(t, adminCtx, "collab", "add", "repo", "peer", "no-access"))
	level, isCollab, err = be.IsCollaborator(ctx, "repo", "peer")
	is.NoErr(err)
	is.True(isCollab)
	is.Equal(level, access.NoAccess)
}

// TestCollabManagementRefusesAnonymous verifies that anon-access set to
// admin-access does not let an anonymous caller manage collaborators.
func TestCollabManagementRefusesAnonymous(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)

	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)
	_ = withUser(t, ctx, be, "peer", false)
	is.NoErr(be.AddCollaborator(ownerCtx, "repo", "peer", access.ReadWriteAccess))
	is.NoErr(be.SetAnonAccess(ctx, access.AdminAccess))

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
	is := is.New(t)
	ctx, be := newAuthTestContext(t)

	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)
	_ = withUser(t, ctx, be, "alice", false)
	_ = withUser(t, ctx, be, "bob", false)
	is.NoErr(be.AddCollaborator(ownerCtx, "repo", "alice", access.AdminAccess))
	is.NoErr(be.AddCollaborator(ownerCtx, "repo", "bob", access.NoAccess))

	out, _, err := runRepoOutput(t, ownerCtx, "collab", "list", "repo")
	is.NoErr(err)
	is.True(strings.Contains(out, "alice\tadmin-access\n"))
	is.True(strings.Contains(out, "bob\tno-access\n"))
}
