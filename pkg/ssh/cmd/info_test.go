package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// repo info shows a mirror's last sync, and the error of its last sync to
// those with write access: the error can carry what the remote wrote.
func TestRepoInfoMirrorSync(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	readerCtx := withUser(t, ctx, be, "reader", false)
	_, err := be.CreateRepository(ownerCtx, "m", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Mirror: true})
	is.NoErr(err)

	info := func(c context.Context) string {
		t.Helper()
		stdout, _, err := runRepoOutput(t, c, "info", "m")
		is.NoErr(err)
		return stdout
	}

	// Never synced: nothing to show.
	if out := info(ownerCtx); strings.Contains(out, "Last Sync") {
		t.Errorf("a mirror never synced shows a sync: %q", out)
	}

	m, err := be.Repository(ctx, "m")
	is.NoErr(err)
	is.NoErr(be.RecordMirrorSync(ctx, m, errors.New("fetch failed:\nremote: boom"+escapes)))
	out := info(ownerCtx)
	assertNoControls(t, out)
	if !strings.Contains(out, "Last Sync Error: fetch failed: remote: boom") {
		t.Errorf("the owner does not see the error: %q", out)
	}
	if out := info(readerCtx); strings.Contains(out, "Last Sync Error") {
		t.Errorf("a reader sees the error: %q", out)
	}

	is.NoErr(be.RecordMirrorSync(ctx, m, nil))
	out = info(ownerCtx)
	if !strings.Contains(out, "Last Sync: ") || strings.Contains(out, "Last Sync Error") {
		t.Errorf("after a sync that succeeded: %q", out)
	}
}
