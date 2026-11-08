package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// The repository's description file, which gitweb-style tools and the git
// daemon read, must follow the description stored in the database: written
// on create, rewritten on update.
func TestRepositoryDescriptionFile(t *testing.T) {
	be, _ := newTestBackend(t)
	ctx := context.Background()

	readDesc := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(be.repoPath(name), "description"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if _, err := be.CreateRepository(ctx, "described", nil, proto.RepositoryOptions{Description: "first words"}); err != nil {
		t.Fatal(err)
	}
	if got := readDesc("described"); got != "first words" {
		t.Errorf("description file after create = %q, want %q", got, "first words")
	}

	if err := be.SetDescription(ctx, "described", "second words"); err != nil {
		t.Fatal(err)
	}
	if got := readDesc("described"); got != "second words" {
		t.Errorf("description file after SetDescription = %q, want %q", got, "second words")
	}
	desc, err := be.Description(ctx, "described")
	if err != nil {
		t.Fatal(err)
	}
	if desc != "second words" {
		t.Errorf("Description() = %q, want %q", desc, "second words")
	}

	// A repository created without one still gets an empty description
	// file, so git does not fall back to its "Unnamed repository" template.
	if _, err := be.CreateRepository(ctx, "plain", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readDesc("plain"); got != "" {
		t.Errorf("description file of a repository created without one = %q, want empty", got)
	}

	if err := be.SetDescription(ctx, "plain", ""); err != nil {
		t.Fatal(err)
	}
	if got := readDesc("plain"); got != "" {
		t.Errorf("description file after clearing = %q, want empty", got)
	}
}
