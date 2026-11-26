package webhook

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"charm.land/log/v2"
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

func TestGetDefaultBranch(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	path, _ := gitRepo(t)

	branch, err := getDefaultBranch(t.Context(), fakeRepo{path: path})
	is.NoErr(err)
	is.Equal(branch, "main")
}

func TestGetDefaultBranchEmptyRepository(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	empty := t.TempDir() + "/empty.git"
	_ = gitOutput(t, t.TempDir(), "init", "-q", "--bare", empty)

	// An empty repository has no branch yet: that is not an error.
	branch, err := getDefaultBranch(t.Context(), fakeRepo{path: empty})
	is.NoErr(err)
	is.Equal(branch, "")
}

// A default branch that cannot be read leaves the payload's field empty but
// is logged, not dropped silently.
func TestDefaultBranchLogsError(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs bytes.Buffer
	s := NewSender(nil, nil, nil, log.New(&logs))

	branch := s.defaultBranch(t.Context(), fakeRepo{name: "repo1", openErr: errors.New("boom")})
	is.Equal(branch, "")
	is.True(strings.Contains(logs.String(), "default branch"))
	is.True(strings.Contains(logs.String(), "repo1"))
	is.True(strings.Contains(logs.String(), "boom"))

	// An empty repository has no default branch yet: nothing to log.
	logs.Reset()
	empty := t.TempDir() + "/empty.git"
	_ = gitOutput(t, t.TempDir(), "init", "-q", "--bare", empty)
	is.Equal(s.defaultBranch(t.Context(), fakeRepo{path: empty}), "")
	is.Equal(logs.String(), "")
}

func TestGetDefaultBranchOpenErrorReturned(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	_, err := getDefaultBranch(t.Context(), fakeRepo{openErr: errors.New("boom")})
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "boom"))
}
