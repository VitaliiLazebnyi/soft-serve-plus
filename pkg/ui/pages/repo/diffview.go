package repo

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	gansi "charm.land/glamour/v2/ansi"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/termtext"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/styles"
	"github.com/charmbracelet/soft-serve/pkg/ui/syntax"
)

// diffHighlightMax is the largest patch, in bytes, that is highlighted. A
// larger one is shown as plain text: chroma costs far more per byte than
// sanitising and wrapping, and a diff may hold up to git.DiffMaxBytes.
const diffHighlightMax = 256 << 10

// diffHighlightMaxLines is the most lines a highlighted patch may have.
// chroma's cost grows with the tokens, so many short lines cost far more
// than their bytes: 256 KiB of "+" lines took 700 MB to highlight.
const diffHighlightMaxLines = 10000

// diffTruncatedNote tops a diff that was cut to its limits.
const diffTruncatedNote = "This diff is too large to show in full; the rest is left out."

// RenderedMsg is the result of a render a tab of the repository page
// started. The program must hand it to the repository page even while
// another page is shown: the tab starts its next render only once the
// running one has returned.
type RenderedMsg interface {
	repoRendered()
}

// diffRenderedMsg is a diff rendered for a pane's viewport by
// renderDiffCmd.
type diffRenderedMsg struct {
	content string
	to      paneID
	// render is the pane's render count when the render started. A pane
	// drops a result from an earlier render: the diff or the width it was
	// rendered for has changed since. It has no generation, so that the
	// repository page never drops it: the pane must see every render it
	// started return, to start the next one (see common.RenderGate).
	render int
	// epoch is the common.RenderGate epoch the render started in.
	epoch int
}

func (m diffRenderedMsg) pane() paneID { return m.to }

func (diffRenderedMsg) repoRendered() {}

// renderDiffCmd returns a command that renders diff under header for a
// viewport width columns wide. Sanitising, highlighting and wrapping a diff
// of a few MiB takes seconds, so it runs in a command and not in Update,
// which would block the session meanwhile. header runs in the command too.
func renderDiffCmd(to paneID, render, epoch int, st *styles.Styles, width int, diff *git.Diff, header func() string) tea.Cmd {
	return func() tea.Msg {
		return diffRenderedMsg{
			content: renderDiffView(header(), diff, st, width),
			to:      to,
			render:  render,
			epoch:   epoch,
		}
	}
}

// renderDiffView renders a diff for a viewport: header, the stat, a note
// when the diff was truncated, and the patch. The parts are joined with
// newlines rather than lipgloss.JoinVertical, which would pad every line of
// the patch to the widest one.
func renderDiffView(header string, diff *git.Diff, st *styles.Styles, width int) string {
	parts := []string{header, renderSummary(diff, st, width)}
	if diff.Truncated {
		parts = append(parts, st.Log.CommitStatsDel.Render(wrapText(diffTruncatedNote, width-2)))
	}
	parts = append(parts, renderDiff(diff, width))
	return strings.Join(parts, "\n")
}

func renderSummary(diff *git.Diff, styles *styles.Styles, width int) string {
	// The summary names files: like file names elsewhere, they lose
	// bidirectional controls. The patch keeps them, as file contents do.
	stats := strings.Split(common.SanitizeText(diff.Stat), "\n")
	for i, line := range stats {
		ch := strings.Split(line, "|")
		if len(ch) > 1 && !git.IsBinaryStat(ch[len(ch)-1]) {
			adddel := ch[len(ch)-1]
			adddel = strings.ReplaceAll(adddel, "+", styles.Log.CommitStatsAdd.Render("+"))
			adddel = strings.ReplaceAll(adddel, "-", styles.Log.CommitStatsDel.Render("-"))
			stats[i] = strings.Join(ch[:len(ch)-1], "|") + "|" + adddel
		}
	}
	return wrapText(strings.Join(stats, "\n"), width-2)
}

// renderDiff sanitises, highlights and wraps diff's patch. A patch over
// diffHighlightMax bytes or diffHighlightMaxLines lines is not highlighted.
func renderDiff(diff *git.Diff, width int) string {
	patch := termtext.Sanitize(diff.Patch)
	if len(patch) > diffHighlightMax || strings.Count(patch, "\n") > diffHighlightMaxLines {
		return wrapText("\n"+patch, width)
	}
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
	return wrapText(s.String(), width)
}
