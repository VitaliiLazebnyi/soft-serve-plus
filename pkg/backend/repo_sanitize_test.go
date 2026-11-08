package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// A description or project name given at creation reaches every user who
// lists the repository, so it is stripped of escape sequences and control
// characters just as when it is set later.
func TestCreateRepositorySanitisesDescriptionAndProjectName(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	r, err := be.CreateRepository(ctx, "repo", nil, proto.RepositoryOptions{
		Description: "a\x1bP1|x\x1b\\b\x07\nc",
		ProjectName: "p\x1b]52;c;Zm9v\x07q",
	})
	is.NoErr(err)
	is.Equal(r.Description(), "ab\nc")
	is.Equal(r.ProjectName(), "pq")

	desc, err := os.ReadFile(filepath.Join(be.repoPath("repo"), "description"))
	is.NoErr(err)
	is.Equal(string(desc), "ab\nc")
}
