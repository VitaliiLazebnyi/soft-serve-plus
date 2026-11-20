package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// newMemServer starts an httptest server for h on testNet and closes it when
// the test ends. Its URL is an http URL on 127.0.0.1, which a webhook may
// use once webhook.allowed_hosts covers it.
func newMemServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	// NewUnstartedServer has already opened a loopback listener; it is
	// replaced before anything can connect to it.
	_ = srv.Listener.Close()
	srv.Listener = testNet.Listen()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// senderEvent is what a test receiver keeps of a webhook request.
type senderEvent struct {
	event    string
	id       int64
	username string
}

// TestEventSenderIsCurrentAccountPerCommand is TestEventSenderIsCurrentAccount
// for the other commands that send an event: the session's account is
// renamed after the session resolved it, and each event names the account
// as it is now.
func TestEventSenderIsCurrentAccountPerCommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		event webhook.Event
	}{
		{"branch delete", []string{"branch", "delete", "hooks", "feature"}, webhook.EventBranchTagDelete},
		{"tag delete", []string{"tag", "delete", "hooks", "v1"}, webhook.EventBranchTagDelete},
		{"branch default", []string{"branch", "default", "hooks", "feature"}, webhook.EventRepository},
		{"repo delete", []string{"delete", "hooks"}, webhook.EventRepository},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			ctx, be := newAuthTestContextWith(t, func(cfg *config.Config) {
				cfg.Webhook.AllowedHosts = []string{"127.0.0.0/8"}
			})
			ownerCtx := whRepo(t, ctx, be) // the session resolved "owner"
			r, err := be.Repository(ctx, "hooks")
			is.NoErr(err)
			gitRefs(t, filepath.Join(config.FromContext(ctx).DataPath, "repos", "hooks.git"))

			got := make(chan senderEvent, 8)
			srv := newMemServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, _ := io.ReadAll(req.Body)
				var payload struct {
					Sender struct {
						ID       int64  `json:"id"`
						Username string `json:"username"`
					} `json:"sender"`
				}
				_ = json.Unmarshal(body, &payload)
				got <- senderEvent{req.Header.Get("X-SoftServe-Event"), payload.Sender.ID, payload.Sender.Username}
				w.WriteHeader(http.StatusOK)
			}))
			is.NoErr(be.CreateWebhook(ownerCtx, r, srv.URL+"/hook", webhook.ContentTypeJSON, "",
				[]webhook.Event{webhook.EventBranchTagDelete, webhook.EventRepository}, true))

			is.NoErr(be.SetUsername(ctx, "owner", "renamed"))
			is.NoErr(runRepo(t, ownerCtx, tc.args...))

			select {
			case ev := <-got:
				is.Equal(ev.event, tc.event.String())
				is.Equal(ev.id, proto.UserFromContext(ownerCtx).ID())
				if ev.username != "renamed" {
					t.Errorf("%s event: sender %q, want %q", tc.name, ev.username, "renamed")
				}
			case <-time.After(time.Minute):
				t.Fatalf("%s: no event delivered", tc.name)
			}
			be.WaitForDeliveries(context.Background())
		})
	}
}

// gitRefs gives the bare repository dir the branches main, its HEAD, and
// feature, and the tag v1, all on one empty commit.
func gitRefs(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	commit := run("commit-tree", run("mktree"), "-m", "refs")
	run("update-ref", "refs/heads/main", commit)
	run("update-ref", "refs/heads/feature", commit)
	run("update-ref", "refs/tags/v1", commit)
	run("symbolic-ref", "HEAD", "refs/heads/main")
}
