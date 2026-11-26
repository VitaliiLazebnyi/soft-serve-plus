package backend

import (
	"io"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

// A redelivery sends the body of the delivery it repeats byte for byte, in
// the content type it was sent with, for both content types.
func TestRedeliverSendsStoredBytes(t *testing.T) {
	t.Parallel()
	for _, ct := range []webhook.ContentType{webhook.ContentTypeJSON, webhook.ContentTypeForm} {
		t.Run(ct.String(), func(t *testing.T) {
			t.Parallel()
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
			if err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: url, ContentType: ct, Secret: "secret", Events: []webhook.Event{webhook.EventRepositoryVisibilityChange}, Active: true}); err != nil {
				t.Fatal(err)
			}
			hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(hooks); got != 1 {
				t.Fatalf("len(hooks) = %v, want %v", got, 1)
			}
			id := hooks[0].ID

			if err := e.be.SetPrivate(e.ctx, "repo", true); err != nil {
				t.Fatal(err)
			}
			e.be.WaitForDeliveries(e.ctx)
			ds, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(ds); got != 1 {
				t.Fatalf("len(ds) = %v, want %v", got, 1)
			}
			stored, err := e.be.WebhookDelivery(e.ctx, e.repo, id, ds[0].ID)
			if err != nil {
				t.Fatal(err)
			}

			if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, ds[0].ID); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			defer mu.Unlock()
			if got := len(reqs); got != 2 {
				t.Fatalf("len(reqs) = %v, want %v", got, 2)
			}
			if got := reqs[0].body; got != stored.RequestBody {
				t.Fatalf("reqs[0].body = %v, want %v", got, stored.RequestBody)
			}
			if got, want := reqs[1], reqs[0]; !reflect.DeepEqual(got, want) {
				t.Fatalf("reqs[1] = %v, want %v", got, want)
			}
			if got, want := reqs[1].contentType, ct.String(); got != want {
				t.Fatalf("reqs[1].contentType = %v, want %v", got, want)
			}
		})
	}
}
