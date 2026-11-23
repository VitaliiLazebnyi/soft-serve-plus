package termtext

import (
	"testing"
)

func TestSanitize(t *testing.T) {
	cases := []struct {
		name, in, out string
	}{
		{"plain", "hello, 世界", "hello, 世界"},
		{"newline and tab", "a\n\tb", "a\n\tb"},
		{"crlf", "a\r\nb", "a\nb"},
		{"sgr", "a\x1b[31mred\x1b[0m", "ared"},
		{"osc 52", "a\x1b]52;c;Zm9v\x07b", "ab"},
		{"osc 8", "\x1b]8;;http://evil\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"dcs", "a\x1bP1;2|x\x1b\\b", "ab"},
		{"apc", "a\x1b_Gf=100;AAAA\x1b\\b", "ab"},
		{"unterminated dcs", "a\x1bPq#0;2;0;0;0", "aPq#0;2;0;0;0"},
		{"unterminated dcs keeps later lines", "hidden \x1bP start\nline3 MALICIOUS_CODE", "hidden P start\nline3 MALICIOUS_CODE"},
		{"unterminated osc keeps later lines", "a\x1b]0;title\nb\nc", "a]0;title\nb\nc"},
		{"string sequence does not span lines", "a\x1b]0;x\ny\x07z", "a]0;x\nyz"},
		{"unterminated csi", "a\x1b[31\nb", "a[31\nb"},
		{"esc charset", "a\x1b(Bb\x1b7c", "abc"},
		{"c0", "a\x07b\x00c\x08d\x7fe", "abcde"},
		{"lone esc", "a\x1b", "a"},
		{"c1 csi", "a\u009b31mb", "a31mb"},
		{"c1 dcs", "a\u0090q\u009cb", "aqb"},
		{"cp1252 byte", "na\x9eve\nline2", "na\ufffdve\nline2"},
		{"lone 8-bit c1 bytes", "a\x90b\x98c\x9dd\x9ee\x9ff\x9bg\nh", "a\ufffdb\ufffdc\ufffdd\ufffde\ufffdf\ufffdg\nh"},
		{"latin-1 text", "caf\xe9 cr\xe8me\n\xa9 2001", "caf\ufffd cr\ufffdme\n\ufffd 2001"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sanitize(c.in)
			if got != c.out {
				t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.out)
			}
			if again := Sanitize(got); again != got {
				t.Errorf("Sanitize is not idempotent: %q -> %q", got, again)
			}
		})
	}
}
