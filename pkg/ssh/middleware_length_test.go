package ssh

import (
	"bytes"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	gossh "golang.org/x/crypto/ssh"
)

func TestCommandLengthMiddleware(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cmd     string
		refused bool
	}{
		{"empty", "", false},
		{"short", "repo info test", false},
		{"at limit", strings.Repeat("a", MaxCommandLength), false},
		{"over limit", strings.Repeat("a", MaxCommandLength+1), true},
		{"far over limit", "repo create " + strings.Repeat("a ", 50<<10), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var reached atomic.Bool
			sess := testsession.New(t, &ssh.Server{
				Handler: CommandLengthMiddleware(func(s ssh.Session) {
					reached.Store(true)
					s.Exit(0) //nolint: errcheck
				}),
			}, nil)

			var stderr bytes.Buffer
			sess.Stderr = &stderr
			err := sess.Run(tc.cmd)

			if !tc.refused {
				if err != nil {
					t.Fatalf("run: %v (stderr %q)", err, stderr.String())
				}
				if !reached.Load() {
					t.Fatal("command within the limit did not reach the next handler")
				}
				return
			}

			var exitErr *gossh.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 1 {
				t.Fatalf("run: got %v, want exit status 1", err)
			}
			if reached.Load() {
				t.Fatal("overlong command reached the next handler")
			}
			if !strings.Contains(stderr.String(), ErrCommandTooLong.Error()) {
				t.Fatalf("stderr %q does not mention %q", stderr.String(), ErrCommandTooLong)
			}
		})
	}
}
