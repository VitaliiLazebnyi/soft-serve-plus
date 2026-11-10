package cmd

import (
	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/spf13/cobra"
)

func tagCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tag",
		Short: "Manage repository tags",
	}

	cmd.AddCommand(
		tagListCommand(),
		tagDeleteCommand(),
	)

	return cmd
}

func tagListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "list REPOSITORY",
		Aliases:           []string{"ls"},
		Short:             "List repository tags",
		Args:              cobra.ExactArgs(1),
		PersistentPreRunE: requireRepo(access.ReadOnlyAccess),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			rr, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			r, err := rr.Open()
			if err != nil {
				return err
			}

			tags, _ := r.Tags()
			for _, t := range tags {
				cmd.Println(t)
			}

			return nil
		},
	}

	return cmd
}

func tagDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "delete REPOSITORY TAG",
		Aliases:           []string{"remove", "rm", "del"},
		Short:             "Delete a tag",
		Args:              cobra.ExactArgs(2),
		PersistentPreRunE: requireRepo(access.ReadWriteAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			rr, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}
			if rr.IsMirror() {
				return proto.ErrMirrorReadOnly
			}

			logger := log.FromContext(ctx)
			r, err := rr.Open()
			if err != nil {
				logger.Error("failed to open repo", "err", err, "repo", rr.Name())
				return err
			}

			tag := args[1]
			tags, _ := r.Tags()
			var exists bool
			for _, t := range tags {
				if tag == t {
					exists = true
					break
				}
			}

			if !exists {
				return git.ErrReferenceNotExist
			}

			tagCommit, err := r.TagCommit(tag)
			if err != nil {
				logger.Error("failed to get tag commit", "err", err, "repo", rr.Name(), "tag", tag)
				return err
			}

			if err := r.DeleteTag(tag); err != nil {
				logger.Error("failed to delete tag", "err", err, "repo", rr.Name(), "tag", tag)
				return err
			}

			// The tag is already deleted, so a failed event is logged rather
			// than reported as a failed command. The event is built
			// and sent even if the session ends now.
			ctx, cancel := backend.AfterChangeContext(ctx)
			defer cancel()
			wh, err := webhook.NewBranchTagEvent(ctx, proto.UserFromContext(ctx), rr, git.RefsTags+tag, tagCommit.ID.String(), git.ZeroID)
			if err != nil {
				logger.Error("tag deleted but webhook event skipped", "err", err, "repo", rr.Name())
				return nil
			}

			// Sent in the background like the backend's own events, so the
			// session neither waits on receivers nor cancels the delivery.
			backend.FromContext(ctx).SendEventAfterChange(ctx, wh)
			return nil
		},
	}

	return cmd
}
