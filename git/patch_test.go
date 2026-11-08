package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit runs git in dir with a fixed identity and no line ending conversion.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	args = append([]string{
		"-c", "user.email=test@example.com",
		"-c", "user.name=Test",
		"-c", "core.autocrlf=false",
	}, args...)
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setPatchLimits lowers the diff limits for one test.
func setPatchLimits(t *testing.T, files, lines, chars int) {
	t.Helper()
	oldFiles, oldLines, oldChars := DiffMaxFiles, DiffMaxFileLines, DiffMaxLineChars
	DiffMaxFiles, DiffMaxFileLines, DiffMaxLineChars = files, lines, chars
	t.Cleanup(func() {
		DiffMaxFiles, DiffMaxFileLines, DiffMaxLineChars = oldFiles, oldLines, oldChars
	})
}

// setupPatchRepo creates a repository with a root commit and a second commit
// whose diff goes over small limits: several files, a file with two hunks, a
// long line and files without a trailing newline.
func setupPatchRepo(t *testing.T) (repo *Repository, dir, root, head string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init")
	writeFile(t, filepath.Join(dir, "a.txt"), "one\ntwo")
	writeFile(t, filepath.Join(dir, "b.txt"), "keep\n")
	var many strings.Builder
	for i := range 50 {
		fmt.Fprintf(&many, "line %d\n", i)
	}
	writeFile(t, filepath.Join(dir, "c.txt"), many.String())
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "root")
	root = strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	// Changes at both ends of c.txt give it two hunks.
	changed := strings.Replace(many.String(), "line 0\n", "first\n", 1)
	changed = strings.Replace(changed, "line 49\n", "last\n", 1)
	writeFile(t, filepath.Join(dir, "a.txt"), "one\nTWO")
	writeFile(t, filepath.Join(dir, "b.txt"), "keep\nno newline")
	writeFile(t, filepath.Join(dir, "c.txt"), changed)
	writeFile(t, filepath.Join(dir, "d.txt"), strings.Repeat("x", 200)+"\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "change")
	head = strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	repo, err := Open(dir)
	if err != nil {
		t.Fatalf("failed to open repository: %v", err)
	}
	return repo, dir, root, head
}

// TestRepositoryPatchApplies checks that the patch is complete and applies
// to the parent commit to give the commit's tree, even when the diff is
// larger than the display limits.
func TestRepositoryPatchApplies(t *testing.T) {
	setPatchLimits(t, 1, 2, 100)
	repo, dir, root, head := setupPatchRepo(t)

	commit, err := repo.CommitByRevision(head)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := repo.Patch(commit)
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if got := strings.Count(patch, `\ No newline at end of file`); got != 3 {
		t.Errorf("patch has %d no-newline markers, want 3:\n%s", got, patch)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		if !strings.Contains(patch, "diff --git a/"+name+" b/"+name) {
			t.Errorf("patch is missing %s:\n%s", name, patch)
		}
	}

	patchFile := filepath.Join(t.TempDir(), "change.patch")
	writeFile(t, patchFile, patch)
	runGit(t, dir, "checkout", "-q", root)
	runGit(t, dir, "apply", "--index", patchFile)
	if diff := runGit(t, dir, "diff", "--cached", head); diff != "" {
		t.Errorf("applied patch differs from %s:\n%s", head, diff)
	}
}

// TestRepositoryPatchRootCommit checks the patch of a commit without parents.
func TestRepositoryPatchRootCommit(t *testing.T) {
	repo, _, root, _ := setupPatchRepo(t)

	commit, err := repo.CommitByRevision(root)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := repo.Patch(commit)
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if strings.Contains(patch, root) {
		t.Errorf("patch starts with the commit id:\n%s", patch)
	}
	for _, want := range []string{
		"diff --git a/a.txt b/a.txt\nnew file mode 100644\n",
		"--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1,2 @@\n+one\n+two\n\\ No newline at end of file\n",
		"diff --git a/b.txt b/b.txt\nnew file mode 100644\n",
		"diff --git a/c.txt b/c.txt\nnew file mode 100644\n",
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch does not contain %q:\n%s", want, patch)
		}
	}
}

// TestDiffPatchMarksTruncation checks that the display patch says when the
// diff was cut short by the limits.
func TestDiffPatchMarksTruncation(t *testing.T) {
	tests := []struct {
		name                string
		files, lines, chars int
		want                []string
		wantTruncatedEnding bool
	}{
		{
			name: "complete", files: 100, lines: 100, chars: 1000,
		},
		{
			name: "too many files", files: 2, lines: 100, chars: 1000,
			wantTruncatedEnding: true,
		},
		{
			name: "too many lines", files: 100, lines: 3, chars: 1000,
			want:                []string{"[diff of c.txt truncated]\n"},
			wantTruncatedEnding: true,
		},
		{
			name: "line too long", files: 100, lines: 100, chars: 100,
			want:                []string{"[diff of d.txt truncated]\n"},
			wantTruncatedEnding: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setPatchLimits(t, tt.files, tt.lines, tt.chars)
			repo, _, _, head := setupPatchRepo(t)
			commit, err := repo.CommitByRevision(head)
			if err != nil {
				t.Fatal(err)
			}
			diff, err := repo.Diff(commit)
			if err != nil {
				t.Fatal(err)
			}
			patch := diff.Patch()
			for _, want := range tt.want {
				if !strings.Contains(patch, want) {
					t.Errorf("patch does not contain %q:\n%s", want, patch)
				}
			}
			if got := strings.HasSuffix(patch, "[diff truncated]\n"); got != tt.wantTruncatedEnding {
				t.Errorf("patch ends with truncation marker = %v, want %v:\n%s", got, tt.wantTruncatedEnding, patch)
			}
			if !tt.wantTruncatedEnding && strings.Contains(patch, "truncated]") {
				t.Errorf("complete patch has a truncation marker:\n%s", patch)
			}
		})
	}
}
