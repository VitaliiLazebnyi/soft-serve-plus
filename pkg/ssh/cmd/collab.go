package cmd

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

func collabCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "collab",
		Aliases: []string{"collabs", "collaborator", "collaborators"},
		Short:   "Manage collaborators",
	}

	cmd.AddCommand(
		collabAddCommand(),
		collabRemoveCommand(),
		collabListCommand(),
	)

	return cmd
}

// checkCollabManage reports whether the caller may add or remove
// collaborators on the gated repository. The command gate already requires
// admin access to the repository; this additionally refuses an anonymous
// caller, who only reaches admin access through anon-access, since managing
// collaborators is an account-level decision.
//
// Collaborator management needs admin access rather than write access: a
// read-write collaborator could otherwise remove or demote peers, or, with
// anon-access set to no-access, lock any registered user out of a public
// repository by adding them as a no-access collaborator.
func checkCollabManage(ctx context.Context) error {
	if isServerAdmin(ctx) {
		return nil
	}

	if proto.UserFromContext(ctx) == nil {
		return proto.ErrUnauthorized
	}

	return nil
}

func collabAddCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "add REPOSITORY USERNAME [LEVEL]",
		Short:             "Add a collaborator to a repo",
		Long:              "Add a collaborator to a repo. LEVEL can be one of: no-access, read-only, read-write, or admin-access. Defaults to read-write. Requires admin access to the repo.",
		Args:              cobra.RangeArgs(2, 3),
		PersistentPreRunE: requireRepo(access.AdminAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			repo := repoArg(args)
			username := args[1]
			level := access.ReadWriteAccess
			if len(args) > 2 {
				level = access.ParseAccessLevel(args[2])
				if level < 0 {
					return access.ErrInvalidAccessLevel
				}
			}

			if err := checkCollabManage(ctx); err != nil {
				return err
			}

			return be.AddCollaborator(ctx, repo, username, level)
		},
	}

	return cmd
}

func collabRemoveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "remove REPOSITORY USERNAME",
		Args:              cobra.ExactArgs(2),
		Short:             "Remove a collaborator from a repo",
		PersistentPreRunE: requireRepo(access.AdminAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			repo := repoArg(args)
			username := args[1]

			if err := checkCollabManage(ctx); err != nil {
				return err
			}

			return be.RemoveCollaborator(ctx, repo, username)
		},
	}

	return cmd
}

func collabListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "list REPOSITORY",
		Short:             "List collaborators and their access levels for a repo",
		Args:              cobra.ExactArgs(1),
		PersistentPreRunE: requireRepo(access.ReadWriteAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			repo := repoArg(args)
			collabs, err := be.Collaborators(ctx, repo)
			if err != nil {
				return err
			}

			for _, c := range collabs {
				cmd.Printf("%s\t%s\n", c.Username, c.AccessLevel)
			}

			return nil
		},
	}

	return cmd
}
