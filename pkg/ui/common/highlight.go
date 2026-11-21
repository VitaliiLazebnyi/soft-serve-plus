package common

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	gansi "charm.land/glamour/v2/ansi"
	"charm.land/log/v2"
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

// Highlight highlights content, the file at path, as a glamour code block
// styled by st would, indented by the code block's margin unless noMargin is
// set, within HighlightBudget. It reports false when the content is to be
// shown as plain text instead; see tokeniseUntil. It must not be called
// before the program's init functions have run, which wrap the eager
// lexers; see init.
func Highlight(st gansi.StyleConfig, path, content string, noMargin bool) (string, bool) {
	return highlight(st, path, content, noMargin, func() (<-chan time.Time, func()) {
		timer := time.NewTimer(HighlightBudget)
		return timer.C, func() { timer.Stop() }
	})
}

// HighlightUntil is Highlight with the budget ending when budget receives,
// or never when it is nil, in place of HighlightBudget.
func HighlightUntil(st gansi.StyleConfig, path, content string, noMargin bool, budget <-chan time.Time) (string, bool) {
	return highlight(st, path, content, noMargin, func() (<-chan time.Time, func()) {
		return budget, func() {}
	})
}

// highlight is Highlight with start giving the budget and a function that
// releases it. start is called once the lexer is chosen, just before the
// tokenisation: the budget covers the tokenisation only.
func highlight(st gansi.StyleConfig, path, content string, noMargin bool, start func() (<-chan time.Time, func())) (string, bool) {
	lexer := lexers.Match(path)
	if lexer == nil {
		// A file without a known name, such as a script with a shebang.
		// Some analysers take time quadratic in their input, so only the
		// start of the file is looked at.
		lexer = analyse(content)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	if eager(lexer) && len(content) > EagerHighlightMax {
		return "", false
	}
	budget, stop := start()
	tokens, ok := tokeniseUntil(lexer, content, budget)
	stop()
	if !ok {
		return "", false
	}

	var out strings.Builder
	if err := formatters.TTY256.Format(&out, newChromaStyle(st.CodeBlock.Chroma), chroma.Literator(tokens...)); err != nil {
		return "", false
	}
	var margin uint
	if st.CodeBlock.Indent != nil {
		margin += *st.CodeBlock.Indent
	}
	if st.CodeBlock.Margin != nil && !noMargin {
		margin += *st.CodeBlock.Margin
	}
	return indentLines(out.String(), int(margin)), true
}

// analyseMax is how much of a file without a known name, in bytes, is
// looked at to tell its language. The Templ analyser took 6 s over 10,000
// short lines.
const analyseMax = 4 << 10

// EagerHighlightMax is the largest content, in bytes, highlighted with a
// lexer that tokenises all of its input up front; see eager. Their time
// grows faster than their input: a Svelte file of "{a}" took 1.1 s at
// 8 KiB, 4.2 s at 16 KiB and over three minutes at 256 KiB; ERB's worst
// input, "<%" repeated, took 0.6 s at 8 KiB and 2.4 s at 16 KiB.
const EagerHighlightMax = 8 << 10

// cappedLexer is an eager lexer that does no work on more than
// EagerHighlightMax bytes: it returns them as one token of plain text. It
// takes the place of the eager lexer in chroma's registry, so that a code
// block in markdown or reStructuredText, which looks its language up there,
// is bounded too.
type cappedLexer struct {
	chroma.Lexer
}

func (l cappedLexer) Tokenise(options *chroma.TokeniseOptions, text string) (chroma.Iterator, error) {
	if len(text) > EagerHighlightMax {
		return chroma.Literator(chroma.Token{Type: chroma.Text, Value: text}), nil
	}
	return l.Lexer.Tokenise(options, text)
}

func (l cappedLexer) SetRegistry(r *chroma.LexerRegistry) chroma.Lexer {
	l.Lexer.SetRegistry(r)
	return l
}

func (l cappedLexer) SetAnalyser(analyser func(text string) float32) chroma.Lexer {
	l.Lexer.SetAnalyser(analyser)
	return l
}

// delegating reports whether lexer is one of chroma's delegating lexers,
// which tokenise all of their input when they are asked for an iterator:
// the budget cannot stop them. TestEagerLexers fails if a chroma upgrade
// renames the type.
func delegating(lexer chroma.Lexer) bool {
	return fmt.Sprintf("%T", lexer) == "*chroma.delegatingLexer"
}

// eager reports whether lexer tokenises all of its input up front.
func eager(lexer chroma.Lexer) bool {
	_, capped := lexer.(cappedLexer)
	return capped || delegating(lexer)
}

// init puts a cappedLexer in place of each delegating lexer of chroma's
// registry. Register replaces a lexer of the same name, in the list Match
// and Analyse go through and in the name and alias maps Get uses.
func init() {
	for _, lexer := range slices.Clone(lexers.GlobalLexerRegistry.Lexers) {
		if delegating(lexer) {
			lexers.GlobalLexerRegistry.Register(cappedLexer{lexer})
		}
	}
}

// coalesceMax is the length at which chroma.Coalesce stops adding tokens to
// a run of one type.
const coalesceMax = 8192

// analyse returns the lexer chroma thinks fits content best, from its first
// analyseMax bytes: some analysers take time quadratic in their input.
func analyse(content string) chroma.Lexer {
	return lexers.Analyse(content[:min(len(content), analyseMax)])
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

// tokeniseUntil returns the tokens of content, with neighbours of the same
// type merged, or false when the content is to be shown as plain text: when
// budget receives before tokenising ends, the content has more than
// HighlightMaxTokens tokens, no tokenising slot is free, or the lexer fails
// or panics. A nil budget never ends.
//
// Most lexers tokenise as their iterator is read, embedded lexers and their
// language lookups included, so the reads stop at the budget and so does
// the work. The tokens are read one by one, not through chroma.Coalesce,
// which reads a whole run of tokens of one type before it returns one. A
// lexer that tokenises up front cannot be stopped: it runs on in its
// goroutine, holding its slot, until it is done. The slots bound the CPU
// this takes, but a user who opens such files can hold them all, and files
// are then shown as plain text for everyone until one ends; those lexers
// are limited to EagerHighlightMax to keep that short. A loaded server,
// where the budget runs out sooner and slots stay taken longer, shows more
// files as plain text.
func tokeniseUntil(lexer chroma.Lexer, content string, budget <-chan time.Time) ([]chroma.Token, bool) {
	if !takeSlot() {
		return nil, false
	}
	type result struct {
		tokens []chroma.Token
		ok     bool
	}
	var stop atomic.Bool
	done := make(chan result, 1)
	go func() {
		defer tokenising.Add(-1)
		defer func() {
			// chroma panics on some input, the YANG lexer on lines of
			// "/*" for one: show the file plain.
			if p := recover(); p != nil {
				log.Debug("highlight: lexer panicked", "lexer", lexer.Config().Name, "panic", p)
				done <- result{}
			}
		}()
		it, err := lexer.Tokenise(nil, content)
		if err != nil {
			done <- result{}
			return
		}
		var (
			tokens []chroma.Token
			run    strings.Builder
			typ    chroma.TokenType
			n      int
		)
		flush := func() {
			if run.Len() > 0 {
				tokens = append(tokens, chroma.Token{Type: typ, Value: run.String()})
				run.Reset()
			}
		}
		for t := it(); t != chroma.EOF; t = it() {
			// Merge as chroma.Coalesce does, for the same output: empty
			// tokens are dropped, and a run of one type is cut once it
			// reaches coalesceMax bytes. Empty tokens are dropped before
			// they are counted, so the cap counts the tokens it counted
			// when chroma.Coalesce ran first.
			if t.Value == "" {
				continue
			}
			n++
			if n > HighlightMaxTokens || stop.Load() {
				done <- result{}
				return
			}
			if t.Type != typ || run.Len() >= coalesceMax {
				flush()
				typ = t.Type
			}
			run.WriteString(t.Value)
		}
		flush()
		done <- result{tokens: tokens, ok: true}
	}()
	select {
	case r := <-done:
		return r.tokens, r.ok
	case <-budget:
		// A lexer read token by token stops at its next token; one that
		// tokenised up front runs on, holding its slot.
		stop.Store(true)
		return nil, false
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
