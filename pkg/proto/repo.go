// Package proto defines the types the backend and the transports share:
// repositories, users, access tokens, repository names, and the errors
// reported to clients.
package proto

import (
	"context"
	"time"

	"github.com/charmbracelet/soft-serve/git"
)

// Repository is a Git repository interface.
type Repository interface {
	// ID returns the repository's ID.
	ID() int64
	// Name returns the repository's name.
	Name() string
	// ProjectName returns the repository's project name.
	ProjectName() string
	// Description returns the repository's description.
	Description() string
	// IsPrivate returns whether the repository is private.
	IsPrivate() bool
	// IsMirror returns whether the repository is a mirror.
	IsMirror() bool
	// IsHidden returns whether the repository is hidden.
	IsHidden() bool
	// UserID returns the ID of the user who owns the repository. Every
	// repository has an owner: one created without a user belongs to the
	// default admin.
	UserID() int64
	// CreatedAt returns the time the repository was created.
	CreatedAt() time.Time
	// UpdatedAt returns the time the repository was last updated.
	// If the repository has never been updated, it returns the time it was created.
	UpdatedAt() time.Time
	// Open returns the underlying git.Repository, whose git commands run
	// under ctx (see git.Open).
	Open(ctx context.Context) (*git.Repository, error)
}

// RepositoryOptions are options for creating a new repository.
type RepositoryOptions struct {
	Private     bool
	Description string
	ProjectName string
	Mirror      bool
	Hidden      bool
	LFS         bool
	LFSEndpoint string
}

// RepositoryDefaultBranch returns the default branch of a repository, read
// under ctx.
func RepositoryDefaultBranch(ctx context.Context, repo Repository) (string, error) {
	r, err := repo.Open(ctx)
	if err != nil {
		return "", err
	}

	ref, err := r.HEAD()
	if err != nil {
		return "", err
	}

	return ref.Name().Short(), nil
}
