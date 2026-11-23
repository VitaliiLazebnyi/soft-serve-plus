package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git/gittest"
)

// TestDeleteTagNotOnCommit deletes tags that point at a tree, at a blob,
// and, through an annotated tag, at a blob. Such tags are legal; reading
// the tagged commit for the event's old ID made the delete fail for them.
// The event carries the ID the tag ref held.
func TestDeleteTagNotOnCommit(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be, "-e", "branch_tag_delete")
	whPointAtLoopback(t, ownerCtx, "hooks", id)
	bare := filepath.Join(cmdsOf(ownerCtx).cfg.DataPath, "repos", "hooks.git")

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
		if err := runRepo(t, ownerCtx, "tag", "delete", "hooks", strings.TrimPrefix(ref, "refs/tags/")); err != nil {
			t.Fatal(err)
		}
		if gitRefExists(t, bare, ref) {
			t.Fatal("not true: !gitRefExists(t, bare, ref)")
		}
		n++
		// Each delete's delivery is recorded in the background; let it
		// finish before the next delete, as TestDeleteRefAtMalformedCommit
		// explains.
		whAwaitDeliveries(t, ownerCtx, be, "hooks", id, n)
	}

	be.WaitForDeliveries(context.Background())
	r, err := be.Repository(ownerCtx, "hooks")
	if err != nil {
		t.Fatal(err)
	}
	ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(ds), len(want); got != want {
		t.Fatalf("len(ds) = %v, want %v", got, want)
	}
	got := map[string]string{}
	for _, d := range ds {
		full, err := be.WebhookDelivery(ownerCtx, r, id, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Ref    string `json:"ref"`
			Before string `json:"before"`
		}
		if err := json.Unmarshal([]byte(full.RequestBody), &payload); err != nil {
			t.Fatal(err)
		}
		got[payload.Ref] = payload.Before
	}
	if got, want := got, want; !reflect.DeepEqual(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}
