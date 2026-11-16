package backend

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// RemoteGitWaitDelay is how long a git command against a remote has to exit
// once it was asked to stop, and then how long RunRemoteGit waits for the
// helpers git started (git-remote-https, ssh) to let go of its output. Past
// the first, git is killed; past the second, the output is closed and
// RunRemoteGit returns.
const RemoteGitWaitDelay = 5 * time.Second

// remoteGitOutputLimit caps how much of git's output RunRemoteGit keeps for
// its error. The remote writes to it ("remote:" lines, relayed even with
// --quiet), as much as it likes, so it is not kept in full.
const remoteGitOutputLimit = 4 << 10

// RunRemoteGit runs a git command that talks to a remote, an import's clone
// or a mirror's fetch, in dir with env applied after the server's
// environment (see RemoteGitEnv). It returns an error that holds at most
// remoteGitOutputLimit bytes of what git printed, with the user info and
// query of every URL in it, and in the command, removed.
//
// Once ctx is done git is asked to stop (see terminateOnCancel): on Unix it
// and the helpers it started get SIGTERM, on which git removes the ref and
// packed-refs locks it holds and exits. A killed git would leave them, and
// every later fetch into the repository would fail on "File exists" until
// someone deleted them by hand. git is killed if it is still running
// RemoteGitWaitDelay later.
func RunRemoteGit(ctx context.Context, dir string, env []string, args ...string) error {
	out := limitedBuffer{limit: remoteGitOutputLimit}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	// One buffer for both: os/exec then copies them through one pipe, in
	// the order git wrote them.
	cmd.Stdout = &out
	cmd.Stderr = &out
	terminateOnCancel(cmd)
	cmd.WaitDelay = RemoteGitWaitDelay

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w: %w", ctxErr, err)
		}
		if msg := redactURLs(strings.TrimSpace(out.String())); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return fmt.Errorf("git %s: %w", redactURLs(strings.Join(args, " ")), err)
	}
	return nil
}

// urlPattern matches a URL as git and remotes print it in messages: a scheme
// and everything up to the next space or quote.
var urlPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s'"<>]+`)

// redactURLs strips the user info and query from every URL in msg. git leaves
// out the user info of the remote URLs it prints but keeps their query, which
// can hold a token, and the remote's own lines can hold any URL.
func redactURLs(msg string) string {
	return urlPattern.ReplaceAllStringFunc(msg, utils.RedactURL)
}

// limitedBuffer keeps the first limit bytes written to it and discards the
// rest, reporting every write as complete so the writer is not cut off.
//
// The buffer is a named field rather than embedded so that limitedBuffer has
// no ReadFrom: os/exec copies a pipe into a non-file writer with io.Copy,
// which would call an embedded bytes.Buffer.ReadFrom and bypass the cap.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

// Write implements io.Writer.
func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// String returns the bytes kept so far.
func (b *limitedBuffer) String() string {
	return b.buf.String()
}
