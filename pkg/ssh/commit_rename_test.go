package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// R1: `ssh host repo commit REPO REV` on a commit that renames a file to a
// name git quotes ended the whole server from the diff parser's goroutine.
// Over a real SSH connection the command must print the rename, and the
// server must still answer afterwards.
func TestSSHRepoCommitRenameQuotedName(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, ctx := startTestServer(t, cfg)
	if _, err := backend.FromContext(ctx).CreateRepository(ctx, "renames", fixtureAdmin(t, backend.FromContext(ctx)), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(cfg.DataPath, "repos", "renames.git")
	work := t.TempDir()
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = work
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "init", "-q", "-b", "main")
	body := strings.Repeat("same line\n", 20)
	put := func(name, content string) {
		blob := git(content, "hash-object", "-w", "--stdin")
		git("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	}
	put("plain.txt", body)
	git("", "commit", "-q", "-m", "add")
	git("", "update-index", "--force-remove", "--", "plain.txt")
	put("café.txt", body+"changed\n")
	git("", "commit", "-q", "-m", "to café")
	to := git("", "rev-parse", "HEAD")
	git("", "update-index", "--force-remove", "--", "café.txt")
	put("plain.txt", body+"changed\nagain\n")
	git("", "commit", "-q", "-m", "from café")
	from := git("", "rev-parse", "HEAD")
	git("", "push", "-q", bare, "main")

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	for _, rev := range []string{to, from, to} {
		s, err := c.NewSession()
		if err != nil {
			t.Fatalf("new session (server gone?): %v", err)
		}
		out, err := s.Output("repo commit renames " + rev)
		_ = s.Close()
		if err != nil {
			t.Fatalf("repo commit %s: %v\n%s", rev, err, out)
		}
		for _, want := range []string{"rename from ", "rename to ", "café.txt"} {
			if !strings.Contains(string(out), want) {
				t.Errorf("repo commit %s: output lacks %q:\n%s", rev, want, out)
			}
		}
	}
}
