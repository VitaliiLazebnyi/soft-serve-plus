package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// binaryContents is a file git and the blob command treat as binary.
var binaryContents = []byte("bin\x00ary")

// viewRepo creates the repository "view" with one commit holding README.md
// and the binary file data.bin, and returns an admin context and the
// commit's hash.
func viewRepo(t *testing.T) (context.Context, string) {
	t.Helper()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ctx = withUser(t, ctx, be, "boss", true)
	_, err := be.CreateRepository(ctx, "view", proto.UserFromContext(ctx), proto.RepositoryOptions{})
	is.NoErr(err)

	bare := filepath.Join(config.FromContext(ctx).DataPath, "repos", "view.git")
	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	is.NoErr(os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o600))
	is.NoErr(os.WriteFile(filepath.Join(work, "data.bin"), binaryContents, 0o600))
	git("add", "-A")
	git("commit", "-q", "-m", "first")
	git("push", "-q", bare, "main")
	// The bare repository's HEAD follows the server git's init.defaultBranch,
	// which may be master; point it at the pushed branch.
	git("--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return ctx, git("rev-parse", "HEAD")
}

func TestBlobRawPrintsExactBytes(t *testing.T) {
	is := is.New(t)
	ctx, _ := viewRepo(t)

	stdout, _, err := runRepoOutput(t, ctx, "blob", "view", "data.bin", "--raw")
	is.NoErr(err)
	is.True(bytes.Equal([]byte(stdout), binaryContents)) // no newline added
}

func TestBlobMissingPathIsFileNotFound(t *testing.T) {
	is := is.New(t)
	ctx, _ := viewRepo(t)

	for _, path := range []string{"nope.txt", "dir/nope.txt"} {
		_, _, err := runRepoOutput(t, ctx, "blob", "view", path)
		is.True(errors.Is(err, proto.ErrFileNotFound))
	}
}

func TestCommitPrintsHash(t *testing.T) {
	is := is.New(t)
	ctx, sha := viewRepo(t)

	stdout, _, err := runRepoOutput(t, ctx, "commit", "view", "HEAD")
	is.NoErr(err)
	is.True(strings.HasPrefix(stdout, "commit "+sha+"\n"))
}
