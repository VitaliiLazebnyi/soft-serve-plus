package git

import (
	"context"

	"github.com/aymanbagabas/git-module"
)

// RunInDirOptions are options for RunInDir.
type RunInDirOptions = git.RunInDirOptions

// NewCommand creates a new git command.
func NewCommand(args ...string) *git.Command {
	return git.NewCommand(args...)
}

// NewCommandWithContext creates a new git command that is killed when ctx is
// done.
func NewCommandWithContext(ctx context.Context, args ...string) *git.Command {
	return git.NewCommandWithContext(ctx, args...)
}
