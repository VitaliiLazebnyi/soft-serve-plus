package cmd

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
)

// cancelAfterRefChangeStore cancels the session context on the first user
// lookup made once changed reports that the command has changed the ref. It
// stands for an SSH client that disconnects right after the ref change, while
// the command builds the webhook event.
type cancelAfterRefChangeStore struct {
	store.Store

	changed func() bool

	mu     sync.Mutex
	cancel context.CancelFunc
}

func (s *cancelAfterRefChangeStore) GetUserByID(ctx context.Context, h db.Handler, id int64) (models.User, error) {
	s.mu.Lock()
	if s.cancel != nil && s.changed() {
		s.cancel()
		s.cancel = nil
	}
	s.mu.Unlock()
	return s.Store.GetUserByID(ctx, h, id)
}

// Branch delete, tag delete and default branch change still send their event
// when the session ends after the ref has changed but before the event is
// built.
func TestBranchAndTagEventsSentWhenSessionEndsAfterRefChange(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		event   string
		changed func(t *testing.T, bare string) bool
	}{
		{
			name:  "branch delete",
			args:  []string{"branch", "delete", "hooks", "feature"},
			event: "branch_tag_delete",
			changed: func(t *testing.T, bare string) bool {
				t.Helper()
				return !gitRefExists(t, bare, "refs/heads/feature")
			},
		},
		{
			name:  "tag delete",
			args:  []string{"tag", "delete", "hooks", "v1"},
			event: "branch_tag_delete",
			changed: func(t *testing.T, bare string) bool {
				t.Helper()
				return !gitRefExists(t, bare, "refs/tags/v1")
			},
		},
		{
			name:  "branch default",
			args:  []string{"branch", "default", "hooks", "other"},
			event: "repository",
			changed: func(t *testing.T, bare string) bool {
				t.Helper()
				out, err := exec.CommandContext(t.Context(), "git", "--git-dir", bare, "symbolic-ref", "HEAD").Output()
				return err == nil && strings.TrimSpace(string(out)) == "refs/heads/other"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			whPushRefs(t, ownerCtx)
			id := whCreate(t, ownerCtx, be, "-e", "branch_tag_delete", "-e", "repository")
			whPointAtLoopback(t, ownerCtx, "hooks", id)
			bare := filepath.Join(config.FromContext(ownerCtx).DataPath, "repos", "hooks.git")

			sess, cancel := context.WithCancel(ownerCtx)
			defer cancel()
			st := &cancelAfterRefChangeStore{
				Store:   store.FromContext(ownerCtx),
				changed: func() bool { return tc.changed(t, bare) },
				cancel:  cancel,
			}
			sess = store.WithContext(sess, st)

			is.NoErr(runRepo(t, sess, tc.args...))
			is.True(sess.Err() != nil) // the session ended after the ref change

			be.WaitForDeliveries(context.Background())
			r, err := be.Repository(ownerCtx, "hooks")
			is.NoErr(err)
			ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
			is.NoErr(err)
			events := make([]string, 0, len(ds))
			for _, d := range ds {
				events = append(events, d.Event.String())
			}
			is.Equal(events, []string{tc.event})
		})
	}
}

// gitRefExists reports whether ref exists in the bare repository.
func gitRefExists(t *testing.T, bare, ref string) bool {
	t.Helper()
	return exec.CommandContext(t.Context(), "git", "--git-dir", bare, "rev-parse", "--verify", "-q", ref).Run() == nil
}
