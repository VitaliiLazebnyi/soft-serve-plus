package cmd

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestMirrorRefusesRefDeletes verifies that `repo branch delete` and `repo
// tag delete` refuse a mirror, even for a server admin: the next sync would
// bring the ref back. Turning mirroring off lets them through again.
func TestMirrorRefusesRefDeletes(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)

	_, err := be.CreateRepository(adminCtx, "m", proto.UserFromContext(adminCtx), proto.RepositoryOptions{})
	is.NoErr(err)
	r, err := git.Open(filepath.Join(config.FromContext(ctx).DataPath, "repos", "m.git"))
	is.NoErr(err)
	rcfg, err := r.Config()
	is.NoErr(err)
	rcfg.Section("remote").Subsection("origin").SetOption("url", "https://1.1.1.1/x.git")
	is.NoErr(r.SetConfig(rcfg))
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
	hidden, err := be.IsHidden(ctx, "repo")
	is.NoErr(err)
	is.True(hidden)
	if err := runRepo(t, writerCtx, "hidden", "repo", "false"); !errors.Is(err, proto.ErrUnauthorized) {
		t.Errorf("unhide by a read-write collaborator: got %v, want ErrUnauthorized", err)
	}
	hidden, err = be.IsHidden(ctx, "repo")
	is.NoErr(err)
	is.True(hidden)
}
