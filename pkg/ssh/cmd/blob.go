package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/termtext"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/styles"
	"github.com/spf13/cobra"
)

// blobCommand returns a command that prints the contents of a file.
func (cmds *Commands) blobCommand() *cobra.Command {
	var linenumber bool
	var color bool
	var raw bool
	cmd := &cobra.Command{
		Use:     "blob REPOSITORY [REFERENCE] [PATH]",
		Aliases: []string{"cat", "show"},
		Short:   "Print out the contents of file at path",
		Args:    cobra.RangeArgs(1, 3),
		RunE: cmds.withRepo(access.ReadOnlyAccess, func(cmd *cobra.Command, args []string, repo proto.Repository, _ access.AccessLevel) error {
			ctx := cmd.Context()
			ref := ""
			fp := ""
			switch len(args) {
			case 2:
				fp = args[1]
			case 3:
				ref = args[1]
				fp = args[2]
			}

			// Stop git when the session goes away.
			r, err := cmds.openRepository(ctx, repo)
			if err != nil {
				return err
			}

			if ref == "" {
				head, err := cmds.repoHead(cmd.Context(), repo, r)
				if err != nil {
					return err
				}
				ref = head.ID()
			}

			tree, err := cmds.readTree(ctx, repo, r, ref)
			if err != nil {
				return err
			}

			te, err := tree.TreeEntry(fp)
			if errors.Is(err, git.ErrRevisionNotExist) {
				return proto.ErrFileNotFound
			}
			// A tree this server will not list is the repository
			// content's, as in `repo tree`.
			if errors.Is(err, git.ErrMalformedTree) {
				return err
			}
			if err != nil {
				return cmds.gitFailed(ctx, "read tree", repo.Name(), err)
			}

			if te.Type() != "blob" {
				return proto.ErrFileNotFound
			}

			if raw {
				// Stream the exact bytes: nothing is held in memory, so no
				// size limit applies.
				if err := r.WriteBlob(ctx, te.ID(), cmd.OutOrStdout()); err != nil {
					return cmds.gitFailed(ctx, "read blob", repo.Name(), err)
				}
				return nil
			}

			bts, err := te.File().ContentsLimit(config.MaxBlobSize)
			if errors.Is(err, git.ErrFileTooLarge) {
				return fmt.Errorf("%w: use --raw to print", err)
			}
			if err != nil {
				return cmds.gitFailed(ctx, "read blob", repo.Name(), err)
			}

			if isBin, _ := git.IsBinary(bytes.NewReader(bts)); isBin {
				return fmt.Errorf("binary file: use --raw to print")
			}

			c := string(bts)
			// Plain output is the file byte for byte, as git prints it.
			// Highlighted or numbered output is for reading in a
			// terminal, so it is stripped of the escape sequences and
			// control characters a file can carry.
			// Read only for the output that uses them: every session builds
			// the command tree, whichever command it runs.
			noColor, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_NO_COLOR"))
			if (color && !noColor) || linenumber {
				c = termtext.Sanitize(c)
			}
			if color && !noColor {
				c, err = common.FormatHighlight(fp, c, cmds.blobBudget)
				if err != nil {
					return err
				}
			}

			if linenumber {
				c, _ = common.FormatLineNumber(styles.DefaultStyles(), c, color && !noColor)
			}

			cmd.Println(c)
			return nil
		}),
	}

	cmd.Flags().BoolVarP(&raw, "raw", "r", false, "Print the raw contents, unformatted and without a size limit")
	cmd.Flags().BoolVarP(&linenumber, "linenumber", "l", false, "Print line numbers")
	cmd.Flags().BoolVarP(&color, "color", "c", false, "Colorize output")

	return cmd
}
