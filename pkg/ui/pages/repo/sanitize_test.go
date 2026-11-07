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
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
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
	for _, tc := range []struct {
		ext, content, want string
		glamour            bool
	}{
		{ext: "main.go", content: "package main\n\n// comment" + escapes + "\nfunc main() {}\n", want: "comment"},
		{ext: "README.md", content: "# Title" + escapes + "\n\ntext" + escapes + "\n", want: "text", glamour: true},
	} {
		cd := code.New(c, "", "")
		cd.UseGlamour = tc.glamour
		cd.SetSize(120, 40)
		cd.SetContent(tc.content, tc.ext)
		assertClean(t, cd.View(), tc.want)
	}
}
