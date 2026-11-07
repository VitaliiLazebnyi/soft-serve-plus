package utils

import (
	"strings"
	"testing"
)

func TestValidateRepo(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		for _, repo := range []string{
			"lower",
			"Upper",
			"with-dash",
			"with/slash",
			"withnumb3r5",
			"with.dot",
			"with_underline",
			"a/b/c",
			"foo.github/bar",
			"gitfoo/bar.gitx",
		} {
			t.Run(repo, func(t *testing.T) {
				if err := ValidateRepo(repo); err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			})
		}
	})
	t.Run("invalid", func(t *testing.T) {
		for _, repo := range []string{
			"with$",
			"with@",
			"with!",
			"",
			".",
			"..",
			"nested.git/refs/heads/x",
			"A.GIT/b",
			"victim.git/hooks",
			"foo/.git",
			"foo/../bar",
			"foo//bar",
			"/leading",
			"trailing/",
			"suffix.git",
			"double.git.git",
		} {
			t.Run(repo, func(t *testing.T) {
				if err := ValidateRepo(repo); err == nil {
					t.Error("expected an error, got nil")
				}
			})
		}
	})
}

func TestSanitizeRepo(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"lower", "lower"},
		{"Upper", "Upper"},
		{"with/slash", "with/slash"},
		{"with.dot", "with.dot"},
		{"/with_forward_slash", "with_forward_slash"},
		{"withgitsuffix.git", "withgitsuffix"},
		{"double.git.git", "double"},
		{"triple.git.git.git", "triple"},
		{"..git", ""},
		{".git", ""},
		{"foo/.git", "foo"},
		{"foo/../bar", "bar"},
		{"../../etc", "etc"},
		{"trailing/", "trailing"},
		{"nested.git/refs/heads/x", "nested.git/refs/heads/x"},
		{"a/..git", "a"},
		{"a/b/...git", "a"},
		{"a/b/..git", "a/b"},
		{"foo.git/.git", "foo"},
		{strings.Repeat(".git", 20000), ""},
		{strings.Repeat("/.git", 20000), ""},
		{"x" + strings.Repeat(".git", 20000), "x"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := SanitizeRepo(c.in)
			if got != c.out {
				t.Errorf("expected %q, got %q", c.out, got)
			}
			if again := SanitizeRepo(got); again != got {
				t.Errorf("not a fixed point: %q -> %q", got, again)
			}
		})
	}
}

// Authentication failures log the username only when it is valid, so
// credentials sent in the username field must never pass.
func TestValidateUsernameRejectsCredentials(t *testing.T) {
	for _, s := range []string{
		"ss_x",
		"ss_0123456789abcdef0123456789abcdef",
		"eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiJ4In0.c2ln",
	} {
		if err := ValidateUsername(s); err == nil {
			t.Errorf("ValidateUsername(%q) = nil, want an error", s)
		}
	}
	if err := ValidateUsername("stranger"); err != nil {
		t.Errorf("ValidateUsername(%q) = %v, want nil", "stranger", err)
	}
}

func TestRedactURL(t *testing.T) {
	tests := map[string]string{
		"https://user:secret@example.com/x.git":               "https://example.com/x.git",
		"https://TOKEN@example.com/x.git":                     "https://example.com/x.git",
		"https://example.com/x.git":                           "https://example.com/x.git",
		"https://s3.example.com/obj?X-Amz-Signature=SIG#frag": "https://s3.example.com/obj",
		"https://example.com/obj?":                            "https://example.com/obj",
		"ssh://git@10.0.0.1/x.git":                            "ssh://10.0.0.1/x.git",
		"git@github.com:x/y.git":                              "git@github.com:x/y.git",
		"https://TOKEN@example.com/%zz":                       "[redacted]",
	}
	for in, want := range tests {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

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
