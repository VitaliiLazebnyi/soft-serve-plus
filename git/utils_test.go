package git

import (
	"context"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun runs git in dir and returns its trimmed output.
func gitRun(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s failed: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// setupTreeRepo commits the given entries, each a path mapped to its git
// mode and content, straight into the index, so symlinks and submodules can
// be made on any OS. A submodule's content is the commit it points to.
func setupTreeRepo(t *testing.T, entries map[string][2]string) (*Repository, *Reference) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, "", "", "init", dir)
	for p, e := range entries {
		mode, content := e[0], e[1]
		sha := content
		if mode != "160000" {
			sha = gitRun(t, dir, content, "hash-object", "-w", "--stdin")
		}
		gitRun(t, dir, "", "update-index", "--add", "--cacheinfo", mode+","+sha+","+p)
	}
	gitRun(t, dir, "", "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-m", "init")

	repo, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ref, err := repo.HEAD()
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	return repo, ref
}

const someCommit = "1111111111111111111111111111111111111111"

func TestLatestFile(t *testing.T) {
	repo, ref := setupTreeRepo(t, map[string][2]string{
		"docs/real.md": {"100644", "real"},
		// A symlink out of its own directory.
		".github/README": {"120000", "../docs/real.md"},
		// A chain of symlinks.
		"chain/README": {"120000", "next"},
		"chain/next":   {"120000", "../docs/real.md"},
		// A matching submodule comes before the file.
		"mods/README":    {"160000", someCommit},
		"mods/readme.md": {"100644", "mods"},
		// Symlinks that lead nowhere, out of the repository, to an
		// absolute path, to a directory, or in a loop are skipped.
		"broken/README":  {"120000", "missing"},
		"broken/Readme":  {"120000", "../../outside"},
		"broken/rEadme":  {"120000", "/etc/passwd"},
		"broken/reAdme":  {"120000", "../docs"},
		"broken/reaDme":  {"120000", "reaDme"},
		"broken/readme9": {"100644", "broken"},
	})

	for _, tc := range []struct {
		dir, content, path string
	}{
		{".github", "real", "docs/real.md"},
		{"chain", "real", "docs/real.md"},
		{"mods", "mods", "mods/readme.md"},
		{"broken", "broken", "broken/readme9"},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			content, path, err := LatestFile(repo, ref, tc.dir+"/[rR][eE][aA][dD][mM][eE]*")
			if err != nil {
				t.Fatalf("LatestFile: %v", err)
			}
			if strings.TrimSpace(content) != tc.content || path != tc.path {
				t.Errorf("LatestFile = %q, %q; want %q, %q", content, path, tc.content, tc.path)
			}
		})
	}
}

func TestTreeEntryModeAndPath(t *testing.T) {
	repo, ref := setupTreeRepo(t, map[string][2]string{
		"dir/file": {"100644", "file"},
		"exec":     {"100755", "exec"},
		"link":     {"120000", "dir/file"},
		"sub":      {"160000", someCommit},
	})
	tree, err := repo.Tree(ref)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}

	for _, tc := range []struct {
		path    string
		mode    fs.FileMode
		regular bool
	}{
		{"dir", fs.ModeDir | fs.ModePerm, false},
		{"dir/file", 0o644, true},
		{"exec", 0o755, true},
		{"link", fs.ModeSymlink | fs.ModePerm, false},
		{"sub", fs.ModeIrregular, false},
	} {
		te, err := tree.TreeEntry(tc.path)
		if err != nil {
			t.Fatalf("TreeEntry(%q): %v", tc.path, err)
		}
		if got := te.Mode(); got != tc.mode || got.IsRegular() != tc.regular {
			t.Errorf("%s: Mode() = %v, want %v", tc.path, got, tc.mode)
		}
		if got := te.File().Path(); got != tc.path {
			t.Errorf("%s: path = %q", tc.path, got)
		}
	}
}
