package cmd

import (
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

func deleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "delete REPOSITORY",
		Aliases: []string{"del", "remove", "rm"},
		Short:   "Delete a repository",
		Args:    cobra.ExactArgs(1),
		RunE: withRepoOrName(access.AdminAccess, func(cmd *cobra.Command, args []string, _ proto.Repository, _ access.AccessLevel) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			return be.DeleteRepository(ctx, repoArg(args))
		}),
	}

	return cmd
}
