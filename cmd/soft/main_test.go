package main

import "testing"

func TestIsHookCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"hook", "pre-receive"}, true},
		{[]string{"hook", "update", "refs/heads/main", "a", "b"}, true},
		{[]string{"hook", "post-update", "refs/heads/main"}, true},
		{[]string{"hook"}, true},
		{[]string{"serve"}, false},
		{[]string{"admin", "migrate"}, false},
		{nil, false},
		{[]string{"no-such-command"}, false},
	} {
		if got := isHookCommand(tc.args); got != tc.want {
			t.Errorf("isHookCommand(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
