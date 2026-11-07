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
			"-leading-dash",
			"a/-b",
			"...",
			"a/.../b",
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

// A repository whose name breaks the rules for new names can still be
// named, to rename it, but not one that is unsafe as a path.
func TestValidateExistingRepo(t *testing.T) {
	for _, repo := range []string{"-foo", "a/-b", "...", "a/.../b", "lower"} {
		if err := ValidateExistingRepo(repo); err != nil {
			t.Errorf("ValidateExistingRepo(%q) = %v, want nil", repo, err)
		}
	}
	for _, repo := range []string{"", "..", "foo/../bar", "suffix.git", "with$"} {
		if err := ValidateExistingRepo(repo); err == nil {
			t.Errorf("ValidateExistingRepo(%q) = nil, want an error", repo)
		}
	}
}

func TestValidateNewUsername(t *testing.T) {
	for _, name := range []string{
		"admin",
		"user-1",
		"андрей",    // Cyrillic only
		"ελένη",     // Greek only
		"山田太郎",      // Han
		"やまだ-taro",  // Hiragana and Latin
		"김철수",       // Hangul
		"mañana",    // Latin with a diacritic
		"testuser1", // digits are of no script
	} {
		if err := ValidateNewUsername(name); err != nil {
			t.Errorf("ValidateNewUsername(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{
		"\u0430dmin",   // Cyrillic a, then Latin
		"adm\u0456n",   // Latin with a Cyrillic i
		"\u03b1lpha",   // Greek alpha, then Latin
		"\u0430\u03b1", // Cyrillic and Greek
		"\u0430\u5c71", // Cyrillic and Han
		"1abc",         // not a letter first
		"\u0663abc",    // an Arabic-Indic digit first
	} {
		if err := ValidateNewUsername(name); err == nil {
			t.Errorf("ValidateNewUsername(%q) = nil, want an error", name)
		}
	}
	// Existing accounts are still looked up.
	if err := ValidateUsername("\u0430dmin"); err != nil {
		t.Errorf("ValidateUsername(%q) = %v, want nil", "\u0430dmin", err)
	}
}
