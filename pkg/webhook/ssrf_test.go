package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

// TestSSRFProtection is an integration test verifying the webhook send path
// blocks private IPs end-to-end (models.Webhook -> secureHTTPClient -> ssrf).
// Each row must fail with ssrf.ErrPrivateIP, which only the SSRF check
// produces: a dial to these addresses fails anyway on most machines, so an
// error alone would pass without the protection. The loopback row targets a
// listener that must not be reached. Public destinations are covered in
// pkg/ssrf, where the connection attempt can be stubbed; nothing here leaves
// the machine.
func TestSSRFProtection(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tests := []struct {
		name       string
		webhookURL string
	}{
		{"block loopback", srv.URL + "/webhook"},
		{"block metadata", "http://169.254.169.254/latest/meta-data/"},
		{"block private", "http://10.0.0.1:8080/webhook"},
		{"block IPv4-mapped loopback", "http://[::ffff:127.0.0.1]:8080/webhook"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := models.Webhook{
				URL:         tt.webhookURL,
				ContentType: int(ContentTypeJSON),
			}

			req, err := http.NewRequestWithContext(context.Background(), "POST", w.URL, nil)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}

			resp, err := NewSender(nil, nil, nil, nil).client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, ssrf.ErrPrivateIP) {
				t.Errorf("%s: err = %v, want ssrf.ErrPrivateIP", tt.name, err)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("loopback listener reached %d times", n)
	}
}
