package webhook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
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
	_, err := git.Init(t.Context(), dir, true)
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
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	// A one-line message far past every limit, ending in multi-byte runes.
	msg := strings.Repeat("é", 100<<10)
	path, head := gitRepoWithCommits(t, 1, msg)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
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
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	// 20 commits whose capped text alone is past the push event budget.
	path, head := gitRepoWithCommits(t, 20, strings.Repeat("x", 100<<10))

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
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

func TestNewPushEventFirstPush(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	is.NoErr(err)
	is.Equal(ev.EventType, EventPush)
	is.Equal(ev.Ref, "refs/heads/main")
	is.Equal(ev.After, head)
	is.Equal(ev.Repository.DefaultBranch, "main")
	is.Equal(ev.Repository.Owner.Username, "alice")
	is.Equal(ev.Repository.HTTPURL, "http://127.0.0.1:23232/repo.git")
	is.Equal(ev.Repository.SSHURL, "ssh://127.0.0.1:23231/repo.git")
	is.Equal(ev.Repository.GitURL, "git://127.0.0.1:9418/repo.git")

	// A first push with no earlier commits lists the whole history, newest first.
	is.Equal(len(ev.Commits), 2)
	is.Equal(ev.Commits[0].ID, head)
	is.Equal(ev.Commits[0].Title, "second commit")
	is.Equal(ev.Commits[1].Title, "first commit")
	is.Equal(strings.TrimSpace(ev.Commits[1].Message), "first commit\n\nbody of first")
	is.Equal(ev.Commits[1].Author.Name, "Alice")
	is.Equal(ev.Commits[1].Author.Email, "alice@example.com")
	is.True(!ev.Commits[1].Timestamp.IsZero())
}

func TestNewPushEventRangePush(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	first := gitOutput(t, path, "rev-parse", "main~1")

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", first, head)
	is.NoErr(err)
	// Only commits after `before` are listed.
	is.Equal(len(ev.Commits), 1)
	is.Equal(ev.Commits[0].Title, "second commit")
}

// commitOn writes a commit with an empty tree on top of parent (none when
// parent is empty) into the bare repository at path and returns its id.
func commitOn(t *testing.T, path, parent, msg string) string {
	t.Helper()
	tree := gitOutput(t, path, "mktree")
	args := []string{"-c", "user.name=Alice", "-c", "user.email=alice@example.com", "commit-tree", tree, "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return gitOutput(t, path, args...)
}

// A new branch lists only the commits no other branch or tag has, whether
// the ref already points at after (post-receive) or not yet (update hook).
func TestNewPushEventNewBranchListsOnlyNewCommits(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	dev := commitOn(t, path, head, "dev commit")

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/dev", zeroHash, dev)
	is.NoErr(err)
	is.Equal(len(ev.Commits), 1)
	is.Equal(ev.Commits[0].ID, dev)
	is.True(!ev.Forced)

	runGit(t, path, "update-ref", "refs/heads/dev", dev)
	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/dev", zeroHash, dev)
	is.NoErr(err)
	is.Equal(len(ev.Commits), 1)
	is.Equal(ev.Commits[0].ID, dev)

	// Commits a tag already has are not new either.
	runGit(t, path, "update-ref", "refs/heads/dev", head)
	runGit(t, path, "update-ref", "refs/tags/v1", dev)
	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/other", zeroHash, dev)
	is.NoErr(err)
	is.Equal(len(ev.Commits), 0)
}

// A new branch at a commit another branch already has brings no commits.
func TestNewPushEventNewBranchAtExistingCommit(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	runGit(t, path, "update-ref", "refs/heads/copy", head)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/copy", zeroHash, head)
	is.NoErr(err)
	is.Equal(len(ev.Commits), 0)
	// An empty list, not null.
	is.True(ev.Commits != nil)
}

func TestNewPushEventForced(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	first := gitOutput(t, path, "rev-parse", "main~1")
	other := commitOn(t, path, first, "rewritten second")
	unrelated := commitOn(t, path, "", "unrelated root")

	// A fast-forward is not forced.
	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", first, head)
	is.NoErr(err)
	is.True(!ev.Forced)

	// Rewriting the last commit is.
	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, other)
	is.NoErr(err)
	is.True(ev.Forced)
	is.Equal(len(ev.Commits), 1)
	is.Equal(ev.Commits[0].ID, other)

	// So is moving back, and moving to an unrelated history.
	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, first)
	is.NoErr(err)
	is.True(ev.Forced)
	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, unrelated)
	is.NoErr(err)
	is.True(ev.Forced)
}

// Annotated tags are compared by the commits they point at.
func TestNewPushEventForcedAnnotatedTag(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	tagOn := func(commit string) string {
		runGit(t, path, "-c", "user.name=Alice", "-c", "user.email=alice@example.com",
			"tag", "-f", "-a", "-m", "release", "v1", commit)
		return gitOutput(t, path, "rev-parse", "refs/tags/v1")
	}
	older := tagOn("main~1")
	newer := tagOn(head)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/tags/v1", older, newer)
	is.NoErr(err)
	is.True(!ev.Forced)

	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/tags/v1", newer, older)
	is.NoErr(err)
	is.True(ev.Forced)
}

func TestNewPushEventPusher(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	is.NoErr(err)
	is.Equal(ev.Pusher.Username, "alice")
	is.Equal(ev.Pusher.ID, e.owner.ID)

	// A push without a user has an empty pusher.
	ev, err = e.s.NewPushEvent(e.ctx, nil, e.senderRepo(path), "refs/heads/main", zeroHash, head)
	is.NoErr(err)
	is.Equal(ev.Pusher, User{})
}

// A push that deletes a branch or tag lists no commits, since there is no
// revision range to walk, and says the ref was deleted.
func TestNewPushEventDelete(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, zeroHash)
	is.NoErr(err)
	is.Equal(ev.EventType, EventPush)
	is.Equal(ev.Before, head)
	is.Equal(ev.After, zeroHash)
	is.True(ev.Deleted)
	is.True(!ev.Created)
	is.True(!ev.Forced)
	// Empty rather than nil, so the JSON payload has "commits":[].
	is.True(ev.Commits != nil)
	is.Equal(len(ev.Commits), 0)
}

func TestNewPushEventCreatedFlag(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	first := gitOutput(t, path, "rev-parse", "main~1")

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	is.NoErr(err)
	is.True(ev.Created)
	is.True(!ev.Deleted)

	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", first, head)
	is.NoErr(err)
	is.True(!ev.Created)
	is.True(!ev.Deleted)
}

func TestNewPushEventUnknownAfterFails(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	path, _ := gitRepo(t)

	_, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, "3333333333333333333333333333333333333333")
	is.True(err != nil)
}

func TestNewPushEventOpenFails(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	repo := e.senderRepo("")
	repo.openErr = errors.New("cannot open repository")

	_, err := e.s.NewPushEvent(e.ctx, e.sender(), repo, "refs/heads/main", zeroHash, "1111111111111111111111111111111111111111")
	is.True(err != nil)
}

func TestNewPushEventOwnerLookupFails(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	_, err := e.s.NewPushEvent(e.ctx, e.sender(), e.ownerLess(), "refs/heads/main", zeroHash, "1111111111111111111111111111111111111111")
	is.True(err != nil)
}

// TestNewPushEventSkipsMalformedCommits pushes, for each kind of commit
// git-module cannot parse, a malformed commit with a valid one on top. The
// push event leaves the malformed commit out and lists the valid one,
// instead of panicking in the hook process and sending no webhook at all.
func TestNewPushEventSkipsMalformedCommits(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	for _, kind := range gittest.Malformed {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			dir := filepath.Join(t.TempDir(), "repo.git")
			_, err := git.Init(t.Context(), dir, true)
			is.NoErr(err)
			bad := gittest.Commit(t, dir, kind, gittest.Tree(t, dir, "f", "bad\n"), "bad")
			top := gittest.Commit(t, dir, gittest.Valid, gittest.Tree(t, dir, "f", "top\n"), "top", bad)
			gittest.Git(t, dir, nil, "update-ref", "refs/heads/main", top)

			ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(dir), "refs/heads/main", zeroHash, top)
			is.NoErr(err)
			is.Equal(len(ev.Commits), 1)
			is.Equal(ev.Commits[0].ID, top)
			is.Equal(ev.Commits[0].Author.Email, "author@example.com")
		})
	}
}
