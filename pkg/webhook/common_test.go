package webhook

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"charm.land/log/v2"
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
	path, _ := gitRepo(t)

	branch, err := getDefaultBranch(t.Context(), fakeRepo{path: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := branch; got != "main" {
		t.Fatalf("branch = %v, want %v", got, "main")
	}
}

func TestGetDefaultBranchEmptyRepository(t *testing.T) {
	t.Parallel()
	empty := t.TempDir() + "/empty.git"
	_ = gitOutput(t, t.TempDir(), "init", "-q", "--bare", empty)

	// An empty repository has no branch yet: that is not an error.
	branch, err := getDefaultBranch(t.Context(), fakeRepo{path: empty})
	if err != nil {
		t.Fatal(err)
	}
	if got := branch; got != "" {
		t.Fatalf("branch = %v, want %v", got, "")
	}
}

// A default branch that cannot be read leaves the payload's field empty but
// is logged, not dropped silently.
func TestDefaultBranchLogsError(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := NewSender(nil, nil, nil, log.New(&logs))

	branch := s.defaultBranch(t.Context(), fakeRepo{name: "repo1", openErr: errors.New("boom")})
	if got := branch; got != "" {
		t.Fatalf("branch = %v, want %v", got, "")
	}
	if !strings.Contains(logs.String(), "default branch") {
		t.Fatal("not true: strings.Contains(logs.String(), \"default branch\")")
	}
	if !strings.Contains(logs.String(), "repo1") {
		t.Fatal("not true: strings.Contains(logs.String(), \"repo1\")")
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Fatal("not true: strings.Contains(logs.String(), \"boom\")")
	}

	// An empty repository has no default branch yet: nothing to log.
	logs.Reset()
	empty := t.TempDir() + "/empty.git"
	_ = gitOutput(t, t.TempDir(), "init", "-q", "--bare", empty)
	if got := s.defaultBranch(t.Context(), fakeRepo{path: empty}); got != "" {
		t.Fatalf("s.defaultBranch(t.Context(), fakeRepo{path: empty}) = %v, want %v", got, "")
	}
	if got := logs.String(); got != "" {
		t.Fatalf("logs.String() = %v, want %v", got, "")
	}
}

func TestGetDefaultBranchOpenErrorReturned(t *testing.T) {
	t.Parallel()

	_, err := getDefaultBranch(t.Context(), fakeRepo{openErr: errors.New("boom")})
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatal("not true: strings.Contains(err.Error(), \"boom\")")
	}
}
