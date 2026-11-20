package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
)

// requestSession is an ssh.Session whose SendRequest fails with err, as a
// send on a channel the client has closed does. Only SendRequest is used.
type requestSession struct {
	ssh.Session
	err error
}

func (s requestSession) SendRequest(string, bool, []byte) (bool, error) {
	return false, s.err
}

// A git service that fails while its session context is still live is a
// server failure, unless the client has closed the channel: git may see
// the end of its input and fail before the channel's context ends.
func TestGitServiceErrorClosedChannel(t *testing.T) {
	failed := errors.New("exit status 128: the remote end hung up unexpectedly")
	for _, tc := range []struct {
		name    string
		sendErr error
		gone    bool
	}{
		{"channel open", nil, false},
		{"channel closed", io.EOF, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel})
			ctx := context.WithValue(t.Context(), sshutils.ContextKeySession, ssh.Session(requestSession{err: tc.sendErr}))

			err := gitServiceError(ctx, logger, git.UploadPackService, "r", failed)
			if tc.gone {
				if !errors.Is(err, context.Canceled) || !errors.Is(err, failed) {
					t.Fatalf("err = %v, want context.Canceled wrapping the git failure", err)
				}
				if strings.Contains(logs.String(), "ERRO") {
					t.Fatalf("a closed channel was logged as an error:\n%s", logs.String())
				}
				return
			}
			if !errors.Is(err, git.ErrSystemMalfunction) {
				t.Fatalf("err = %v, want %v", err, git.ErrSystemMalfunction)
			}
			if !strings.Contains(logs.String(), "ERRO") {
				t.Fatalf("a server failure was not logged as an error:\n%s", logs.String())
			}
		})
	}
}
