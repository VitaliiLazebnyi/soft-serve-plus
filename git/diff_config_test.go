package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aymanbagabas/git-module"
)

// TestDiffIgnoresOutputConfig checks that config which changes git's diff
// output does not reach the diff parser, whether it comes from the system
// config or from the repository's own config.
func TestDiffIgnoresOutputConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the external diff driver is a shell script")
	}
	ctx := context.Background()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	ext := filepath.Join(dir, "ext-diff.sh")
	if err := os.WriteFile(ext, []byte("#!/bin/sh\necho GARBAGE\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A system config that GIT_CONFIG_NOSYSTEM must keep out.
	// GIT_CONFIG_SYSTEM points git at it for this test only. diff.noprefix
	// drops the a/ and b/ prefixes the parser needs (it panics without
	// them), and no command-line flag we pass undoes it, so only
	// GIT_CONFIG_NOSYSTEM keeps the system subtest passing. Forced color is
	// there too, although --no-color also covers it.
	sysConfig := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(sysConfig, []byte("[diff]\n\tnoprefix = true\n[color]\n\tui = always\n\tdiff = always\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", sysConfig)

	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repoPath
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repoPath, "f.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := []string{"-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "-a", "-m"}
	run("init", "-q")
	write("one\n")
	run("add", ".")
	run(append(commit, "first")...)
	write("two\n")
	run(append(commit, "second")...)
	write("three\n")
	run("-c", "user.email=test@example.com", "-c", "user.name=Test", "stash", "-q")

	repo, err := Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}

	check := func(t *testing.T, d *Diff, err error, wantAdd string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Files) != 1 {
			t.Fatalf("got %d files, want 1; patch:\n%s", len(d.Files), d.Patch())
		}
		if name := d.Files[0].Name; name != "f.txt" {
			t.Errorf("file name = %q, want f.txt", name)
		}
		var added []string
		for _, s := range d.Files[0].Sections {
			for _, l := range s.Lines {
				if l.Type == git.DiffLineAdd {
					added = append(added, l.Content)
				}
			}
		}
		if len(added) != 1 || added[0] != "+"+wantAdd {
			t.Errorf("added lines = %q, want [%q]", added, "+"+wantAdd)
		}
	}

	checkAll := func(t *testing.T) {
		for _, tc := range []struct{ rev, want string }{
			{"HEAD", "two"},   // git diff against the parent
			{"HEAD~1", "one"}, // git show for the root commit
		} {
			t.Run(tc.rev, func(t *testing.T) {
				c, err := repo.CatFileCommit(tc.rev)
				if err != nil {
					t.Fatal(err)
				}
				d, err := repo.Diff(c)
				check(t, d, err, tc.want)
			})
		}
		t.Run("stash", func(t *testing.T) {
			d, err := repo.StashDiff(0)
			check(t, d, err, "three")
		})
	}

	// diff.noprefix and color from the system config only.
	t.Run("system", checkAll)
	// An external diff driver and forced color from the repository's config,
	// which is always read and so is covered by the command-line flags.
	run("config", "diff.external", ext)
	run("config", "color.ui", "always")
	t.Run("repository", checkAll)
}
