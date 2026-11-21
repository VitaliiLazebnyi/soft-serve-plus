package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

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
			is := is.New(t)
			ctx, be := newAuthTestContext(t)
			aliceCtx := withUser(t, ctx, be, "alice", false)
			bobCtx := withUser(t, ctx, be, "bob", false)
			for _, u := range []string{"carol", "dave"} {
				_, err := be.CreateUser(ctx, u, proto.UserOptions{})
				is.NoErr(err)
			}
			_, err := be.CreateRepository(aliceCtx, "r", proto.UserFromContext(aliceCtx), proto.RepositoryOptions{})
			is.NoErr(err)
			is.NoErr(be.AddCollaborator(aliceCtx, "r", "dave", access.ReadOnlyAccess))

			afterGate(ctx, func() {
				is.NoErr(be.RenameRepository(aliceCtx, "r", "r-old"))
				_, err := be.CreateRepository(bobCtx, "r", proto.UserFromContext(bobCtx), proto.RepositoryOptions{})
				is.NoErr(err)
				is.NoErr(be.AddCollaborator(bobCtx, "r", "dave", access.ReadOnlyAccess))
			})
			err = runRepo(t, aliceCtx, tc.args...)
			if !errors.Is(err, proto.ErrRepoNotFound) {
				t.Errorf("err = %v, want %v", err, proto.ErrRepoNotFound)
			}

			other, err := be.Repository(ctx, "r")
			is.NoErr(err) // bob's repository is still there
			is.Equal(other.UserID(), proto.UserFromContext(bobCtx).ID())
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
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be) // the session resolved "owner"
	id := whCreate(t, ownerCtx, be, "-e", "collaborator")
	whPointAtLoopback(t, ownerCtx, "hooks", id)
	_, err := be.CreateUser(ctx, "carol", proto.UserOptions{})
	is.NoErr(err)

	is.NoErr(be.SetUsername(ctx, "owner", "renamed"))
	is.NoErr(runRepo(t, ownerCtx, "collab", "add", "hooks", "carol"))
	whAwaitDeliveries(t, ownerCtx, be, "hooks", id, 1)
	is.NoErr(runRepo(t, ownerCtx, "collab", "remove", "hooks", "carol"))
	whAwaitDeliveries(t, ownerCtx, be, "hooks", id, 2)

	be.WaitForDeliveries(context.Background())
	r, err := be.Repository(ctx, "hooks")
	is.NoErr(err)
	ds, err := be.ListWebhookDeliveries(ctx, r, id)
	is.NoErr(err)
	is.Equal(len(ds), 2)
	for _, d := range ds {
		full, err := be.WebhookDelivery(ctx, r, id, d.ID)
		is.NoErr(err)
		var payload struct {
			Action string `json:"action"`
			Sender struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"sender"`
		}
		is.NoErr(json.Unmarshal([]byte(full.RequestBody), &payload))
		is.Equal(payload.Sender.ID, proto.UserFromContext(ownerCtx).ID())
		if payload.Sender.Username != "renamed" {
			t.Errorf("collaborator %s event: sender %q, want %q", payload.Action, payload.Sender.Username, "renamed")
		}
	}
}
