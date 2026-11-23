package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// commitInRepo writes a commit into the bare repository at dir, without a
// ref, and returns its ID.
func commitInRepo(t *testing.T, dir string) string {
	t.Helper()
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"--git-dir", dir}, args...)...) //nolint:gosec
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	blob := run("hello\n", "hash-object", "-w", "--stdin")
	tree := run("100644 blob "+blob+"\tREADME.md\n", "mktree")
	return run("", "commit-tree", tree, "-m", "first")
}

// A push whose client goes away after receive-pack updated its refs still
// gets the work done after the push: git ran post-receive, which wrote the
// refs file, and the service then failed with the cancellation. The refs
// are read, the backend writes info/last-modified, and the file is removed.
// Here a stand-in for git updates the ref, writes the refs file as the
// post-receive hook does, and returns context.Canceled.
func TestPushAfterClientWentAwayStillRunsAfterPush(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	f := newOracleFixture(t)
	// The fixture's token is the stranger's, so the stranger owns it.
	stranger, err := f.be.User(f.ctx, "stranger")
	is.NoErr(err)
	_, err = f.be.CreateRepository(proto.WithUserContext(f.ctx, stranger), "public", stranger, proto.RepositoryOptions{})
	is.NoErr(err)
	cfg := f.env.cfg
	dir := hooks.RepoPath(cfg, "public")
	commit := commitInRepo(t, dir)
	lastModified := filepath.Join(dir, "info", "last-modified")
	_, err = os.Stat(lastModified)
	is.True(os.IsNotExist(err))

	var refsFile string
	orig := f.server.runService
	f.server.runService = func(ctx context.Context, service git.Service, cmd git.ServiceCommand) error {
		if service != git.ReceivePackService {
			return orig(ctx, service, cmd)
		}
		for _, kv := range cmd.Env {
			if v, ok := strings.CutPrefix(kv, hooks.PushRefsEnv+"="); ok {
				refsFile = v
			}
		}
		if refsFile == "" {
			t.Error("receive-pack got no refs file")
			return context.Canceled
		}
		ref := exec.CommandContext(t.Context(), "git", "--git-dir", dir, "update-ref", "refs/heads/main", commit) //nolint:gosec
		ref.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if err := ref.Run(); err != nil {
			t.Errorf("update-ref: %v", err)
		}
		line := strings.Repeat("0", 40) + " " + commit + " refs/heads/main\n"
		if err := os.WriteFile(refsFile, []byte(line), 0o600); err != nil {
			t.Error(err)
		}
		return context.Canceled
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/public.git/git-receive-pack", bytes.NewReader([]byte("0000")))
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Authorization", "Token "+f.token)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)

	is.True(refsFile != "")
	// AfterPush ran: it wrote last-modified before the handler returned.
	_, err = os.Stat(lastModified)
	is.NoErr(err)
	// And the refs file is gone.
	_, err = os.Stat(refsFile)
	is.True(os.IsNotExist(err))
}
