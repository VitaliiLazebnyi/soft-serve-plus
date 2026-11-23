package termtext

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Filename returns a file name that is safe to print in a terminal.
// Tree entry names are real (unquoted) names, so a name made of printable
// characters, including non-ASCII ones, is returned as is; one with control
// characters or invalid UTF-8 is escaped like a Go string literal.
func Filename(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return !strconv.IsPrint(r) }) < 0 {
		return s
	}
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

// Text returns repository text, such as a commit subject, an author or a
// description, ready to show: Sanitize removes escape sequences and control
// characters, and Text also removes the Unicode bidirectional controls, which
// would let the text reorder what is shown around it in a terminal that
// implements bidi. Filename escapes them in file names.
func Text(s string) string {
	s = Sanitize(s)
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
