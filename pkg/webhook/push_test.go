package webhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/matryer/is"
)

func TestTruncate(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	is.Equal(truncate("short", 5, "!"), "short")
	is.Equal(truncate("longer", 4, "!"), "long!")
	// A multi-byte character is never cut in half.
	is.Equal(truncate("aé", 2, ""), "a")
	is.Equal(truncate("ééé", 5, "~"), "éé~")
}

// gitRepoWithCommits makes a bare repository with n commits by a long-named
// author, each with message msg, and returns its path and head.
func gitRepoWithCommits(t *testing.T, n int, msg string) (string, string) {
	t.Helper()
	is := is.New(t)
	dir := filepath.Join(t.TempDir(), "repo.git")
	_, err := git.Init(dir, true)
	is.NoErr(err)
	work := t.TempDir()
	msgFile := filepath.Join(t.TempDir(), "msg")
	is.NoErr(os.WriteFile(msgFile, []byte(msg), 0o600))
	name := strings.Repeat("n", 1000)
	email := strings.Repeat("e", 1000) + "@example.com"
	runGit(t, work, "init", "-q", "-b", "main")
	for range n {
		runGit(t, work, "-c", "user.name="+name, "-c", "user.email="+email,
			"commit", "-q", "--allow-empty", "--cleanup=verbatim", "-F", msgFile)
	}
	runGit(t, work, "push", "-q", dir, "main")
	runGit(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	return dir, gitOutput(t, work, "rev-parse", "HEAD")
}

func TestNewPushEventCapsCommitText(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	// A one-line message far past every limit, ending in multi-byte runes.
	msg := strings.Repeat("é", 100<<10)
	path, head := gitRepoWithCommits(t, 1, msg)

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	is.NoErr(err)
	is.Equal(len(ev.Commits), 1)
	c := ev.Commits[0]
	is.Equal(c.ID, head)
	is.True(strings.HasSuffix(c.Message, truncatedMessageMarker))
	is.True(len(c.Message) <= maxCommitMessage+len(truncatedMessageMarker))
	is.True(utf8.ValidString(c.Message))
	is.True(len(c.Title) <= maxCommitTitle)
	is.True(utf8.ValidString(c.Title))
	for _, s := range []string{c.Author.Name, c.Author.Email, c.Committer.Name, c.Committer.Email} {
		is.True(s != "")
		is.True(len(s) <= maxCommitIdentity)
	}
}

func TestNewPushEventCapsTotalCommitSize(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	// 20 commits whose capped text alone is past the push event budget.
	path, head := gitRepoWithCommits(t, 20, strings.Repeat("x", 100<<10))

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	is.NoErr(err)
	is.True(len(ev.Commits) > 1)
	is.True(len(ev.Commits) < 20)
	// The newest commits are the ones kept.
	is.Equal(ev.Commits[0].ID, head)
	total := 0
	for _, c := range ev.Commits {
		total += commitSize(c)
	}
	is.True(total <= maxPushCommitsSize)
}
