package cmd

import (
	"fmt"
	"strconv"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/spf13/cobra"
)

func hiddenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "hidden REPOSITORY [true|false]",
		Short:             "Hide or unhide a repository",
		Aliases:           []string{"hide"},
		Args:              cobra.RangeArgs(1, 2),
		PersistentPreRunE: requireRepo(access.ReadOnlyAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			rr, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			switch len(args) {
			case 1:
				cmd.Println(rr.IsHidden())
			case 2:
				if err := requireLevel(ctx, access.ReadWriteAccess); err != nil {
					return err
				}

				// Read anything but a boolean as false would unhide the
				// repository on a typo such as "True " or "yes".
				hidden, err := strconv.ParseBool(args[1])
				if err != nil {
					return fmt.Errorf("invalid hidden value %q: want true or false", args[1])
				}
				if err := be.SetHidden(ctx, rr.Name(), hidden); err != nil {
					return err
				}
			}

			return nil
		},
	}

	return cmd
}
