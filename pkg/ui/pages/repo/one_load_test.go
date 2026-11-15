package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
)

// gatedRepo is a repository whose Open waits for gate while gating is set,
// so that loads stay in flight while the test sends more input, as they do
// when git is slow.
type gatedRepo struct {
	diskRepo
	gating  *atomic.Bool
	gate    chan struct{}
	waiting *atomic.Int32
}

func (r gatedRepo) Open() (*git.Repository, error) {
	if r.gating.Load() {
		r.waiting.Add(1)
		<-r.gate
	}
	return r.diskRepo.Open()
}

// gitLog puts a git on PATH that records each of its commands in a file
// before it runs the real git, and returns a function that counts the
// recorded commands that run sub, such as "ls-tree".
func gitLog(t *testing.T) func(sub string) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the recording git is a shell script")
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "git.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // an executable script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(sub string) int {
		b, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(" "+line+" ", " "+sub+" ") {
				n++
			}
		}
		return n
	}
}

// loop is a stand-in for the bubbletea runtime: it runs each command in its
// own goroutine and hands its message back to the model, one at a time.
type loop struct {
	t       *testing.T
	m       interface{ Update(tea.Msg) tea.Cmd }
	results chan tea.Msg
	running atomic.Int32
}

func (l *loop) spawn(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	l.running.Add(1)
	go func() { l.results <- cmd() }()
}

// deliver hands msg to the model and spawns the commands it returns.
func (l *loop) deliver(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, cmd := range msg {
			l.spawn(cmd)
		}
	default:
		l.spawn(l.m.Update(msg))
	}
}

// settle delivers messages until every running command is waiting at the
// gate.
func (l *loop) settle(waiting *atomic.Int32) {
	deadline := time.Now().Add(uitest.HangGuard)
	for l.running.Load() > waiting.Load() {
		if time.Now().After(deadline) {
			l.t.Fatal("commands never settle")
		}
		select {
		case msg := <-l.results:
			l.running.Add(-1)
			l.deliver(msg)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// 25 "l" and "h" pairs, sent faster than git answers, leave one load
// running: each new load cancels the one before, so the superseded ones
// start no git command. A resize loads nothing.
func TestFilesOneLoadInFlight(t *testing.T) {
	count := gitLog(t)
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 120, 40))
	f.SetSize(120, 40)
	repo := gatedRepo{
		diskRepo: newPinRepo(t),
		gating:   new(atomic.Bool),
		gate:     make(chan struct{}),
		waiting:  new(atomic.Int32),
	}
	gr, err := repo.Open()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := gr.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	l := &loop{t: t, m: f, results: make(chan tea.Msg, 1000)}
	l.deliver(RepoMsg(repo))
	l.deliver(RefMsg{Ref: ref})
	l.settle(repo.waiting)
	selectItem(t, f.selector, "file.txt")

	// A resize in the file list lists nothing again.
	before := count("ls-tree")
	l.deliver(tea.WindowSizeMsg{Width: 100, Height: 30})
	l.settle(repo.waiting)
	if n := count("ls-tree") - before; n != 0 {
		t.Fatalf("a resize listed the directory %d times; want none", n)
	}

	repo.gating.Store(true)
	before, files := count("ls-tree"), count("check-attr")
	for range 25 {
		l.deliver(tea.KeyPressMsg{Code: 'l', Text: "l"})
		l.settle(repo.waiting)
		l.deliver(tea.KeyPressMsg{Code: 'h', Text: "h"})
		l.settle(repo.waiting)
	}
	started := repo.waiting.Load()
	close(repo.gate)
	for l.running.Load() > 0 {
		select {
		case <-l.results:
			l.running.Add(-1)
		case <-time.After(uitest.HangGuard):
			t.Fatal("loads never finish")
		}
	}
	listings, opened := count("ls-tree")-before, count("check-attr")-files
	t.Logf("%d loads reached the repository; %d listings and %d file loads ran git", started, listings, opened)
	if started < 50 {
		t.Fatalf("only %d loads started; the test did not keep them in flight", started)
	}
	if listings+opened > 1 {
		t.Fatalf("%d listings and %d file loads ran git; want at most one load, the newest", listings, opened)
	}
	if cancelled := int(started) - listings - opened; cancelled < 49 {
		t.Fatalf("%d superseded loads were cancelled; want 49", cancelled)
	}
	if listings != 1 {
		t.Fatalf("the newest load, a listing, ran %d times; want once", listings)
	}
}

// Each resize or footer toggle reloads the commit page; a reload cancels the
// one before, whose command then sends nothing.
func TestLogPageLoadSupersedes(t *testing.T) {
	l := newTestLog(t, filepath.Join(t.TempDir(), "repo"))
	first := resizeLog(l, 80, 30)
	second := resizeLog(l, 80, 20)
	for _, m := range runCmd(first) {
		if _, ok := m.(LogItemsMsg); ok {
			t.Fatal("a superseded page load ran to completion")
		}
	}
	findMsg[LogItemsMsg](t, runCmd(second))
}
