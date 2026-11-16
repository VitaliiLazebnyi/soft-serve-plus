package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/google/uuid"
	"github.com/matryer/is"
)

// setAllowedHosts sets webhook.allowed_hosts on the test config and parses it
// the way the server does.
func (e *testEnv) setAllowedHosts(t *testing.T, entries ...string) {
	t.Helper()
	e.cfg.Webhook.AllowedHosts = entries
	if err := e.cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestSendWebhookAllowedHosts sends to a loopback receiver through the real
// SSRF-protected client: it is reached only while webhook.allowed_hosts
// covers it, and the list is read from the config at send time.
func TestSendWebhookAllowedHosts(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	// The SSRF checks run on the socket, so this receiver is on loopback
	// TCP, not the in-memory network newReceiver uses.
	rec := &recorder{status: http.StatusOK}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	w := e.newWebhook(t, srv.URL+"/hook", "", ContentTypeJSON, EventBranchTagCreate)

	// Not listed: refused at send time, recorded with the reason.
	is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))
	is.Equal(rec.calls, 0)
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(ds[0].RequestError.Valid)
	is.True(strings.Contains(ds[0].RequestError.String, ssrf.ErrPrivateIP.Error()))

	// Listed: delivered. A loopback connect can fail outright while the
	// machine is busy with other loopback connections (see memnet_test.go),
	// so a send that fails for a reason other than the allowlist is tried
	// again, for up to a minute.
	e.setAllowedHosts(t, "127.0.0.0/8")
	calls := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return rec.calls
	}
	deadline := time.Now().Add(time.Minute)
	for {
		is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))
		ds = e.deliveries(t, w.ID)
		last := ds[len(ds)-1]
		if calls() > 0 {
			is.Equal(last.ResponseStatus, http.StatusOK)
			is.True(!last.RequestError.Valid)
			break
		}
		if strings.Contains(last.RequestError.String, ssrf.ErrPrivateIP.Error()) || time.Now().After(deadline) {
			t.Fatalf("listed send not delivered: status %d, error %q", last.ResponseStatus, last.RequestError.String)
		}
		// A failure that comes back at once would otherwise spin and
		// record a delivery row per try.
		time.Sleep(100 * time.Millisecond)
	}
	is.Equal(calls(), 1)

	// Removed from the list again: the stored webhook is refused, since
	// the current config applies to every delivery.
	e.setAllowedHosts(t, "10.0.0.0/8")
	err := sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload(), uuid.Nil, false)
	is.True(errors.Is(err, ssrf.ErrPrivateIP))
	is.Equal(calls(), 1)
}

// TestSendWebhookAllowedHostsKeepsMetadataBlocked checks that a broad entry
// does not open the cloud metadata address.
func TestSendWebhookAllowedHostsKeepsMetadataBlocked(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	e.setAllowedHosts(t, "0.0.0.0/0")
	w := e.newWebhook(t, "http://169.254.169.254/latest/meta-data/", "", ContentTypeJSON, EventBranchTagCreate)
	err := sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload(), uuid.Nil, false)
	is.True(errors.Is(err, ssrf.ErrPrivateIP))
}

func TestValidateWebhookURLAllowing(t *testing.T) {
	is := is.New(t)
	allowed, err := ssrf.ParseAllowedHosts([]string{"127.0.0.1"})
	is.NoErr(err)
	is.NoErr(ValidateWebhookURLAllowing(context.Background(), "http://127.0.0.1:8080/hook", allowed))
	is.True(errors.Is(ValidateWebhookURLAllowing(context.Background(), "http://127.0.0.1:8080/hook", nil), ErrPrivateIP))
	is.True(errors.Is(ValidateWebhookURLAllowing(context.Background(), "http://169.254.169.254/", allowed), ErrPrivateIP))
}
