// Package termtext makes text safe to write to a terminal.
package termtext

import (
	"strings"
	"unicode"
)

// Sanitize returns s without ANSI escape sequences (CSI, OSC, DCS, APC, SOS
// and PM strings included) and without any other C0 or C1 control character
// except newline and tab, so that the result can be written to a terminal
// without the terminal acting on it.
//
// Invalid UTF-8 is replaced with U+FFFD first, so a stray 8-bit byte from a
// legacy-encoded file cannot act as a C1 introducer. Escape sequences are
// matched within a single line, and a string sequence (DCS, OSC, SOS, PM,
// APC) is removed only when it is terminated on that line; otherwise only its
// ESC is dropped and the rest stays visible as plain text. An unterminated
// sequence therefore never hides the content that follows it.
func Sanitize(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = stripLineEscapes(line)
	}
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.Join(lines, "\n"))
}

// stripLineEscapes removes the complete 7-bit escape sequences in line. An
// incomplete sequence loses only its ESC; the control characters left behind
// are removed by Sanitize.
func stripLineEscapes(line string) string {
	if !strings.ContainsRune(line, '\x1b') {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		if line[i] != '\x1b' {
			b.WriteByte(line[i])
			i++
			continue
		}
		if n := escapeLen(line[i:]); n > 0 {
			i += n
		} else {
			i++ // drop the lone ESC, keep what follows
		}
	}
	return b.String()
}

// escapeLen returns the length of the complete escape sequence at the start of
// s (which begins with ESC), or 0 when the sequence is incomplete.
func escapeLen(s string) int {
	if len(s) < 2 {
		return 0
	}
	switch s[1] {
	case '[': // CSI: parameters and intermediates, then a final byte.
		for j := 2; j < len(s); j++ {
			switch c := s[j]; {
			case c >= 0x20 && c <= 0x3f:
			case c >= 0x40 && c <= 0x7e:
				return j + 1
			default:
				return 0
			}
		}
		return 0
	case 'P', ']', 'X', '^', '_': // DCS, OSC, SOS, PM, APC: up to ST or BEL.
		for j := 2; j < len(s); j++ {
			switch s[j] {
			case '\a':
				return j + 1
			case '\x1b':
				if j+1 < len(s) && s[j+1] == '\\' {
					return j + 2
				}
			}
		}
		return 0
	default: // ESC, intermediates, then a final byte (e.g. ESC ( B, ESC 7).
		for j := 1; j < len(s); j++ {
			switch c := s[j]; {
			case c >= 0x20 && c <= 0x2f:
			case c >= 0x30 && c <= 0x7e:
				return j + 1
			default:
				return 0
			}
		}
		return 0
	}
}
