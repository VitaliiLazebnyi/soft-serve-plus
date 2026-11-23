package cmd

import (
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

func (cmds *Commands) deleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "delete REPOSITORY",
		Aliases: []string{"del", "remove", "rm"},
		Short:   "Delete a repository",
		Args:    cobra.ExactArgs(1),
		RunE: cmds.withRepoOrName(access.AdminAccess, func(cmd *cobra.Command, args []string, rr proto.Repository, _ access.AccessLevel) error {
			ctx, err := cmds.eventSenderContext(cmd.Context())
			if err != nil {
				return err
			}
			be := cmds.be
			// A server admin may delete a row with no repository
			// directory, which only the name can reach.
			if rr == nil {
				return be.DeleteRepository(ctx, repoArg(args))
			}
			return be.DeleteResolvedRepository(ctx, rr)
		}),
	}

	return cmd
}
