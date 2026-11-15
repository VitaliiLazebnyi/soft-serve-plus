package code

import (
	"fmt"
	"runtime"
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
// reports false when the file is shown as plain text instead; see tokenise.
func highlight(st gansi.StyleConfig, path, content string, noMargin bool) (string, bool, error) {
	lexer := lexers.Match(path)
	if lexer == nil {
		// A file without a known name, such as a script with a shebang.
		lexer = lexers.Analyse(content)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	if eager(lexer) && len(content) > EagerHighlightMax {
		return "", false, nil
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

// EagerHighlightMax is the largest content, in bytes, highlighted with a
// lexer that tokenises all of its input up front; see eager. A 256 KiB
// Svelte file of braces took over three minutes of CPU; at 16 KiB such
// files take from milliseconds to about 1.5 s.
const EagerHighlightMax = 16 << 10

// eagerLexers names the lexers that tokenise all of their input when they
// are asked for an iterator, so that the budget cannot stop them.
var eagerLexers = map[string]bool{
	"ERB":              true,
	"Go HTML Template": true,
	"Go Text Template": true,
	"PHP":              true,
	"PHTML":            true,
	"Svelte":           true,
	"YAML+Jinja":       true,
}

// eager reports whether lexer tokenises all of its input up front: one of
// eagerLexers, or any of chroma's delegating lexers.
func eager(lexer chroma.Lexer) bool {
	if cfg := lexer.Config(); cfg != nil && eagerLexers[cfg.Name] {
		return true
	}
	return fmt.Sprintf("%T", lexer) == "*chroma.delegatingLexer"
}

// maxTokenising is the most tokenisations that run at once in the server.
var maxTokenising = int32(max(2, runtime.GOMAXPROCS(0)/2))

// tokenising counts the tokenisations running, including those that ran
// past their budget and could not be stopped.
var tokenising atomic.Int32

// takeSlot takes one of the maxTokenising slots, or reports false when
// none is free.
func takeSlot() bool {
	for {
		n := tokenising.Load()
		if n >= maxTokenising {
			return false
		}
		if tokenising.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// tokenise returns the tokens of content, or false when the content is to
// be shown as plain text: when tokenising it would take more than
// HighlightBudget or HighlightMaxTokens, or no tokenising slot is free.
//
// Most lexers tokenise as their iterator is read, embedded lexers and their
// language lookups included, so the reads stop at the budget and so does
// the work. A lexer that tokenises up front cannot be stopped: it runs on in
// its goroutine, holding its slot, until it is done. The slots bound the
// CPU this takes, but a user who opens such files can hold them all, and
// files are then shown as plain text for everyone until one ends; highlight
// limits these lexers to EagerHighlightMax to keep that short. A loaded
// server, where the budget runs out sooner and slots stay taken longer,
// shows more files as plain text.
func tokenise(lexer chroma.Lexer, content string) ([]chroma.Token, bool, error) {
	if !takeSlot() {
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
		defer tokenising.Add(-1)
		defer func() {
			// chroma panics on some errors; the render must fail, not
			// the server.
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("highlight: %v", p)}
			}
		}()
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
		// A lexer read token by token stops at its next token; one that
		// tokenised up front runs on, holding its slot.
		stop.Store(true)
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
