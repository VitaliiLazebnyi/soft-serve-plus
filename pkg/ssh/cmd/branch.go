package cmd

import (
	"errors"
	"fmt"

	"charm.land/log/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/spf13/cobra"
)

func branchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "branch",
		Short: "Manage repository branches",
	}

	cmd.AddCommand(
		branchListCommand(),
		branchDefaultCommand(),
		branchDeleteCommand(),
	)

	return cmd
}

func branchListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list REPOSITORY",
		Short: "List repository branches",
		Args:  cobra.ExactArgs(1),
		RunE: withRepo(access.ReadOnlyAccess, func(cmd *cobra.Command, _ []string, rr proto.Repository, _ access.AccessLevel) error {
			r, err := rr.Open()
			if err != nil {
				return err
			}

			branches, _ := r.Branches()
			for _, b := range branches {
				// A ref name cannot hold C0 controls, but it can hold
				// C1 ones encoded as UTF-8.
				cmd.Println(utils.Sanitize(b))
			}

			return nil
		}),
	}

	return cmd
}

func branchDefaultCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "default REPOSITORY [BRANCH]",
		Short: "Set or get the default branch",
		Args:  cobra.RangeArgs(1, 2),
		RunE: withRepo(access.ReadOnlyAccess, func(cmd *cobra.Command, args []string, rr proto.Repository, level access.AccessLevel) error {
			ctx := cmd.Context()

			r, err := rr.Open()
			if err != nil {
				return err
			}

			switch len(args) {
			case 1:
				head, err := repoHead(r)
				if err != nil {
					return err
				}

				cmd.Println(utils.Sanitize(head.Name().Short()))
			case 2:
				if level < access.ReadWriteAccess {
					return proto.ErrUnauthorized
				}

				branch := args[1]
				branches, _ := r.Branches()
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
				oldHead, _ := r.SymbolicRef(git.HEAD, "")
				oldBranch := git.ReferenceName(oldHead).Short()

				if _, err := r.SymbolicRef(git.HEAD, gitm.RefsHeads+branch, gitm.SymbolicRefOptions{
					CommandOptions: gitm.CommandOptions{
						Context: ctx,
					},
				}); err != nil {
					return err
				}

				// TODO: move this to backend?
				user := proto.UserFromContext(ctx)
				// The default branch is already changed, so a failed event is
				// logged rather than reported as a failed command. The event is built
				// and sent even if the session ends now.
				ctx, cancel := backend.AfterChangeContext(ctx)
				defer cancel()
				wh, err := webhook.NewDefaultBranchChangeEvent(ctx, user, rr, oldBranch)
				if err != nil {
					log.FromContext(ctx).Error("default branch changed but webhook event skipped", "err", err, "repo", rr.Name())
					return nil
				}

				// Sent in the background like the backend's own events, so the
				// session neither waits on receivers nor cancels the delivery.
				backend.FromContext(ctx).SendEventAfterChange(ctx, wh)
				return nil
			}

			return nil
		}),
	}

	return cmd
}

func branchDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "delete REPOSITORY BRANCH",
		Aliases: []string{"remove", "rm", "del"},
		Short:   "Delete a branch",
		Args:    cobra.ExactArgs(2),
		RunE: withRepo(access.ReadWriteAccess, func(cmd *cobra.Command, args []string, rr proto.Repository, _ access.AccessLevel) error {
			ctx := cmd.Context()
			if rr.IsMirror() {
				return proto.ErrMirrorReadOnly
			}

			r, err := rr.Open()
			if err != nil {
				return err
			}

			branch := args[1]
			branches, _ := r.Branches()
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
				return err
			}

			if head != nil && head.Name().Short() == branch {
				return fmt.Errorf("cannot delete the default branch")
			}

			// The event needs only the ID. Reading the commit would
			// fail for one git cannot parse, and such a branch must
			// still be deletable.
			branchID, err := r.ShowRefVerify(git.RefsHeads+branch, gitm.ShowRefVerifyOptions{CommandOptions: gitm.CommandOptions{Context: ctx}})
			if err != nil {
				return err
			}

			if err := r.DeleteBranch(branch, gitm.DeleteBranchOptions{Force: true}); err != nil {
				return err
			}

			// The branch is already deleted, so a failed event is logged rather
			// than reported as a failed command. The event is built
			// and sent even if the session ends now.
			ctx, cancel := backend.AfterChangeContext(ctx)
			defer cancel()
			wh, err := webhook.NewBranchTagEvent(ctx, proto.UserFromContext(ctx), rr, git.RefsHeads+branch, branchID, git.ZeroID)
			if err != nil {
				log.FromContext(ctx).Error("branch deleted but webhook event skipped", "err", err, "repo", rr.Name())
				return nil
			}

			// Sent in the background like the backend's own events, so the
			// session neither waits on receivers nor cancels the delivery.
			backend.FromContext(ctx).SendEventAfterChange(ctx, wh)
			return nil
		}),
	}

	return cmd
}
