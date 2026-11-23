package common

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/soft-serve/internal/termtext"
)

// DisplayFilename returns a file name that is safe to print in a terminal.
// Tree entry names are real (unquoted) names, so a name made of printable
// characters, including non-ASCII ones, is returned as is; one with control
// characters or invalid UTF-8 is escaped like a Go string literal.
func DisplayFilename(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return !strconv.IsPrint(r) }) < 0 {
		return s
	}
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

// SanitizeText returns repository text, such as a commit subject, an author
// or a description, ready to show: termtext.Sanitize removes escape sequences
// and control characters, and SanitizeText also removes the Unicode
// bidirectional controls, which would let the text reorder what is shown
// around it in a terminal that implements bidi. DisplayFilename escapes them
// in file names.
func SanitizeText(s string) string {
	s = termtext.Sanitize(s)
	if !strings.ContainsFunc(s, isBidiControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isBidiControl(r) {
			return -1
		}
		return r
	}, s)
}

func isBidiControl(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) }
