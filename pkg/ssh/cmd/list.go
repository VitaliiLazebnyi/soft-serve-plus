package cmd

import (
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

// listCommand returns a command that list file or directory at path.
func (cmds *Commands) listCommand() *cobra.Command {
	var all bool

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List repositories",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			be := cmds.be
			// The session user, not a fresh lookup of the key: a lookup
			// that failed would list what an anonymous caller sees.
			repos, err := be.ReadableRepositoriesForUser(ctx, proto.UserFromContext(ctx))
			if err != nil {
				return lookupFailed(ctx, cmds.logger, "list repositories", err)
			}
			for _, r := range repos {
				if !r.IsHidden() || all {
					cmd.Println(r.Name())
				}
			}
			return nil
		},
	}

	listCmd.Flags().BoolVarP(&all, "all", "a", false, "List all repositories")

	return listCmd
}
