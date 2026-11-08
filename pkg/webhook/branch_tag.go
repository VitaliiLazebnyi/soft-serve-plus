package webhook

import (
	"context"
	"fmt"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// BranchTagEvent is a branch or tag event.
type BranchTagEvent struct {
	Common

	// Ref is the branch or tag name.
	Ref string `json:"ref" url:"ref"`
	// Before is the previous commit SHA.
	Before string `json:"before" url:"before"`
	// After is the current commit SHA.
	After string `json:"after" url:"after"`
	// Created is whether the branch or tag was created.
	Created bool `json:"created" url:"created"`
	// Deleted is whether the branch or tag was deleted.
	Deleted bool `json:"deleted" url:"deleted"`
}

// NewBranchTagEvent sends a branch or tag event.
func NewBranchTagEvent(ctx context.Context, user proto.User, repo proto.Repository, ref, before, after string) (BranchTagEvent, error) {
	var event Event
	if git.IsZeroHash(before) {
		event = EventBranchTagCreate
	} else if git.IsZeroHash(after) {
		event = EventBranchTagDelete
	} else {
		return BranchTagEvent{}, fmt.Errorf("invalid branch or tag event: before=%q after=%q", before, after)
	}

	repository, err := newRepository(ctx, repo)
	if err != nil {
		return BranchTagEvent{}, err
	}

	payload := BranchTagEvent{
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
	}

	return payload, nil
}
