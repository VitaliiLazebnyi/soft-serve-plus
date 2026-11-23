package backend

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// A description or project name given at creation reaches every user who
// lists the repository, so it is stripped of escape sequences and control
// characters just as when it is set later.
func TestCreateRepositorySanitisesDescriptionAndProjectName(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	r, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{
		Description: "a\x1bP1|x\x1b\\b\x07\nc",
		ProjectName: "p\x1b]52;c;Zm9v\x07q",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Description(); got != "ab\nc" {
		t.Fatalf("r.Description() = %v, want %v", got, "ab\nc")
	}
	if got := r.ProjectName(); got != "pq" {
		t.Fatalf("r.ProjectName() = %v, want %v", got, "pq")
	}
}
