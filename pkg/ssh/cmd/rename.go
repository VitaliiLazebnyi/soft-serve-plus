package cmd

import (
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

func (cmds *Commands) renameCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rename REPOSITORY NEW_NAME",
		Aliases: []string{"mv", "move"},
		Short:   "Rename an existing repository",
		Args:    cobra.ExactArgs(2),
		RunE: cmds.withRepoOrName(access.AdminAccess, func(cmd *cobra.Command, args []string, rr proto.Repository, _ access.Level) error {
			ctx := cmd.Context()
			be := cmds.be
			oldName := repoArg(args)
			newName := proto.SanitizeRepo(args[1])

			// The destination is gated like create: it may already be
			// somebody else's repository.
			level, err := cmds.repoAccessLevel(ctx, newName)
			if err != nil {
				return err
			}
			if level < access.ReadWriteAccess {
				return proto.ErrUnauthorized
			}

			// A server admin may rename a row with no repository
			// directory, which only the name can reach.
			if rr == nil {
				return cmds.createFailed(ctx, "rename repository", be.RenameRepository(ctx, oldName, newName))
			}
			return cmds.createFailed(ctx, "rename repository", be.RenameResolvedRepository(ctx, rr, newName))
		}),
	}

	return cmd
}
