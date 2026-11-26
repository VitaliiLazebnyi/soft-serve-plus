package testscript

import (
	"context"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/testnet"
)

// post sends one webhook request with event to the receiver over network.
func post(t *testing.T, rcv *webhookReceiver, network, event, path string) {
	t.Helper()
	addr := rcv.unix.Addr().String()
	if network == "tcp" {
		addr = rcv.tcp.Addr().String()
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return testnet.Dial(ctx, network, addr)
		},
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://receiver"+path, strings.NewReader("{}"))
	if err != nil {
		t.Error(err)
		return
	}
	req.Header.Set("X-SoftServe-Event", event)
	res, err := client.Do(req)
	if err != nil {
		t.Error(err)
		return
	}
	_ = res.Body.Close()
}

func newTestReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	rcv, err := newWebhookReceiver(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rcv.close)
	return rcv
}

// waitFor returns once the requests it was asked for have arrived, over
// either listener, including one sent only after it started to wait.
func TestWebhookReceiverWaitFor(t *testing.T) {
	t.Parallel()
	rcv := newTestReceiver(t)
	post(t, rcv, "unix", "push", "/hooks/A")

	// The second request is sent once waitFor has seen one request, too few,
	// and is about to wait: the request it waits for has not been sent yet.
	blocked := make(chan struct{})
	waits := 0
	rcv.blocking = func() {
		waits++
		if waits == 1 {
			close(blocked)
		}
	}
	// stop ends the sender when waitFor returned without waiting, so the
	// failure below is reported instead of the test hanging.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-blocked:
			post(t, rcv, "tcp", "branch_tag_create", "/hooks/B")
		case <-stop:
		}
	}()
	got, ok := rcv.waitFor(2, webhookWait)
	close(stop)
	<-done
	if waits != 1 {
		t.Errorf("waitFor waited %d times, want 1", waits)
	}
	want := []string{"push /hooks/A", "branch_tag_create /hooks/B"}
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("waitFor(2) = %q, %v; want %q, true", got, ok, want)
	}

	// A total already reached returns at once with everything received.
	got, ok = rcv.waitFor(1, webhookWait)
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("waitFor(1) = %q, %v; want %q, true", got, ok, want)
	}
}

// waitFor reports a total that was not reached, with what did arrive. Nothing
// sends a second request, so the short timeout cannot be cut by load.
func TestWebhookReceiverWaitForMissing(t *testing.T) {
	t.Parallel()
	rcv := newTestReceiver(t)
	post(t, rcv, "unix", "push", "/hooks/A")

	got, ok := rcv.waitFor(2, 0)
	if ok || !slices.Equal(got, []string{"push /hooks/A"}) {
		t.Fatalf("waitFor(2) = %q, %v; want [push /hooks/A], false", got, ok)
	}
}
