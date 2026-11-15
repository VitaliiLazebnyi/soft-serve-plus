package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aymanbagabas/git-module"
)

// Default caps on a diff; see DiffLimits. They are variables for tests.
var (
	// DiffMaxFiles is the most files a diff shows.
	DiffMaxFiles = 1000
	// DiffMaxFileLines is the most lines of one file's patch a diff shows.
	DiffMaxFileLines = 1000
	// DiffMaxLineChars is the most bytes of one line a diff shows.
	DiffMaxLineChars = 1000
	// DiffMaxBytes is the most bytes a diff holds in all, stat and patch
	// together.
	DiffMaxBytes = 4 << 20
)

// DiffLimits caps the size of a Diff. A zero field means no cap.
type DiffLimits struct {
	// Files is the most files shown. The diff ends before the next one.
	Files int
	// FileLines is the most lines of one file's patch shown. The count
	// includes the file's header lines (diff --git, index, ---, +++ and
	// the like), not only its hunks, so a file gets a few lines fewer of
	// content than the cap. The rest of that file is left out and the line
	// diffFileTruncated put in its place.
	FileLines int
	// LineBytes is the most bytes of one line shown; the rest of the line
	// is left out.
	LineBytes int
	// Bytes is the most bytes of output, stat and patch together. The diff
	// ends at the last whole line that fits.
	Bytes int
}

// DefaultDiffLimits returns the limits for a diff shown to a reader:
// DiffMaxFiles, DiffMaxFileLines, DiffMaxLineChars and DiffMaxBytes.
func DefaultDiffLimits() DiffLimits {
	return DiffLimits{
		Files:     DiffMaxFiles,
		FileLines: DiffMaxFileLines,
		LineBytes: DiffMaxLineChars,
		Bytes:     DiffMaxBytes,
	}
}

// PatchLimits returns the limits of Repository.Patch, a patch meant to be
// applied: only the total size is capped, as cutting a file or a line would
// make the patch useless to git apply.
func PatchLimits() DiffLimits {
	return DiffLimits{Bytes: DiffMaxBytes}
}

// Diff is a diff as git prints it, cut to its limits.
type Diff struct {
	// Stat is git's --stat summary: a line per file and a total.
	Stat string
	// Patch is git's patch. When Truncated is false it is exactly what git
	// printed and applies with git apply. When it is true, the parts past
	// the limits are left out and it ends with the line DiffTruncated.
	Patch string
	// Truncated reports that the diff was cut to its limits.
	Truncated bool
}

// IsBinaryStat reports whether graph, the part of a --stat line after its
// last "|", is git's summary of a binary file ("Bin 2 -> 3 bytes") rather
// than a +/- graph, so its "->" is not coloured as a deletion.
func IsBinaryStat(graph string) bool {
	return strings.HasPrefix(strings.TrimSpace(graph), "Bin ")
}

// DiffTruncated ends a diff that was cut to its limits.
const DiffTruncated = "[diff truncated]\n"

// diffFileTruncated stands for the lines of a file past
// DiffLimits.FileLines.
const diffFileTruncated = "[rest of this file's diff not shown]\n"

// diffHeader starts the line that opens each file in a patch.
var diffHeader = []byte("diff --git ")

// diffArgs are the options of every diff command. The output is git's own
// text, so only what keeps it what git apply reads matters: no external
// diff driver or textconv filter, no color, and the full blob IDs. Paths git
// would C-quote only for being non-ASCII are left as they are, so names read
// as they are; git still quotes control characters, quotes and backslashes.
var diffArgs = []string{
	"-r", "-p", "-M", "--full-index",
	"--no-ext-diff", "--no-textconv", "--no-color",
}

// diffViewArgs add the stat to a diff for reading.
var diffViewArgs = []string{"--stat=80"}

// patchArgs make a patch to apply carry binary changes, which git apply
// needs to apply them, and no stat, which would only use up its size cap.
var patchArgs = []string{"--binary"}

// diffCommand returns a diff-tree command with diffArgs, mode (diffViewArgs
// or patchArgs) and then args. It is plumbing, so porcelain settings such
// as diff.noprefix do not change it; user and system config are left out
// too, as before.
func (r *Repository) diffCommand(mode []string, args ...string) *git.Command {
	return git.NewCommand("-c", "core.quotePath=false", "diff-tree").
		AddOptions(git.CommandOptions{
			Envs:    []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"},
			Context: r.Context(),
		}).
		AddArgs(diffArgs...).
		AddArgs(mode...).
		AddArgs(args...)
}

// commitDiffArgs are the arguments that name what commit's diff compares:
// its first parent and itself, or for a root commit itself alone.
func commitDiffArgs(commit *Commit) ([]string, error) {
	rev := commit.ID.String()
	if commit.ParentsCount() == 0 {
		return []string{"--root", "--no-commit-id", "--end-of-options", rev}, nil
	}
	parent, err := commit.ParentID(0)
	if err != nil {
		return nil, err
	}
	return []string{"--end-of-options", parent.String(), rev}, nil
}

// Diff returns the diff of commit against its first parent, or for a root
// commit against nothing, with DefaultDiffLimits.
func (r *Repository) Diff(commit *Commit) (*Diff, error) {
	return r.DiffLimited(commit, DefaultDiffLimits())
}

// DiffLimited returns the diff of commit against its first parent, or for a
// root commit against nothing, cut to limits, for reading: with its stat.
func (r *Repository) DiffLimited(commit *Commit, limits DiffLimits) (*Diff, error) {
	args, err := commitDiffArgs(commit)
	if err != nil {
		return nil, err
	}
	return r.runDiff(r.diffCommand(diffViewArgs, args...), limits)
}

// Patch returns the patch of commit against its first parent, or for a root
// commit against nothing, for git apply: binary changes included, no stat,
// and cut only by PatchLimits. A caller must not hand on a patch with
// Truncated set, as git apply would apply the files before the cut.
func (r *Repository) Patch(commit *Commit) (*Diff, error) {
	args, err := commitDiffArgs(commit)
	if err != nil {
		return nil, err
	}
	return r.runDiff(r.diffCommand(patchArgs, args...), PatchLimits())
}

// StashDiff returns the diff of the stash at index, its work tree against
// the commit it was made on as git stash show prints it, with
// DefaultDiffLimits.
func (r *Repository) StashDiff(index int) (*Diff, error) {
	stash := "stash@{" + strconv.Itoa(index) + "}"
	return r.runDiff(r.diffCommand(diffViewArgs, "--end-of-options", stash+"^1", stash), DefaultDiffLimits())
}

// runDiff runs cmd and returns its output cut to limits. Once the output is
// over Bytes or Files, the writer fails, so git stops at its next write.
func (r *Repository) runDiff(cmd *git.Command, limits DiffLimits) (*Diff, error) {
	w := &diffWriter{limits: limits, statEnd: -1}
	var stderr bytes.Buffer
	err := cmd.RunInDirPipeline(w, &stderr, r.Path)
	w.flushLine()
	if err != nil && !w.full {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w - %s", err, msg)
		}
		return nil, err
	}
	return w.diff(), nil
}

// errDiffFull fails writes to a diffWriter that has all it will take.
var errDiffFull = errors.New("diff limit reached")

// diffWriter keeps git's diff output up to its limits. It works a line at a
// time and holds no more than LineBytes of a line it has not finished.
type diffWriter struct {
	limits DiffLimits
	out    bytes.Buffer

	line    []byte // the current line so far, cut to LineBytes
	lineCut bool   // bytes of the current line were dropped

	statEnd   int  // where the patch starts in out, -1 before it does
	files     int  // files seen
	fileLines int  // lines of the current file seen
	skipFile  bool // drop the rest of the current file
	truncated bool
	full      bool // nothing more is taken
}

func (w *diffWriter) Write(p []byte) (int, error) {
	if w.full {
		return 0, errDiffFull
	}
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		seg := p
		if i >= 0 {
			seg = p[:i+1]
		}
		p = p[len(seg):]
		w.addToLine(seg)
		if w.full {
			return n - len(p), errDiffFull
		}
		if i >= 0 {
			w.endLine()
			if w.full {
				return n - len(p), errDiffFull
			}
		}
	}
	return n, nil
}

// addToLine adds b to the current line, keeping at most LineBytes of it
// (and always the newline).
func (w *diffWriter) addToLine(b []byte) {
	nl := len(b) > 0 && b[len(b)-1] == '\n'
	if nl {
		b = b[:len(b)-1]
	}
	if lim := w.limits.LineBytes; lim > 0 {
		// Keep enough to recognise a file header however small the cap.
		lim = max(lim, len(diffHeader))
		if room := lim - len(w.line); len(b) > room {
			b = b[:max(room, 0)]
			w.lineCut = true
		}
	}
	// A line that cannot fit in the total is not kept at all, so a huge
	// line under no line cap costs no more memory than the total cap.
	if lim := w.limits.Bytes; lim > 0 && w.out.Len()+len(w.line)+len(b) > lim {
		w.stop()
		return
	}
	w.line = append(w.line, b...)
	if nl {
		w.line = append(w.line, '\n')
	}
}

// flushLine takes a last line git ended without a newline.
func (w *diffWriter) flushLine() {
	if len(w.line) > 0 && !w.full {
		w.endLine()
	}
}

// endLine takes the current line, applying the limits, and starts a new one.
func (w *diffWriter) endLine() {
	line, cut := w.line, w.lineCut
	w.line, w.lineCut = w.line[:0], false
	if bytes.HasPrefix(line, diffHeader) {
		if w.statEnd < 0 {
			w.statEnd = w.out.Len()
		}
		w.files++
		w.fileLines = 0
		w.skipFile = false
		if f := w.limits.Files; f > 0 && w.files > f {
			w.stop()
			return
		}
	}
	if w.skipFile {
		return
	}
	if w.statEnd >= 0 {
		w.fileLines++
		if f := w.limits.FileLines; f > 0 && w.fileLines > f {
			w.skipFile = true
			w.truncated = true
			line = []byte(diffFileTruncated)
			cut = false
		}
	}
	if b := w.limits.Bytes; b > 0 && w.out.Len()+len(line) > b {
		w.stop()
		return
	}
	if cut {
		w.truncated = true
	}
	w.out.Write(line)
}

// stop marks the diff cut and takes nothing more.
func (w *diffWriter) stop() {
	w.truncated = true
	w.full = true
}

// diff returns what w took as a Diff.
func (w *diffWriter) diff() *Diff {
	s := w.out.String()
	d := &Diff{Truncated: w.truncated}
	if w.statEnd < 0 {
		d.Stat = s
	} else {
		d.Stat, d.Patch = s[:w.statEnd], s[w.statEnd:]
	}
	// git separates the stat from the patch with an empty line.
	if strings.HasSuffix(d.Stat, "\n\n") {
		d.Stat = d.Stat[:len(d.Stat)-1]
	}
	if d.Truncated {
		if d.Patch != "" && !strings.HasSuffix(d.Patch, "\n") {
			d.Patch += "\n"
		}
		d.Patch += DiffTruncated
	}
	return d
}
