package common_test

import (
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

func TestRepoURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		publicURL, want string
	}{
		{"ssh://example.com", "git@example.com:repo.git"},
		{"ssh://example.com:22", "git@example.com:repo.git"},
		{"ssh://example.com:23231", "ssh://example.com:23231/repo.git"},
		{"ssh://user@example.com", "user@example.com:repo.git"},
		{"ssh://user@example.com:23231", "ssh://user@example.com:23231/repo.git"},
		{"ssh://[::1]", "git@[::1]:repo.git"},
		{"ssh://[::1]:22", "git@[::1]:repo.git"},
		{"ssh://[::1]:23231", "ssh://[::1]:23231/repo.git"},
		{"https://example.com", "https://example.com/repo.git"},
	} {
		if got := common.RepoURL(tc.publicURL, "repo"); got != tc.want {
			t.Errorf("RepoURL(%q) = %q, want %q", tc.publicURL, got, tc.want)
		}
	}
}

func TestUnquoteFilename(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want string
	}{
		{"file.txt", "file.txt"},
		{`a"`, `a\"`},
		{`"a`, `\"a`},
		{`\303\251`, "é"},
		{`a\tb`, `a\tb`},
	} {
		if got := common.UnquoteFilename(tc.in); got != tc.want {
			t.Errorf("UnquoteFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
