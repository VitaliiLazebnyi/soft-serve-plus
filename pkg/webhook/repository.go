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
	// Changes holds the previous values of what the event changed. It is
	// set for renames and default branch changes, and left out otherwise.
	Changes *RepositoryChanges `json:"changes,omitempty" url:"changes,omitempty"`
}

// RepositoryChanges holds the previous values of what a repository event
// changed. Only the changed fields are set.
type RepositoryChanges struct {
	// Name is the repository name before a rename.
	Name *ChangeFrom `json:"name,omitempty" url:"name,omitempty"`
	// DefaultBranch is the default branch before it was changed. Its From
	// is empty when HEAD did not name a branch before.
	DefaultBranch *ChangeFrom `json:"default_branch,omitempty" url:"default_branch,omitempty"`
}

// ChangeFrom is the previous value of a changed field.
type ChangeFrom struct {
	// From is the previous value.
	From string `json:"from" url:"from"`
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

// NewRepositoryRenameEvent builds the payload for a repository rename. repo
// is the renamed repository, and oldName the name it had before.
func NewRepositoryRenameEvent(ctx context.Context, user proto.User, repo proto.Repository, oldName string) (RepositoryEvent, error) {
	payload, err := NewRepositoryEvent(ctx, user, repo, RepositoryEventActionRename)
	if err != nil {
		return RepositoryEvent{}, err
	}
	payload.Changes = &RepositoryChanges{Name: &ChangeFrom{From: oldName}}
	return payload, nil
}

// NewDefaultBranchChangeEvent builds the payload for a default branch change.
// oldBranch is the default branch before the change, as a short branch name.
func NewDefaultBranchChangeEvent(ctx context.Context, user proto.User, repo proto.Repository, oldBranch string) (RepositoryEvent, error) {
	payload, err := NewRepositoryEvent(ctx, user, repo, RepositoryEventActionDefaultBranchChange)
	if err != nil {
		return RepositoryEvent{}, err
	}
	payload.Changes = &RepositoryChanges{DefaultBranch: &ChangeFrom{From: oldBranch}}
	return payload, nil
}
