package webhook

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
)

func TestTruncate(t *testing.T) {
	t.Parallel()

	if got := truncate("short", 5, "!"); got != "short" {
		t.Fatalf("truncate(\"short\", 5, \"!\") = %v, want %v", got, "short")
	}
	if got := truncate("longer", 4, "!"); got != "long!" {
		t.Fatalf("truncate(\"longer\", 4, \"!\") = %v, want %v", got, "long!")
	}
	// A multi-byte character is never cut in half.
	if got := truncate("aé", 2, ""); got != "a" {
		t.Fatalf("truncate(\"aé\", 2, \"\") = %v, want %v", got, "a")
	}
	if got := truncate("ééé", 5, "~"); got != "éé~" {
		t.Fatalf("truncate(\"ééé\", 5, \"~\") = %v, want %v", got, "éé~")
	}
}

// gitRepoWithCommits makes a bare repository with n commits by a long-named
// author, each with message msg, and returns its path and head.
func gitRepoWithCommits(t *testing.T, n int, msg string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo.git")
	_, err := git.Init(t.Context(), dir, true)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	msgFile := filepath.Join(t.TempDir(), "msg")
	if err := os.WriteFile(msgFile, []byte(msg), 0o600); err != nil {
		t.Fatal(err)
	}
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
	e := newTestEnv(t)
	// A one-line message far past every limit, ending in multi-byte runes.
	msg := strings.Repeat("é", 100<<10)
	path, head := gitRepoWithCommits(t, 1, msg)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ev.Commits); got != 1 {
		t.Fatalf("len(ev.Commits) = %v, want %v", got, 1)
	}
	c := ev.Commits[0]
	if got := c.ID; got != head {
		t.Fatalf("c.ID = %v, want %v", got, head)
	}
	if !strings.HasSuffix(c.Message, truncatedMessageMarker) {
		t.Fatal("not true: strings.HasSuffix(c.Message, truncatedMessageMarker)")
	}
	if len(c.Message) > maxCommitMessage+len(truncatedMessageMarker) {
		t.Fatal("not true: len(c.Message) <= maxCommitMessage+len(truncatedMessageMarker)")
	}
	if !utf8.ValidString(c.Message) {
		t.Fatal("not true: utf8.ValidString(c.Message)")
	}
	if len(c.Title) > maxCommitTitle {
		t.Fatal("not true: len(c.Title) <= maxCommitTitle")
	}
	if !utf8.ValidString(c.Title) {
		t.Fatal("not true: utf8.ValidString(c.Title)")
	}
	for _, s := range []string{c.Author.Name, c.Author.Email, c.Committer.Name, c.Committer.Email} {
		if s == "" {
			t.Fatal("not true: s != \"\"")
		}
		if len(s) > maxCommitIdentity {
			t.Fatal("not true: len(s) <= maxCommitIdentity")
		}
	}
}

func TestNewPushEventCapsTotalCommitSize(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	// 20 commits whose capped text alone is past the push event budget.
	path, head := gitRepoWithCommits(t, 20, strings.Repeat("x", 100<<10))

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Commits) <= 1 {
		t.Fatal("not true: len(ev.Commits) > 1")
	}
	if len(ev.Commits) >= 20 {
		t.Fatal("not true: len(ev.Commits) < 20")
	}
	// The newest commits are the ones kept.
	if got := ev.Commits[0].ID; got != head {
		t.Fatalf("ev.Commits[0].ID = %v, want %v", got, head)
	}
	total := 0
	for _, c := range ev.Commits {
		total += commitSize(c)
	}
	if total > maxPushCommitsSize {
		t.Fatal("not true: total <= maxPushCommitsSize")
	}
}

func TestNewPushEventFirstPush(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	path, head := gitRepo(t)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.EventType; got != EventPush {
		t.Fatalf("ev.EventType = %v, want %v", got, EventPush)
	}
	if got := ev.Ref; got != "refs/heads/main" {
		t.Fatalf("ev.Ref = %v, want %v", got, "refs/heads/main")
	}
	if got := ev.After; got != head {
		t.Fatalf("ev.After = %v, want %v", got, head)
	}
	if got := ev.Repository.DefaultBranch; got != "main" {
		t.Fatalf("ev.Repository.DefaultBranch = %v, want %v", got, "main")
	}
	if got := ev.Repository.Owner.Username; got != "alice" {
		t.Fatalf("ev.Repository.Owner.Username = %v, want %v", got, "alice")
	}
	if got := ev.Repository.HTTPURL; got != "http://127.0.0.1:23232/repo.git" {
		t.Fatalf("ev.Repository.HTTPURL = %v, want %v", got, "http://127.0.0.1:23232/repo.git")
	}
	if got := ev.Repository.SSHURL; got != "ssh://127.0.0.1:23231/repo.git" {
		t.Fatalf("ev.Repository.SSHURL = %v, want %v", got, "ssh://127.0.0.1:23231/repo.git")
	}
	if got := ev.Repository.GitURL; got != "git://127.0.0.1:9418/repo.git" {
		t.Fatalf("ev.Repository.GitURL = %v, want %v", got, "git://127.0.0.1:9418/repo.git")
	}

	// A first push with no earlier commits lists the whole history, newest first.
	if got := len(ev.Commits); got != 2 {
		t.Fatalf("len(ev.Commits) = %v, want %v", got, 2)
	}
	if got := ev.Commits[0].ID; got != head {
		t.Fatalf("ev.Commits[0].ID = %v, want %v", got, head)
	}
	if got := ev.Commits[0].Title; got != "second commit" {
		t.Fatalf("ev.Commits[0].Title = %v, want %v", got, "second commit")
	}
	if got := ev.Commits[1].Title; got != "first commit" {
		t.Fatalf("ev.Commits[1].Title = %v, want %v", got, "first commit")
	}
	if got := strings.TrimSpace(ev.Commits[1].Message); got != "first commit\n\nbody of first" {
		t.Fatalf("strings.TrimSpace(ev.Commits[1].Message) = %v, want %v", got, "first commit\n\nbody of first")
	}
	if got := ev.Commits[1].Author.Name; got != "Alice" {
		t.Fatalf("ev.Commits[1].Author.Name = %v, want %v", got, "Alice")
	}
	if got := ev.Commits[1].Author.Email; got != "alice@example.com" {
		t.Fatalf("ev.Commits[1].Author.Email = %v, want %v", got, "alice@example.com")
	}
	if ev.Commits[1].Timestamp.IsZero() {
		t.Fatal("not true: !ev.Commits[1].Timestamp.IsZero()")
	}
}

func TestNewPushEventRangePush(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	path, head := gitRepo(t)
	first := gitOutput(t, path, "rev-parse", "main~1")

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", first, head)
	if err != nil {
		t.Fatal(err)
	}
	// Only commits after `before` are listed.
	if got := len(ev.Commits); got != 1 {
		t.Fatalf("len(ev.Commits) = %v, want %v", got, 1)
	}
	if got := ev.Commits[0].Title; got != "second commit" {
		t.Fatalf("ev.Commits[0].Title = %v, want %v", got, "second commit")
	}
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
	e := newTestEnv(t)
	path, head := gitRepo(t)
	dev := commitOn(t, path, head, "dev commit")

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/dev", zeroHash, dev)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ev.Commits); got != 1 {
		t.Fatalf("len(ev.Commits) = %v, want %v", got, 1)
	}
	if got := ev.Commits[0].ID; got != dev {
		t.Fatalf("ev.Commits[0].ID = %v, want %v", got, dev)
	}
	if ev.Forced {
		t.Fatal("not true: !ev.Forced")
	}

	runGit(t, path, "update-ref", "refs/heads/dev", dev)
	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/dev", zeroHash, dev)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ev.Commits); got != 1 {
		t.Fatalf("len(ev.Commits) = %v, want %v", got, 1)
	}
	if got := ev.Commits[0].ID; got != dev {
		t.Fatalf("ev.Commits[0].ID = %v, want %v", got, dev)
	}

	// Commits a tag already has are not new either.
	runGit(t, path, "update-ref", "refs/heads/dev", head)
	runGit(t, path, "update-ref", "refs/tags/v1", dev)
	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/other", zeroHash, dev)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ev.Commits); got != 0 {
		t.Fatalf("len(ev.Commits) = %v, want %v", got, 0)
	}
}

// A new branch at a commit another branch already has brings no commits.
func TestNewPushEventNewBranchAtExistingCommit(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	path, head := gitRepo(t)
	runGit(t, path, "update-ref", "refs/heads/copy", head)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/copy", zeroHash, head)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ev.Commits); got != 0 {
		t.Fatalf("len(ev.Commits) = %v, want %v", got, 0)
	}
	// An empty list, not null.
	if ev.Commits == nil {
		t.Fatal("not true: ev.Commits != nil")
	}
}

func TestNewPushEventForced(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	path, head := gitRepo(t)
	first := gitOutput(t, path, "rev-parse", "main~1")
	other := commitOn(t, path, first, "rewritten second")
	unrelated := commitOn(t, path, "", "unrelated root")

	// A fast-forward is not forced.
	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", first, head)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Forced {
		t.Fatal("not true: !ev.Forced")
	}

	// Rewriting the last commit is.
	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, other)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Forced {
		t.Fatal("not true: ev.Forced")
	}
	if got := len(ev.Commits); got != 1 {
		t.Fatalf("len(ev.Commits) = %v, want %v", got, 1)
	}
	if got := ev.Commits[0].ID; got != other {
		t.Fatalf("ev.Commits[0].ID = %v, want %v", got, other)
	}

	// So is moving back, and moving to an unrelated history.
	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, first)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Forced {
		t.Fatal("not true: ev.Forced")
	}
	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Forced {
		t.Fatal("not true: ev.Forced")
	}
}

// Annotated tags are compared by the commits they point at.
func TestNewPushEventForcedAnnotatedTag(t *testing.T) {
	t.Parallel()
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
	if err != nil {
		t.Fatal(err)
	}
	if ev.Forced {
		t.Fatal("not true: !ev.Forced")
	}

	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/tags/v1", newer, older)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Forced {
		t.Fatal("not true: ev.Forced")
	}
}

func TestNewPushEventPusher(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	path, head := gitRepo(t)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.Pusher.Username; got != "alice" {
		t.Fatalf("ev.Pusher.Username = %v, want %v", got, "alice")
	}
	if got := ev.Pusher.ID; got != e.owner.ID {
		t.Fatalf("ev.Pusher.ID = %v, want %v", got, e.owner.ID)
	}

	// A push without a user has an empty pusher.
	ev, err = e.s.NewPushEvent(e.ctx, nil, e.senderRepo(path), "refs/heads/main", zeroHash, head)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ev.Pusher, (User{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ev.Pusher = %v, want %v", got, want)
	}
}

// A push that deletes a branch or tag lists no commits, since there is no
// revision range to walk, and says the ref was deleted.
func TestNewPushEventDelete(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	path, head := gitRepo(t)

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, zeroHash)
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.EventType; got != EventPush {
		t.Fatalf("ev.EventType = %v, want %v", got, EventPush)
	}
	if got := ev.Before; got != head {
		t.Fatalf("ev.Before = %v, want %v", got, head)
	}
	if got := ev.After; got != zeroHash {
		t.Fatalf("ev.After = %v, want %v", got, zeroHash)
	}
	if !ev.Deleted {
		t.Fatal("not true: ev.Deleted")
	}
	if ev.Created {
		t.Fatal("not true: !ev.Created")
	}
	if ev.Forced {
		t.Fatal("not true: !ev.Forced")
	}
	// Empty rather than nil, so the JSON payload has "commits":[].
	if ev.Commits == nil {
		t.Fatal("not true: ev.Commits != nil")
	}
	if got := len(ev.Commits); got != 0 {
		t.Fatalf("len(ev.Commits) = %v, want %v", got, 0)
	}
}

func TestNewPushEventCreatedFlag(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	path, head := gitRepo(t)
	first := gitOutput(t, path, "rev-parse", "main~1")

	ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Created {
		t.Fatal("not true: ev.Created")
	}
	if ev.Deleted {
		t.Fatal("not true: !ev.Deleted")
	}

	ev, err = e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", first, head)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Created {
		t.Fatal("not true: !ev.Created")
	}
	if ev.Deleted {
		t.Fatal("not true: !ev.Deleted")
	}
}

func TestNewPushEventUnknownAfterFails(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	path, _ := gitRepo(t)

	_, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, "3333333333333333333333333333333333333333")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

func TestNewPushEventOpenFails(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	repo := e.senderRepo("")
	repo.openErr = errors.New("cannot open repository")

	_, err := e.s.NewPushEvent(e.ctx, e.sender(), repo, "refs/heads/main", zeroHash, "1111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

func TestNewPushEventOwnerLookupFails(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	_, err := e.s.NewPushEvent(e.ctx, e.sender(), e.ownerLess(), "refs/heads/main", zeroHash, "1111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
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
			dir := filepath.Join(t.TempDir(), "repo.git")
			_, err := git.Init(t.Context(), dir, true)
			if err != nil {
				t.Fatal(err)
			}
			bad := gittest.Commit(t, dir, kind, gittest.Tree(t, dir, "f", "bad\n"), "bad")
			top := gittest.Commit(t, dir, gittest.Valid, gittest.Tree(t, dir, "f", "top\n"), "top", bad)
			gittest.Git(t, dir, nil, "update-ref", "refs/heads/main", top)

			ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(dir), "refs/heads/main", zeroHash, top)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(ev.Commits); got != 1 {
				t.Fatalf("len(ev.Commits) = %v, want %v", got, 1)
			}
			if got := ev.Commits[0].ID; got != top {
				t.Fatalf("ev.Commits[0].ID = %v, want %v", got, top)
			}
			if got := ev.Commits[0].Author.Email; got != "author@example.com" {
				t.Fatalf("ev.Commits[0].Author.Email = %v, want %v", got, "author@example.com")
			}
		})
	}
}
