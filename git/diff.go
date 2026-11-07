package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aymanbagabas/git-module"
)

// diffCommandOptions are the options every diff command runs with.
var diffCommandOptions = git.CommandOptions{
	Envs: []string{"GIT_CONFIG_GLOBAL=/dev/null"},
}

// Diff returns the diff for the given commit against its first parent.
func (r *Repository) Diff(commit *Commit) (*Diff, error) {
	rev := commit.ID.String()
	c, err := r.CatFileCommit(rev)
	if err != nil {
		return nil, err
	}

	var cmd *git.Command
	if c.ParentsCount() == 0 {
		// First commit of the repository.
		cmd = git.NewCommand("show").AddOptions(diffCommandOptions).
			AddArgs("--full-index", "--end-of-options", rev)
	} else {
		parent, err := c.ParentID(0)
		if err != nil {
			return nil, err
		}
		cmd = git.NewCommand("diff").AddOptions(diffCommandOptions).
			AddArgs("--full-index", "-M", parent.String(), "--end-of-options", rev)
	}
	return r.parseDiff(cmd)
}

// StashDiff returns the diff of the given stash index.
func (r *Repository) StashDiff(index int) (*Diff, error) {
	cmd := git.NewCommand("stash", "show", "-p", "--full-index", "-M", strconv.Itoa(index)).
		AddOptions(diffCommandOptions)
	return r.parseDiff(cmd)
}

// parseDiff runs cmd and parses its patch output with the Diff* caps.
//
// The parser in git-module reads a whole line into memory before it checks
// DiffMaxLineChars, so one huge line (a large file with no newlines) would be
// held in memory several times over. The output therefore goes through a
// lineCapWriter that drops the part of each content line past the cap. A
// dropped line is still longer than DiffMaxLineChars, so the parser marks
// the file incomplete exactly as it would for the full line.
func (r *Repository) parseDiff(cmd *git.Command) (*Diff, error) {
	stdout, pw := io.Pipe()
	done := make(chan git.SteamParseDiffResult, 1)
	go func() {
		git.StreamParseDiff(stdout, done, DiffMaxFiles, DiffMaxFileLines, DiffMaxLineChars)
		// The parser stops reading early on an error. Close the pipe so
		// git's output is not left blocked on a reader that is gone.
		_ = stdout.CloseWithError(errDiffParsed)
	}()

	var w io.Writer = pw
	if DiffMaxLineChars > 0 {
		w = newLineCapWriter(pw, DiffMaxLineChars+1)
	}

	stderr := new(bytes.Buffer)
	err := cmd.RunInDirPipeline(w, stderr, r.Path)
	_ = pw.Close() // Let the parser reach EOF.
	result := <-done
	if result.Err != nil {
		return nil, result.Err
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w - %s", err, msg)
		}
		return nil, err
	}
	return toDiff(result.Diff), nil
}

// errDiffParsed fails writes to a diff pipe whose parser has returned.
var errDiffParsed = errors.New("diff parser stopped reading")

// diffHeader starts the line that opens each file in a patch. The parser
// slices the paths out of it, so it is never cut.
var diffHeader = []byte("diff --git ")

// lineCapWriter passes at most max bytes of each line to w and drops the
// rest of the line, keeping the newline. Lines starting with diffHeader pass
// through whole. It keeps no more than len(diffHeader) bytes of its own.
type lineCapWriter struct {
	w      io.Writer
	max    int
	n      int    // bytes of the current line seen so far
	prefix []byte // first bytes of the current line
}

func newLineCapWriter(w io.Writer, maxLen int) *lineCapWriter {
	return &lineCapWriter{
		w:      w,
		max:    max(maxLen, len(diffHeader)),
		prefix: make([]byte, 0, len(diffHeader)),
	}
}

func (w *lineCapWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		seg := p
		nl := bytes.IndexByte(p, '\n')
		if nl >= 0 {
			seg = p[:nl+1]
		}
		p = p[len(seg):]

		body := seg
		if nl >= 0 {
			body = seg[:len(seg)-1]
		}
		if need := cap(w.prefix) - len(w.prefix); need > 0 {
			w.prefix = append(w.prefix, body[:min(need, len(body))]...)
		}

		out := seg
		if room := w.max - w.n; len(body) > room && !bytes.Equal(w.prefix, diffHeader) {
			// The line is already past max: keep only what fits. An empty
			// write to an io.Pipe still waits for a reader, so skip it.
			if room > 0 {
				if _, err := w.w.Write(body[:room]); err != nil {
					return 0, err
				}
			}
			out = seg[len(body):] // the newline, if any
		}
		if len(out) > 0 {
			if _, err := w.w.Write(out); err != nil {
				return 0, err
			}
		}

		if nl >= 0 {
			w.n = 0
			w.prefix = w.prefix[:0]
		} else {
			w.n += len(body)
		}
	}
	return total, nil
}
