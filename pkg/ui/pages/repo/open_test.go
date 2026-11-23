package repo

import (
	"context"
	"path/filepath"
	"testing"
)

// The TUI's git commands run under the session's context, so they stop when
// the client disconnects.
func TestOpenWithContext(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "trunk", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")

	r, err := openWithContext(t.Context(), openRepo{path: dir})
	if err != nil {
		t.Fatalf("openWithContext: %v", err)
	}
	if _, err := r.HEAD(); err != nil {
		t.Fatalf("HEAD with a live context: %v", err)
	}

	live, cancelLive := context.WithCancel(t.Context())
	r, err = openWithContext(live, openRepo{path: dir})
	if err != nil {
		t.Fatalf("openWithContext: %v", err)
	}
	cancelLive()
	if _, err := r.HEAD(); err == nil {
		t.Fatal("HEAD with a cancelled context succeeded; want an error")
	}

	// A load cancelled before or while it opens the repository goes no
	// further, and sends nothing.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := openWithContext(ctx, openRepo{path: dir}); err == nil {
		t.Fatal("openWithContext with a cancelled context succeeded; want an error")
	}
	if msg := updateRefCmd(ctx, openRepo{path: dir}, 0)(); msg != nil {
		t.Fatalf("updateRefCmd with a cancelled context = %v; want nothing", msg)
	}
}
