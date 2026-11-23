package cmd

import (
	"errors"
	"fmt"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/termtext"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

func (cmds *Commands) branchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "branch",
		Short: "Manage repository branches",
	}

	cmd.AddCommand(
		cmds.branchListCommand(),
		cmds.branchDefaultCommand(),
		cmds.branchDeleteCommand(),
	)

	return cmd
}

func (cmds *Commands) branchListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list REPOSITORY",
		Short: "List repository branches",
		Args:  cobra.ExactArgs(1),
		RunE: cmds.withRepo(access.ReadOnlyAccess, func(cmd *cobra.Command, _ []string, rr proto.Repository, _ access.Level) error {
			r, err := cmds.openRepository(cmd.Context(), rr)
			if err != nil {
				return err
			}

			branches, err := r.Branches()
			if err != nil {
				return cmds.gitFailed(cmd.Context(), "list branches", rr.Name(), err)
			}
			for _, b := range branches {
				// A ref name cannot hold C0 controls, but it can hold
				// C1 ones encoded as UTF-8.
				cmd.Println(termtext.Sanitize(b))
			}

			return nil
		}),
	}

	return cmd
}

func (cmds *Commands) branchDefaultCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "default REPOSITORY [BRANCH]",
		Short: "Set or get the default branch",
		Args:  cobra.RangeArgs(1, 2),
		RunE: cmds.withRepo(access.ReadOnlyAccess, func(cmd *cobra.Command, args []string, rr proto.Repository, level access.Level) error {
			ctx := cmd.Context()

			r, err := cmds.openRepository(cmd.Context(), rr)
			if err != nil {
				return err
			}

			switch len(args) {
			case 1:
				head, err := cmds.repoHead(cmd.Context(), rr, r)
				if err != nil {
					return err
				}

				cmd.Println(termtext.Sanitize(head.Name().Short()))
			case 2:
				if level < access.ReadWriteAccess {
					return proto.ErrUnauthorized
				}
				ctx, err := cmds.eventSenderContext(ctx)
				if err != nil {
					return err
				}

				branch := args[1]
				branches, err := r.Branches()
				if err != nil {
					return cmds.gitFailed(ctx, "list branches", rr.Name(), err)
				}
				var exists bool
				for _, b := range branches {
					if branch == b {
						exists = true
						break
					}
				}

				if !exists {
					return git.ErrReferenceNotExist
				}

				// The default branch before the change, for the event. HEAD
				// can name a branch that does not exist yet, so it is read
				// as a symbolic ref rather than resolved.
				// A failure to read it only leaves the event without the
				// old branch, so it is logged and the change goes on.
				oldHead, err := r.SymbolicRef(git.HEAD, "")
				if err != nil {
					cmds.logger.Warn("failed to read the default branch before changing it", "err", err, "repo", rr.Name())
				}
				oldBranch := git.ReferenceName(oldHead).Short()

				if _, err := r.SymbolicRef(git.HEAD, git.RefsHeads+branch); err != nil {
					return cmds.gitFailed(ctx, "set default branch", rr.Name(), err)
				}

				// The default branch is already changed, so a failed event is
				// logged rather than reported as a failed command. The event
				// is built and sent even if the session ends now, in the
				// background like the backend's own events.
				if err := cmds.be.SendDefaultBranchChangeEvent(ctx, proto.UserFromContext(ctx), rr, oldBranch); err != nil {
					cmds.logger.Error("default branch changed but webhook event skipped", "err", err, "repo", rr.Name())
				}
				return nil
			}

			return nil
		}),
	}

	return cmd
}

func (cmds *Commands) branchDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "delete REPOSITORY BRANCH",
		Aliases: []string{"remove", "rm", "del"},
		Short:   "Delete a branch",
		Args:    cobra.ExactArgs(2),
		RunE: cmds.withRepo(access.ReadWriteAccess, func(cmd *cobra.Command, args []string, rr proto.Repository, _ access.Level) error {
			ctx, err := cmds.eventSenderContext(cmd.Context())
			if err != nil {
				return err
			}
			if rr.IsMirror() {
				return proto.ErrMirrorReadOnly
			}

			r, err := cmds.openRepository(cmd.Context(), rr)
			if err != nil {
				return err
			}

			branch := args[1]
			branches, err := r.Branches()
			if err != nil {
				return cmds.gitFailed(ctx, "list branches", rr.Name(), err)
			}
			var exists bool
			for _, b := range branches {
				if branch == b {
					exists = true
					break
				}
			}

			if !exists {
				return git.ErrReferenceNotExist
			}

			// A HEAD that names a branch that does not exist cannot be
			// the branch being deleted, which does exist.
			head, err := r.HEAD()
			if err != nil && !errors.Is(err, git.ErrReferenceNotExist) {
				return cmds.gitFailed(ctx, "read HEAD", rr.Name(), err)
			}

			if head != nil && head.Name().Short() == branch {
				return fmt.Errorf("cannot delete the default branch")
			}

			// The event needs only the ID. Reading the commit would
			// fail for one git cannot parse, and such a branch must
			// still be deletable. The branch was just listed, so a
			// failure to read or delete it is the server's: it is
			// logged, and the client gets a generic error. That
			// includes a delete of the same branch that another
			// session finished in between.
			branchID, err := r.ShowRefVerify(git.RefsHeads + branch)
			if err != nil {
				return cmds.gitFailed(ctx, "read branch", rr.Name(), err)
			}

			if err := r.DeleteBranch(branch); err != nil {
				return cmds.gitFailed(ctx, "delete branch", rr.Name(), err)
			}

			// The branch is already deleted, so a failed event is logged
			// rather than reported as a failed command. The event is built
			// and sent even if the session ends now, in the background like
			// the backend's own events.
			if err := cmds.be.SendBranchTagEvent(ctx, proto.UserFromContext(ctx), rr, git.RefsHeads+branch, branchID, git.ZeroID); err != nil {
				cmds.logger.Error("branch deleted but webhook event skipped", "err", err, "repo", rr.Name())
			}
			return nil
		}),
	}

	return cmd
}
