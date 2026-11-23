package backend

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// The description and the visibility live in the database only. The
// repository's description and git-daemon-export-ok files, which nothing in
// the server reads, are no longer written: creating a repository or
// changing either setting does no file I/O inside the transaction.
func TestRepositorySettingsWriteNoFiles(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	_, err := be.CreateRepository(ctx, "described", defaultAdmin(t, be), proto.RepositoryOptions{Description: "first words"})
	if err != nil {
		t.Fatal(err)
	}
	dir := be.repoPath("described")
	template, err := os.ReadFile(filepath.Join(dir, "description"))
	if err != nil {
		t.Fatal(err)
	} // git init's own template
	if !(string(template) != "first words") {
		t.Fatal("not true: string(template) != \"first words\"")
	}
	_, err = os.Stat(filepath.Join(dir, "git-daemon-export-ok"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}

	if err := be.SetDescription(ctx, "described", "second words"); err != nil {
		t.Fatal(err)
	}
	if err := be.SetPrivate(ctx, "described", true); err != nil {
		t.Fatal(err)
	}
	if err := be.SetPrivate(ctx, "described", false); err != nil {
		t.Fatal(err)
	}
	r, err := be.Repository(ctx, "described")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Description(); got != "second words" {
		t.Fatalf("r.Description() = %v, want %v", got, "second words")
	}
	if r.IsPrivate() {
		t.Fatal("not true: !r.IsPrivate()")
	}

	got, err := os.ReadFile(filepath.Join(dir, "description"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(got), string(template); got != want {
		t.Fatalf("string(got) = %v, want %v", got, want)
	}
	_, err = os.Stat(filepath.Join(dir, "git-daemon-export-ok"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}
