package common

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// waitSlots waits for tokenisations left running by an earlier test, which
// hold tokenising slots.
func waitSlots(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for tokenising.Load() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("tokenisations of earlier tests never ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// hangGuard is how long a test waits for work that ends at once when the
// code is right, before it fails rather than wait for the test binary's
// timeout.
const hangGuard = time.Minute

// analyseProbe is a lexer whose analyser records the most text it was given
// and never claims the text.
type analyseProbe struct {
	fakeLexer
	most *atomic.Int64
}

func (l analyseProbe) Config() *chroma.Config { return &chroma.Config{Name: "analyse probe"} }

func (l analyseProbe) AnalyseText(text string) float32 {
	for {
		m := l.most.Load()
		if int64(len(text)) <= m || l.most.CompareAndSwap(m, int64(len(text))) {
			return 0
		}
	}
}

// probeAnalysers adds an analyseProbe to chroma's registry for the rest of
// the test and returns the most text, in bytes, it is given.
func probeAnalysers(t *testing.T) *atomic.Int64 {
	t.Helper()
	// No tokenisation may be reading the registry while it changes.
	waitSlots(t)
	saved := slices.Clone(lexers.GlobalLexerRegistry.Lexers)
	probe := analyseProbe{most: new(atomic.Int64)}
	lexers.GlobalLexerRegistry.Register(probe)
	t.Cleanup(func() {
		waitSlots(t)
		// Register also wrote the probe into the registry's name map,
		// which is private and stays as it is. That is harmless: only
		// Get("analyse probe") finds it there.
		lexers.GlobalLexerRegistry.Lexers = saved
	})
	return probe.most
}

// The output matches what glamour's code block renderer produced.
func TestHighlightMatchesGlamour(t *testing.T) {
	waitSlots(t)
	got, ok := Highlight(StyleConfig(), "main.go", "package main\n\nfunc main() {}\n", false)
	if !ok {
		t.Fatal("a small Go file was not highlighted")
	}
	want := "  \x1b[38;5;204mpackage\x1b[0m\x1b[38;5;251m \x1b[0m\x1b[38;5;251mmain\x1b[0m\x1b[38;5;251m\x1b[0m\n" +
		"  \x1b[38;5;251m\x1b[0m\n" +
		"  \x1b[38;5;39mfunc\x1b[0m\x1b[38;5;251m \x1b[0m\x1b[38;5;42mmain\x1b[0m\x1b[38;5;187m()\x1b[0m\x1b[38;5;251m \x1b[0m\x1b[38;5;187m{}\x1b[0m\x1b[38;5;251m\x1b[0m\n"
	if got != want {
		t.Fatalf("Highlight =\n%q\nwant\n%q", got, want)
	}
}

// A script without a known name is highlighted by its content; a long file
// without a known name is told quickly, from its start.
func TestHighlightByContent(t *testing.T) {
	waitSlots(t)
	script := "#!/bin/sh\nset -e\necho hi\n"
	out, ok := Highlight(StyleConfig(), "setup", script, false)
	if !ok {
		t.Fatal("a shell script without an extension was not highlighted")
	}
	// Plain text gets the text colour throughout.
	if plain, _ := Highlight(StyleConfig(), "setup.txt", script, false); out == plain {
		t.Fatal("a shell script without an extension was highlighted as plain text")
	}

	// Templ's analyser took 6 s over 10,000 lines of this: the analysers
	// are given the start of the file only, and that start is short
	// enough. The time grows with the square of the length: 13 ms at
	// 4 KiB, 0.5 s at 32 KiB and 2 s at 64 KiB, and twenty times that
	// under the race detector.
	long := strings.Repeat("package a\n", 10000)
	limit := time.Second
	if raceEnabled {
		limit = 5 * time.Second
	}
	start := time.Now()
	lexers.Analyse(long[:analyseMax])
	if took := time.Since(start); took > limit {
		t.Fatalf("analysing the first %d bytes took %v; want at most %v", analyseMax, took, limit)
	}
	most := probeAnalysers(t)
	Highlight(StyleConfig(), "noext", long, false)
	if n := most.Load(); n == 0 || n > analyseMax {
		t.Fatalf("the analysers were given %d bytes of a file of %d; want at most %d", n, len(long), analyseMax)
	}
}

// A file without a known name is told quickly, from its start, when the
// file view works out whether it is markdown.
func TestIsFileMarkdownBounded(t *testing.T) {
	most := probeAnalysers(t)
	long := strings.Repeat("package a\n", 10000)
	if IsFileMarkdown(long, "noext") {
		t.Fatal("Go source taken for markdown")
	}
	if n := most.Load(); n == 0 || n > analyseMax {
		t.Fatalf("the analysers were given %d bytes of a file of %d; want at most %d", n, len(long), analyseMax)
	}
	if !IsFileMarkdown("# T\n\ntext\n", "README.md") {
		t.Fatal("README.md not taken for markdown")
	}
}

// Every delegating lexer, which tokenises all of its input up front, is
// replaced by a cappedLexer in the registry, for lookups by file name, by
// name and by alias. If chroma renames its delegating type, nothing is
// capped and this fails.
func TestEagerLexers(t *testing.T) {
	names := []string{"ERB", "Go HTML Template", "PHTML", "Svelte", "YAML+Jinja"}
	for _, name := range names {
		l, ok := lexers.Get(name).(cappedLexer)
		if !ok {
			t.Fatalf("lexer %q is %T, not capped", name, lexers.Get(name))
		}
		if got := fmt.Sprintf("%T", l.Lexer); got != "*chroma.delegatingLexer" {
			t.Fatalf("lexer %q wraps a %s; want *chroma.delegatingLexer", name, got)
		}
	}
	if _, ok := lexers.Get("svelte").(cappedLexer); !ok {
		t.Fatal("the svelte alias does not give the capped lexer")
	}
	if _, ok := lexers.Match("app.svelte").(cappedLexer); !ok {
		t.Fatal("matching app.svelte does not give the capped lexer")
	}
	seen := 0
	for _, l := range lexers.GlobalLexerRegistry.Lexers {
		if l.Config().Name == "Svelte" {
			seen++
		}
		if delegating(l) {
			t.Fatalf("delegating lexer %q is in the registry uncapped", l.Config().Name)
		}
	}
	if seen != 1 {
		t.Fatalf("the registry lists Svelte %d times; want once", seen)
	}
}

// Files in a lexer that tokenises up front are highlighted only when small;
// a large one is refused at once, without a slot.
func TestEagerLexersCapped(t *testing.T) {
	waitSlots(t)
	small := "<script>\nlet a = 1;\n</script>\n<p>{a}</p>\n"
	if out, ok := Highlight(StyleConfig(), "app.svelte", small, false); !ok || !strings.Contains(out, "\x1b[") {
		t.Fatal("a small Svelte file was not highlighted")
	}
	// The large one never takes a slot.
	waitSlots(t)
	large := strings.Repeat("{a}", EagerHighlightMax/3+1)
	if _, ok := Highlight(StyleConfig(), "app.svelte", large, false); ok || tokenising.Load() != 0 {
		t.Fatalf("a Svelte file of %d bytes: highlighted %v, %d slots taken after", len(large), ok, tokenising.Load())
	}
}

// An eager lexer reached from a code block of another file, which looks its
// language up in the registry, is capped too: the block comes back as one
// token of plain text. Svelte's lexer takes ten seconds over the block,
// holding its slot, as the budget cannot stop it, and highlights it.
func TestEmbeddedEagerLexersCapped(t *testing.T) {
	cases := map[string]string{
		"README.md":  "# T\n\n```svelte\n" + strings.Repeat("{a}", (32<<10)/3) + "\n```\n",
		"README.rst": "T\n=\n\n.. code:: svelte\n\n" + strings.Repeat("   {a}{a}{a}{a}\n", (32<<10)/16) + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			waitSlots(t)
			// Without a budget, the tokenisation runs to its end.
			tokens, ok := tokeniseUntil(lexers.Match(name), content, nil)
			if !ok {
				t.Fatal("tokenise failed")
			}
			want := strings.Count(content, "{a}")
			for _, tok := range tokens {
				if n := strings.Count(tok.Value, "{a}"); n > 0 {
					if n != want || tok.Type != chroma.Text {
						t.Fatalf("the Svelte block was highlighted: a %s token has %d of its %d {a}", tok.Type, n, want)
					}
					return
				}
			}
			t.Fatal("the Svelte block is missing")
		})
	}
}

// pausingLexer wraps a lexer. At its pauseAt-th token, its iterator ends the
// budget and waits for resume; it counts the tokens read after.
type pausingLexer struct {
	chroma.Lexer
	pauseAt int
	budget  chan time.Time
	resume  chan struct{}
	after   *atomic.Int64
}

func (l pausingLexer) Tokenise(options *chroma.TokeniseOptions, text string) (chroma.Iterator, error) {
	it, err := l.Lexer.Tokenise(options, text)
	if err != nil {
		return nil, err
	}
	// Only tokens that are not empty are counted to pauseAt: tokenise
	// skips empty ones without looking at the budget.
	n := 0
	return func() chroma.Token {
		t := it()
		switch {
		case n == l.pauseAt:
			l.after.Add(1)
		case t.Value != "":
			n++
			if n == l.pauseAt {
				close(l.budget)
				<-l.resume
			}
		}
		return t
	}, nil
}

// Without chroma.Coalesce, which reads a run of tokens of one type before
// it returns, the budget stops a long run of one token type too: once the
// budget ends, no more tokens are read. chroma.Coalesce read on to the end
// of the run.
func TestBudgetStopsLongRuns(t *testing.T) {
	cases := map[string]string{
		"page.html": strings.Repeat("<script>", (256<<10)/8),
		"notes.md":  strings.Repeat("<!--\n", (256<<10)/5),
		"notes.org": strings.Repeat(" \n", (256<<10)/2),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			waitSlots(t)
			l := pausingLexer{
				Lexer:   lexers.Match(name),
				pauseAt: 100,
				budget:  make(chan time.Time),
				resume:  make(chan struct{}),
				after:   new(atomic.Int64),
			}
			// The lexer is paused when the budget ends, so the budget
			// ends the call.
			if _, ok := tokeniseUntil(l, content, l.budget); ok {
				t.Fatal("the file was tokenised before the budget ended")
			}
			close(l.resume)
			waitSlots(t)
			if n := l.after.Load(); n > 0 {
				t.Fatalf("%d tokens were read after the budget ended; want none", n)
			}
		})
	}
}

// Merging neighbours of one type leaves the tokens as chroma.Coalesce gave
// them, and so the output: empty tokens are dropped, and a run of one type
// is cut at coalesceMax bytes.
func TestTokensMerged(t *testing.T) {
	cases := map[string]struct{ lexer, content string }{
		"go":   {"go", strings.Repeat("package main\n\nfunc main() { println(\"a\") }\n", 50)},
		"html": {"html", "<!doctype html>\n<html><head><style>p { color: red }</style></head>\n<body><p class=\"a\">hi</p><script>let a = 1</script></body></html>\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			waitSlots(t)
			// No budget: a loaded machine would show the file plain.
			got, ok := tokeniseUntil(lexers.Get(tc.lexer), tc.content, nil)
			if !ok {
				t.Fatal("tokenise failed")
			}
			it, err := chroma.Coalesce(lexers.Get(tc.lexer)).Tokenise(nil, tc.content)
			if err != nil {
				t.Fatal(err)
			}
			want := it.Tokens()
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("tokens differ from chroma.Coalesce:\n%q\n%q", got, want)
			}
		})
	}

	// A run of 4,000 words, with empty tokens of another type between
	// them.
	var words []chroma.Token
	for range 4000 {
		words = append(words, chroma.Token{Type: chroma.Text, Value: "word "}, chroma.Token{Type: chroma.Punctuation})
	}
	run := fakeLexer{tokenise: func() (chroma.Iterator, error) { return chroma.Literator(words...), nil }}
	waitSlots(t)
	got, ok := tokeniseUntil(run, "", nil)
	if !ok {
		t.Fatal("tokenise failed")
	}
	it, err := chroma.Coalesce(run).Tokenise(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want := it.Tokens()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("tokens of a long run differ from chroma.Coalesce:\n%q\n%q", got, want)
	}
	if len(want) < 3 {
		t.Fatalf("the run was cut into %d tokens; want it cut at coalesceMax", len(want))
	}
}

// fakeLexer is a chroma lexer whose Tokenise runs tokenise.
type fakeLexer struct {
	tokenise func() (chroma.Iterator, error)
}

func (l fakeLexer) Config() *chroma.Config { return &chroma.Config{Name: "fake"} }

func (l fakeLexer) Tokenise(*chroma.TokeniseOptions, string) (chroma.Iterator, error) {
	return l.tokenise()
}
func (l fakeLexer) SetRegistry(*chroma.LexerRegistry) chroma.Lexer { return l }
func (l fakeLexer) SetAnalyser(func(string) float32) chroma.Lexer  { return l }
func (l fakeLexer) AnalyseText(string) float32                     { return 0 }

// However many highlights start at once, no more than maxTokenising
// tokenisations run, even when each runs past its budget; the others show
// plain text at once.
func TestTokenisingSlotsBounded(t *testing.T) {
	waitSlots(t)
	release := make(chan struct{})
	budget := make(chan time.Time)
	var ended atomic.Bool
	var running, most atomic.Int32
	slow := fakeLexer{tokenise: func() (chroma.Iterator, error) {
		n := running.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return chroma.Literator(), nil
	}}
	const calls = 16
	var wg sync.WaitGroup
	var refused atomic.Int32
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A call that took a slot waits in the lexer until the
			// budget ends; one that returns before was refused.
			if _, ok := tokeniseUntil(slow, "x", budget); !ok && !ended.Load() {
				refused.Add(1)
			}
		}()
	}
	// Each call is refused or runs the lexer.
	deadline := time.Now().Add(hangGuard)
	for running.Load()+refused.Load() < calls {
		if time.Now().After(deadline) {
			t.Fatalf("after %v, %d calls ran and %d were refused, of %d", hangGuard, running.Load(), refused.Load(), calls)
		}
		time.Sleep(time.Millisecond)
	}
	ended.Store(true)
	close(budget)
	wg.Wait()
	if n := most.Load(); n > maxTokenising {
		t.Fatalf("%d tokenisations ran at once; want at most %d", n, maxTokenising)
	}
	if n := refused.Load(); n != calls-maxTokenising {
		t.Fatalf("%d calls were refused at once; want %d", n, calls-maxTokenising)
	}
	close(release)
	waitSlots(t)
}

// A lexer that panics shows the file plain, does not crash the server, and
// frees its slot. The YANG lexer panics on lines of "/*".
func TestTokenisePanicShowsPlain(t *testing.T) {
	waitSlots(t)
	boom := fakeLexer{tokenise: func() (chroma.Iterator, error) { panic("boom") }}
	if _, ok := tokenise(boom, "x"); ok {
		t.Fatal("a panicking lexer gave tokens")
	}
	waitSlots(t)
	if _, ok := Highlight(StyleConfig(), "a.yang", strings.Repeat("/*\n", 100), false); ok {
		t.Fatal("the YANG file that makes the lexer panic was highlighted")
	}
	waitSlots(t)
}
