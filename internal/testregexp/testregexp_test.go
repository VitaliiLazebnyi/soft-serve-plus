package testregexp

import (
	"slices"
	"testing"
	"time"

	"github.com/alecthomas/chroma/v2"
)

func init() { DisableTimeouts() }

// expired gives the rule it is on a timeout that is over before the match
// starts, then takes itself off the rule, as chroma requires of a mutator
// that changes the compiled rules.
type expired struct{}

func (expired) Mutate(*chroma.LexerState) error { return nil }

func (expired) MutateLexer(rules chroma.CompiledRules, state string, i int) error {
	rules[state][i].Regexp.MatchTimeout = -time.Hour
	rules[state][i].Mutator = nil
	return nil
}

// A timeout that is over before the match starts stands in for a stall that
// outlasts the timeout: chroma's lexer must match all the same. Without
// DisableTimeouts it takes that for no match and returns the text as errors,
// so this fails if an upgrade of chroma or regexp2 makes DisableTimeouts do
// nothing.
func TestDisableTimeouts(t *testing.T) {
	lexer := chroma.MustNewLexer(&chroma.Config{Name: "expired"}, func() chroma.Rules {
		return chroma.Rules{"root": {{Pattern: `a+b`, Type: chroma.Keyword, Mutator: expired{}}}}
	})
	got, err := chroma.Tokenise(lexer, nil, "aab")
	if err != nil {
		t.Fatal(err)
	}
	if want := []chroma.Token{{Type: chroma.Keyword, Value: "aab"}}; !slices.Equal(got, want) {
		t.Fatalf("tokens = %v; want %v: a match timed out", got, want)
	}
}
