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
// its error, half from the start and half from the end. The remote writes
// to it ("remote:" lines, relayed even with --quiet), as much as it likes,
// so it is not kept in full.
const remoteGitOutputLimit = 4 << 10

// RunRemoteGit runs a git command that talks to a remote, an import's clone
// or a mirror's fetch, in dir with env applied after the server's
// environment (see RemoteGitEnv), and never prompts for credentials. It
// returns an error that holds at most remoteGitOutputLimit bytes of what git
// printed, the start and the end, with the user info and query of every URL
// in it, and in the command, removed.
//
// Once ctx is done git is asked to stop (see terminateOnCancel): on Unix it
// and the helpers it started get SIGTERM, on which git removes the ref and
// packed-refs locks it holds and exits. A killed git would leave them, and
// every later fetch into the repository would fail on "File exists" until
// someone deleted them by hand. git is killed if it is still running
// RemoteGitWaitDelay later.
func RunRemoteGit(ctx context.Context, dir string, env []string, args ...string) error {
	out := headTailBuffer{limit: remoteGitOutputLimit / 2}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// git runs in a process group of its own (see terminateOnCancel), in
	// the background of any terminal, where a credential prompt would
	// stop it with SIGTTIN rather than fail.
	cmd.Env = append(append(os.Environ(), env...), "GIT_TERMINAL_PROMPT=0")
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
		if msg := remoteGitOutput(&out); msg != "" {
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

// remoteGitOutput returns what out kept, redacted. out cuts its text at
// whitespace, so redaction never sees part of a URL: cut just before "@",
// "https://TOKEN@host/" reads as a URL whose host is the token, which
// redaction would keep.
func remoteGitOutput(out *headTailBuffer) string {
	return redactURLs(strings.TrimSpace(out.String()))
}

// headTailBuffer keeps the first and the last bytes written to it, up to
// limit each, and counts what it leaves out between them. It reports every
// write as complete so the writer is not cut off. git prints what went wrong
// last ("fatal: ..."), after whatever the remote wrote, so both ends count.
//
// The buffers are named fields rather than embedded so that headTailBuffer
// has no ReadFrom: os/exec copies a pipe into a non-file writer with
// io.Copy, which would call an embedded bytes.Buffer.ReadFrom and bypass the
// cap.
type headTailBuffer struct {
	head, tail []byte
	limit      int
	dropped    int64
}

// Write implements io.Writer.
func (b *headTailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - len(b.head); room > 0 {
		take := min(room, len(p))
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if len(p) >= b.limit {
		b.dropped += int64(len(b.tail) + len(p) - b.limit)
		b.tail = append(b.tail[:0], p[len(p)-b.limit:]...)
		return n, nil
	}
	if over := len(b.tail) + len(p) - b.limit; over > 0 {
		b.dropped += int64(over)
		b.tail = append(b.tail[:0], b.tail[over:]...)
	}
	b.tail = append(b.tail, p...)
	return n, nil
}

// String returns what was kept. When bytes were left out, the kept head is
// cut back, and the kept tail forward, to whitespace, so neither ends inside
// a word: a URL cut short could otherwise read as one with another host
// ("https://TOKEN" of "https://TOKEN@host/"), which redaction would keep.
func (b *headTailBuffer) String() string {
	if b.dropped == 0 {
		return string(b.head) + string(b.tail)
	}
	head := b.head
	if i := bytes.LastIndexAny(head, " \t\r\n"); i >= 0 {
		head = head[:i+1]
	} else {
		head = nil
	}
	tail := b.tail
	if i := bytes.IndexAny(tail, " \t\r\n"); i >= 0 {
		tail = tail[i:]
	} else {
		tail = nil
	}
	return string(head) + fmt.Sprintf("\n... (%d bytes left out) ...\n", b.dropped) + string(tail)
}
