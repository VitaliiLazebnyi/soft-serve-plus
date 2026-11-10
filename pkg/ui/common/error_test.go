package common_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

func TestErrorMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
		user bool
	}{
		{"user error", common.ErrMissingRepo, "missing repo", true},
		{"wrapped user error", fmt.Errorf("open: %w", common.ErrMissingRepo), "missing repo", true},
		{"internal error", errors.New("fatal: not a git repository: '/srv/repos/a.git'"), "something went wrong", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := common.ErrorMessage(c.err); got != c.want {
				t.Errorf("ErrorMessage(%q) = %q, want %q", c.err, got, c.want)
			}
			if got := common.IsUserError(c.err); got != c.user {
				t.Errorf("IsUserError(%q) = %v, want %v", c.err, got, c.user)
			}
		})
	}
}
