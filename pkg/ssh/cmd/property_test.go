package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// repo hidden must reject a value that is not a boolean instead of reading it
// as false, which would silently unhide the repository.
func TestRepoHiddenParsesBool(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "secret", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)

	hidden := func() bool {
		t.Helper()
		r, err := be.Repository(ctx, "secret")
		is.NoErr(err)
		return r.IsHidden()
	}

	for _, tc := range []struct {
		arg  string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"1", true},
		{"0", false},
		{"True", true},
		{"F", false},
		{"t", true},
	} {
		is.NoErr(runRepo(t, ownerCtx, "hidden", "secret", tc.arg))
		if got := hidden(); got != tc.want {
			t.Errorf("hidden %s: IsHidden() = %v, want %v", tc.arg, got, tc.want)
		}
	}

	// The repository is hidden now; no bad value may unhide it.
	is.True(hidden())
	for _, arg := range []string{"yes", "no", "off", "maybe", "", "true "} {
		_, _, err := runRepoOutput(t, ownerCtx, "hidden", "secret", arg)
		if err == nil || !strings.Contains(err.Error(), "invalid boolean value") {
			t.Errorf("hidden %q: err = %v, want an invalid boolean value error", arg, err)
		}
		if !hidden() {
			t.Fatalf("hidden %q unhid the repository", arg)
		}
	}

	// A stray extra argument is refused rather than ignored.
	is.True(runRepo(t, ownerCtx, "hidden", "secret", "false", "extra") != nil)
	is.True(hidden())

	// Reading the value still works.
	out, _, err := runRepoOutput(t, ownerCtx, "hidden", "secret")
	is.NoErr(err)
	is.Equal(strings.TrimSpace(out), "true")
}

// TestMirrorRefusesRefDeletes verifies that `repo branch delete` and `repo
// tag delete` refuse a mirror, even for a server admin: the next sync would
// bring the ref back. Turning mirroring off lets them through again.
func TestMirrorRefusesRefDeletes(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)

	_, err := be.CreateRepository(adminCtx, "m", proto.UserFromContext(adminCtx), proto.RepositoryOptions{})
	is.NoErr(err)
	r, err := git.Open(t.Context(), filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "m.git"))
	is.NoErr(err)
	is.NoErr(r.ConfigSet("remote.origin.url", "https://1.1.1.1/x.git"))
	is.NoErr(be.SetMirror(adminCtx, "m", true))

	for _, args := range [][]string{
		{"branch", "delete", "m", "main"},
		{"tag", "delete", "m", "v1"},
	} {
		if err := runRepo(t, adminCtx, args...); !errors.Is(err, proto.ErrMirrorReadOnly) {
			t.Errorf("repo %v on a mirror: got %v, want ErrMirrorReadOnly", args, err)
		}
	}

	// Without mirroring the commands go on to look the ref up.
	is.NoErr(be.SetMirror(adminCtx, "m", false))
	for _, args := range [][]string{
		{"branch", "delete", "m", "main"},
		{"tag", "delete", "m", "v1"},
	} {
		if err := runRepo(t, adminCtx, args...); !errors.Is(err, git.ErrReferenceNotExist) {
			t.Errorf("repo %v on a plain repository: got %v, want ErrReferenceNotExist", args, err)
		}
	}
}

// TestMirrorHiddenSettersNeedAdmin verifies that `repo mirror` and `repo
// hidden` setters need repository admin access: a read-write collaborator
// could otherwise turn mirroring off to push, or hide the repository from
// everyone's listing. Reading the properties still needs only read access.
func TestMirrorHiddenSettersNeedAdmin(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)

	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)
	writerCtx := withUser(t, ctx, be, "writer", false)
	bossCtx := withUser(t, ctx, be, "boss", false)
	is.NoErr(be.AddCollaborator(ownerCtx, "repo", "writer", access.ReadWriteAccess))
	is.NoErr(be.AddCollaborator(ownerCtx, "repo", "boss", access.AdminAccess))

	for _, prop := range []string{"mirror", "hidden"} {
		is.NoErr(runRepo(t, writerCtx, prop, "repo"))
		if err := runRepo(t, writerCtx, prop, "repo", "false"); !errors.Is(err, proto.ErrUnauthorized) {
			t.Errorf("repo %s set by a read-write collaborator: got %v, want ErrUnauthorized", prop, err)
		}
		is.NoErr(runRepo(t, bossCtx, prop, "repo", "false"))
		is.NoErr(runRepo(t, ownerCtx, prop, "repo", "false"))
	}

	is.NoErr(runRepo(t, bossCtx, "hidden", "repo", "true"))
	r, err := be.Repository(ctx, "repo")
	is.NoErr(err)
	is.True(r.IsHidden())
	if err := runRepo(t, writerCtx, "hidden", "repo", "false"); !errors.Is(err, proto.ErrUnauthorized) {
		t.Errorf("unhide by a read-write collaborator: got %v, want ErrUnauthorized", err)
	}
	r, err = be.Repository(ctx, "repo")
	is.NoErr(err)
	is.True(r.IsHidden())
}
