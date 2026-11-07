package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/tabs"
)

// hostile holds the string sequences the bubbletea renderer passes through to
// the terminal: a DCS (DECRQSS, which makes the terminal answer), an APC
// (kitty graphics), an OSC 8 hyperlink with a hidden target, an OSC 52
// clipboard write, a C1 CSI encoded as UTF-8 and a BEL. It has no slash, so
// it can be part of a path in a git tree, but it holds control characters,
// ':' and '\', which Windows does not allow in a file name, so it never
// goes on disk.
const hostile = "\x1bP1$qm\x1b\\" +
	"\x1b_Gf=100;AAAA\x1b\\" +
	"\x1b]8;;https:evil.example\x1b\\link\x1b]8;;\x1b\\" +
	"\x1b]52;c;Zm9v\x07" +
	"\u009b2J" +
	"\x07"

// escapedRepo is a repository whose metadata and git data all carry hostile.
type escapedRepo struct {
	fakeRepo
	path string
}

func (r escapedRepo) Open() (*git.Repository, error) { return git.Open(r.path) }

// newEscapedRepo makes a repository where everything the TUI shows from git
// carries hostile: commit subjects and bodies, author names, a file name, a
// source file, a README, a tag message, a branch name, a tag name and a
// stash message. Git allows no ESC in a ref name, so the branch and tag
// names carry a C1 CSI, the only control git lets through.
func newEscapedRepo(t *testing.T) escapedRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	files := map[string]string{
		"README.md": "# Readme" + hostile + "\n\nreadme-text" + hostile + "\n",
		"main.go":   "package main\n\n// go-comment" + hostile + "\nfunc main() {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", "-A")
	// The hostile file name goes straight into the index, so no file of that
	// name is made; core.protectNTFS would reject the '\' in it on Windows.
	// Marking it skip-worktree keeps the stash below from recording it as
	// deleted, and git from checking it out when the stash resets the tree.
	hostileName := "name" + hostile + "end.txt"
	blob := gitStdin(t, dir, "plain-text\n", "hash-object", "-w", "--stdin")
	gitRun(t, dir, "-c", "core.protectNTFS=false", "update-index", "--add",
		"--cacheinfo", "100644,"+blob+","+hostileName)
	gitRun(t, dir, "-c", "core.protectNTFS=false", "update-index", "--skip-worktree", "--", hostileName)
	gitRun(t, dir, "-c", "user.name=eve-author"+hostile,
		"commit", "-q", "--author", "eve-author"+hostile+" <eve@example.com>",
		"-m", "commit-subject"+hostile+"\n\ncommit-body"+hostile)
	gitRun(t, dir, "tag", "-a", "v1", "-m", "tag-message"+hostile)
	gitRun(t, dir, "branch", "branch\u009b2Jname")
	gitRun(t, dir, "tag", "tag\u009b2Jname")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "stash", "push", "-q", "-m", "stash-message"+hostile)
	if _, err := os.Lstat(filepath.Join(dir, hostileName)); !os.IsNotExist(err) {
		t.Fatalf("hostile file name reached the disk: %v", err)
	}
	return escapedRepo{
		fakeRepo: fakeRepo{
			projectName: "project-name" + hostile,
			description: "description" + hostile,
		},
		path: dir,
	}
}

// gitStdin runs git in dir with stdin as its input and returns its output.
func gitStdin(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// driver feeds messages to a model and runs the commands it returns, the way
// the bubbletea runtime would, until no more messages come.
type driver struct {
	t *testing.T
	m common.Model
}

// cmdType lets send unpack the unexported message tea.Sequence returns, a
// slice of commands like tea.BatchMsg.
var cmdType = reflect.TypeOf((*tea.Cmd)(nil)).Elem()

func (d *driver) send(msg tea.Msg) {
	d.t.Helper()
	queue := []tea.Msg{msg}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 10000 {
			d.t.Fatal("model never settles")
		}
		msg, queue = queue[0], queue[1:]
		switch msg := msg.(type) {
		case nil, spinner.TickMsg:
			// Spinners tick forever; nothing a test checks depends on them.
			continue
		case common.ErrorMsg:
			d.t.Fatalf("model reported an error: %v", msg)
		case tea.BatchMsg:
			for _, cmd := range msg {
				queue = append(queue, d.run(cmd))
			}
			continue
		}
		if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == cmdType {
			for i := range v.Len() {
				queue = append(queue, d.run(v.Index(i).Interface().(tea.Cmd)))
			}
			continue
		}
		m, cmd := d.m.Update(msg)
		d.m = m
		queue = append(queue, d.run(cmd))
	}
}

// cmdTimeout bounds how long run waits for one command. Every command the
// panes return loads git data and finishes; a command still running after
// this long is a hang, not a timer to skip.
const cmdTimeout = 30 * time.Second

// run runs cmd and returns its message, or nil if it has none. It waits for
// the command however slow the runner is, so no message is lost; the only
// timers the panes start are spinner ticks, which send drops by type.
func (d *driver) run(cmd tea.Cmd) tea.Msg {
	d.t.Helper()
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(cmdTimeout):
		d.t.Fatalf("command did not finish within %v", cmdTimeout)
		return nil
	}
}

// Every pane of the repository page, and the header and status bar around
// it, shows data from the repository. Whatever a repository carries, none of
// it may reach the terminal as an escape sequence or control character, while
// the text around the sequences stays visible.
func TestEveryPaneSanitisesRepositoryData(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 200, 60)
	rr := newEscapedRepo(t)

	readme := NewReadme(c)
	files := NewFiles(c)
	log := NewLog(c)
	branches := NewRefs(c, git.RefsHeads)
	tags := NewRefs(c, git.RefsTags)
	stash := NewStash(c)
	r := New(c, readme, files, log, branches, tags, stash)
	r.SetSize(200, 60)
	d := &driver{t: t, m: r}

	d.send(RepoMsg(rr))
	d.send(UpdateRefCmd(rr)())

	check := func(name string, want ...string) {
		t.Helper()
		out := d.m.View()
		assertClean(t, out, append([]string{"project-name", "description"}, want...)...)
		if t.Failed() {
			t.Fatalf("%s: view is not clean", name)
		}
	}
	tab := func(p common.TabComponent) {
		t.Helper()
		for i, q := range r.panes {
			if q == p {
				d.send(tabs.SelectTabMsg(i))
				return
			}
		}
		t.Fatalf("no tab %s", p.TabName())
	}

	tab(readme)
	check("readme", "readme-text")

	tab(files)
	check("file list", "name\\x1bP1$qm")
	for _, it := range files.selector.Items() {
		fi := it.(FileItem)
		if strings.HasPrefix(fi.entry.Name(), "name") {
			// ls-tree quotes a name that holds control characters, and the
			// Files pane passes the quoted name back to git, which cannot
			// find it; the name is covered by the list above.
			continue
		}
		d.send(selector.SelectMsg{IdentifiableItem: fi})
		switch fi.entry.Name() {
		case "main.go":
			check("source file", "go-comment")
		case "README.md":
			check("markdown file", "readme-text")
		}
		d.send(tea.KeyPressMsg{Code: 'b', Text: "b"})
		check("blame "+fi.entry.Name(), "commit-subject", "eve-author")
		d.send(tea.KeyPressMsg{Code: 'b', Text: "b"})
		d.send(GoBackMsg{})
	}

	tab(log)
	check("commit list", "commit-subject", "eve-author")
	d.send(selector.SelectMsg{IdentifiableItem: log.selector.SelectedItem()})
	check("commit", "commit-subject", "commit-body", "eve-author", "plain-text", "go-comment")
	d.send(GoBackMsg{})

	tab(branches)
	for i := range branches.selector.Items() {
		branches.selector.Select(i)
		d.send(selector.ActiveMsg{IdentifiableItem: branches.selector.SelectedItem()})
		check("branches", "branch2Jname")
	}

	tab(tags)
	check("tags", "tag-message", "v1", "tag2Jname")

	tab(stash)
	check("stash list", "stash-message")
	d.send(selector.SelectMsg{IdentifiableItem: stash.list.SelectedItem()})
	check("stash patch", "stash-message", "go-comment")
}
