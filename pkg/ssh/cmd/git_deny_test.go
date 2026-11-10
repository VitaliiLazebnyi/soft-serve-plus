package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// A git service denies a repository it cannot serve the way the repo
// commands do: a key in initial_admin_keys that no account holds is a
// server admin, not an anonymous caller, so a missing repository is
// "invalid repo" for it rather than "not authorized".
func TestGitDenyReadAdminKeyIsNotAnonymous(t *testing.T) {
	pub, ak := slashFreeKey(t)
	ctx, _ := newAuthTestContextWith(t, func(c *config.Config) {
		c.InitialAdminKeys = []string{ak}
	})

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"anonymous", ctx, git.ErrNotAuthed},
		{"admin key", context.WithValue(ctx, ssh.ContextKeyPublicKey, pub), git.ErrInvalidRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			is.True(proto.UserFromContext(tc.ctx) == nil)
			c := GitUploadPackCommand()
			c.SetIn(&bytes.Buffer{})
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			c.SetArgs([]string{"ghost"})
			err := c.ExecuteContext(tc.ctx)
			is.True(errors.Is(err, tc.want))
		})
	}
}
