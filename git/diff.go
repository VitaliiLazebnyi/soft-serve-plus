package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/aymanbagabas/git-module"
)

// diffCommandOptions returns the options for git commands whose diff output
// is parsed. User and system config must not change the output: an external
// diff driver (diff.external) or forced color (color.ui=always) produce text
// the parser does not understand, so both are turned off on the command line
// too, which also covers the repository's own config.
func diffCommandOptions() git.CommandOptions {
	return git.CommandOptions{
		Args: []string{"--no-ext-diff", "--no-color"},
		Envs: []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"},
	}
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
		cmd = git.NewCommand("show").AddOptions(diffCommandOptions()).
			AddArgs("--full-index", "--end-of-options", rev)
	} else {
		parent, err := c.ParentID(0)
		if err != nil {
			return nil, err
		}
		cmd = git.NewCommand("diff").AddOptions(diffCommandOptions()).
			AddArgs("--full-index", "-M", parent.String(), "--end-of-options", rev)
	}
	return r.parseDiff(cmd)
}

// StashDiff returns the diff of the given stash index.
func (r *Repository) StashDiff(index int) (*Diff, error) {
	cmd := git.NewCommand("stash", "show", "-p", "--full-index", "-M", strconv.Itoa(index)).
		AddOptions(diffCommandOptions())
	return r.parseDiff(cmd)
}

// parseDiff runs cmd and parses its patch output with the Diff* caps.
//
// The parser in git-module reads a whole line into memory before it checks
// DiffMaxLineChars, and checks DiffMaxFileLines only between hunks, so one
// huge line, or one huge hunk (a large added file), would be held in memory
// in full and several times over. The output therefore goes through a
// lineCapWriter. It drops the part of each content line past the line cap,
// and every line of a file past the line count cap. The parser still marks
// such a file incomplete, exactly as it would for the full output: see
// lineCapWriter.
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
	if DiffMaxLineChars > 0 || DiffMaxFileLines > 0 {
		w = newLineCapWriter(pw, DiffMaxLineChars, DiffMaxFileLines)
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

// diffOverflowHunk is written in place of the lines of a file past the line
// count cap. It is a hunk header, and the parser checks the line count of the
// file on every hunk header, so the parser sees the file is over the cap and
// marks it incomplete.
var diffOverflowHunk = []byte("@@ -0,0 +0,0 @@\n")

// lineCapWriter limits the patch passed to w, so that the parser behind w
// never holds more of a file than its caps allow.
//
// Lines longer than maxChars are cut to maxChars+1 bytes, keeping the
// newline. The cut line is still over maxChars, so the parser marks the file
// incomplete. Lines starting with diffHeader pass through whole.
//
// Hunk lines of a file (hunk headers and the content lines after the first
// one) are counted the way the parser counts them. Once maxLines+1 have
// passed, so the parser's count is over maxLines, the writer passes
// diffOverflowHunk and drops the rest of the file up to the next diffHeader.
//
// A cap of 0 means no limit. The writer keeps no more than len(diffHeader)
// bytes of its own.
type lineCapWriter struct {
	w        io.Writer
	maxChars int // bytes passed per line, 0 for no limit
	maxLines int // hunk lines passed per file, 0 for no limit

	n      int    // bytes of the current line seen so far
	prefix []byte // first bytes of the current line

	inHunk   bool // the current file has had a hunk header
	lines    int  // hunk lines of the current file passed so far
	skipFile bool // drop the rest of the current file
}

func newLineCapWriter(w io.Writer, maxChars, maxLines int) *lineCapWriter {
	if maxChars > 0 {
		// Keep one byte over the cap, so the parser sees the line is too
		// long, and never cut inside a diffHeader prefix.
		maxChars = max(maxChars+1, len(diffHeader))
	}
	return &lineCapWriter{
		w:        w,
		maxChars: maxChars,
		maxLines: maxLines,
		prefix:   make([]byte, 0, len(diffHeader)),
	}
}

// startLine counts a line that starts with first and reports whether the
// line opens the part of the file that is dropped.
func (w *lineCapWriter) startLine(first byte) bool {
	switch first {
	case '@':
		w.inHunk = true
	case ' ', '+', '-':
		if !w.inHunk {
			return false
		}
	default:
		// Not a hunk line: the parser does not count it.
		return false
	}
	if w.maxLines > 0 && w.lines > w.maxLines {
		return true
	}
	w.lines++
	return false
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

		if w.n == 0 && len(body) > 0 && !w.skipFile && w.startLine(body[0]) {
			if _, err := w.w.Write(diffOverflowHunk); err != nil {
				return 0, err
			}
			w.skipFile = true
		}

		wasHeader := bytes.Equal(w.prefix, diffHeader)
		took := 0
		if need := cap(w.prefix) - len(w.prefix); need > 0 {
			took = min(need, len(body))
			w.prefix = append(w.prefix, body[:took]...)
		}
		isHeader := bytes.Equal(w.prefix, diffHeader)
		if isHeader && !wasHeader {
			// A new file starts: reset the per-file state.
			w.inHunk = false
			w.lines = 0
			if w.skipFile {
				// The prefix was held back while dropping; pass it now.
				w.skipFile = false
				if _, err := w.w.Write(w.prefix[:len(w.prefix)-took]); err != nil {
					return 0, err
				}
			}
		}

		if !w.skipFile {
			out := seg
			if room := w.maxChars - w.n; w.maxChars > 0 && len(body) > room && !isHeader {
				// The line is already past the cap: keep only what fits. An
				// empty write to an io.Pipe still waits for a reader, so
				// skip it.
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
