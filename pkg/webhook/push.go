package webhook

import (
	"context"
	"fmt"

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
	// Created is whether the push created the branch or tag.
	Created bool `json:"created" url:"created"`
	// Deleted is whether the push deleted the branch or tag.
	Deleted bool `json:"deleted" url:"deleted"`
	// Forced is whether the push moved the ref to a commit that does not
	// descend from the one it pointed at, dropping commits from it.
	Forced bool `json:"forced" url:"forced"`
	// Pusher is the user who pushed. It is empty for a push made without a
	// user, such as an anonymous or keyless one.
	Pusher User `json:"pusher" url:"pusher"`
	// Commits is the list of commits the push brought to the ref, newest
	// first. For a new branch or tag these are only the commits no other
	// branch or tag already had. It is empty when the push deleted the
	// branch or tag.
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
		Ref:     ref,
		Before:  before,
		After:   after,
		Created: git.IsZeroHash(before),
		Deleted: git.IsZeroHash(after),
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

	// A deleted branch or tag has no new commits, and "before..0000" is not
	// a revision git can list.
	if payload.Deleted {
		payload.Commits = []Commit{}
		return payload, nil
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

	payload.Commits = make([]Commit, len(commits))
	for i, c := range commits {
		payload.Commits[i] = Commit{
			ID:      c.ID.String(),
			Message: c.Message,
			Title:   c.Summary(),
			Author: Author{
				Name:  c.Author.Name,
				Email: c.Author.Email,
				Date:  c.Author.When,
			},
			Committer: Author{
				Name:  c.Committer.Name,
				Email: c.Committer.Email,
				Date:  c.Committer.When,
			},
			Timestamp: c.Committer.When,
		}
	}

	return payload, nil
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
