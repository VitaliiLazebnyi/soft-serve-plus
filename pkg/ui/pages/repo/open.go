package repo

import (
	"context"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// openWithContext opens repo with its git commands bound to ctx, the session's
// context, so that they stop when the client disconnects instead of running
// until git-module's one-minute default timeout.
func openWithContext(ctx context.Context, repo proto.Repository) (*git.Repository, error) {
	r, err := repo.Open()
	if err != nil {
		return nil, err
	}
	return r.WithContext(ctx), nil
}
