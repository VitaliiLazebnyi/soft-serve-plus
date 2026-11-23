package git

import (
	"context"

	"github.com/aymanbagabas/git-module"
)

// RunInDirOptions are options for RunInDir.
type RunInDirOptions = git.RunInDirOptions

// NewCommand creates a new git command that is killed when ctx is done.
func NewCommand(ctx context.Context, args ...string) *git.Command {
	return git.NewCommandWithContext(ctx, args...)
}
