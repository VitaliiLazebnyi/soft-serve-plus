package backend

import (
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// A redelivery sends the body of the delivery it repeats byte for byte, in
// the content type it was sent with, for both content types.
func TestRedeliverSendsStoredBytes(t *testing.T) {
	t.Parallel()
	for _, ct := range []webhook.ContentType{webhook.ContentTypeJSON, webhook.ContentTypeForm} {
		t.Run(ct.String(), func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			e := newWebhookEnv(t)
			type request struct{ body, contentType string }
			var (
				mu   sync.Mutex
				reqs []request
			)
			srv := newMemServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				mu.Lock()
				reqs = append(reqs, request{string(b), r.Header.Get("Content-Type")})
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			e.setWebhookAllowedHosts(t, "127.0.0.0/8")
			url := srv.URL + "/hook"
			is.NoErr(e.be.CreateWebhook(e.ctx, e.repo, url, ct, "secret", []webhook.Event{webhook.EventRepositoryVisibilityChange}, true))
			hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
			is.NoErr(err)
			is.Equal(len(hooks), 1)
			id := hooks[0].ID

			is.NoErr(e.be.SetPrivate(e.ctx, "repo", true))
			e.be.WaitForDeliveries(e.ctx)
			ds, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
			is.NoErr(err)
			is.Equal(len(ds), 1)
			stored, err := e.be.WebhookDelivery(e.ctx, e.repo, id, ds[0].ID)
			is.NoErr(err)

			is.NoErr(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, ds[0].ID))

			mu.Lock()
			defer mu.Unlock()
			is.Equal(len(reqs), 2)
			is.Equal(reqs[0].body, stored.RequestBody)
			is.Equal(reqs[1], reqs[0])
			is.Equal(reqs[1].contentType, ct.String())
		})
	}
}
