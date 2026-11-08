package webhook

import (
	"context"
	"fmt"
	"unicode/utf8"

	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// PushEvent is a push event.
type PushEvent struct {
	Common

	// Ref is the branch or tag name.
	Ref string `json:"ref" url:"ref"`
	// Before is the previous commit SHA.
	Before string `json:"before" url:"before"`
	// After is the current commit SHA.
	After string `json:"after" url:"after"`
	// Forced is whether the push moved the ref to a commit that does not
	// descend from the one it pointed at, dropping commits from it.
	Forced bool `json:"forced" url:"forced"`
	// Pusher is the user who pushed. It is empty for a push made without a
	// user, such as an anonymous or keyless one.
	Pusher User `json:"pusher" url:"pusher"`
	// Commits is the list of commits the push brought to the ref, newest
	// first. For a new branch or tag these are only the commits no other
	// branch or tag already had.
	Commits []Commit `json:"commits" url:"commits"`
}

// NewPushEvent sends a push event.
func NewPushEvent(ctx context.Context, user proto.User, repo proto.Repository, ref, before, after string) (PushEvent, error) {
	event := EventPush

	repository, err := newRepository(ctx, repo)
	if err != nil {
		return PushEvent{}, err
	}

	payload := PushEvent{
		Ref:    ref,
		Before: before,
		After:  after,
		Common: Common{
			EventType:  event,
			Repository: repository,
			Sender:     senderOf(user),
		},
		Pusher: senderOf(user),
	}

	// Find commits.
	r, err := repo.Open()
	if err != nil {
		return PushEvent{}, err
	}

	rev := after
	var args []string
	if git.IsZeroHash(before) {
		// A new ref lists only the commits it brings: those not reachable
		// from any other branch or tag. The pushed ref itself is left out of
		// that set, since it may already point at after when the event is
		// built. Each --exclude only applies to the --glob after it (which
		// takes nested refs too), and the second --not turns the revision
		// after it back into a positive one. Ref names cannot hold glob
		// characters, so ref matches only itself.
		args = []string{
			"--not",
			"--exclude=" + ref, "--glob=refs/heads",
			"--exclude=" + ref, "--glob=refs/tags",
			"--not",
		}
	} else {
		rev = fmt.Sprintf("%s..%s", before, after)
		payload.Forced, err = isForced(r, before, after)
		if err != nil {
			return PushEvent{}, err
		}
	}

	commits, err := r.Log(rev, gitm.LogOptions{
		// XXX: limit to 20 commits for now
		// TODO: implement a commits api
		MaxCount:       20,
		CommandOptions: gitm.CommandOptions{Args: args},
	})
	if err != nil {
		return PushEvent{}, err
	}

	payload.Commits = make([]Commit, 0, len(commits))
	size := 0
	for _, c := range commits {
		commit := Commit{
			ID:      c.ID.String(),
			Message: truncate(c.Message, maxCommitMessage, truncatedMessageMarker),
			Title:   truncate(c.Summary(), maxCommitTitle, ""),
			Author: Author{
				Name:  truncate(c.Author.Name, maxCommitIdentity, ""),
				Email: truncate(c.Author.Email, maxCommitIdentity, ""),
				Date:  c.Author.When,
			},
			Committer: Author{
				Name:  truncate(c.Committer.Name, maxCommitIdentity, ""),
				Email: truncate(c.Committer.Email, maxCommitIdentity, ""),
				Date:  c.Committer.When,
			},
			Timestamp: c.Committer.When,
		}
		// Commits are newest first. Keep the newest one whatever its size and
		// leave out older ones once the list would grow past its budget.
		size += commitSize(commit)
		if len(payload.Commits) > 0 && size > maxPushCommitsSize {
			break
		}
		payload.Commits = append(payload.Commits, commit)
	}

	return payload, nil
}

// Limits on the commits a push event lists. The payload is sent to every
// webhook of the repository and stored with each delivery, so a push of
// commits with huge messages or identities must not make it unbounded.
const (
	// maxCommitMessage is how many bytes of a commit message are kept.
	maxCommitMessage = 4 << 10
	// maxCommitTitle is how many bytes of a commit title are kept.
	maxCommitTitle = 1 << 10
	// maxCommitIdentity is how many bytes of an author or committer name or
	// email are kept.
	maxCommitIdentity = 256
	// maxPushCommitsSize is roughly how many bytes of commit text a push
	// event carries. Older commits past it are left out.
	maxPushCommitsSize = 64 << 10
)

// truncatedMessageMarker is appended to a commit message that was cut off at
// maxCommitMessage.
const truncatedMessageMarker = "\n[message truncated by Soft Serve]"

// truncate returns s cut to at most n bytes, followed by marker, when s is
// longer than n. It never cuts a UTF-8 character in half.
func truncate(s string, n int, marker string) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + marker
}

// commitSize is the number of bytes of variable-length text in c.
func commitSize(c Commit) int {
	return len(c.ID) + len(c.Message) + len(c.Title) +
		len(c.Author.Name) + len(c.Author.Email) +
		len(c.Committer.Name) + len(c.Committer.Email)
}

// isForced reports whether moving a ref from before to after drops commits:
// whether before has a commit that after does not. Tag objects are peeled to
// the commits they point at.
func isForced(r *git.Repository, before, after string) (bool, error) {
	n, err := r.RevListCount([]string{after + ".." + before}, gitm.RevListCountOptions{
		CommandOptions: gitm.CommandOptions{Args: []string{"--max-count=1"}},
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
