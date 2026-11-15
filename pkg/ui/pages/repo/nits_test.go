package repo

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/x/ansi"
)

// bidi are Unicode bidirectional controls: right-to-left override, left-to-
// right isolate and pop directional isolate.
const bidi = "\u202e\u2066\u2069"

// Commit subjects, authors and repository descriptions lose their
// bidirectional controls, as file names do, so they cannot reorder the text
// around them in a terminal that implements bidi.
func TestBidiControlsRemoved(t *testing.T) {
	repo := newDiskRepo(t, "r", "main", "# r\n")
	gitRun(t, repo.path, "commit", "-q", "--allow-empty",
		"--author", "auth"+bidi+"or <a@b>", "-m", "sub"+bidi+"ject\n\nbo"+bidi+"dy")

	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 120, 40)
	p := &pinPage{t: t, repo: repo, log: NewLog(c)}
	p.r = New(c, p.log)
	p.r.SetSize(120, 40)
	p.open()
	p.wantView("commit list", "subject", "author")

	p.r.selectedRepo = fakeRepo{projectName: "pro" + bidi + "ject", description: "des" + bidi + "cription"}
	p.settle(keyEnter)
	p.wantView("commit", "subject", "author", "body", "project", "description")
	if v := p.r.View(); strings.ContainsAny(v, bidi) {
		t.Fatalf("the page shows bidirectional controls:\n%q", ansi.Strip(v))
	}
	if strings.ContainsAny(p.log.StatusBarValue(), bidi) {
		t.Fatalf("the status bar value %q has bidirectional controls", p.log.StatusBarValue())
	}
}

// A submodule in the file list has nothing in this repository to show. Opening
// it says so, as a user error, rather than running git on a commit the
// repository does not have, which logged a server error per click.
func TestFilesSubmoduleIsUserError(t *testing.T) {
	repo := newDiskRepo(t, "r", "main", "# r\n")
	gitRun(t, repo.path, "update-index", "--add", "--cacheinfo",
		"160000,0123456789abcdef0123456789abcdef01234567,mod")
	gitRun(t, repo.path, "commit", "-q", "-m", "add a submodule")

	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.SetSize(80, 40)
	f.Update(RepoMsg(repo))
	ref, ok := updateRefCmd(t.Context(), repo, 1)().(RefMsg)
	if !ok {
		t.Fatal("updateRefCmd did not load the reference")
	}
	deliverAll(f, f.Update(ref))
	selectItem(t, f.selector, "mod")

	msg := findMsg[errorMsg](t, runCmd(f.Update(selector.SelectMsg{IdentifiableItem: f.selector.SelectedItem()})))
	if !common.IsUserError(msg.err) || common.ErrorMessage(msg.err) != "submodule" {
		t.Fatalf("opening a submodule reported %v (shown as %q); want the user error %q",
			msg.err, common.ErrorMessage(msg.err), "submodule")
	}
}

// Turning blame off on a Markdown file shows it rendered again, as it was
// before blame was turned on.
func TestFilesBlameOffRestoresMarkdown(t *testing.T) {
	p := newPinPage(t)
	p.open()
	p.tab(1)
	selectItem(t, p.files.selector, "README.md")
	p.settle(keyEnter)
	if !p.files.code.UseGlamour {
		t.Fatal("the Markdown file is not rendered")
	}
	p.notView("rendered", "# Pin")

	p.settle(keyB)
	if p.files.code.UseGlamour {
		t.Fatal("blame shows the Markdown file rendered")
	}
	p.wantView("blame", "# Pin", "first-subject")

	p.settle(keyB)
	if !p.files.code.UseGlamour {
		t.Fatal("after blame is turned off, the Markdown file is shown as source")
	}
	p.wantView("blame off", "readme-body")
	p.notView("blame off", "# Pin", "first-subject")
}

// newReadmePage returns a repository page with only the Readme tab over repo,
// whose files are no larger than limit.
func newReadmePage(t *testing.T, repo diskRepo, limit int64) *pinPage {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.MaxBlobSize = limit
	c := common.NewCommon(config.WithContext(context.Background(), cfg), 120, 40)
	p := &pinPage{t: t, repo: repo, readme: NewReadme(c)}
	p.r = New(c, p.readme)
	p.r.SetSize(120, 40)
	return p
}

// A README larger than the size limit is reported as such, not as missing; a
// repository without one still shows that it has none.
func TestReadmeTooLarge(t *testing.T) {
	big := newDiskRepo(t, "big", "main", "# big\n\n"+strings.Repeat("text ", 100)+"\n")
	p := newReadmePage(t, big, 64)
	p.open()
	p.wantView("too large", "larger than the size limit")
	p.notView("too large", "No readme found.")

	none := newDiskRepo(t, "none", "main", "# none\n")
	gitRun(t, none.path, "rm", "-q", "README.md")
	gitRun(t, none.path, "commit", "-q", "-m", "no readme")
	p = newReadmePage(t, none, 64)
	p.open()
	p.wantView("none", "No readme found.")
}

// A README that fails to load for another reason reports the error, and
// does not leave the README of the ref shown before on screen.
func TestReadmeLoadErrorReported(t *testing.T) {
	repo := newDiskRepo(t, "r", "main", "# r\n\nold-readme\n")
	p := newReadmePage(t, repo, 1<<20)
	p.open()
	p.wantView("open", "old-readme")

	// The repository goes away after its reference was read.
	ref, ok := updateRefCmd(t.Context(), repo, p.r.gen)().(RefMsg)
	if !ok {
		t.Fatal("updateRefCmd did not load the reference")
	}
	if err := os.Rename(repo.path, repo.path+".gone"); err != nil {
		t.Fatal(err)
	}
	var errs []errorMsg
	queue := p.update(ref)
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 1000 {
			t.Fatal("page never settles")
		}
		msg := queue[0]
		queue = queue[1:]
		if m, ok := msg.(errorMsg); ok {
			errs = append(errs, m)
		}
		if _, ok := msg.(common.ErrorMsg); ok {
			continue
		}
		queue = append(queue, nonTicks(p.update(msg))...)
	}
	if len(errs) != 1 || errs[0].to != paneReadme {
		t.Fatalf("the failed README load reported %v; want one error for the Readme tab", errs)
	}
	p.notView("after the error", "old-readme")
}

// The status bar shows what a tab's load changed as soon as the result
// arrives, whichever result it is: here the commit count, which changes the
// number of pages.
func TestStatusBarRefreshedOnEveryResult(t *testing.T) {
	p := newPinPage(t)
	p.open()
	p.tab(2)
	if v := ansi.Strip(p.r.statusbar.View()); !strings.Contains(v, "p. 1/1") {
		t.Fatalf("status bar before the count: %q; want p. 1/1", v)
	}
	p.update(LogCountMsg{count: 1000, gen: p.r.gen})
	per := p.log.selector.PerPage()
	want := fmt.Sprintf("p. 1/%d", (1000+per-1)/per)
	if v := ansi.Strip(p.r.statusbar.View()); !strings.Contains(v, want) {
		t.Fatalf("status bar after the count: %q; want %s", v, want)
	}
}

// SanitizeText removes bidirectional controls along with what
// utils.Sanitize removes, and keeps joiners that emoji need.
func TestSanitizeTextBidi(t *testing.T) {
	t.Parallel()
	in := "a\u202eb\x1b[31mc\u200dd\u061ce\u200ff"
	if got, want := common.SanitizeText(in), "abc\u200ddef"; got != want {
		t.Fatalf("SanitizeText(%q) = %q; want %q", in, got, want)
	}
	if got := common.SanitizeText("plain\ttext\n"); got != "plain\ttext\n" {
		t.Fatalf("plain text changed to %q", got)
	}
}
