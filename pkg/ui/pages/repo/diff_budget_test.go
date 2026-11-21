package repo

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/charmbracelet/x/ansi"
)

// updateAllocMax is the most bytes Update may allocate for a message that
// carries a diff of several MiB, or for a resize while one is shown. It is far
// below the size of the diff: Update must hand the work to a command.
const updateAllocMax = 512 << 10

// allocated returns the bytes f allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// largeDiff returns a diff of about 3 MiB, cut to its limits as git.Diff
// would cut it.
func largeDiff() *git.Diff {
	var b strings.Builder
	b.WriteString("diff --git a/big.txt b/big.txt\nnew file mode 100644\n--- /dev/null\n+++ b/big.txt\n@@ -0,0 +1,30000 @@\n")
	line := "+" + strings.Repeat("x", 99) + "\n"
	for b.Len() < 3<<20 {
		b.WriteString(line)
	}
	b.WriteString(git.DiffTruncated)
	return &git.Diff{
		Stat:      " big.txt | 30000 +++++\n 1 file changed, 30000 insertions(+)\n",
		Patch:     b.String(),
		Truncated: true,
	}
}

// deliverAll runs cmd and hands every message it produces back to m, and so
// on until none is left. It returns how many messages it delivered.
func deliverAll(m interface{ Update(tea.Msg) tea.Cmd }, cmd tea.Cmd) int {
	n := 0
	for msgs := runCmd(cmd); len(msgs) > 0; {
		msg := msgs[0]
		msgs = msgs[1:]
		n++
		msgs = append(msgs, runCmd(m.Update(msg))...)
	}
	return n
}

// A commit's diff of several MiB is rendered in a command, not in Update, and
// is not highlighted; a resize that keeps the width does not render it again.
//
// Not parallel: it measures the bytes the process allocates.
func TestLogLargeDiffRendersOutsideUpdate(t *testing.T) {
	ctx := context.Background()
	l := NewLog(uitest.NewCommon(t, ctx, nil, nil))
	l.SetSize(120, 40)
	l.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	sig := &gitm.Signature{Name: "a", Email: "a@b", When: time.Now()}
	commit := &git.Commit{
		ID:        gitm.MustIDFromString("0123456789abcdef0123456789abcdef01234567"),
		Author:    sig,
		Committer: sig,
		Message:   "large-subject\n",
	}
	l.Update(selector.SelectMsg{IdentifiableItem: LogItem{Commit: commit}})
	diff := largeDiff()

	var cmd tea.Cmd
	if n := allocated(func() { cmd = l.Update(LogDiffMsg{commit: commit, diff: diff, gen: l.gen}) }); n > updateAllocMax {
		t.Fatalf("Update allocated %d bytes for a %d-byte diff; want at most %d", n, len(diff.Patch), updateAllocMax)
	}
	if deliverAll(l, cmd) == 0 {
		t.Fatal("no render result arrived as a message")
	}
	if l.activeView != logViewDiff {
		t.Fatalf("view = %v after the render; want the diff", l.activeView)
	}
	content := ansi.Strip(l.vp.GetContent())
	for _, want := range []string{"large-subject", "big.txt", "too large to show in full", strings.TrimSpace(git.DiffTruncated)} {
		if !strings.Contains(content, want) {
			t.Errorf("diff view lacks %q", want)
		}
	}

	// A resize that keeps the width renders nothing, neither in Update nor
	// in its command.
	if n := allocated(func() { deliverAll(l, l.Update(tea.WindowSizeMsg{Width: 120, Height: 30})) }); n > updateAllocMax {
		t.Fatalf("a height-only resize allocated %d bytes; want at most %d", n, updateAllocMax)
	}

	// A width change renders again, in a command.
	if n := allocated(func() { cmd = l.Update(tea.WindowSizeMsg{Width: 100, Height: 30}) }); n > updateAllocMax {
		t.Fatalf("Update allocated %d bytes on a width change; want at most %d", n, updateAllocMax)
	}
	if deliverAll(l, cmd) == 0 {
		t.Fatal("a width change did not render the diff again")
	}
	if l.activeView != logViewDiff {
		t.Fatalf("view = %v after the resize; want the diff", l.activeView)
	}
}

// A stash patch of several MiB is rendered in a command, not in Update; a
// resize that keeps the width does not render it again.
//
// Not parallel: it measures the bytes the process allocates.
func TestStashLargePatchRendersOutsideUpdate(t *testing.T) {
	ctx := context.Background()
	s := NewStash(uitest.NewCommon(t, ctx, nil, nil))
	s.SetSize(120, 40)
	s.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	s.Update(StashListMsg{stashes: []*gitm.Stash{{Message: "large-stash"}}})
	diff := largeDiff()

	var cmd tea.Cmd
	if n := allocated(func() { cmd = s.Update(StashPatchMsg{Diff: diff}) }); n > updateAllocMax {
		t.Fatalf("Update allocated %d bytes for a %d-byte patch; want at most %d", n, len(diff.Patch), updateAllocMax)
	}
	if deliverAll(s, cmd) == 0 {
		t.Fatal("no render result arrived as a message")
	}
	if s.state != stashStatePatch {
		t.Fatalf("state = %v after the render; want the patch", s.state)
	}
	if content := ansi.Strip(s.vp.GetContent()); !strings.Contains(content, "large-stash") || !strings.Contains(content, "big.txt") {
		t.Fatalf("patch view lacks the title or the patch")
	}
	if n := allocated(func() { deliverAll(s, s.Update(tea.WindowSizeMsg{Width: 120, Height: 30})) }); n > updateAllocMax {
		t.Fatalf("a height-only resize allocated %d bytes; want at most %d", n, updateAllocMax)
	}
}

// A patch over diffHighlightMax is shown as plain text.
//
// Not parallel: the server highlights at most a few files at once, and
// a parallel test holding the slots would leave this one unhighlighted.
func TestLargeDiffNotHighlighted(t *testing.T) {
	out := renderDiff(largeDiff(), 120)
	if strings.Contains(out, "\x1b[") {
		t.Fatal("a patch of several MiB was highlighted")
	}
	if !strings.Contains(out, "+xxxx") {
		t.Fatal("the patch is missing from the render")
	}
	// Short lines cost chroma far more than their bytes: a patch of many
	// lines is not highlighted either, however small.
	lines := &git.Diff{Patch: strings.Repeat("+\n", 20000)}
	if strings.Contains(renderDiff(lines, 120), "\x1b[") {
		t.Fatal("a patch of 20000 lines was highlighted")
	}
	small := &git.Diff{Patch: "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-old\n+new\n"}
	// Plain text is coloured too, so look for the colours a diff's
	// highlighting gives removed and added lines.
	out = renderDiff(small, 120)
	for _, want := range []string{"\x1b[38;5;203m-old", "\x1b[38;5;42m+new"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a small patch was not highlighted as a diff: it lacks %q:\n%q", want, out)
		}
	}
}
