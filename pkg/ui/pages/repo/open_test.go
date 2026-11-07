package repo

import (
	"context"
	"path/filepath"
	"testing"
)

// The TUI's git commands run under the session's context, so they stop when
// the client disconnects.
func TestOpenWithContext(t *testing.T) {
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

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r, err = openWithContext(ctx, openRepo{path: dir})
	if err != nil {
		t.Fatalf("openWithContext: %v", err)
	}
	if r.Context() != ctx {
		t.Fatal("openWithContext did not bind the context")
	}
	if _, err := r.HEAD(); err == nil {
		t.Fatal("HEAD with a cancelled context succeeded; want an error")
	}
	if msg, ok := UpdateRefCmd(ctx, openRepo{path: dir})().(RefMsg); ok {
		t.Fatalf("UpdateRefCmd with a cancelled context = %v; want no reference", msg)
	}
}
