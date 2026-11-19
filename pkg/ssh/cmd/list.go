package cmd

import (
	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

// listCommand returns a command that list file or directory at path.
func listCommand() *cobra.Command {
	var all bool

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List repositories",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			// The session user, not a fresh lookup of the key: a lookup
			// that failed would list what an anonymous caller sees.
			repos, err := be.ReadableRepositoriesForUser(ctx, proto.UserFromContext(ctx))
			if err != nil {
				return lookupFailed(ctx, log.FromContext(ctx), "list repositories", err)
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
