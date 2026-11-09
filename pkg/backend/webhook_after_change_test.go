package backend

import (
	"context"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// cancelAfterCommitStore cancels the caller's context on the first read made
// after one of the tracked changes, which is after its transaction has
// committed. It stands for a client that disconnects right after the change.
type cancelAfterCommitStore struct {
	store.Store

	mu     sync.Mutex
	armed  bool
	cancel context.CancelFunc
}

func (s *cancelAfterCommitStore) arm(err error) error {
	if err == nil {
		s.mu.Lock()
		s.armed = true
		s.mu.Unlock()
	}
	return err
}

func (s *cancelAfterCommitStore) fire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed && s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

func (s *cancelAfterCommitStore) SetRepoNameByName(ctx context.Context, h db.Handler, name, newName string) error {
	return s.arm(s.Store.SetRepoNameByName(ctx, h, name, newName))
}

func (s *cancelAfterCommitStore) SetRepoIsPrivateByName(ctx context.Context, h db.Handler, name string, private bool) error {
	return s.arm(s.Store.SetRepoIsPrivateByName(ctx, h, name, private))
}

func (s *cancelAfterCommitStore) AddCollabByUsernameAndRepo(ctx context.Context, h db.Handler, username, repo string, level access.AccessLevel) error {
	return s.arm(s.Store.AddCollabByUsernameAndRepo(ctx, h, username, repo, level))
}

func (s *cancelAfterCommitStore) RemoveCollabByUsernameAndRepo(ctx context.Context, h db.Handler, username, repo string) error {
	return s.arm(s.Store.RemoveCollabByUsernameAndRepo(ctx, h, username, repo))
}

func (s *cancelAfterCommitStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	s.fire()
	return s.Store.GetRepoByName(ctx, h, name)
}

func (s *cancelAfterCommitStore) GetUserByID(ctx context.Context, h db.Handler, id int64) (models.User, error) {
	s.fire()
	return s.Store.GetUserByID(ctx, h, id)
}

func (s *cancelAfterCommitStore) GetWebhooksByRepoIDWhereEvent(ctx context.Context, h db.Handler, repoID int64, events []int) ([]models.Webhook, error) {
	s.fire()
	return s.Store.GetWebhooksByRepoIDWhereEvent(ctx, h, repoID, events)
}

// A change's webhook event is sent even when the caller's context ends right
// after the change commits, such as an SSH client that disconnects as soon
// as the command has run.
func TestWebhookEventSentWhenContextEndsAfterCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		event  webhook.Event
		setup  func(t *testing.T, e whEnv)
		change func(ctx context.Context, e whEnv) error
	}{
		{
			name:  "rename",
			event: webhook.EventRepository,
			change: func(ctx context.Context, e whEnv) error {
				return e.be.RenameRepository(ctx, "repo", "renamed")
			},
		},
		{
			name:  "visibility",
			event: webhook.EventRepositoryVisibilityChange,
			change: func(ctx context.Context, e whEnv) error {
				return e.be.SetPrivate(ctx, "repo", true)
			},
		},
		{
			name:  "collaborator added",
			event: webhook.EventCollaborator,
			setup: func(t *testing.T, e whEnv) {
				t.Helper()
				_, err := e.be.CreateUser(e.ctx, "alice", proto.UserOptions{})
				is.New(t).NoErr(err)
			},
			change: func(ctx context.Context, e whEnv) error {
				return e.be.AddCollaborator(ctx, "repo", "alice", access.ReadOnlyAccess)
			},
		},
		{
			name:  "collaborator removed",
			event: webhook.EventCollaborator,
			setup: func(t *testing.T, e whEnv) {
				t.Helper()
				is := is.New(t)
				_, err := e.be.CreateUser(e.ctx, "alice", proto.UserOptions{})
				is.NoErr(err)
				is.NoErr(e.be.AddCollaborator(e.ctx, "repo", "alice", access.ReadOnlyAccess))
			},
			change: func(ctx context.Context, e whEnv) error {
				return e.be.RemoveCollaborator(ctx, "repo", "alice")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			e := newWebhookEnv(t)
			if tc.setup != nil {
				tc.setup(t, e)
			}
			id := e.seedHook(t, tc.event)

			ctx, cancel := context.WithCancel(e.ctx)
			defer cancel()
			st := &cancelAfterCommitStore{Store: e.be.store, cancel: cancel}
			e.be.store = st
			ctx = store.WithContext(ctx, st)

			is.NoErr(tc.change(ctx, e))
			is.True(ctx.Err() != nil) // the context ended after the commit
			e.be.WaitForDeliveries(e.ctx)

			is.Equal(e.deliveredEvents(t, id), []webhook.Event{tc.event})
		})
	}
}
