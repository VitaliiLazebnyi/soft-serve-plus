package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/matryer/is"
)

// TestDeleteRefAtMalformedCommit deletes a branch and a tag that point at a
// commit git-module cannot parse. Reading that commit used to panic before
// the delete; the delete needs only the ref's ID, so it succeeds and still
// sends its event.
func TestDeleteRefAtMalformedCommit(t *testing.T) {
	for _, kind := range gittest.Malformed {
		t.Run(string(kind), func(t *testing.T) {
			is := is.New(t)
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			id := whCreate(t, ownerCtx, be, "-e", "branch_tag_delete")
			whPointAtLoopback(t, ownerCtx, "hooks", id)
			bare := filepath.Join(config.FromContext(ownerCtx).DataPath, "repos", "hooks.git")

			root := gittest.Commit(t, bare, gittest.Valid, gittest.Tree(t, bare, "f", "root\n"), "root")
			bad := gittest.Commit(t, bare, kind, gittest.Tree(t, bare, "f", "bad\n"), "bad", root)
			gittest.Git(t, bare, nil, "update-ref", "refs/heads/main", root)
			gittest.Git(t, bare, nil, "update-ref", "refs/heads/bad", bad)
			gittest.Git(t, bare, nil, "update-ref", "refs/tags/bad", bad)

			is.NoErr(runRepo(t, ownerCtx, "branch", "delete", "hooks", "bad"))
			is.True(!gitRefExists(t, bare, "refs/heads/bad"))
			// The branch delete's delivery is recorded in the background.
			// Let it finish first: on SQLite without WAL the tag delete's
			// repository read waits out that commit, and one longer than
			// busy_timeout reads as "repository not found".
			// WaitForDeliveries would refuse the tag delete's delivery, so
			// wait for the row instead.
			whAwaitDeliveries(t, ownerCtx, be, "hooks", id, 1)
			is.NoErr(runRepo(t, ownerCtx, "tag", "delete", "hooks", "bad"))
			is.True(!gitRefExists(t, bare, "refs/tags/bad"))

			be.WaitForDeliveries(context.Background())
			r, err := be.Repository(ownerCtx, "hooks")
			is.NoErr(err)
			ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
			is.NoErr(err)
			is.Equal(len(ds), 2)
			refs := map[string]bool{}
			for _, d := range ds {
				is.Equal(d.Event.String(), "branch_tag_delete")
				full, err := be.WebhookDelivery(ownerCtx, r, id, d.ID)
				is.NoErr(err)
				var payload struct {
					Ref    string `json:"ref"`
					Before string `json:"before"`
				}
				is.NoErr(json.Unmarshal([]byte(full.RequestBody), &payload))
				is.Equal(payload.Before, bad) // the deleted ref's ID
				refs[payload.Ref] = true
			}
			is.Equal(refs, map[string]bool{"refs/heads/bad": true, "refs/tags/bad": true})
		})
	}
}

// TestRepoCommitMalformed checks that "repo commit" on a commit git-module
// cannot parse reports an error instead of panicking.
func TestRepoCommitMalformed(t *testing.T) {
	for _, kind := range gittest.Malformed {
		t.Run(string(kind), func(t *testing.T) {
			is := is.New(t)
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			bare := filepath.Join(config.FromContext(ownerCtx).DataPath, "repos", "hooks.git")
			bad := gittest.Commit(t, bare, kind, gittest.Tree(t, bare, "f", "bad\n"), "bad")

			_, _, err := runRepoOutput(t, ownerCtx, "commit", "hooks", bad)
			is.True(errors.Is(err, git.ErrMalformedCommit))
		})
	}
}
