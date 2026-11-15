package code

import (
	"fmt"
	"math"
	"strings"
	"sync/atomic"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	vp "github.com/charmbracelet/soft-serve/pkg/ui/components/viewport"
	"github.com/charmbracelet/soft-serve/pkg/ui/styles"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/x/ansi"
)

const (
	defaultTabWidth        = 4
	defaultSideNotePercent = 0.3
)

// HighlightMax is the largest content, in bytes, that is highlighted or
// rendered as markdown. Larger content is shown as plain text: chroma and
// glamour cost about a second per MiB of ordinary input, and several for
// some languages.
const HighlightMax = 256 << 10

// HighlightLineMax is the longest line, in bytes, of content that is
// highlighted or rendered as markdown. chroma's time grows much faster than
// the length of a token: one identifier of 64 KiB takes seconds, one of
// 1 MiB minutes.
const HighlightLineMax = 4 << 10

// HighlightMaxLines is the most lines content may have to be highlighted.
// chroma's cost grows with the tokens, so many short lines cost far more
// than their bytes.
const HighlightMaxLines = 10000

// GlamourMax is the largest content, in bytes, that is rendered as
// markdown. glamour's cost is far from linear: 240 KiB of short paragraphs
// took 20 s and 8 GiB.
const GlamourMax = 32 << 10

// GlamourMaxLines is the most lines content may have to be rendered as
// markdown: glamour takes about half a millisecond for a list item.
const GlamourMaxLines = 1000

// GlamourMaxDepth is the deepest nesting of block quotes, lists and
// indentation at the start of a line that is rendered as markdown. glamour's
// time grows exponentially with it.
const GlamourMaxDepth = 8

// GlamourMaxMarkers is the most block quote and list markers, over all
// lines, that content may have to be rendered as markdown. Each nested
// container costs glamour far more than its text: 1,000 lines of ten
// nested list markers took 7.6 s and 2.9 GiB.
const GlamourMaxMarkers = 750

// lastID numbers the Code views, so a RenderedMsg finds its view.
var lastID atomic.Int64

// RenderedMsg is content a Code rendered in a command. Whoever owns the
// Code must hand it back to Code.Update, whichever view is shown.
type RenderedMsg struct {
	id     int64
	render int
	// epoch is the common.RenderGate epoch the render started in.
	epoch int
	lines []string
	// numbers holds, for each line, the number of the line of content it
	// starts, or 0 for a line that continues a wrapped one.
	numbers []int
	// sidenote holds the side note lines, truncated, and sideWidth their
	// widest.
	sidenote  []string
	sideWidth int
	// lineNumbers tells whether the lines are shown with their numbers.
	lineNumbers bool
	// markdown is whether the content is markdown, when the render worked
	// it out.
	markdown *bool
	err      error
}

// Code is a code snippet.
type Code struct {
	*vp.Viewport
	common      common.Common
	sidenote    string
	content     string
	extension   string
	styleConfig gansi.StyleConfig
	// id names this view in its RenderedMsg; render counts its renders, so
	// that the result of an earlier one is dropped.
	id     int64
	render int
	// pending is set while a render of the current content is wanted and
	// has not returned, and shown once the current content has been on
	// screen.
	pending bool
	shown   bool
	// renders keeps one render in flight.
	renders common.RenderGate
	// winWidth is the window width of the last tea.WindowSizeMsg. The
	// content is wrapped to the width, so it is rendered again only when
	// that changes.
	winWidth int
	// resizes counts the window size messages the view has handled.
	resizes int

	// isMarkdown caches IsFileMarkdown for content and extension, which can
	// mean running chroma's analysers over the whole content. SetContent
	// clears it.
	isMarkdown *bool

	SideNotePercent float64
	TabWidth        int
	ShowLineNumber  bool
	NoContentStyle  lipgloss.Style
	UseGlamour      bool
}

// New returns a new Code.
func New(c common.Common, content, extension string) *Code {
	r := &Code{
		common:          c,
		content:         content,
		extension:       extension,
		id:              lastID.Add(1),
		winWidth:        c.Width,
		TabWidth:        defaultTabWidth,
		SideNotePercent: defaultSideNotePercent,
		Viewport:        vp.New(c),
		NoContentStyle:  c.Styles.NoContent.SetString("No Content."),
	}
	r.styleConfig = common.StyleConfig()
	r.SetSize(c.Width, c.Height)
	return r
}

// SetSize implements common.Component.
func (r *Code) SetSize(width, height int) {
	r.common.SetSize(width, height)
	r.Viewport.SetSize(width, height)
}

// SetContent sets the content of the Code. The content is sanitised when it
// is rendered, so it may come straight from a repository.
func (r *Code) SetContent(c, ext string) tea.Cmd {
	r.content = c
	r.extension = ext
	r.isMarkdown = nil
	r.shown = false
	return r.Init()
}

// SetMarkdownContent is SetContent for callers that have already worked out
// whether the content is markdown, so it is not worked out a second time.
func (r *Code) SetMarkdownContent(c, ext string, isMarkdown bool) tea.Cmd {
	r.content = c
	r.extension = ext
	r.isMarkdown = &isMarkdown
	r.shown = false
	return r.Init()
}

// markdown reports whether the content is markdown, working it out only once
// per SetContent.
func (r *Code) markdown() bool {
	if r.isMarkdown == nil {
		md := common.IsFileMarkdown(r.content, r.extension)
		r.isMarkdown = &md
	}
	return *r.isMarkdown
}

// SetSideNote sets the sidenote of the Code.
func (r *Code) SetSideNote(s string) tea.Cmd {
	r.sidenote = s
	return r.Init()
}

// ClearSideNote removes the sidenote from the next render without
// rendering, for a view that is about to be hidden or given new content.
func (r *Code) ClearSideNote() {
	r.sidenote = ""
}

// Rendering reports whether the current content is being rendered and
// nothing of it is on screen yet. A render after a resize or an option
// change leaves the previous one on screen meanwhile.
func (r *Code) Rendering() bool {
	return r.pending && !r.shown
}

// Init implements tea.Model. It returns a command that renders the content;
// its RenderedMsg must come back to Update. While a render is in flight it
// returns nil, and the content is rendered once that one returns, so that
// key presses and resizes cost one render at a time.
func (r *Code) Init() tea.Cmd {
	r.render++
	if r.content == "" {
		r.pending = false
		r.shown = true
		r.Viewport.Model.LeftGutterFunc = nil
		r.Viewport.Model.SetContent(r.NoContentStyle.String())
		return nil
	}
	r.pending = true
	epoch, ok := r.renders.Start()
	if !ok {
		return nil
	}
	return r.startRender(epoch)
}

// startRender returns the command of a render RenderGate let start in
// epoch.
func (r *Code) startRender(epoch int) tea.Cmd {
	j := renderJob{
		content:       r.content,
		extension:     r.extension,
		isMarkdown:    r.isMarkdown,
		glamour:       r.UseGlamour,
		lineNumbers:   r.ShowLineNumber,
		sidenote:      r.sidenote,
		sideNoteWidth: int(math.Ceil(float64(r.Model.Width()) * r.SideNotePercent)),
		width:         r.common.Width - r.common.Styles.App.GetHorizontalFrameSize(),
		tabWidth:      r.TabWidth,
		styleConfig:   r.styleConfig,
	}
	id, render := r.id, r.render
	return func() tea.Msg {
		msg := j.run()
		msg.id, msg.render, msg.epoch = id, render, epoch
		return msg
	}
}

// Update implements common.Model.
func (r *Code) Update(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case RenderedMsg:
		if msg.id != r.id || !r.renders.InFlight() {
			// Another view's, or delivered twice.
			return nil
		}
		again := r.renders.Done(msg.epoch)
		if msg.render != r.render || !r.pending {
			// The content, an option or the width changed while it was
			// rendered: render what is wanted now, once for every change
			// meanwhile.
			if again && r.pending {
				if epoch, ok := r.renders.Start(); ok {
					return r.startRender(epoch)
				}
			}
			return nil
		}
		r.pending = false
		if msg.markdown != nil {
			r.isMarkdown = msg.markdown
		}
		if msg.err != nil {
			return common.ErrorCmd(msg.err)
		}
		r.Viewport.Model.LeftGutterFunc = gutter(r.common.Styles, msg)
		r.Viewport.Model.SetContentLines(msg.lines)
		if !r.shown {
			r.shown = true
			r.GotoTop()
		}
		return nil
	case tea.WindowSizeMsg:
		r.resizes++
		// A resize that keeps the width, such as a height-only change,
		// needs no new render, unless nothing was rendered yet: a view
		// without content shows NoContentStyle from its first resize on.
		if msg.Width != r.winWidth || r.render == 0 {
			r.winWidth = msg.Width
			cmds = append(cmds, r.Init())
		}
	}
	cmds = append(cmds, r.Viewport.Update(msg))
	return tea.Batch(cmds...)
}

// Owns reports whether msg is a render of this view.
func (r *Code) Owns(msg RenderedMsg) bool {
	return msg.id == r.id
}

// Resizes returns how many window size messages the view has handled. Tests
// use it to check that a parent delivers each resize to the view only once.
func (r *Code) Resizes() int {
	return r.resizes
}

// View implements tea.Model.
func (r *Code) View() string {
	return r.Viewport.View()
}

// gutter returns the viewport gutter for a render: the side note and the
// line number of each line. The viewport calls it for the lines it shows
// only, so a file of a million lines does not get a million styled numbers.
// It returns nil when there is neither.
func gutter(st *styles.Styles, m RenderedMsg) viewport.GutterFunc {
	if !m.lineNumbers && len(m.sidenote) == 0 {
		return nil
	}
	total := 0
	for i := len(m.numbers) - 1; i >= 0; i-- {
		if m.numbers[i] > 0 {
			total = m.numbers[i]
			break
		}
	}
	digits := len(fmt.Sprint(total))
	bar := st.Code.LineBar.Render("│")
	return func(ctx viewport.GutterContext) string {
		n := 0
		if ctx.Index >= 0 && ctx.Index < len(m.numbers) {
			n = m.numbers[ctx.Index]
		}
		var b strings.Builder
		if m.sideWidth > 0 {
			side := ""
			if n > 0 && n <= len(m.sidenote) {
				side = m.sidenote[n-1]
			}
			b.WriteString(side)
			b.WriteString(strings.Repeat(" ", max(0, m.sideWidth-ansi.StringWidth(side))))
		}
		if m.lineNumbers {
			if n > 0 {
				fmt.Fprintf(&b, " %s %s ", st.Code.LineDigit.Render(fmt.Sprintf("%*d", digits, n)), bar)
			} else {
				fmt.Fprintf(&b, " %*s %s ", digits, "", bar)
			}
		}
		return b.String()
	}
}

// renderJob is what a render needs, copied out of the Code so that it runs
// in a command without touching the model.
type renderJob struct {
	content, extension string
	isMarkdown         *bool
	glamour            bool
	lineNumbers        bool
	sidenote           string
	sideNoteWidth      int
	// width is the width of the view, less the App margin.
	width       int
	tabWidth    int
	styleConfig gansi.StyleConfig
}

// run renders the content: sanitised, highlighted or rendered as markdown
// unless it is too large, and wrapped to the width without padding. Line
// numbers and the side note are left to the gutter.
func (j renderJob) run() RenderedMsg {
	var msg RenderedMsg
	// File contents come from the repository and must not reach the
	// terminal as escape sequences; chroma and glamour pass them through.
	content := utils.Sanitize(j.content)
	// FIXME chroma & glamour might break wrapping when using tabs since tab
	// width depends on the terminal. This is a workaround to replace tabs
	// with spaces.
	content = strings.ReplaceAll(content, "\t", strings.Repeat(" ", j.tabWidth))
	small := len(content) <= HighlightMax
	longest := longestLine(content)

	if j.sidenote != "" {
		msg.sidenote = strings.Split(j.sidenote, "\n")
		for i, l := range msg.sidenote {
			msg.sidenote[i] = common.TruncateString(l, j.sideNoteWidth)
			msg.sideWidth = max(msg.sideWidth, ansi.StringWidth(msg.sidenote[i]))
		}
	}
	width := j.width - msg.sideWidth

	if j.glamour && len(content) <= GlamourMax && longest <= HighlightLineMax &&
		strings.Count(content, "\n") <= GlamourMaxLines && markdownNestingOK(content) {
		md := j.isMarkdown
		if md == nil {
			v := common.IsFileMarkdown(content, j.extension)
			md = &v
			msg.markdown = md
		}
		if *md {
			out, err := glamourize(j.styleConfig, width, content)
			if err != nil {
				msg.err = err
				return msg
			}
			// Each line of glamour's output is a line of its own; wrap
			// them one by one, as wrapping the whole output at once
			// costs far more.
			for l := range strings.SplitSeq(out, "\n") {
				for _, w := range wrapLine(l, width) {
					msg.lines = append(msg.lines, w)
					msg.numbers = append(msg.numbers, len(msg.numbers)+1)
				}
			}
			return msg
		}
	}

	msg.lineNumbers = j.lineNumbers
	if j.lineNumbers {
		n := strings.Count(content, "\n")
		if !strings.HasSuffix(content, "\n") {
			n++
		}
		// The gutter: a space, the digits, a space, the bar, a space.
		width -= len(fmt.Sprint(n)) + 4
	}
	margin := 0
	if !j.lineNumbers && j.styleConfig.CodeBlock.Margin != nil {
		margin = int(*j.styleConfig.CodeBlock.Margin)
	}
	highlighted := false
	if small && longest <= HighlightLineMax && strings.Count(content, "\n") <= HighlightMaxLines {
		if out, ok := common.Highlight(j.styleConfig, j.extension, content, j.lineNumbers); ok {
			content, highlighted = out, true
		}
	}
	if !highlighted && margin > 0 {
		// Indent as the highlighted code block would be.
		pad := strings.Repeat(" ", margin)
		content = pad + strings.ReplaceAll(content, "\n", "\n"+pad)
	}

	lines := strings.Split(content, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		// The newline that ends the last line starts no line of its own.
		lines = lines[:len(lines)-1]
	}
	msg.lines = make([]string, 0, len(lines))
	msg.numbers = make([]int, 0, len(lines))
	for i, l := range lines {
		for k, w := range wrapLine(l, width) {
			msg.lines = append(msg.lines, w)
			n := 0
			if k == 0 {
				n = i + 1
			}
			msg.numbers = append(msg.numbers, n)
		}
	}
	return msg
}

// wrapLine wraps s to width columns, keeping its styles on each line, and
// returns its lines. Lines are not padded to the width.
func wrapLine(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	if !strings.Contains(s, "\n") && ansi.StringWidth(s) <= width {
		return []string{s}
	}
	return strings.Split(lipgloss.Wrap(s, width, ""), "\n")
}

// markdownNestingOK reports whether s nests block quotes and lists little
// enough for glamour: no line deeper than GlamourMaxDepth, and no more than
// GlamourMaxMarkers markers in all.
func markdownNestingOK(s string) bool {
	deepest, markers := markdownNesting(s)
	return deepest <= GlamourMaxDepth && markers <= GlamourMaxMarkers
}

// markdownNesting returns the deepest nesting at the start of a line of s,
// and the number of block quote and list markers at the start of all its
// lines. A marker is ">", "-", "*", "+", or up to nine digits followed by
// "." or ")" and then a space or the end of the line; the nesting of a line
// is its markers plus one for every two columns of indentation.
func markdownNesting(s string) (deepest, markers int) {
	for line := range strings.SplitSeq(s, "\n") {
		depth, spaces := 0, 0
		for i := 0; i < len(line); {
			c := line[i]
			switch {
			case c == ' ':
				spaces++
				i++
				continue
			case c == '>' || c == '-' || c == '*' || c == '+':
				depth++
				i++
				continue
			case c >= '0' && c <= '9':
				if n := orderedMarker(line[i:]); n > 0 {
					depth++
					i += n
					continue
				}
			}
			break
		}
		markers += depth
		deepest = max(deepest, depth+spaces/2)
	}
	return deepest, markers
}

// orderedMarker returns the length of the ordered list marker s starts
// with, such as "12." or "3)", or 0 when it starts with none.
func orderedMarker(s string) int {
	n := 0
	for n < len(s) && n < 10 && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n == 0 || n > 9 || n == len(s) || (s[n] != '.' && s[n] != ')') {
		return 0
	}
	if n+1 < len(s) && s[n+1] != ' ' {
		return 0
	}
	return n + 1
}

// longestLine returns the length in bytes of the longest line of s.
func longestLine(s string) int {
	longest := 0
	for line := range strings.SplitSeq(s, "\n") {
		longest = max(longest, len(line))
	}
	return longest
}

func glamourize(st gansi.StyleConfig, w int, md string) (string, error) {
	if w > 120 {
		w = 120
	}
	// Code blocks are shown as plain styled text. Highlighting them is
	// chroma's work, which costs about 2.4 ms a block for a block in a
	// language chroma does not know, whatever its size.
	st.CodeBlock.Chroma = nil
	st.CodeBlock.Theme = ""
	tr, err := glamour.NewTermRenderer(
		glamour.WithStyles(st),
		glamour.WithWordWrap(w),
	)
	if err != nil {
		return "", err
	}
	mdt, err := tr.Render(md)
	if err != nil {
		return "", err
	}
	return mdt, nil
}
