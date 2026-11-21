package backend

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// The description and the visibility live in the database only. The
// repository's description and git-daemon-export-ok files, which nothing in
// the server reads, are no longer written: creating a repository or
// changing either setting does no file I/O inside the transaction.
func TestRepositorySettingsWriteNoFiles(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	_, err := be.CreateRepository(ctx, "described", defaultAdmin(t, be), proto.RepositoryOptions{Description: "first words"})
	is.NoErr(err)
	dir := be.repoPath("described")
	template, err := os.ReadFile(filepath.Join(dir, "description"))
	is.NoErr(err) // git init's own template
	is.True(string(template) != "first words")
	_, err = os.Stat(filepath.Join(dir, "git-daemon-export-ok"))
	is.True(os.IsNotExist(err))

	is.NoErr(be.SetDescription(ctx, "described", "second words"))
	is.NoErr(be.SetPrivate(ctx, "described", true))
	is.NoErr(be.SetPrivate(ctx, "described", false))
	r, err := be.Repository(ctx, "described")
	is.NoErr(err)
	is.Equal(r.Description(), "second words")
	is.True(!r.IsPrivate())

	got, err := os.ReadFile(filepath.Join(dir, "description"))
	is.NoErr(err)
	is.Equal(string(got), string(template))
	_, err = os.Stat(filepath.Join(dir, "git-daemon-export-ok"))
	is.True(os.IsNotExist(err))
}
