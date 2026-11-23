package termtext

import "testing"

func TestFilename(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want string
	}{
		{"file.txt", "file.txt"},
		{`a"`, `a"`},
		{`"a`, `"a`},
		{"café.txt", "café.txt"},
		{`a\b`, `a\b`},
		{"a\tb", `a\tb`},
		{"a\tb\"", `a\tb\"`},
		{"\xffa", `\xffa`},
	} {
		if got := Filename(tc.in); got != tc.want {
			t.Errorf("Filename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Text removes bidirectional controls along with what Sanitize
// removes, and keeps joiners that emoji need.
func TestTextBidi(t *testing.T) {
	t.Parallel()
	in := "a\u202eb\x1b[31mc\u200dd\u061ce\u200ff"
	if got, want := Text(in), "abc\u200ddef"; got != want {
		t.Fatalf("Text(%q) = %q; want %q", in, got, want)
	}
	if got := Text("plain\ttext\n"); got != "plain\ttext\n" {
		t.Fatalf("plain text changed to %q", got)
	}
}
