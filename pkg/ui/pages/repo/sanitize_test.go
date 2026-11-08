package repo

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// escapes holds sequences the bubbletea renderer would pass to the terminal:
// a DCS, an APC, an OSC 52 clipboard write, a C1 CSI and a BEL.
const escapes = "\x1bP1|x\x1b\\\x1b_Gx\x1b\\\x1b]52;c;Zm9v\x07\u009b2J\x07"

// csi matches the CSI sequences the TUI itself emits (SGR styles and zone
// marks); anything else that controls the terminal must come from data.
var csi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// assertClean fails if out, less the TUI's own CSI sequences, holds a
// control character other than newline and tab, or lacks any of want.
func assertClean(t *testing.T, out string, want ...string) {
	t.Helper()
	rest := csi.ReplaceAllString(out, "")
	for _, r := range rest {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)) {
			t.Fatalf("output holds control character %q: %q", r, out)
		}
	}
	for _, w := range want {
		if !strings.Contains(rest, w) {
			t.Errorf("output lacks %q: %q", w, rest)
		}
	}
}

func escapedCommit() *git.Commit {
	sig := &gitm.Signature{Name: "eve" + escapes, Email: "eve@example.com" + escapes, When: time.Now()}
	return &git.Commit{
		ID:        gitm.MustIDFromString("0123456789abcdef0123456789abcdef01234567"),
		Author:    sig,
		Committer: sig,
		Message:   "subject" + escapes + "\r\n\r\nbody" + escapes + "\r\n",
	}
}

func TestHeaderSanitisesProjectNameAndDescription(t *testing.T) {
	r := &Repo{
		common:       common.NewCommon(context.Background(), 120, 24),
		selectedRepo: fakeRepo{projectName: "proj" + escapes, description: "desc" + escapes},
	}
	assertClean(t, r.headerView(), "proj", "desc")
}

func TestLogSanitisesCommit(t *testing.T) {
	c := common.NewCommon(context.Background(), 120, 24)
	commit := escapedCommit()

	l := &Log{common: c, activeView: logViewCommits, activeCommit: commit}
	assertClean(t, l.renderCommit(commit), "subject", "body", "eve")
	assertClean(t, l.StatusBarValue(), "eve")

	item := LogItem{commit}
	d := LogItemDelegate{&c}
	m := list.New([]list.Item{item}, d, 120, 24)
	var buf bytes.Buffer
	d.Render(&buf, m, 0, item)
	assertClean(t, buf.String(), "subject", "eve")
}

func TestStashItemSanitisesMessage(t *testing.T) {
	item := StashItem{&gitm.Stash{Message: "stash" + escapes}}
	assertClean(t, item.Title(), "stash")
}

// File contents and READMEs reach the terminal through chroma and glamour,
// which keep DCS and APC strings.
func TestCodeSanitisesContent(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 120, 40)

	f := NewFiles(c)
	f.SetSize(120, 40)
	f.Update(FileContentMsg{"package main\n\n// comment" + escapes + "\nfunc main() {}\n", "main.go"})
	assertClean(t, f.code.View(), "comment")
	// The raw bytes are kept for copying to the clipboard.
	if !strings.Contains(f.currentContent.content, escapes) {
		t.Errorf("file content for copying was altered: %q", f.currentContent.content)
	}

	r := NewReadme(c)
	r.SetSize(120, 40)
	r.Update(ReadmeMsg{Content: "# Title" + escapes + "\n\ntext" + escapes + "\n", Path: "README.md"})
	assertClean(t, r.code.View(), "text")
}

// A stray 8-bit byte from a legacy-encoded file, or an unterminated string
// sequence, must not hide the lines after it.
func TestCodeKeepsContentAfterBrokenSequences(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 120, 40)
	for _, content := range []string{
		"line1 na\x9eve\nline2 VISIBLE_AFTER_C1\n",
		"line1 \x90\nline2 VISIBLE_AFTER_C1\n",
		"line1\nhidden \x1bP start\nline3 MALICIOUS_CODE\n",
		"line1\nhidden \x1b]0;title\nline3 MALICIOUS_CODE\n",
	} {
		f := NewFiles(c)
		f.SetSize(120, 40)
		f.Update(FileContentMsg{content, "file.txt"})
		want := "MALICIOUS_CODE"
		if strings.Contains(content, "VISIBLE_AFTER_C1") {
			want = "VISIBLE_AFTER_C1"
		}
		assertClean(t, f.code.View(), want)
	}
}

// The stash patch view is built from pieces the TUI has already styled
// (and sanitised at their source); the code view must keep those styles.
func TestStashPatchKeepsStyling(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 120, 40)

	const raw = "diff --git a/a.txt b/a.txt\n" +
		"index 0123456..789abcd 100644\n" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n"
	done := make(chan gitm.SteamParseDiffResult, 1)
	gitm.StreamParseDiff(strings.NewReader(raw), done, 10, 100, 1000)
	res := <-done
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	diff := &git.Diff{Diff: res.Diff}
	for _, df := range res.Diff.Files {
		sections := make([]*git.DiffSection, 0, len(df.Sections))
		for _, ds := range df.Sections {
			sections = append(sections, &git.DiffSection{DiffSection: ds})
		}
		diff.Files = append(diff.Files, &git.DiffFile{DiffFile: df, Sections: sections})
	}

	s := NewStash(c)
	s.SetSize(120, 40)
	s.Update(StashListMsg{&gitm.Stash{Message: "stash title"}})
	s.Update(StashPatchMsg{diff})
	out := s.vp.View()
	for _, want := range []string{
		c.Styles.Stash.Title.Render("stash title"),
		c.Styles.Log.CommitStatsAdd.Render("+"),
		c.Styles.Log.CommitStatsDel.Render("-"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stash patch view lacks styled %q: %q", want, out)
		}
	}
}
