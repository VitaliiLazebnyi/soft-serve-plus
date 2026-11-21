package repo

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// R10: a directory with more than git.MaxTreeEntries entries is listed in
// part, and the status bar says the listing is truncated.
func TestFilesListingTruncated(t *testing.T) {
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

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.Update(RepoMsg(openRepo{path: dir}))
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
	gr, err := r.Open()
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
	ctx := config.WithContext(t.Context(), cfg)
	f := NewFiles(common.NewCommon(ctx, 80, 40))
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
}

// A file listing that fails to load must report an ErrorMsg, so the error is
// shown, rather than a tea.Cmd that nothing ever runs. Fed back to the pane,
// that message must also take the pane off its loading spinner.
func TestFilesUpdateFilesCmdReturnsErrorMsg(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	cases := map[string]struct {
		repo openRepo
		path string
	}{
		"open fails":      {openRepo{path: filepath.Join(dir, "missing")}, ""},
		"tree path fails": {openRepo{path: dir}, "no/such/dir"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := NewFiles(common.NewCommon(ctx, 80, 40))
			f.Update(RepoMsg(tc.repo))
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
	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
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
	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	c := common.NewCommon(ctx, 80, 40)
	files := NewFiles(c)
	r := New(c, NewReadme(c), files)
	files.activeView = filesViewLoading

	r.Update(errorMsg{err: errors.New("listing failed"), to: paneFiles, gen: r.gen})
	if files.activeView == filesViewLoading {
		t.Error("inactive Files pane still loading after the error")
	}
}

// The Files pane is reused across repositories and refs. When a reload fails,
// the pane must not show, or let the user open, the previous listing.
func TestFilesListingErrorClearsPreviousItems(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "README")
	gitRun(t, dir, "commit", "-q", "-m", "first")

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.Update(RepoMsg(openRepo{path: dir}))
	f.Update(RefMsg{Ref: ref})
	f.Update(f.updateFilesCmd()())
	if n := len(f.selector.Items()); n != 1 {
		t.Fatalf("after the first listing items = %d; want 1", n)
	}

	f.Update(RepoMsg(openRepo{path: filepath.Join(dir, "missing")}))
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

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.Update(RepoMsg(openRepo{path: dir}))
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
