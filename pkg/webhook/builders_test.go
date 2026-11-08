package webhook

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/google/go-querystring/query"
	"github.com/matryer/is"
)

const zeroHash = "0000000000000000000000000000000000000000"

// ownerLess is a repository whose owner does not exist, so the owner lookup
// in every builder fails.
func (e *testEnv) ownerLess() fakeRepo {
	return fakeRepo{id: e.repo.ID, name: e.repo.Name, userID: 999, path: ""}
}

func (e *testEnv) senderRepo(path string) fakeRepo {
	return fakeRepo{id: e.repo.ID, name: e.repo.Name, userID: e.owner.ID, path: path}
}

func (e *testEnv) sender() fakeUser {
	return fakeUser{id: e.owner.ID, name: e.owner.Username}
}

func TestNewBranchTagEventCreate(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/heads/dev", zeroHash, "1111111111111111111111111111111111111111")
	is.NoErr(err)
	is.Equal(ev.EventType, EventBranchTagCreate)
	is.True(ev.Created)
	is.True(!ev.Deleted)
	is.Equal(ev.Ref, "refs/heads/dev")
	is.Equal(ev.Repository.Owner.Username, "alice")
	is.Equal(ev.Repository.HTTPURL, "http://127.0.0.1:23232/repo.git")
	is.Equal(ev.Repository.SSHURL, "ssh://127.0.0.1:23231/repo.git")
	is.Equal(ev.Repository.GitURL, "git://127.0.0.1:9418/repo.git")
	is.Equal(ev.Sender.Username, "alice")
}

func TestNewBranchTagEventDelete(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/tags/v1", "1111111111111111111111111111111111111111", zeroHash)
	is.NoErr(err)
	is.Equal(ev.EventType, EventBranchTagDelete)
	is.True(ev.Deleted)
	is.True(!ev.Created)
}

func TestNewBranchTagEventRejectsUpdate(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	// Neither side is zero: that is a plain update, not a branch or tag event.
	_, err := NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/heads/main",
		"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "invalid branch or tag event"))
}

func TestNewBranchTagEventOwnerLookupFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewBranchTagEvent(e.ctx, e.sender(), e.ownerLess(), "refs/heads/dev", zeroHash, "1111111111111111111111111111111111111111")
	is.True(err != nil)
}

func TestNewCollaboratorEvent(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	is.NoErr(e.ds.CreateUser(e.ctx, e.dbx, "bob", false, nil))
	is.NoErr(e.ds.AddCollabByUsernameAndRepo(e.ctx, e.dbx, "bob", "repo", access.ReadWriteAccess))

	ev, err := NewCollaboratorEvent(e.ctx, e.sender(), e.senderRepo(""), "bob", CollaboratorEventAdded)
	is.NoErr(err)
	is.Equal(ev.EventType, EventCollaborator)
	is.Equal(ev.Action, CollaboratorEventAdded)
	is.Equal(ev.Collaborator.Username, "bob")
	is.True(ev.Collaborator.ID != 0)
	is.Equal(ev.AccessLevel, access.ReadWriteAccess)
	is.Equal(ev.Repository.Owner.Username, "alice")
	// Collaborator events carry the same clone URLs as every other event.
	is.Equal(ev.Repository.HTTPURL, "http://127.0.0.1:23232/repo.git")
	is.Equal(ev.Repository.SSHURL, "ssh://127.0.0.1:23231/repo.git")
	is.Equal(ev.Repository.GitURL, "git://127.0.0.1:9418/repo.git")
}

func TestNewCollaboratorEventUnknownCollaborator(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewCollaboratorEvent(e.ctx, e.sender(), e.senderRepo(""), "nobody", CollaboratorEventRemoved)
	is.True(err != nil)
}

func TestNewCollaboratorEventOwnerLookupFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewCollaboratorEvent(e.ctx, e.sender(), e.ownerLess(), "alice", CollaboratorEventAdded)
	is.True(err != nil)
}

func TestNewRepositoryEventVisibilityChange(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), RepositoryEventActionVisibilityChange)
	is.NoErr(err)
	is.Equal(ev.EventType, EventRepositoryVisibilityChange)
	is.Equal(ev.Action, RepositoryEventActionVisibilityChange)
	is.Equal(ev.Repository.Owner.Username, "alice")
	is.Equal(ev.Repository.HTTPURL, "http://127.0.0.1:23232/repo.git")
	is.Equal(ev.Repository.SSHURL, "ssh://127.0.0.1:23231/repo.git")
	is.Equal(ev.Repository.GitURL, "git://127.0.0.1:9418/repo.git")
}

func TestNewRepositoryEventOtherActions(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	for _, action := range []RepositoryEventAction{
		RepositoryEventActionDelete,
		RepositoryEventActionRename,
		RepositoryEventActionDefaultBranchChange,
	} {
		ev, err := NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), action)
		is.NoErr(err)
		is.Equal(ev.EventType, EventRepository)
		is.Equal(ev.Action, action)
	}
}

// Events other than renames and default branch changes leave changes out.
func TestNewRepositoryEventNoChanges(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), RepositoryEventActionDelete)
	is.NoErr(err)
	is.True(ev.Changes == nil)
	b, err := json.Marshal(ev)
	is.NoErr(err)
	is.True(!strings.Contains(string(b), `"changes"`))
	v, err := query.Values(ev)
	is.NoErr(err)
	for k := range v {
		is.True(!strings.HasPrefix(k, "changes"))
	}
}

func TestNewRepositoryRenameEvent(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := NewRepositoryRenameEvent(e.ctx, e.sender(), e.senderRepo(""), "old-name")
	is.NoErr(err)
	is.Equal(ev.EventType, EventRepository)
	is.Equal(ev.Action, RepositoryEventActionRename)
	b, err := json.Marshal(ev)
	is.NoErr(err)
	is.True(strings.Contains(string(b), `"changes":{"name":{"from":"old-name"}}`))
	v, err := query.Values(ev)
	is.NoErr(err)
	is.Equal(v.Get("changes[name][from]"), "old-name")
	is.Equal(v.Get("changes[default_branch][from]"), "")
}

func TestNewDefaultBranchChangeEvent(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := NewDefaultBranchChangeEvent(e.ctx, e.sender(), e.senderRepo(""), "master")
	is.NoErr(err)
	is.Equal(ev.EventType, EventRepository)
	is.Equal(ev.Action, RepositoryEventActionDefaultBranchChange)
	b, err := json.Marshal(ev)
	is.NoErr(err)
	is.True(strings.Contains(string(b), `"changes":{"default_branch":{"from":"master"}}`))
	v, err := query.Values(ev)
	is.NoErr(err)
	is.Equal(v.Get("changes[default_branch][from]"), "master")
}

func TestRepositoryChangeEventsOwnerLookupFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewRepositoryRenameEvent(e.ctx, e.sender(), e.ownerLess(), "old")
	is.True(err != nil)
	_, err = NewDefaultBranchChangeEvent(e.ctx, e.sender(), e.ownerLess(), "old")
	is.True(err != nil)
}

func TestNewRepositoryEventOwnerLookupFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewRepositoryEvent(e.ctx, e.sender(), e.ownerLess(), RepositoryEventActionDelete)
	is.True(err != nil)
}

func TestNewPushEventFirstPush(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
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
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	first := gitOutput(t, path, "rev-parse", "main~1")

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", first, head)
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
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	dev := commitOn(t, path, head, "dev commit")

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/dev", zeroHash, dev)
	is.NoErr(err)
	is.Equal(len(ev.Commits), 1)
	is.Equal(ev.Commits[0].ID, dev)
	is.True(!ev.Forced)

	runGit(t, path, "update-ref", "refs/heads/dev", dev)
	ev, err = NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/dev", zeroHash, dev)
	is.NoErr(err)
	is.Equal(len(ev.Commits), 1)
	is.Equal(ev.Commits[0].ID, dev)

	// Commits a tag already has are not new either.
	runGit(t, path, "update-ref", "refs/heads/dev", head)
	runGit(t, path, "update-ref", "refs/tags/v1", dev)
	ev, err = NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/other", zeroHash, dev)
	is.NoErr(err)
	is.Equal(len(ev.Commits), 0)
}

// A new branch at a commit another branch already has brings no commits.
func TestNewPushEventNewBranchAtExistingCommit(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	runGit(t, path, "update-ref", "refs/heads/copy", head)

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/copy", zeroHash, head)
	is.NoErr(err)
	is.Equal(len(ev.Commits), 0)
	// An empty list, not null.
	is.True(ev.Commits != nil)
}

func TestNewPushEventForced(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	first := gitOutput(t, path, "rev-parse", "main~1")
	other := commitOn(t, path, first, "rewritten second")
	unrelated := commitOn(t, path, "", "unrelated root")

	// A fast-forward is not forced.
	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", first, head)
	is.NoErr(err)
	is.True(!ev.Forced)

	// Rewriting the last commit is.
	ev, err = NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, other)
	is.NoErr(err)
	is.True(ev.Forced)
	is.Equal(len(ev.Commits), 1)
	is.Equal(ev.Commits[0].ID, other)

	// So is moving back, and moving to an unrelated history.
	ev, err = NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, first)
	is.NoErr(err)
	is.True(ev.Forced)
	ev, err = NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, unrelated)
	is.NoErr(err)
	is.True(ev.Forced)
}

// Annotated tags are compared by the commits they point at.
func TestNewPushEventForcedAnnotatedTag(t *testing.T) {
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

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/tags/v1", older, newer)
	is.NoErr(err)
	is.True(!ev.Forced)

	ev, err = NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/tags/v1", newer, older)
	is.NoErr(err)
	is.True(ev.Forced)
}

func TestNewPushEventPusher(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	is.NoErr(err)
	is.Equal(ev.Pusher.Username, "alice")
	is.Equal(ev.Pusher.ID, e.owner.ID)

	// A push without a user has an empty pusher.
	ev, err = NewPushEvent(e.ctx, nil, e.senderRepo(path), "refs/heads/main", zeroHash, head)
	is.NoErr(err)
	is.Equal(ev.Pusher, User{})
}

// A push that deletes a branch or tag lists no commits, since there is no
// revision range to walk, and says the ref was deleted.
func TestNewPushEventDelete(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", head, zeroHash)
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
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	first := gitOutput(t, path, "rev-parse", "main~1")

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	is.NoErr(err)
	is.True(ev.Created)
	is.True(!ev.Deleted)

	ev, err = NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", first, head)
	is.NoErr(err)
	is.True(!ev.Created)
	is.True(!ev.Deleted)
}

func TestNewPushEventUnknownAfterFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	path, _ := gitRepo(t)

	_, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, "3333333333333333333333333333333333333333")
	is.True(err != nil)
}

func TestNewPushEventOpenFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	repo := e.senderRepo("")
	repo.openErr = errors.New("cannot open repository")

	_, err := NewPushEvent(e.ctx, e.sender(), repo, "refs/heads/main", zeroHash, "1111111111111111111111111111111111111111")
	is.True(err != nil)
}

func TestNewPushEventOwnerLookupFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewPushEvent(e.ctx, e.sender(), e.ownerLess(), "refs/heads/main", zeroHash, "1111111111111111111111111111111111111111")
	is.True(err != nil)
}

func TestGetDefaultBranch(t *testing.T) {
	is := is.New(t)
	path, _ := gitRepo(t)

	branch, err := getDefaultBranch(fakeRepo{path: path})
	is.NoErr(err)
	is.Equal(branch, "main")
}

func TestGetDefaultBranchEmptyRepository(t *testing.T) {
	is := is.New(t)
	empty := t.TempDir() + "/empty.git"
	_ = gitOutput(t, t.TempDir(), "init", "-q", "--bare", empty)

	// An empty repository has no branch yet: that is not an error.
	branch, err := getDefaultBranch(fakeRepo{path: empty})
	is.NoErr(err)
	is.Equal(branch, "")
}

func TestGetDefaultBranchOpenErrorReturned(t *testing.T) {
	is := is.New(t)

	_, err := getDefaultBranch(fakeRepo{openErr: errors.New("boom")})
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "boom"))
}
