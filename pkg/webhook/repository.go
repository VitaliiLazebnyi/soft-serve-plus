package webhook

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// RepositoryEvent is a repository payload.
type RepositoryEvent struct {
	Common

	// Action is the repository event action.
	Action RepositoryEventAction `json:"action" url:"action"`
}

// RepositoryEventAction is a repository event action.
type RepositoryEventAction string

const (
	// RepositoryEventActionDelete is a repository deleted event.
	RepositoryEventActionDelete RepositoryEventAction = "delete"
	// RepositoryEventActionRename is a repository renamed event.
	RepositoryEventActionRename RepositoryEventAction = "rename"
	// RepositoryEventActionVisibilityChange is a repository visibility changed event.
	RepositoryEventActionVisibilityChange RepositoryEventAction = "visibility_change"
	// RepositoryEventActionDefaultBranchChange is a repository default branch changed event.
	RepositoryEventActionDefaultBranchChange RepositoryEventAction = "default_branch_change"
)

// NewRepositoryEvent sends a repository event.
func NewRepositoryEvent(ctx context.Context, user proto.User, repo proto.Repository, action RepositoryEventAction) (RepositoryEvent, error) {
	var event Event
	switch action {
	case RepositoryEventActionVisibilityChange:
		event = EventRepositoryVisibilityChange
	default:
		event = EventRepository
	}

	repository, err := newRepository(ctx, repo)
	if err != nil {
		return RepositoryEvent{}, err
	}

	payload := RepositoryEvent{
		Action: action,
		Common: Common{
			EventType:  event,
			Repository: repository,
			Sender:     senderOf(user),
		},
	}

	return payload, nil
}
