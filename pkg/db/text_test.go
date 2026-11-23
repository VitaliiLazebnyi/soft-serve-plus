package db

import (
	"testing"
)

// StorableText replaces bytes that are not UTF-8 and drops NULs, in that
// order, as webhook deliveries and mirror sync errors both store it.
func TestStorableText(t *testing.T) {
	for in, want := range map[string]string{
		"plain \u00e9 \u2713": "plain \u00e9 \u2713",
		"bad \xff\xfe":        "bad \uFFFD",
		"nul\x00 here":        "nul here",
		"\xff\x00\xfe":        "\uFFFD\uFFFD",
		"\x00":                "",
	} {
		if got := StorableText(in); got != want {
			t.Errorf("StorableText(%q) = %q, want %q", in, got, want)
		}
	}
}
