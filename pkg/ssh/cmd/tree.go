package cmd

import (
	"errors"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

// treeCommand returns a command that list file or directory at path.
func (cmds *Commands) treeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tree REPOSITORY [REFERENCE] [PATH]",
		Short: "Print repository tree at path",
		Args:  cobra.RangeArgs(1, 3),
		RunE: cmds.withRepo(access.ReadOnlyAccess, func(cmd *cobra.Command, args []string, rr proto.Repository, _ access.AccessLevel) error {
			ctx := cmd.Context()
			path := ""
			ref := ""
			switch len(args) {
			case 2:
				path = args[1]
			case 3:
				ref = args[1]
				path = args[2]
			}

			// Stop git when the session goes away.
			r, err := cmds.openRepository(ctx, rr)
			if err != nil {
				return err
			}

			if ref == "" {
				head, err := cmds.repoHead(cmd.Context(), rr, r)
				if err != nil {
					return err
				}

				ref = head.ID()
			}

			tree, err := cmds.readTree(ctx, rr, r, ref)
			if err != nil {
				return err
			}

			if path != "" && path != "/" {
				te, err := tree.TreeEntry(path)
				if errors.Is(err, git.ErrRevisionNotExist) {
					return proto.ErrFileNotFound
				}
				// A tree git lists but this server will not, as with a
				// name too long for any file system, is the repository
				// content's, as for the listing below.
				if errors.Is(err, git.ErrMalformedTree) {
					return err
				}
				if err != nil {
					return cmds.gitFailed(ctx, "read tree", rr.Name(), err)
				}
				if !te.IsTree() {
					printEntries(cmd, git.Entries{te})
					return nil
				}
				// It fails only for an entry that is not a directory,
				// which IsTree ruled out; it reads nothing.
				tree, err = te.Tree()
				if err != nil {
					return err
				}
			}
			ents, err := tree.Entries()
			// A directory with too many entries is listed in part, and
			// says so after the listing.
			truncated := errors.Is(err, git.ErrTreeTruncated)
			if errors.Is(err, git.ErrMalformedTree) {
				return err
			}
			if err != nil && !truncated {
				return cmds.gitFailed(ctx, "read tree", rr.Name(), err)
			}
			printEntries(cmd, ents)
			if truncated {
				cmd.PrintErrln(err)
			}
			return nil
		}),
	}
	return cmd
}

// printEntries prints ents sorted, one line each: mode, size and name.
func printEntries(cmd *cobra.Command, ents git.Entries) {
	ents.Sort()
	for _, ent := range ents {
		ssize := "-"
		if size := ent.Size(); size != 0 {
			ssize = humanize.Bytes(uint64(size)) //nolint:gosec
		}
		cmd.Printf("%s\t%s\t %s\n", ent.Mode(), ssize, common.DisplayFilename(ent.Name()))
	}
}
