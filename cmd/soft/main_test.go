package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

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

// TestManPageDateFollowsSourceDateEpoch checks that the man page is dated by
// SOURCE_DATE_EPOCH, so a release rebuilt on another day gives the same page.
func TestManPageDateFollowsSourceDateEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000") // 2023-11-14T22:13:20Z
	var out bytes.Buffer
	manCmd.SetOut(&out)
	t.Cleanup(func() { manCmd.SetOut(nil) })
	if err := manCmd.RunE(manCmd, nil); err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(out.String(), "\n")
	if !strings.HasPrefix(first, ".TH SOFT 1 ") || !strings.Contains(first, `"2023-11-14"`) {
		t.Errorf("man page heading = %q, want the SOURCE_DATE_EPOCH date 2023-11-14", first)
	}
}

func TestSourceDate(t *testing.T) {
	t.Parallel()
	if d, err := sourceDate("1700000000"); err != nil || !d.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("sourceDate(1700000000) = %v, %v", d, err)
	}
	if _, err := sourceDate("not-a-number"); err == nil {
		t.Error("sourceDate accepted an invalid SOURCE_DATE_EPOCH")
	}
	if d, err := sourceDate(""); err != nil || time.Since(d) > time.Minute {
		t.Errorf("sourceDate(\"\") = %v, %v; want now", d, err)
	}
}
