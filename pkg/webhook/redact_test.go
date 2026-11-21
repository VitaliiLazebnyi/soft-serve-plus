package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/matryer/is"
)

func TestRedactURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"https://hooks.slack.com/services/T000/B000/XXXX", "https://hooks.slack.com/[redacted]"},
		{"https://example.com/hook?token=XXXX", "https://example.com/[redacted]"},
		{"https://example.com?token=XXXX", "https://example.com/[redacted]"},
		{"https://example.com/#XXXX", "https://example.com/[redacted]"},
		{"https://user:XXXX@example.com:8443/", "https://example.com:8443"},
		// net/http prints a username in full (only a password is masked).
		{"https://XXXX@example.com/x", "https://example.com/[redacted]"},
		{"https://XXXX@example.com", "https://example.com"},
		{"https://example.com", "https://example.com"},
		{"https://example.com/", "https://example.com"},
		{"http://[::1", "[redacted]"},
		{"not a url XXXX", "[redacted]"},
		{"", "[redacted]"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			is := is.New(t)
			is.Equal(redactURL(c.in), c.want)
		})
	}
}

func TestRedactURLErrorKeepsChain(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	in := &url.Error{Op: "Post", URL: "https://hooks.slack.com/services/T000/B000/XXXX", Err: context.DeadlineExceeded}

	err := utils.RedactURLError(in, redactURL)
	is.True(!strings.Contains(err.Error(), "XXXX"))
	is.True(!strings.Contains(err.Error(), "services"))
	is.True(strings.Contains(err.Error(), "hooks.slack.com"))
	is.True(errors.Is(err, context.DeadlineExceeded))
	var uerr *url.Error
	is.True(errors.As(err, &uerr))
	is.True(uerr.Timeout())
	is.Equal(in.URL, "https://hooks.slack.com/services/T000/B000/XXXX") // the original is not changed

	// A parse error's own message can quote part of the URL.
	_, perr := url.Parse("https://example.com:SECRETPORT/x") //nolint:staticcheck // invalid on purpose
	is.True(perr != nil)
	is.True(strings.Contains(perr.Error(), "SECRETPORT"))
	rerr := utils.RedactURLError(perr, redactURL)
	is.True(!strings.Contains(rerr.Error(), "SECRETPORT"))
	is.True(errors.Is(rerr, utils.ErrInvalidURL))
	is.True(errors.Is(rerr, ErrInvalidURL))              // the same error, through ssrf
	_, perr = url.Parse("https://example.com/%zzSECRET") //nolint:staticcheck // invalid on purpose
	is.True(perr != nil)
	is.True(!strings.Contains(utils.RedactURLError(perr, redactURL).Error(), "zz"))

	other := errors.New("other")
	is.Equal(utils.RedactURLError(other, redactURL), other)
	is.Equal(utils.RedactURLError(nil, redactURL), nil)
}

// secretPath and secretQuery stand for the tokens receivers such as Slack put
// in a webhook URL.
const (
	secretPath  = "/services/T000/B000/PATHSECRET"
	secretQuery = "?token=QUERYSECRET"
)

func assertNoSecret(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{"PATHSECRET", "QUERYSECRET", "services", "USERSECRET"} {
		if strings.Contains(s, secret) {
			t.Fatalf("%q contains %q", s, secret)
		}
	}
}

// A failed unrecorded send returns an error for the caller to log; it must
// not reveal the token in the webhook URL's path, query or user info. The
// token is the username here: net/http masks a password itself but prints a
// username in full.
func TestDeliverUnrecordedTransportErrorRedactsURL(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	base := strings.Replace(closingServer(t), "http://", "http://USERSECRET@", 1)
	w := e.newWebhook(t, base+secretPath+secretQuery, "", ContentTypeJSON, EventPush)

	err := e.s.DeliverUnrecorded(e.ctx, []models.Webhook{w}, samplePayloadFor(e))
	is.True(err != nil)
	assertNoSecret(t, err.Error())
	is.True(strings.Contains(err.Error(), "[redacted]"))
	// With the URL redacted, the error names the webhook instead.
	is.True(strings.Contains(err.Error(), fmt.Sprintf("webhook %d: ", w.ID)))
	var uerr *url.Error
	is.True(errors.As(err, &uerr))
}

// A send that runs out of time still reports a deadline error through the
// redacted error, and the recorded error text is redacted too.
func TestDeliverTimeoutRedactsURLAndKeepsDeadline(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	e.s.sendTimeout = 200 * time.Millisecond
	w := e.newWebhook(t, hangingServer(t)+secretPath+secretQuery, "", ContentTypeJSON, EventPush)

	err := e.s.DeliverUnrecorded(e.ctx, []models.Webhook{w}, samplePayloadFor(e))
	is.True(err != nil)
	assertNoSecret(t, err.Error())
	is.True(errors.Is(err, context.DeadlineExceeded))
	var nerr net.Error
	is.True(errors.As(err, &nerr))
	is.True(nerr.Timeout())

	is.NoErr(e.s.Deliver(e.ctx, []models.Webhook{w}, samplePayloadFor(e)))
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(strings.Contains(ds[0].RequestError.String, "deadline exceeded"))
	assertNoSecret(t, ds[0].RequestError.String)
}

// A cancelled send keeps context.Canceled in the chain.
func TestDeliverUnrecordedCancelKeepsCanceled(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, _ := newReceiver(t, 200, "")
	w := e.newWebhook(t, srv.URL+secretPath+secretQuery, "", ContentTypeJSON, EventPush)
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()

	err := e.s.DeliverUnrecorded(ctx, []models.Webhook{w}, samplePayloadFor(e))
	is.True(err != nil)
	assertNoSecret(t, err.Error())
	is.True(errors.Is(err, context.Canceled))
}

// A stored webhook URL that cannot be parsed fails in http.NewRequest. The
// parse error quotes part of the URL (here the port), so both the returned and
// the recorded error must say only "invalid URL" and carry no part of it.
func TestDeliverUnparsableURLRedactsURL(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	w := e.newWebhook(t, "http://USERSECRET@example.com:PORTSECRET"+secretPath+secretQuery, "", ContentTypeJSON, EventPush)

	assertRedacted := func(s string) {
		t.Helper()
		assertNoSecret(t, s)
		for _, part := range []string{"PORTSECRET", "example.com"} {
			if strings.Contains(s, part) {
				t.Fatalf("%q contains %q", s, part)
			}
		}
		is.True(strings.Contains(s, "invalid URL"))
	}

	err := e.s.DeliverUnrecorded(e.ctx, []models.Webhook{w}, samplePayloadFor(e))
	is.True(err != nil)
	is.True(errors.Is(err, utils.ErrInvalidURL))
	is.Equal(err.Error(), fmt.Sprintf("webhook %d: parse \"[redacted]\": invalid URL", w.ID))
	assertRedacted(err.Error())

	is.NoErr(e.s.Deliver(e.ctx, []models.Webhook{w}, samplePayloadFor(e)))
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseStatus, 0)
	assertRedacted(ds[0].RequestError.String)
}
