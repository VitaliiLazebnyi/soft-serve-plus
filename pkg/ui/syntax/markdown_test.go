package syntax_test

import (
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ui/syntax"
)

func TestIsFileMarkdown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		filename string
		content  string // XXX: chroma doesn't correctly analyze mk files
		isMkd    bool
	}{
		{"simple", "README.md", "", true},
		{"empty", "", "", false},
		{"no extension", "README", "", false},
		{"weird extension", "README.foo", "", false},
		{"long ext", "README.markdown", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := syntax.IsFileMarkdown(c.content, c.filename); got != c.isMkd {
				t.Errorf("IsFileMarkdown(%q, %q) = %v, want %v", c.content, c.filename, got, c.isMkd)
			}
		})
	}
}
