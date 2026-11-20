package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/matryer/is"
)

// TestDeleteTagNotOnCommit deletes tags that point at a tree, at a blob,
// and, through an annotated tag, at a blob. Such tags are legal; reading
// the tagged commit for the event's old ID made the delete fail for them.
// The event carries the ID the tag ref held.
func TestDeleteTagNotOnCommit(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be, "-e", "branch_tag_delete")
	whPointAtLoopback(t, ownerCtx, "hooks", id)
	bare := filepath.Join(config.FromContext(ownerCtx).DataPath, "repos", "hooks.git")

	tree := gittest.Tree(t, bare, "f", "content\n")
	blob := strings.TrimSpace(gittest.Git(t, bare, []byte("blob\n"), "hash-object", "-w", "--stdin"))
	annotated := strings.TrimSpace(gittest.Git(t, bare,
		[]byte("object "+blob+"\ntype blob\ntag annotated\ntagger t <t@example.com> 0 +0000\n\nannotated\n"),
		"mktag"))
	want := map[string]string{
		"refs/tags/tree":      tree,
		"refs/tags/blob":      blob,
		"refs/tags/annotated": annotated,
	}
	n := 0
	for ref, target := range want {
		gittest.Git(t, bare, nil, "update-ref", ref, target)
		is.NoErr(runRepo(t, ownerCtx, "tag", "delete", "hooks", strings.TrimPrefix(ref, "refs/tags/")))
		is.True(!gitRefExists(t, bare, ref))
		n++
		// Each delete's delivery is recorded in the background; let it
		// finish before the next delete, as TestDeleteRefAtMalformedCommit
		// explains.
		whAwaitDeliveries(t, ownerCtx, be, "hooks", id, n)
	}

	be.WaitForDeliveries(context.Background())
	r, err := be.Repository(ownerCtx, "hooks")
	is.NoErr(err)
	ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
	is.NoErr(err)
	is.Equal(len(ds), len(want))
	got := map[string]string{}
	for _, d := range ds {
		full, err := be.WebhookDelivery(ownerCtx, r, id, d.ID)
		is.NoErr(err)
		var payload struct {
			Ref    string `json:"ref"`
			Before string `json:"before"`
		}
		is.NoErr(json.Unmarshal([]byte(full.RequestBody), &payload))
		got[payload.Ref] = payload.Before
	}
	is.Equal(got, want)
}
