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
	// Commits is the list of commits.
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
	}

	// Find commits.
	r, err := repo.Open()
	if err != nil {
		return PushEvent{}, err
	}

	rev := after
	if !git.IsZeroHash(before) {
		rev = fmt.Sprintf("%s..%s", before, after)
	}

	commits, err := r.Log(rev, gitm.LogOptions{
		// XXX: limit to 20 commits for now
		// TODO: implement a commits api
		MaxCount: 20,
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
