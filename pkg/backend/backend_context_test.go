package backend

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
	"github.com/matryer/is"
)

// TestWebhooksBareContext verifies that the webhook methods use the
// backend's own database and store, so a caller whose context carries
// neither still works. db.FromContext returns nil for such a context, so
// reading the database from it panicked.
func TestWebhooksBareContext(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	ctx := context.Background()

	is.NoErr(e.be.CreateWebhook(ctx, e.repo, whURL, webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true))
	hooks, err := e.be.ListWebhooks(ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(hooks), 1)
	id := hooks[0].ID

	h, err := e.be.Webhook(ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(h.URL, whURL)

	is.NoErr(e.be.UpdateWebhook(ctx, e.repo, id, whURLOther, webhook.ContentTypeForm, "", []webhook.Event{webhook.EventPush}, true))
	h, err = e.be.Webhook(ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(h.URL, whURLOther)

	dels, err := e.be.ListWebhookDeliveries(ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(len(dels), 0)
	_, err = e.be.WebhookDelivery(ctx, e.repo, id, uuid.New())
	is.True(err != nil) // no such delivery, but no panic either
	is.True(e.be.RedeliverWebhookDelivery(ctx, e.repo, id, uuid.New()) != nil)

	is.NoErr(e.be.DeleteWebhook(ctx, e.repo, id))
	hooks, err = e.be.ListWebhooks(ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(hooks), 0)

}

// TestStoreRepoMissingLFSObjectsBackendConfig verifies that LFS objects are
// stored under the backend's data path, whatever config the context carries.
func TestStoreRepoMissingLFSObjectsBackendConfig(t *testing.T) {
	is := is.New(t)
	lt := newLFSTest(t, 1)

	lt.ctx = context.Background()
	is.NoErr(lt.store())
	p := lt.pointers[0]
	got, err := os.ReadFile(filepath.Join(lt.be.cfg.DataPath, "lfs", strconv.FormatInt(lt.repo.ID(), 10), "objects", p.RelativePath()))
	is.NoErr(err)
	is.Equal(got, lt.client.objects[p.Oid])

	lt.client.requested = nil
	is.NoErr(lt.be.SyncRepoLFSObjects(lt.ctx, lt.repo, lt.client))
	is.Equal(len(lt.client.requested), 0) // already stored
}
