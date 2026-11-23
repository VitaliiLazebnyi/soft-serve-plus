package cmd

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

func (cmds *Commands) collabCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "collab",
		Aliases: []string{"collabs", "collaborator", "collaborators"},
		Short:   "Manage collaborators",
	}

	cmd.AddCommand(
		cmds.collabAddCommand(),
		cmds.collabRemoveCommand(),
		cmds.collabListCommand(),
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
func (cmds *Commands) checkCollabManage(ctx context.Context) error {
	if cmds.isServerAdmin(ctx) {
		return nil
	}

	if proto.UserFromContext(ctx) == nil {
		return proto.ErrUnauthorized
	}

	return nil
}

func (cmds *Commands) collabAddCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add REPOSITORY USERNAME [LEVEL]",
		Short: "Add a collaborator to a repo",
		Long:  "Add a collaborator to a repo. LEVEL can be one of: no-access, read-only, read-write, or admin-access. Defaults to read-write. Requires admin access to the repo.",
		Args:  cobra.RangeArgs(2, 3),
		RunE: cmds.withRepoOrName(access.AdminAccess, func(cmd *cobra.Command, args []string, rr proto.Repository, _ access.Level) error {
			ctx := cmd.Context()
			be := cmds.be
			username := args[1]
			level := access.ReadWriteAccess
			if len(args) > 2 {
				var err error
				if level, err = access.ParseLevel(args[2]); err != nil {
					return err
				}
			}

			if err := cmds.checkCollabManage(ctx); err != nil {
				return err
			}
			ctx, err := cmds.eventSenderContext(ctx)
			if err != nil {
				return err
			}

			if rr == nil {
				return be.AddCollaborator(ctx, repoArg(args), username, level)
			}
			return be.AddResolvedCollaborator(ctx, rr, username, level)
		}),
	}

	return cmd
}

func (cmds *Commands) collabRemoveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove REPOSITORY USERNAME",
		Args:  cobra.ExactArgs(2),
		Short: "Remove a collaborator from a repo",
		RunE: cmds.withRepoOrName(access.AdminAccess, func(cmd *cobra.Command, args []string, rr proto.Repository, _ access.Level) error {
			ctx := cmd.Context()
			be := cmds.be
			username := args[1]

			if err := cmds.checkCollabManage(ctx); err != nil {
				return err
			}
			ctx, err := cmds.eventSenderContext(ctx)
			if err != nil {
				return err
			}

			if rr == nil {
				return be.RemoveCollaborator(ctx, repoArg(args), username)
			}
			return be.RemoveResolvedCollaborator(ctx, rr, username)
		}),
	}

	return cmd
}

func (cmds *Commands) collabListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list REPOSITORY",
		Short: "List collaborators and their access levels for a repo",
		Args:  cobra.ExactArgs(1),
		RunE: cmds.withRepoOrName(access.ReadWriteAccess, func(cmd *cobra.Command, args []string, _ proto.Repository, _ access.Level) error {
			ctx := cmd.Context()
			be := cmds.be
			repo := repoArg(args)
			collabs, err := be.Collaborators(ctx, repo)
			if err != nil {
				return err
			}

			for _, c := range collabs {
				cmd.Printf("%s\t%s\n", c.Username, c.AccessLevel)
			}

			return nil
		}),
	}

	return cmd
}
