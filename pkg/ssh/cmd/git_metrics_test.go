package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Each push over SSH is counted once, and a push that is refused is not
// counted at all.
func TestReceivePackCountedOnce(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	readerCtx := withUser(t, ctx, be, "reader", false)
	const name = "receive-pack-counted"
	_, err := be.CreateRepository(ctx, name, proto.UserFromContext(adminCtx), proto.RepositoryOptions{Private: true})
	is.NoErr(err)

	// The repository's hooks run the server binary, which is the test
	// binary here; point git at no hooks instead.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", t.TempDir())

	// A commit to push, and its pack.
	work := t.TempDir()
	gitCmd := func(stdin string, args ...string) []byte {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.Output()
		is.NoErr(err)
		return out
	}
	gitCmd("", "init", "-q", "--bare")
	tree := strings.TrimSpace(string(gitCmd("", "mktree")))
	commit := strings.TrimSpace(string(gitCmd("", "commit-tree", tree, "-m", "pushed")))
	pack := string(gitCmd(commit+"\n", "pack-objects", "--stdout", "--revs"))

	// push creates branch with the commit.
	push := func(ctx context.Context, branch string) error {
		line := fmt.Sprintf("%s %s refs/heads/%s\x00report-status\n", strings.Repeat("0", 40), commit, branch)
		c := GitReceivePackCommand()
		c.SilenceUsage = true
		c.SilenceErrors = true
		c.SetIn(strings.NewReader(fmt.Sprintf("%04x%s0000", len(line)+4, line) + pack))
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		c.SetArgs([]string{name})
		return c.ExecuteContext(ctx)
	}

	is.NoErr(push(adminCtx, "main"))
	is.NoErr(push(adminCtx, "other"))
	is.True(push(readerCtx, "denied") != nil) // a user who may not write is refused

	is.Equal(testutil.ToFloat64(receivePackCounter.WithLabelValues(name)), 2.0)
}
