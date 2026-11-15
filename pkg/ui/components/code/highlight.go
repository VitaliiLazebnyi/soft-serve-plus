package code

import (
	"strings"
	"sync/atomic"
	"time"

	gansi "charm.land/glamour/v2/ansi"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
)

// HighlightBudget is the most time highlighting one file may take, and
// HighlightMaxTokens the most tokens it may have. Past either the file is
// shown as plain text. chroma's cost does not follow the size of the input:
// a lexer that highlights embedded code, such as markdown's code blocks,
// looks each block's language up by matching every lexer's file name
// patterns, about 2.4 ms a block.
const (
	HighlightBudget    = 200 * time.Millisecond
	HighlightMaxTokens = 200_000
)

// newChromaStyle returns the chroma style for c, as glamour builds it for
// its code blocks.
func newChromaStyle(c *gansi.Chroma) *chroma.Style {
	if c == nil {
		return chroma.MustNewStyle("soft-serve", chroma.StyleEntries{})
	}
	return chroma.MustNewStyle("soft-serve", chroma.StyleEntries{
		chroma.Text:                stylePrimitive(c.Text),
		chroma.Error:               stylePrimitive(c.Error),
		chroma.Comment:             stylePrimitive(c.Comment),
		chroma.CommentPreproc:      stylePrimitive(c.CommentPreproc),
		chroma.Keyword:             stylePrimitive(c.Keyword),
		chroma.KeywordReserved:     stylePrimitive(c.KeywordReserved),
		chroma.KeywordNamespace:    stylePrimitive(c.KeywordNamespace),
		chroma.KeywordType:         stylePrimitive(c.KeywordType),
		chroma.Operator:            stylePrimitive(c.Operator),
		chroma.Punctuation:         stylePrimitive(c.Punctuation),
		chroma.Name:                stylePrimitive(c.Name),
		chroma.NameBuiltin:         stylePrimitive(c.NameBuiltin),
		chroma.NameTag:             stylePrimitive(c.NameTag),
		chroma.NameAttribute:       stylePrimitive(c.NameAttribute),
		chroma.NameClass:           stylePrimitive(c.NameClass),
		chroma.NameConstant:        stylePrimitive(c.NameConstant),
		chroma.NameDecorator:       stylePrimitive(c.NameDecorator),
		chroma.NameException:       stylePrimitive(c.NameException),
		chroma.NameFunction:        stylePrimitive(c.NameFunction),
		chroma.NameOther:           stylePrimitive(c.NameOther),
		chroma.Literal:             stylePrimitive(c.Literal),
		chroma.LiteralNumber:       stylePrimitive(c.LiteralNumber),
		chroma.LiteralDate:         stylePrimitive(c.LiteralDate),
		chroma.LiteralString:       stylePrimitive(c.LiteralString),
		chroma.LiteralStringEscape: stylePrimitive(c.LiteralStringEscape),
		chroma.GenericDeleted:      stylePrimitive(c.GenericDeleted),
		chroma.GenericEmph:         stylePrimitive(c.GenericEmph),
		chroma.GenericInserted:     stylePrimitive(c.GenericInserted),
		chroma.GenericStrong:       stylePrimitive(c.GenericStrong),
		chroma.GenericSubheading:   stylePrimitive(c.GenericSubheading),
		chroma.Background:          stylePrimitive(c.Background),
	})
}

// stylePrimitive returns a chroma style entry for p, as glamour does.
func stylePrimitive(p gansi.StylePrimitive) string {
	var parts []string
	if p.Color != nil {
		parts = append(parts, *p.Color)
	}
	if p.BackgroundColor != nil {
		parts = append(parts, "bg:"+*p.BackgroundColor)
	}
	if p.Italic != nil && *p.Italic {
		parts = append(parts, "italic")
	}
	if p.Bold != nil && *p.Bold {
		parts = append(parts, "bold")
	}
	if p.Underline != nil && *p.Underline {
		parts = append(parts, "underline")
	}
	return strings.Join(parts, " ")
}

// highlight highlights content, the file at path, as a glamour code block
// would, indented by the code block margin unless noMargin is set. It
// reports false when highlighting would take more than HighlightBudget or
// HighlightMaxTokens: the caller then shows the content as plain text.
func highlight(st gansi.StyleConfig, path, content string, noMargin bool) (string, bool, error) {
	lexer := lexers.Match(path)
	if path == "" {
		lexer = lexers.Analyse(content)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	tokens, ok, err := tokenise(chroma.Coalesce(lexer), content)
	if !ok || err != nil {
		return "", false, err
	}

	var out strings.Builder
	if err := formatters.TTY256.Format(&out, newChromaStyle(st.CodeBlock.Chroma), chroma.Literator(tokens...)); err != nil {
		return "", false, err
	}
	var margin uint
	if st.CodeBlock.Indent != nil {
		margin += *st.CodeBlock.Indent
	}
	if st.CodeBlock.Margin != nil && !noMargin {
		margin += *st.CodeBlock.Margin
	}
	return indentLines(out.String(), int(margin)), true, nil
}

// maxOverrun is the most tokenisations that may still run after their
// HighlightBudget ran out; see tokenise. Past it, nothing is highlighted
// with a lexer that tokenises up front until one of them ends.
const maxOverrun = 2

// overrun counts the tokenisations still running after their budget ran
// out.
var overrun atomic.Int32

// tokenise returns the tokens of content, or false when that takes more
// than HighlightBudget or HighlightMaxTokens.
//
// Most lexers tokenise as their iterator is read, embedded lexers and their
// language lookups included, so the reads stop at the budget and so does
// the work. A delegating lexer, such as Svelte's or PHP's, tokenises all of
// its input when it is asked for the iterator, and cannot be stopped: it
// runs on in its goroutine, and while maxOverrun such tokenisations run,
// no other starts.
func tokenise(lexer chroma.Lexer, content string) ([]chroma.Token, bool, error) {
	if overrun.Load() >= maxOverrun {
		return nil, false, nil
	}
	type result struct {
		tokens []chroma.Token
		ok     bool
		err    error
	}
	var stop atomic.Bool
	done := make(chan result, 1)
	go func() {
		it, err := lexer.Tokenise(nil, content)
		if err != nil {
			done <- result{err: err}
			return
		}
		var tokens []chroma.Token
		for t := it(); t != chroma.EOF; t = it() {
			tokens = append(tokens, t)
			if len(tokens) > HighlightMaxTokens || stop.Load() {
				done <- result{}
				return
			}
		}
		done <- result{tokens: tokens, ok: true}
	}()
	timer := time.NewTimer(HighlightBudget)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.tokens, r.ok, r.err
	case <-timer.C:
		stop.Store(true)
		// A lexer read token by token stops at its next token.
		select {
		case <-done:
			return nil, false, nil
		case <-time.After(10 * time.Millisecond):
		}
		overrun.Add(1)
		go func() {
			<-done
			overrun.Add(-1)
		}()
		return nil, false, nil
	}
}

// indentLines indents every line of s that is not empty by n spaces.
func indentLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}
