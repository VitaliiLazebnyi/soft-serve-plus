package common

import (
	"fmt"
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

// slack is how long past HighlightBudget a tokenisation may hold its slot.
func slack() time.Duration {
	if raceEnabled {
		return time.Second
	}
	return 100 * time.Millisecond
}

// highlightTimed highlights content as the file name and returns how long
// the call took and how long its tokenising slot stayed taken.
func highlightTimed(t *testing.T, name, content string) (ok bool, call, held time.Duration) {
	t.Helper()
	waitSlots(t)
	start := time.Now()
	_, ok = Highlight(StyleConfig(), name, content, false)
	call = time.Since(start)
	for tokenising.Load() > 0 && time.Since(start) < 120*time.Second {
		time.Sleep(time.Millisecond)
	}
	return ok, call, time.Since(start)
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

	// Templ's analyser took 6 s over these lines.
	start := time.Now()
	lexers.Analyse(strings.Repeat("package a\n", 10000)[:analyseMax])
	if took := time.Since(start); took > time.Second {
		t.Fatalf("analysing the start of the file took %v", took)
	}
	if _, call, _ := highlightTimed(t, "noext", strings.Repeat("package a\n", 10000)); call > HighlightBudget+slack()+time.Second {
		t.Fatalf("highlighting 10,000 lines without a known name took %v", call)
	}
}

// A file without a known name is told quickly, from its start, when the
// file view works out whether it is markdown.
func TestIsFileMarkdownBounded(t *testing.T) {
	start := time.Now()
	if IsFileMarkdown(strings.Repeat("package a\n", 10000), "noext") {
		t.Fatal("Go source taken for markdown")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("IsFileMarkdown took %v over 10,000 lines", took)
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
	large := strings.Repeat("{a}", EagerHighlightMax/3+1)
	ok, call, held := highlightTimed(t, "app.svelte", large)
	if ok || call > 50*time.Millisecond || held > 50*time.Millisecond {
		t.Fatalf("a Svelte file of %d bytes: highlighted %v, call %v, slot held %v", len(large), ok, call, held)
	}
}

// An eager lexer reached from a code block of another file, which looks its
// language up in the registry, is capped too.
func TestEmbeddedEagerLexersCapped(t *testing.T) {
	cases := map[string]string{
		"README.md":  "# T\n\n```svelte\n" + strings.Repeat("{a}", (200<<10)/3) + "\n```\n",
		"README.rst": "T\n=\n\n.. code:: svelte\n\n" + strings.Repeat("   {a}{a}{a}{a}\n", (200<<10)/16) + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, held := highlightTimed(t, name, content); held > HighlightBudget+slack() {
				t.Fatalf("the slot was held %v; want at most %v", held, HighlightBudget+slack())
			}
		})
	}
}

// Without chroma.Coalesce, which reads a run of tokens of one type before
// it returns, the budget stops a long run of one token type too.
func TestBudgetStopsLongRuns(t *testing.T) {
	cases := map[string]string{
		"page.html": strings.Repeat("<script>", (256<<10)/8),
		"notes.md":  strings.Repeat("<!--\n", (256<<10)/5),
		"notes.org": strings.Repeat(" \n", (256<<10)/2),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, held := highlightTimed(t, name, content); held > HighlightBudget+slack() {
				t.Fatalf("the slot was held %v; want at most %v", held, HighlightBudget+slack())
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
			got, ok := tokenise(lexers.Get(tc.lexer), tc.content)
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
	// them, from a lexer that costs nothing, so that the budget plays no
	// part.
	var words []chroma.Token
	for range 4000 {
		words = append(words, chroma.Token{Type: chroma.Text, Value: "word "}, chroma.Token{Type: chroma.Punctuation})
	}
	run := fakeLexer{tokenise: func() (chroma.Iterator, error) { return chroma.Literator(words...), nil }}
	waitSlots(t)
	got, ok := tokenise(run, "")
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
			start := time.Now()
			if _, ok := tokenise(slow, "x"); !ok && time.Since(start) < HighlightBudget/2 {
				refused.Add(1)
			}
		}()
	}
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
