package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	gansi "charm.land/glamour/v2/ansi"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/termtext"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/styles"
	"github.com/charmbracelet/soft-serve/pkg/ui/syntax"
	"github.com/spf13/cobra"
)

// commitCommand returns a command that prints the contents of a commit.
func (cmds *Commands) commitCommand() *cobra.Command {
	var color bool
	var patchOnly bool

	cmd := &cobra.Command{
		Use:   "commit REPOSITORY REVISION",
		Short: "Print out a commit and its diff",
		Args:  cobra.ExactArgs(2),
		RunE: cmds.withRepo(access.ReadOnlyAccess, func(cmd *cobra.Command, args []string, rr proto.Repository, _ access.AccessLevel) error {
			rev := args[1]

			r, err := cmds.openRepository(cmd.Context(), rr)
			if err != nil {
				return err
			}

			// A revision that does not resolve, or a commit git cannot
			// parse, is the client's to see; anything else is git
			// failing on the server.
			commit, err := r.CommitByRevision(rev)
			if errors.Is(err, git.ErrRevisionNotExist) || errors.Is(err, git.ErrMalformedCommit) {
				return err
			}
			if err != nil {
				return cmds.gitFailed(cmd.Context(), "read commit", rr.Name(), err)
			}

			// A plain --patch is meant for git apply and is printed byte
			// for byte, as git prints it, binary changes included. It is
			// cut only by its total size, and a cut one is not printed at
			// all: git apply would apply the files before the cut and
			// succeed.
			if patchOnly && !color {
				p, err := r.Patch(commit)
				if err != nil {
					return cmds.gitFailed(cmd.Context(), "read diff", rr.Name(), err)
				}
				if p.Truncated {
					return fmt.Errorf("patch truncated at %d bytes; not printed, as a cut patch could be applied in part: clone the repository and use git format-patch or git diff instead", git.PatchLimits().Bytes)
				}
				cmd.Print(p.Patch)
				return nil
			}

			diff, err := r.Diff(commit)
			if err != nil {
				return cmds.gitFailed(cmd.Context(), "read diff", rr.Name(), err)
			}
			patch := diff.Patch

			commonStyle := styles.DefaultStyles()
			style := commonStyle.Log

			s := strings.Builder{}
			commitLine := "commit " + commit.ID.String()
			authorLine := "Author: " + termtext.Sanitize(commit.Author.Name)
			dateLine := "Date:   " + commit.Committer.When.UTC().Format(time.UnixDate)
			msgLine := strings.ReplaceAll(termtext.Sanitize(commit.Message), "\r\n", "\n")
			// Anything formatted for reading in a terminal is stripped of
			// the escape sequences and control characters a commit can
			// carry.
			patch = termtext.Sanitize(patch)
			statsLine := renderStats(diff, commonStyle, color)
			diffLine := renderDiff(patch, color)

			if patchOnly {
				cmd.Println(
					diffLine,
				)
				return nil
			}

			if color {
				fmt.Fprintf(&s, "%s\n%s\n%s\n%s\n",
					style.CommitHash.Render(commitLine),
					style.CommitAuthor.Render(authorLine),
					style.CommitDate.Render(dateLine),
					style.CommitBody.Render(msgLine),
				)
			} else {
				fmt.Fprintf(&s, "%s\n%s\n%s\n%s\n",
					commitLine,
					authorLine,
					dateLine,
					msgLine,
				)
			}

			fmt.Fprintf(&s, "\n%s\n%s",
				statsLine,
				diffLine,
			)

			cmd.Println(
				s.String(),
			)

			return nil
		}),
	}

	cmd.Flags().BoolVarP(&color, "color", "c", false, "Colorize output")
	cmd.Flags().BoolVarP(&patchOnly, "patch", "p", false, "Output patch only")

	return cmd
}

func renderDiff(patch string, color bool) string {
	c := patch

	if color {
		var s strings.Builder
		var pr strings.Builder

		diffChroma := &gansi.CodeBlockElement{
			Code:     patch,
			Language: "diff",
		}

		err := diffChroma.Render(&pr, syntax.StyleRenderer())

		if err != nil {
			s.WriteString("\n" + err.Error())
		} else {
			s.WriteString("\n" + pr.String())
		}

		c = s.String()
	}

	return c
}

func renderStats(diff *git.Diff, commonStyle *styles.Styles, color bool) string {
	style := commonStyle.Log
	// The stats name the changed files, which may hold control characters.
	c := termtext.Sanitize(diff.Stat)

	if color {
		s := strings.Split(c, "\n")

		for i, line := range s {
			ch := strings.Split(line, "|")
			if len(ch) > 1 && !git.IsBinaryStat(ch[len(ch)-1]) {
				adddel := ch[len(ch)-1]
				adddel = strings.ReplaceAll(adddel, "+", style.CommitStatsAdd.Render("+"))
				adddel = strings.ReplaceAll(adddel, "-", style.CommitStatsDel.Render("-"))
				s[i] = strings.Join(ch[:len(ch)-1], "|") + "|" + adddel
			}
		}

		return strings.Join(s, "\n")
	}

	return c
}
