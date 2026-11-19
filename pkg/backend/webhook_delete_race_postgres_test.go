package backend

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// raceDeleteWebhookStore runs race once, inside the repository delete's
// transaction: right after the delete lists the repository's webhooks, or,
// where the delete lists none, just before it deletes the row.
type raceDeleteWebhookStore struct {
	store.Store
	once sync.Once
	race func()
}

func (s *raceDeleteWebhookStore) GetWebhooksByRepoID(ctx context.Context, h db.Handler, repoID int64) ([]models.Webhook, error) {
	whs, err := s.Store.GetWebhooksByRepoID(ctx, h, repoID)
	if _, inTx := h.(*db.Tx); inTx {
		s.once.Do(s.race)
	}
	return whs, err
}

func (s *raceDeleteWebhookStore) DeleteRepoByName(ctx context.Context, h db.Handler, name string) error {
	s.once.Do(s.race)
	return s.Store.DeleteRepoByName(ctx, h, name)
}

// On Postgres, deleting a webhook while its repository is being deleted
// must not make the repository's delete fail. The webhook delete used to
// take no lock on the repository: it removed a webhook the repository's
// delete had already listed, which then found nothing to delete and
// reported the repository not found, rolling everything back. The two now
// take turns on the repository's row.
func TestDeleteWebhookRacingRepositoryDeletePostgres(t *testing.T) {
	is := is.New(t)
	e := newPostgresWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	hookDeleted := make(chan error, 1)
	rs := &raceDeleteWebhookStore{Store: e.be.store}
	rs.race = func() {
		go func() { hookDeleted <- e.be.DeleteWebhook(e.ctx, e.repo, id) }()
		// Go on once the webhook delete has finished or waits for a lock.
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			select {
			case err := <-hookDeleted:
				hookDeleted <- err
				return
			default:
			}
			var waiting bool
			if err := e.be.db.GetContext(e.ctx, &waiting, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock');`); err != nil {
				t.Error(err)
				return
			}
			if waiting {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("the webhook delete neither finished nor waited for a lock")
	}
	e.be.store = rs

	is.NoErr(e.be.DeleteRepository(e.ctx, "repo"))
	_, err := e.be.Repository(e.ctx, "repo")
	is.True(errors.Is(err, proto.ErrRepoNotFound))
	// The webhook went with its repository; deleting it found nothing.
	is.True(errors.Is(<-hookDeleted, db.ErrRecordNotFound))
}
