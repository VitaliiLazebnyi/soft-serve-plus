package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
	"github.com/spf13/cobra"
)

// runLFSCommand runs an LFS command with stdin, writing its output to
// stdout, and returns what it logged and its error.
func runLFSCommand(ctx context.Context, newCmd func() *cobra.Command, stdin io.Reader, stdout io.Writer, args ...string) (string, error) {
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)
	ctx = log.WithContext(ctx, logger)

	c := newCmd()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(stdin)
	c.SetOut(stdout)
	c.SetErr(io.Discard)
	c.SetArgs(args)
	err := c.ExecuteContext(ctx)
	return logs.String(), err
}

// A caller without an account that may read a repository, such as an
// unregistered key on a public repository with anonymous read access, gets
// an answer that says git-lfs-authenticate needs an account, not "something
// went wrong" with an error logged for it. A caller that may not read the
// repository, or that wants to upload, still gets "not authorized".
func TestSSHLFSAuthenticateWithoutAccount(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	owner := proto.UserFromContext(withUser(t, ctx, be, "boss", true))
	_, err := be.CreateRepository(ctx, "pub", owner, proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "priv", owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)

	for _, tc := range []struct {
		repo, op string
		want     error
	}{
		{"pub", "download", errLFSAuthNoAccount},
		{"pub", "upload", git.ErrNotAuthed},
		{"priv", "download", git.ErrNotAuthed},
		{"ghost", "download", git.ErrNotAuthed},
	} {
		t.Run(tc.repo+" "+tc.op, func(t *testing.T) {
			var out bytes.Buffer
			logs, err := runLFSCommand(ctx, GitLFSAuthenticateCommand, strings.NewReader(""), &out, tc.repo, tc.op)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("issued output %q", out.String())
			}
			if strings.Contains(logs, "ERRO") {
				t.Errorf("logged an error:\n%s", logs)
			}
		})
	}
}

// errWriter is a client that went away: every write fails as a write to a
// closed SSH channel does.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.EOF }

// An LFS transfer whose client goes away is not a server failure, so it is
// reported as cancelled and logged at Debug, as a git service is.
func TestSSHLFSTransferClientGone(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	_, err := be.CreateRepository(ctx, "shared", proto.UserFromContext(adminCtx), proto.RepositoryOptions{})
	is.NoErr(err)

	logs, err := runLFSCommand(adminCtx, GitLFSTransfer, strings.NewReader(""), errWriter{}, "shared", "download")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if strings.Contains(logs, "ERRO") {
		t.Errorf("logged an error:\n%s", logs)
	}
}
