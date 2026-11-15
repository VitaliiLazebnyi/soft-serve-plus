package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aymanbagabas/git-module"
)

// Default caps on a diff; see DiffLimits. They are variables for tests.
var (
	// DiffMaxFiles is the most files a diff shows.
	DiffMaxFiles = 1000
	// DiffMaxFileLines is the most lines of one file's patch a diff shows.
	DiffMaxFileLines = 1000
	// DiffMaxLineBytes is the most bytes of one line a diff shows. A line
	// is cut at the start of the last whole UTF-8 character that fits.
	DiffMaxLineBytes = 1000
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
	// is left out. The cut backs off to the start of a UTF-8 character it
	// would split.
	LineBytes int
	// Bytes is the most bytes of output, stat and patch together. The diff
	// ends at the last whole line that fits.
	Bytes int
}

// DefaultDiffLimits returns the limits for a diff shown to a reader:
// DiffMaxFiles, DiffMaxFileLines, DiffMaxLineBytes and DiffMaxBytes.
func DefaultDiffLimits() DiffLimits {
	return DiffLimits{
		Files:     DiffMaxFiles,
		FileLines: DiffMaxFileLines,
		LineBytes: DiffMaxLineBytes,
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
//
// A changed blob over the size cap would be cut anyway, and with --binary
// git spends seconds and gigabytes encoding a large one first, so the sizes
// of the changed blobs are checked before: a patch with one over the cap
// comes back empty and Truncated without running the patch at all.
func (r *Repository) Patch(commit *Commit) (*Diff, error) {
	args, err := commitDiffArgs(commit)
	if err != nil {
		return nil, err
	}
	limits := PatchLimits()
	if limits.Bytes > 0 {
		fits, err := r.changedBlobsFit(args, int64(limits.Bytes))
		if err != nil {
			return nil, err
		}
		if !fits {
			return &Diff{Truncated: true}, nil
		}
	}
	return r.runDiff(r.diffCommand(patchArgs, args...), limits)
}

// changedBlobsFit reports whether every blob the diff named by args changes,
// before and after, is at most limit bytes.
//
// The listing has no -M: rename detection would load both blobs of a
// renamed file to compare them, which is the cost this check avoids. A
// rename then shows as a delete and an add of the same two blobs, so the
// sizes are the same.
//
// The listing is capped at twice limit. A raw record (about 100 bytes plus
// the path) can be longer than the patch it stands for, as for a change of
// mode only, so the same cap would refuse patches that fit; a listing over
// twice the cap still means a patch too large to be worth making.
func (r *Repository) changedBlobsFit(args []string, limit int64) (bool, error) {
	raw := &cappedBuffer{max: int(2 * limit)}
	var stderr bytes.Buffer
	err := r.plainCommand("diff-tree").
		AddArgs("-r", "--raw", "-z", "--full-index").
		AddArgs(args...).
		RunInDirPipeline(raw, &stderr, r.Path)
	if raw.over {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("diff-tree: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	ids, err := rawBlobIDs(raw.buf.String())
	if err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return true, nil
	}
	in := strings.NewReader(strings.Join(ids, "\n") + "\n")
	var out bytes.Buffer
	stderr.Reset()
	if err := r.plainCommand("cat-file", "--batch-check=%(objectsize)").
		RunInDirWithOptions(r.Path, git.RunInDirOptions{Stdin: in, Stdout: &out, Stderr: &stderr}); err != nil {
		return false, fmt.Errorf("cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	for line := range strings.Lines(out.String()) {
		if n, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil && n > limit {
			return false, nil
		}
	}
	return true, nil
}

// plainCommand returns a git command under r's context without user or
// system config.
func (r *Repository) plainCommand(args ...string) *git.Command {
	return git.NewCommand(args...).AddOptions(git.CommandOptions{
		Envs:    []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"},
		Context: r.Context(),
	})
}

// rawBlobIDs returns the blob IDs, before and after, of each change in
// raw, the output of diff-tree --raw -z --full-index. Each change is a
// header ":<mode> <mode> <id> <id> <status>" and then one path, or two
// for a rename or copy (status R or C), each ended by a NUL. Paths are raw
// bytes and can look like a header, so records are read by position, never
// by their look, and only a full hex object ID is taken: anything else
// would reach cat-file as a revision to resolve. A submodule's commit and
// the zero ID of an added or deleted file are left out.
func rawBlobIDs(raw string) ([]string, error) {
	recs := strings.Split(raw, "\x00")
	var ids []string
	for i := 0; i < len(recs); {
		rec := recs[i]
		if rec == "" && i == len(recs)-1 {
			break
		}
		f := strings.Fields(strings.TrimPrefix(rec, ":"))
		if !strings.HasPrefix(rec, ":") || len(f) != 5 {
			return nil, fmt.Errorf("diff-tree: unexpected raw record %q", rec)
		}
		paths := 1
		if s := f[4]; s != "" && (s[0] == 'R' || s[0] == 'C') {
			paths = 2
		}
		i += 1 + paths
		for j, id := range f[2:4] {
			if f[j] == "160000" || strings.Trim(id, "0") == "" {
				continue
			}
			if !isObjectID(id) {
				return nil, fmt.Errorf("diff-tree: unexpected object ID %q", id)
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// isObjectID reports whether s is a full SHA-1 or SHA-256 object ID in
// lowercase hex.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// cappedBuffer keeps up to max bytes and fails the write that would go over.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		b.over = true
		return 0, errDiffFull
	}
	return b.buf.Write(p)
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
	cut := false
	if w.lineCut {
		// The rest of a line already cut is dropped.
		b = nil
	} else if lim := w.limits.LineBytes; lim > 0 {
		// Keep enough to recognise a file header however small the cap.
		lim = max(lim, len(diffHeader))
		if room := lim - len(w.line); len(b) > room {
			b = b[:max(room, 0)]
			w.lineCut = true
			cut = true
		}
	}
	// A line that cannot fit in the total is not kept at all, so a huge
	// line under no line cap costs no more memory than the total cap.
	if lim := w.limits.Bytes; lim > 0 && w.out.Len()+len(w.line)+len(b) > lim {
		w.stop()
		return
	}
	w.line = append(w.line, b...)
	if cut {
		// Do not end the line inside a UTF-8 character.
		w.line = w.line[:len(w.line)-partialRuneLen(w.line)]
	}
	if nl {
		w.line = append(w.line, '\n')
	}
}

// partialRuneLen returns how many bytes at the end of b are the start of a
// UTF-8 character that is not complete there: 0 when b ends on a whole
// character, or on bytes that are not UTF-8, which are kept as they are.
func partialRuneLen(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return len(b) - i
			}
			return 0
		}
	}
	return 0
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
