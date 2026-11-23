package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/termtext"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

// RepoCommand returns a command for managing repositories. Each
// repo-scoped leaf gates itself with withRepo.
func (cmds *Commands) RepoCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "repo",
		Aliases: []string{"repos", "repository", "repositories"},
		Short:   "Manage repositories",
	}

	cmd.AddCommand(
		cmds.blobCommand(),
		cmds.branchCommand(),
		cmds.collabCommand(),
		cmds.commitCommand(),
		cmds.createCommand(),
		cmds.deleteCommand(),
		cmds.importCommand(),
		cmds.listCommand(),
		cmds.renameCommand(),
		cmds.tagCommand(),
		cmds.treeCommand(),
		cmds.webhookCommand(),
	)
	for _, p := range repoProperties {
		cmd.AddCommand(cmds.propertyCommand(p))
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "info REPOSITORY",
			Short: "Get information about a repository",
			Args:  cobra.ExactArgs(1),
			RunE: cmds.withRepo(access.ReadOnlyAccess, func(cmd *cobra.Command, _ []string, rr proto.Repository, level access.Level) error {
				ctx := cmd.Context()
				be := cmds.be

				r, err := cmds.openRepository(cmd.Context(), rr)
				if err != nil {
					return err
				}

				// An empty repository, or one whose HEAD names a branch
				// that does not exist, has no default branch to show.
				head, err := r.HEAD()
				if err != nil && !errors.Is(err, git.ErrReferenceNotExist) {
					return cmds.gitFailed(ctx, "read HEAD", rr.Name(), err)
				}

				var owner proto.User
				if rr.UserID() > 0 {
					owner, err = be.UserByID(ctx, rr.UserID())
					if err != nil {
						return lookupFailed(ctx, cmds.logger, "look up repository owner", err)
					}
				}

				branches, err := r.Branches()
				if err != nil {
					return cmds.gitFailed(ctx, "list branches", rr.Name(), err)
				}
				tags, err := r.Tags()
				if err != nil {
					return cmds.gitFailed(ctx, "list tags", rr.Name(), err)
				}

				// project name and description are optional, handle trailing
				// whitespace to avoid breaking tests.
				cmd.Println(strings.TrimSpace(fmt.Sprint("Project Name: ", termtext.Sanitize(rr.ProjectName()))))
				cmd.Println("Repository:", rr.Name())
				cmd.Println(strings.TrimSpace(fmt.Sprint("Description: ", termtext.Sanitize(rr.Description()))))
				cmd.Println("Private:", rr.IsPrivate())
				cmd.Println("Hidden:", rr.IsHidden())
				cmd.Println("Mirror:", rr.IsMirror())
				if rr.IsMirror() {
					// The error can carry what the remote wrote, so
					// only read-write collaborators and admins see it.
					st, err := be.MirrorSyncStatus(ctx, rr.Name())
					if errors.Is(err, proto.ErrRepoNotFound) {
						// Renamed or deleted since the access check.
						return err
					}
					if err != nil {
						return lookupFailed(ctx, cmds.logger, "read mirror sync status", err)
					}
					if !st.SyncedAt.IsZero() {
						cmd.Println("Last Sync:", st.SyncedAt.UTC().Format(time.RFC3339))
					}
					if st.Error != "" && level >= access.ReadWriteAccess {
						cmd.Println("Last Sync Error:", strings.ReplaceAll(termtext.Sanitize(st.Error), "\n", " "))
					}
				}
				if owner != nil {
					cmd.Println(strings.TrimSpace(fmt.Sprint("Owner: ", owner.Username())))
				}
				if head != nil {
					cmd.Println("Default Branch:", termtext.Sanitize(head.Name().Short()))
				}
				if len(branches) > 0 {
					cmd.Println("Branches:")
					for _, b := range branches {
						cmd.Println("  -", termtext.Sanitize(b))
					}
				}
				if len(tags) > 0 {
					cmd.Println("Tags:")
					for _, t := range tags {
						cmd.Println("  -", termtext.Sanitize(t))
					}
				}

				return nil
			}),
		},
	)

	return cmd
}

// errRepoEmpty is returned for an empty repository by commands that need a
// default branch.
var errRepoEmpty = errors.New("repository is empty")

// readTree returns the root tree of ref in r, rr's repository. A ref that
// does not resolve is the client's to see, as git.ErrRevisionNotExist; any
// other failure is git failing on the server.
func (cmds *Commands) readTree(ctx context.Context, rr proto.Repository, r *git.Repository, ref string) (*git.Tree, error) {
	tree, err := r.LsTree(ref)
	switch {
	case err == nil:
		return tree, nil
	case errors.Is(err, git.ErrRevisionNotExist):
		return nil, err
	default:
		return nil, cmds.gitFailed(ctx, "read tree", rr.Name(), err)
	}
}

// repoHead returns the repository r of rr's HEAD reference, the default branch. An
// empty repository, or a HEAD that names a branch that does not exist, such
// as in a mirror whose remote renamed its default branch, gets an error that
// says so rather than "reference does not exist".
func (cmds *Commands) repoHead(ctx context.Context, rr proto.Repository, r *git.Repository) (*git.Reference, error) {
	head, err := r.HEAD()
	switch {
	case err == nil:
		return head, nil
	case !errors.Is(err, git.ErrReferenceNotExist):
		return nil, cmds.gitFailed(ctx, "read HEAD", rr.Name(), err)
	}
	switch bs, err := r.Branches(); {
	case err != nil:
		return nil, cmds.gitFailed(ctx, "list branches", rr.Name(), err)
	case len(bs) == 0:
		return nil, errRepoEmpty
	}
	name, err := r.SymbolicRef(git.HEAD, "")
	if err != nil {
		return nil, cmds.gitFailed(ctx, "read HEAD", rr.Name(), err)
	}
	return nil, fmt.Errorf("default branch %q does not exist", git.ReferenceName(name).Short())
}
