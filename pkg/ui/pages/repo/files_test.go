package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/syntax"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/charmbracelet/x/ansi"
)

// A directory with more than git.MaxTreeEntries entries is listed in
// part, and the status bar says the listing is truncated.
func TestFilesListingTruncated(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", dir)
	out := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		b, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(b))
	}
	blob := out("x", "hash-object", "-w", "--stdin")
	var in strings.Builder
	for i := range git.MaxTreeEntries + 1 {
		fmt.Fprintf(&in, "100644 blob %s\tf%06d\n", blob, i)
	}
	gitRun(t, dir, "read-tree", "--prefix=big/", out(in.String(), "mktree"))
	gitRun(t, dir, "commit", "-q", "-m", "big")

	r, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	f := NewFiles(uitest.NewCommon(t, ctx, nil, nil))
	f.SetSize(80, 40)
	f.Update(OpenedMsg(openRepo{path: dir}))
	f.Update(RefMsg{Ref: ref})

	msg, ok := f.updateFilesCmd()().(FileItemsMsg)
	if !ok || msg.truncated || len(msg.items) != 1 {
		t.Fatalf("root listing = %d items, truncated %v; want the one directory in full", len(msg.items), msg.truncated)
	}
	f.Update(msg)
	if got := f.StatusBarInfo(); got != "# 1/1" {
		t.Errorf("status bar = %q, want %q", got, "# 1/1")
	}

	f.path = "big"
	msg, ok = f.updateFilesCmd()().(FileItemsMsg)
	if !ok || !msg.truncated || len(msg.items) != git.MaxTreeEntries {
		t.Fatalf("big listing = %d items, truncated %v; want %d and truncated", len(msg.items), msg.truncated, git.MaxTreeEntries)
	}
	f.Update(msg)
	if got, want := f.StatusBarInfo(), fmt.Sprintf("# 1/%d (truncated)", git.MaxTreeEntries); got != want {
		t.Errorf("status bar = %q, want %q", got, want)
	}
}

// Opening a file larger than config.MaxBlobSize is refused without reading it,
// and a file that fits is shown.
func TestSelectFileSizeLimit(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", dir)
	files := map[string]string{
		"small.txt": "hello\n",
		"big.txt":   strings.Repeat("x", config.MaxBlobSize+1),
		"bin.dat":   "\x00\x01\x02",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first")

	r := openRepo{path: dir}
	gr, err := r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := gr.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gr.TreePath(ref, "")
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	ctx := t.Context()
	f := NewFiles(uitest.NewCommon(t, ctx, cfg, nil))
	f.SetSize(80, 40)
	f.repo = r
	f.ref = ref

	sel := func(name string) any {
		te, err := tree.TreeEntry(name)
		if err != nil {
			t.Fatal(err)
		}
		return f.selectFileCmd(FileItem{entry: te}, name, 0)()
	}

	if msg, ok := sel("small.txt").(FileContentMsg); !ok || msg.content != "hello\n" {
		t.Errorf("small.txt: got %#v, want its content", msg)
	}
	if msg, ok := sel("big.txt").(errorMsg); !ok || !errors.Is(msg.err, git.ErrFileTooLarge) {
		t.Errorf("big.txt: got %#v, want ErrFileTooLarge", msg)
	} else if got := common.ErrorMessage(msg.err); got != "file too large" {
		t.Errorf("big.txt: error screen shows %q, want %q", got, "file too large")
	}
	if msg, ok := sel("bin.dat").(errorMsg); !ok || !errors.Is(msg.err, errBinaryFile) {
		t.Errorf("bin.dat: got %#v, want errBinaryFile", msg)
	}

	// A file is read through the repository the load opens, under the
	// load's context, so a repository that no longer opens fails the load
	// rather than reading through the listing's.
	f.repo = openRepo{path: filepath.Join(dir, "missing")}
	if msg := sel("small.txt"); !isErrorMsg(msg) {
		t.Errorf("small.txt in a repository that does not open: got %T, want errorMsg", msg)
	}
}

// A file listing that fails to load must report an ErrorMsg, so the error is
// shown, rather than a tea.Cmd that nothing ever runs. Fed back to the pane,
// that message must also take the pane off its loading spinner.
func TestFilesUpdateFilesCmdReturnsErrorMsg(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")

	r, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	cases := map[string]struct {
		repo openRepo
		path string
	}{
		"open fails":      {openRepo{path: filepath.Join(dir, "missing")}, ""},
		"tree path fails": {openRepo{path: dir}, "no/such/dir"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := NewFiles(uitest.NewCommon(t, ctx, nil, nil))
			f.SetSize(80, 40)
			f.Update(OpenedMsg(tc.repo))
			f.Update(RefMsg{Ref: ref})
			if f.activeView != filesViewLoading {
				t.Fatalf("after RefMsg activeView = %v; want filesViewLoading", f.activeView)
			}
			f.path = tc.path
			msg := f.updateFilesCmd()()
			if _, ok := msg.(errorMsg); !ok {
				t.Fatalf("updateFilesCmd() = %T %v; want errorMsg", msg, msg)
			}
			f.Update(msg)
			if f.activeView == filesViewLoading {
				t.Fatal("pane still loading after the listing error")
			}
		})
	}
}

// A blame that fails to load must return the pane to the file content with
// blame off, not leave it on the loading spinner.
func TestFilesBlameErrorStopsLoading(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFiles(uitest.NewCommon(t, ctx, nil, nil))
	f.SetSize(80, 40)
	f.activeView = filesViewLoading
	f.blameView = true

	f.Update(errorMsg{err: errors.New("blame failed"), to: paneFiles})
	if f.activeView != filesViewContent {
		t.Errorf("activeView = %v; want filesViewContent", f.activeView)
	}
	if f.blameView {
		t.Error("blame still on after the blame error")
	}
}

// The file listing usually loads while another tab is shown. Its error must
// still reach the Files pane so that pane stops loading.
func TestRepoErrorReachesInactiveFilesPane(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := uitest.NewCommon(t, ctx, nil, nil)
	files := NewFiles(c)
	files.SetSize(80, 40)
	r := New(c, NewReadme(c), files)
	r.SetSize(80, 40)
	files.activeView = filesViewLoading

	r.Update(errorMsg{err: errors.New("listing failed"), to: paneFiles, gen: r.gen})
	if files.activeView == filesViewLoading {
		t.Error("inactive Files pane still loading after the error")
	}
}

// The Files pane is reused across repositories and refs. When a reload fails,
// the pane must not show, or let the user open, the previous listing.
func TestFilesListingErrorClearsPreviousItems(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "README")
	gitRun(t, dir, "commit", "-q", "-m", "first")

	r, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	f := NewFiles(uitest.NewCommon(t, ctx, nil, nil))
	f.SetSize(80, 40)
	f.Update(OpenedMsg(openRepo{path: dir}))
	f.Update(RefMsg{Ref: ref})
	f.Update(f.updateFilesCmd()())
	if n := len(f.selector.Items()); n != 1 {
		t.Fatalf("after the first listing items = %d; want 1", n)
	}

	f.Update(OpenedMsg(openRepo{path: filepath.Join(dir, "missing")}))
	f.Update(RefMsg{Ref: ref})
	msg := f.updateFilesCmd()()
	if _, ok := msg.(errorMsg); !ok {
		t.Fatalf("updateFilesCmd() = %T %v; want errorMsg", msg, msg)
	}
	f.Update(msg)
	if f.activeView != filesViewFiles {
		t.Errorf("activeView = %v; want filesViewFiles", f.activeView)
	}
	if n := len(f.selector.Items()); n != 0 {
		t.Errorf("after the failed reload items = %d; want 0", n)
	}
}

// A file with a non-ASCII name, which git C-quotes in ls-tree output, must be
// listed, shown and blamed under its real name.
func TestFilesNonASCIIName(t *testing.T) {
	// Pin core.quotePath=true, git's default, for every git process the test
	// starts, including the ls-tree that git-module runs, so the name is
	// C-quoted even on a machine whose config sets core.quotePath=false.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.quotePath")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")

	const dirName, fileName = "dossier é", "café.txt"
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", dir)
	if err := os.MkdirAll(filepath.Join(dir, dirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dirName, fileName), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first")

	r, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	f := NewFiles(uitest.NewCommon(t, ctx, nil, nil))
	f.SetSize(80, 40)
	f.Update(OpenedMsg(openRepo{path: dir}))
	f.Update(RefMsg{Ref: ref})

	// Walk into the directory, then onto the file, like selecting them.
	selectOnly := func(want string) FileItem {
		t.Helper()
		msg, ok := f.updateFilesCmd()().(FileItemsMsg)
		items := msg.items
		if !ok || len(items) != 1 {
			t.Fatalf("files at %q = %v, want one item", f.path, items)
		}
		item := items[0].(FileItem)
		if item.Title() != want {
			t.Fatalf("item title = %q, want %q", item.Title(), want)
		}
		f.currentItem = &item
		return item
	}
	dirItem := selectOnly(dirName)
	f.path = path.Join(f.path, dirItem.entry.Name())
	fileItem := selectOnly(fileName)

	msg := f.selectFileCmd(fileItem, path.Join(f.path, fileItem.entry.Name()), 0)()
	content, ok := msg.(FileContentMsg)
	if !ok {
		t.Fatalf("selectFileCmd = %T %v, want FileContentMsg", msg, msg)
	}
	f.Update(content)
	if want := dirName + "/" + fileName; f.StatusBarValue() != want {
		t.Errorf("status bar = %q, want %q", f.StatusBarValue(), want)
	}

	if msg := f.fetchBlameCmd()(); !isBlame(msg) {
		t.Fatalf("fetchBlame = %T %v, want FileBlameMsg", msg, msg)
	}
}

func isBlame(msg any) bool {
	_, ok := msg.(FileBlameMsg)
	return ok
}

// isErrorMsg reports whether msg is an errorMsg.
func isErrorMsg(msg any) bool {
	_, ok := msg.(errorMsg)
	return ok
}

// renderAllocMax is the most bytes the render of a file of 1 MiB may
// allocate, whatever the width. Rendering it inside Update would allocate
// gigabytes, more the wider the terminal.
const renderAllocMax = 160 << 20

// newRenderFiles returns a Files pane width columns wide that has seen a
// window size message.
func newRenderFiles(t *testing.T, width int) *Files {
	t.Helper()
	f := NewFiles(uitest.NewCommon(t, context.Background(), nil, nil))
	f.SetSize(width, 40)
	f.Update(tea.WindowSizeMsg{Width: width, Height: 40})
	return f
}

// newUnboundedFiles is newRenderFiles with a free slot and no highlighting
// budget, so that only a limit shows a file plain.
func newUnboundedFiles(t *testing.T) *Files {
	t.Helper()
	uitest.WaitIdle(t, syntax.Tokenising)
	f := newRenderFiles(t, 120)
	f.code.HighlightBudget = uitest.Unbounded
	return f
}

// openFile hands f a file's content and returns the command Update returned,
// failing if Update allocated more than updateAllocMax.
func openFile(t *testing.T, f *Files, content, name string) tea.Cmd {
	t.Helper()
	msg := FileContentMsg{content: content, ext: name, path: name, load: f.load, isMarkdown: syntax.IsFileMarkdown(content, name)}
	var cmd tea.Cmd
	if n := allocated(func() { cmd = f.Update(msg) }); n > updateAllocMax {
		t.Fatalf("Update allocated %d bytes to open %s; want at most %d", n, name, updateAllocMax)
	}
	return cmd
}

// A file of 1 MiB of newlines is rendered in a command with bounded memory
// at any width, its lines are numbered as they are shown rather than all at
// once, and toggling the numbers does not render it in Update.
//
// Not parallel: it measures the bytes the process allocates.
func TestFileOfNewlinesRendersOutsideUpdate(t *testing.T) {
	content := strings.Repeat("\n", 1<<20)
	for _, width := range []int{100, common.MaxWidth} {
		f := newRenderFiles(t, width)
		cmd := openFile(t, f, content, "empty.txt")
		var n int
		if a := allocated(func() { n = deliverAll(f, cmd) }); a > renderAllocMax {
			t.Fatalf("width %d: the render allocated %d bytes; want at most %d", width, a, renderAllocMax)
		}
		if n == 0 {
			t.Fatalf("width %d: no render result arrived as a message", width)
		}
		if f.activeView != filesViewContent {
			t.Fatalf("width %d: view = %v; want the file", width, f.activeView)
		}
		if strings.Contains(f.code.GetContent(), "│") {
			t.Fatalf("width %d: every line was numbered up front", width)
		}
		if v := ansi.Strip(f.code.View()); !strings.Contains(v, " 1 │") {
			t.Fatalf("width %d: the shown lines have no numbers:\n%s", width, v)
		}

		// "l" hides the line numbers, then shows them again.
		for _, numbered := range []bool{false, true} {
			var cmd tea.Cmd
			if a := allocated(func() { cmd = f.Update(tea.KeyPressMsg{Code: 'l', Text: "l"}) }); a > updateAllocMax {
				t.Fatalf("width %d: Update allocated %d bytes for l; want at most %d", width, a, updateAllocMax)
			}
			if a := allocated(func() { deliverAll(f, cmd) }); a > renderAllocMax {
				t.Fatalf("width %d: the render for l allocated %d bytes; want at most %d", width, a, renderAllocMax)
			}
			if got := strings.Contains(ansi.Strip(f.code.View()), " 1 │"); got != numbered {
				t.Fatalf("width %d: line numbers shown = %v; want %v", width, got, numbered)
			}
		}
	}
}

// A JavaScript file that is one identifier of 1 MiB took minutes to
// highlight. It is shown without highlighting, in a command.
//
// Not parallel: the server highlights at most a few files at once, and
// a parallel test holding the slots would leave this one unhighlighted,
// so it would pass for the wrong reason.
func TestFileWithLongLineNotHighlighted(t *testing.T) {
	f := newRenderFiles(t, 120)
	done := make(chan tea.Cmd, 1)
	go func() {
		done <- f.Update(FileContentMsg{content: strings.Repeat("a", 1<<20), ext: "big.js", path: "big.js", load: f.load})
	}()
	cmd, ok := uitest.Receive(done)
	if !ok {
		t.Fatal("Update did not return within a minute for a 1 MiB identifier")
	}
	rendered := make(chan struct{})
	go func() { deliverAll(f, cmd); close(rendered) }()
	if _, ok := uitest.Receive(rendered); !ok {
		t.Fatal("the render did not finish within a minute for a 1 MiB identifier")
	}
	if c := f.code.GetContent(); strings.Contains(c, "\x1b[") || !strings.Contains(c, "aaaa") {
		t.Fatal("the long line was highlighted, or is missing")
	}
}

// Ordinary files are highlighted; files over 256 KiB, or with a line over
// 4 KiB, are not.
//
// Not parallel: the server highlights at most a few files at once, and
// a parallel test holding the slots would leave this one unhighlighted.
func TestFileHighlightLimit(t *testing.T) {
	small := "package main\n\nfunc main() {}\n"
	f := newUnboundedFiles(t)
	deliverAll(f, openFile(t, f, small, "main.go"))
	if !strings.Contains(f.code.GetContent(), "\x1b[") {
		t.Fatal("a small Go file was not highlighted")
	}

	const line = "func f() {} // one of fewer than ten thousand lines\n"
	for limit, content := range map[string]string{
		"over 256 KiB":           strings.Repeat(line, (256<<10)/len(line)+1),
		"with a line over 4 KiB": "func f() {} // " + strings.Repeat("a", 4<<10) + "\n",
	} {
		f = newUnboundedFiles(t)
		deliverAll(f, openFile(t, f, content, "large.go"))
		if c := f.code.GetContent(); strings.Contains(c, "\x1b[") || !strings.Contains(c, "func f() {}") {
			t.Fatalf("a Go file %s was highlighted, or is missing", limit)
		}
	}
}

// codeRenders runs cmd and returns the code view renders it produces.
func codeRenders(cmd tea.Cmd) []tea.Msg {
	var out []tea.Msg
	for _, m := range runCmd(cmd) {
		if _, ok := m.(code.RenderedMsg); ok {
			out = append(out, m)
		}
	}
	return out
}

// A file of many short lines is not highlighted, however small: chroma's
// cost grows with its tokens.
//
// Not parallel: it measures the bytes the process allocates, and it needs
// a free tokenising slot. The server highlights at most a few files at
// once, and a parallel test holding the slots would leave this file
// unhighlighted, so it would pass for the wrong reason.
func TestFileOfManyLinesNotHighlighted(t *testing.T) {
	f := newUnboundedFiles(t)
	deliverAll(f, openFile(t, f, strings.Repeat("x := 1\n", 20000), "many.go"))
	if c := f.code.GetContent(); strings.Contains(c, "\x1b[") || !strings.Contains(c, "x := 1") {
		t.Fatal("a Go file of 20,000 lines was highlighted, or is missing")
	}
}

// Pressing "l" over and over on a large file renders it once at a time:
// presses while a render runs start none, and one render follows it.
//
// Not parallel: it measures the bytes the process allocates.
func TestFileLineNumberTogglesRenderOneAtATime(t *testing.T) {
	f := newRenderFiles(t, 120)
	deliverAll(f, openFile(t, f, strings.Repeat("x\n", 1<<18), "big.txt"))
	if f.activeView != filesViewContent {
		t.Fatal("the file is not shown")
	}
	l := tea.KeyPressMsg{Code: 'l', Text: "l"}
	first := f.Update(l)
	var later []tea.Cmd
	for range 39 {
		later = append(later, f.Update(l))
	}
	rendered := 0
	for _, cmd := range later {
		rendered += len(codeRenders(cmd))
	}
	if rendered != 0 {
		t.Fatalf("39 presses during a render started %d renders; want 0", rendered)
	}
	r := codeRenders(first)
	if len(r) != 1 {
		t.Fatalf("the first press rendered %d times; want 1", len(r))
	}
	next := codeRenders(f.Update(r[0]))
	if len(next) != 1 {
		t.Fatalf("%d renders followed the first; want 1", len(next))
	}
	if n := len(codeRenders(f.Update(next[0]))); n != 0 {
		t.Fatalf("the last render started %d more", n)
	}
}

// The Files pane decides glamour rendering and the preview key from the
// markdown flag worked out when the file was loaded.
func TestFilesUsesLoadedMarkdownFlag(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	f := NewFiles(c)
	f.SetSize(80, 24)

	f.Update(FileContentMsg{content: "# Title\n", ext: "README.md", isMarkdown: true, load: f.load})
	if !f.code.UseGlamour {
		t.Error("markdown file not rendered with glamour")
	}
	if !helpHasDesc(f.FullHelp(), preview.Help().Desc) {
		t.Error("preview key missing from help for a markdown file")
	}

	f.Update(FileContentMsg{content: "package main\n", ext: "main.go", load: f.load})
	if f.code.UseGlamour {
		t.Error("non-markdown file rendered with glamour")
	}
	if helpHasDesc(f.FullHelp(), preview.Help().Desc) {
		t.Error("preview key shown for a non-markdown file")
	}
}

func helpHasDesc(groups [][]key.Binding, desc string) bool {
	for _, g := range groups {
		for _, k := range g {
			if k.Help().Desc == desc {
				return true
			}
		}
	}
	return false
}
