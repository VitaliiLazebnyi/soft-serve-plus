package backend

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestMissingRepositoryAccessors verifies every per-repository getter and
// setter reports a missing repository as proto.ErrRepoNotFound rather than
// leaking a database or filesystem error, or silently succeeding.
func TestMissingRepositoryAccessors(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	tests := map[string]func() error{
		"Description": func() error {
			_, err := be.Description(ctx, "ghost")
			return err
		},
		"IsMirror": func() error {
			_, err := be.IsMirror(ctx, "ghost")
			return err
		},
		"IsPrivate": func() error {
			_, err := be.IsPrivate(ctx, "ghost")
			return err
		},
		"IsHidden": func() error {
			_, err := be.IsHidden(ctx, "ghost")
			return err
		},
		"ProjectName": func() error {
			_, err := be.ProjectName(ctx, "ghost")
			return err
		},
		"SetHidden":      func() error { return be.SetHidden(ctx, "ghost", true) },
		"SetMirror":      func() error { return be.SetMirror(ctx, "ghost", true) },
		"SetDescription": func() error { return be.SetDescription(ctx, "ghost", "desc") },
		"SetPrivate":     func() error { return be.SetPrivate(ctx, "ghost", true) },
		"SetProjectName": func() error { return be.SetProjectName(ctx, "ghost", "name") },
		"Collaborators": func() error {
			_, err := be.Collaborators(ctx, "ghost")
			return err
		},
		"AddCollaborator":    func() error { return be.AddCollaborator(ctx, "ghost", "admin", access.ReadOnlyAccess) },
		"RemoveCollaborator": func() error { return be.RemoveCollaborator(ctx, "ghost", "admin") },
	}

	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			err := fn()
			if !errors.Is(err, proto.ErrRepoNotFound) {
				t.Fatalf("expected ErrRepoNotFound, got %v", err)
			}
			if strings.Contains(err.Error(), cfg.DataPath) {
				t.Fatalf("error leaks the server data path: %v", err)
			}
		})
	}

	// Nothing should have been created along the way.
	if _, err := os.Stat(be.repoPath("ghost")); err == nil {
		t.Fatal("a missing repository was created by a setter")
	}
}

// TestSetDescriptionHidesFilesystemError verifies a filesystem failure while
// updating a repository does not echo the server path back to the caller.
func TestSetDescriptionHidesFilesystemError(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "repo", nil, proto.RepositoryOptions{})
	is.NoErr(err)

	// Make the description path unwritable by turning it into a directory.
	descPath := be.repoPath("repo") + "/description"
	is.NoErr(os.Remove(descPath))
	is.NoErr(os.Mkdir(descPath, 0o755))

	err = be.SetDescription(ctx, "repo", "desc")
	is.True(err != nil)
	is.True(!strings.Contains(err.Error(), cfg.DataPath))
}

// TestRepositoryInvalidNameNotLogged verifies a name the filesystem cannot
// hold, which comes straight from a request, is reported as a missing
// repository without an error log line.
func TestRepositoryInvalidNameNotLogged(t *testing.T) {
	var logs bytes.Buffer
	be, _ := newTestBackendWithLog(t, &logs)
	ctx := context.Background()

	for name, repo := range map[string]string{
		"nul":            "a\x00b",
		"long segment":   strings.Repeat("x", 300),
		"long path":      strings.Repeat("a/", 3000),
		"through a file": "repo/sub",
	} {
		t.Run(name, func(t *testing.T) {
			logs.Reset()
			if repo == "repo/sub" {
				// A file where the parent directory of the repository should be.
				dir := filepath.Dir(be.repoPath("repo"))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "repo"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := be.Repository(ctx, repo)
			if !errors.Is(err, proto.ErrRepoNotFound) {
				t.Fatalf("expected ErrRepoNotFound, got %v", err)
			}
			if strings.Contains(logs.String(), "ERRO") {
				t.Fatalf("invalid name logged an error: %s", logs.String())
			}
		})
	}
}
