package webhook

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/google/uuid"
	"github.com/matryer/is"
)

// recorder is a receiver that remembers the last request it got.
type recorder struct {
	mu      sync.Mutex
	method  string
	header  http.Header
	body    string
	calls   int
	status  int
	resBody string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.method = req.Method
	r.header = req.Header.Clone()
	r.body = string(b)
	r.calls++
	r.mu.Unlock()
	if r.status != 0 {
		w.WriteHeader(r.status)
	}
	if r.resBody != "" {
		_, _ = io.WriteString(w, r.resBody)
	}
}

// newReceiver starts an in-memory receiver (see newTestServer) that answers
// with status and resBody and records what it gets.
func newReceiver(t *testing.T, status int, resBody string) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{status: status, resBody: resBody}
	srv := newTestServer(t, rec)
	return srv, rec
}

// samplePayload has url tags, so it can be sent as JSON or as a form.
func samplePayload() BranchTagEvent {
	return BranchTagEvent{
		Ref:     "refs/heads/main",
		Before:  "0000000000000000000000000000000000000000",
		After:   "1111111111111111111111111111111111111111",
		Created: true,
		Common: Common{
			EventType:  EventBranchTagCreate,
			Repository: Repository{ID: 1, Name: "repo"},
			Sender:     User{ID: 1, Username: "alice"},
		},
	}
}

func TestSendWebhookJSONDelivered(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, `{"ok":true}`)
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	is.Equal(rec.method, http.MethodPost)
	is.Equal(rec.header.Get("Content-Type"), ContentTypeJSON.String())
	is.Equal(rec.header.Get("X-SoftServe-Event"), EventBranchTagCreate.String())
	is.Equal(rec.header.Get("User-Agent"), "SoftServe/"+testVersion)
	// The delivery ID is a random (version 4) UUID: a version 1 UUID would
	// embed the server's MAC address and clock in every request.
	delID, err := uuid.Parse(rec.header.Get("X-SoftServe-Delivery"))
	is.NoErr(err)
	is.Equal(delID.Version(), uuid.Version(4))
	is.True(rec.header.Get("X-SoftServe-Signature") == "")

	var got BranchTagEvent
	is.NoErr(json.Unmarshal([]byte(rec.body), &got))
	is.Equal(got.Ref, "refs/heads/main")
	is.Equal(got.Sender.Username, "alice")

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseStatus, http.StatusOK)
	is.Equal(ds[0].ResponseBody, `{"ok":true}`)
	is.Equal(ds[0].RequestURL, srv.URL)
	is.Equal(ds[0].RequestMethod, http.MethodPost)
	is.Equal(ds[0].RequestError.String, "")
	is.Equal(ds[0].Event, int(EventBranchTagCreate))
	is.True(strings.Contains(ds[0].RequestHeaders, "X-Softserve-Event: branch_tag_create"))
	is.True(strings.Contains(ds[0].ResponseHeaders, "Content-Type"))
}

func TestSendWebhookFormDelivered(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusNoContent, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeForm, EventBranchTagCreate)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	is.Equal(rec.header.Get("Content-Type"), ContentTypeForm.String())
	form, err := url.ParseQuery(rec.body)
	is.NoErr(err)
	is.Equal(form.Get("ref"), "refs/heads/main")
	is.Equal(form.Get("created"), "true")
	is.Equal(form.Get("sender[username]"), "alice")

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseStatus, http.StatusNoContent)
}

// A form-encoded push event gives each commit field its own key, in the same
// bracket notation as the other nested fields, rather than a Go struct dump.
func TestSendWebhookFormPushCommits(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusNoContent, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeForm, EventPush)

	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	payload := PushEvent{
		Ref:    "refs/heads/main",
		Before: "0000000000000000000000000000000000000000",
		After:  "2222222222222222222222222222222222222222",
		Common: Common{
			EventType:  EventPush,
			Repository: Repository{ID: 1, Name: "repo"},
			Sender:     User{ID: 1, Username: "alice"},
		},
		Commits: []Commit{
			{
				ID:        "1111111111111111111111111111111111111111",
				Message:   "first&x=1\n",
				Title:     "first&x=1",
				Author:    Author{Name: "Alice", Email: "alice@example.com", Date: when},
				Committer: Author{Name: "Bob", Email: "bob@example.com", Date: when},
				Timestamp: when,
			},
			{
				ID:        "2222222222222222222222222222222222222222",
				Message:   "second",
				Title:     "second",
				Author:    Author{Name: "Carol", Email: "carol@example.com", Date: when},
				Committer: Author{Name: "Carol", Email: "carol@example.com", Date: when},
				Timestamp: when,
			},
		},
	}
	is.NoErr(e.s.sendWebhook(e.ctx, w, EventPush, payload))

	form, err := url.ParseQuery(rec.body)
	is.NoErr(err)
	is.Equal(form["commits"], nil) // no struct dump under the bare key
	is.Equal(form.Get("ref"), "refs/heads/main")
	is.Equal(form.Get("sender[username]"), "alice")
	is.Equal(form.Get("commits[0][id]"), "1111111111111111111111111111111111111111")
	is.Equal(form.Get("commits[0][message]"), "first&x=1\n")
	is.Equal(form.Get("commits[0][title]"), "first&x=1")
	is.Equal(form.Get("commits[0][author][name]"), "Alice")
	is.Equal(form.Get("commits[0][author][email]"), "alice@example.com")
	is.Equal(form.Get("commits[0][author][date]"), "2026-01-02T03:04:05Z")
	is.Equal(form.Get("commits[0][committer][name]"), "Bob")
	is.Equal(form.Get("commits[0][timestamp]"), "2026-01-02T03:04:05Z")
	is.Equal(form.Get("commits[1][id]"), "2222222222222222222222222222222222222222")
	is.Equal(form.Get("commits[1][author][name]"), "Carol")
}

func TestSendWebhookInvalidContentTypeRejected(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)
	w.ContentType = 99

	err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload())
	is.True(errors.Is(err, ErrInvalidContentType))
	is.Equal(rec.calls, 0)
	is.Equal(len(e.deliveries(t, w.ID)), 0)
}

// chanPayload is an EventPayload that JSON cannot encode.
type chanPayload chan int

func (chanPayload) Event() Event        { return EventBranchTagCreate }
func (chanPayload) RepositoryID() int64 { return 0 }

// intPayload is an EventPayload that a form cannot encode: query.Values only
// accepts structs (or maps).
type intPayload int

func (intPayload) Event() Event        { return EventBranchTagCreate }
func (intPayload) RepositoryID() int64 { return 0 }

func TestSendWebhookJSONEncodeErrorReturned(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	// Channels cannot be encoded as JSON.
	err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, chanPayload(make(chan int)))
	is.True(err != nil)
	is.Equal(rec.calls, 0)
	is.Equal(len(e.deliveries(t, w.ID)), 0)
}

func TestSendWebhookFormEncodeErrorReturned(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeForm, EventBranchTagCreate)

	// query.Values only accepts structs (or maps), not a bare int.
	err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, intPayload(5))
	is.True(err != nil)
	is.Equal(rec.calls, 0)
	is.Equal(len(e.deliveries(t, w.ID)), 0)
}

func TestSendWebhookSignatureMatchesBody(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "s3cret", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(rec.body))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	is.Equal(rec.header.Get("X-SoftServe-Signature"), want)
}

// The V2 signature covers the timestamp and delivery GUID as well as the
// body, so a receiver can refuse a replayed request.
func TestSendWebhookSignatureV2CoversTimestampAndDelivery(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	fixed := time.Unix(1731000000, 0)
	e.s.now = func() time.Time { return fixed }
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "s3cret", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	ts := rec.header.Get(HeaderTimestamp)
	is.Equal(ts, "1731000000")
	guid := rec.header.Get(HeaderDelivery)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(ts + "." + guid + "." + rec.body))
	is.Equal(rec.header.Get(HeaderSignatureV2), "sha256="+hex.EncodeToString(mac.Sum(nil)))

	// The delivery GUID of a first delivery is the id of its record.
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ID.String(), guid)
}

// Without a secret there is nothing to sign, but the timestamp is still sent.
func TestSendWebhookTimestampWithoutSecret(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	is.True(rec.header.Get(HeaderTimestamp) != "")
	is.Equal(rec.header.Get(HeaderSignatureV2), "")
}

// A redelivery sends the GUID it is given, under a record of its own, and
// signs it with a fresh timestamp.
func TestRedeliverWebhookKeepsGUID(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "s3cret", ContentTypeJSON, EventBranchTagCreate)
	guid := uuid.New()
	fixed := time.Unix(1731000500, 0)
	e.s.now = func() time.Time { return fixed }

	body, err := encode(ContentTypeJSON, samplePayload())
	is.NoErr(err)
	is.NoErr(e.s.Redeliver(e.ctx, w, EventBranchTagCreate, ContentTypeJSON, body, guid))

	is.Equal(rec.header.Get(HeaderDelivery), guid.String())
	is.Equal(rec.header.Get(HeaderTimestamp), "1731000500")
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte("1731000500." + guid.String() + "." + rec.body))
	is.Equal(rec.header.Get(HeaderSignatureV2), "sha256="+hex.EncodeToString(mac.Sum(nil)))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(ds[0].ID != guid)
	is.True(strings.Contains(ds[0].RequestHeaders, "X-Softserve-Delivery: "+guid.String()))
}

func TestSendWebhookSignatureDependsOnSecret(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	a := e.newWebhook(t, srv.URL, "one", ContentTypeJSON, EventBranchTagCreate)
	is.NoErr(e.s.sendWebhook(e.ctx, a, EventBranchTagCreate, samplePayload()))
	sigOne := rec.header.Get("X-SoftServe-Signature")

	b := e.newWebhook(t, srv.URL+"/two", "two", ContentTypeJSON, EventBranchTagCreate)
	is.NoErr(e.s.sendWebhook(e.ctx, b, EventBranchTagCreate, samplePayload()))
	sigTwo := rec.header.Get("X-SoftServe-Signature")

	is.True(sigOne != "")
	is.True(sigOne != sigTwo)
}

// closingServer reads each request and closes the connection without
// answering, so every request fails the same way.
func closingServer(t *testing.T) string {
	t.Helper()
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close() //nolint:errcheck
		}
	}))
	return srv.URL
}

func TestSendWebhookNetworkErrorRecorded(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	w := e.newWebhook(t, closingServer(t), "", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(ds[0].RequestError.String != "")
	is.Equal(ds[0].ResponseStatus, 0)
	is.Equal(ds[0].ResponseBody, "")
}

// waitingForWriteTurn reports whether a goroutine waits for its turn to
// begin a write transaction in db.DB to record a delivery.
func waitingForWriteTurn() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, frames, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[select") &&
			strings.HasPrefix(frames, "github.com/charmbracelet/soft-serve/pkg/db.(*DB).transaction(") &&
			strings.Contains(frames, "github.com/charmbracelet/soft-serve/pkg/webhook.(*Sender).recordDelivery(") {
			return true
		}
	}
	return false
}

// TestSendWebhookRecordWaitsForWriteTurn holds this process's SQLite write
// turn while a delivery is recorded, for longer than the record used to
// allow. The record waits for its turn, so the delivery is still in the
// history. It used to give up after 5 seconds, which lost a delivery on a
// loaded Windows runner.
//
// It stays serial: waitingForWriteTurn looks at every goroutine of the
// process, and would see another test's record.
func TestSendWebhookRecordWaitsForWriteTurn(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, _ := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- e.dbx.TransactionContext(context.Background(), func(*db.Tx) error {
			close(held)
			// Hold the turn from when the record waits for it for
			// longer than the 5 seconds the record used to wait. Load
			// can only make the hold longer.
			deadline := time.Now().Add(time.Minute)
			for !waitingForWriteTurn() {
				if time.Now().After(deadline) {
					return errors.New("the record never waited for the write turn")
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(6 * time.Second)
			return nil
		})
	}()
	<-held

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))
	is.NoErr(<-done)
	is.Equal(len(e.deliveries(t, w.ID)), 1)
}

func TestSendWebhookNonSuccessStatusRecorded(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, _ := newReceiver(t, http.StatusInternalServerError, "boom")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestError.String, "") // the request itself succeeded
	is.Equal(ds[0].ResponseStatus, http.StatusInternalServerError)
	is.Equal(ds[0].ResponseBody, "boom")
}

func TestSendWebhookInvalidURLRecorded(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	w := e.newWebhook(t, "http://[::1", "", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(ds[0].RequestError.String != "")
	is.Equal(ds[0].ResponseStatus, 0)
}

// truncatingServer answers every request with a Content-Length longer than the
// body it sends, then closes the connection. Reading the body fails.
func truncatingServer(t *testing.T) string {
	t.Helper()
	// In memory, like newTestServer.
	ln := newMemListener()
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck
				r := bufio.NewReader(c)
				req, err := http.ReadRequest(r)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort")
			}(conn)
		}
	}()
	return fmt.Sprintf("http://%s/hook", ln.Addr().String())
}

// Regression for suspect (b): a body-read error used to return before the
// delivery was recorded. The delivery must be recorded and the read error kept.
func TestSendWebhookBodyReadErrorStillRecordsDelivery(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	w := e.newWebhook(t, truncatingServer(t), "", ContentTypeJSON, EventBranchTagCreate)

	err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload())
	is.NoErr(err)

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseStatus, http.StatusOK)
	is.True(strings.Contains(ds[0].RequestError.String, "unexpected EOF"))
}

// sendEvent sends payload to the repository's subscribers, as the server's
// delivery queue does: Subscribers, then Deliver.
func (s *Sender) sendEvent(ctx context.Context, payload EventPayload) error {
	hooks, err := s.Subscribers(ctx, payload)
	if err != nil {
		return err
	}
	return s.Deliver(ctx, hooks, payload)
}

func TestSendEventNoWebhooksIsNoop(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)

	is.NoErr(e.s.sendEvent(e.ctx, samplePayloadFor(e)))
}

func TestSendEventDeliversToSubscribedWebhooksOnly(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")

	push := e.newWebhook(t, srv.URL+"/push", "", ContentTypeJSON, EventPush)
	other := e.newWebhook(t, srv.URL+"/collab", "", ContentTypeJSON, EventCollaborator)

	is.NoErr(e.s.sendEvent(e.ctx, samplePayloadFor(e)))

	is.Equal(rec.calls, 1)
	is.Equal(len(e.deliveries(t, push.ID)), 1)
	is.Equal(len(e.deliveries(t, other.ID)), 0)
}

func TestSendEventDeliversToEveryWebhook(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")

	var ws []models.Webhook
	for i := 0; i < 3; i++ {
		ws = append(ws, e.newWebhook(t, fmt.Sprintf("%s/%d", srv.URL, i), "", ContentTypeJSON, EventPush))
	}

	is.NoErr(e.s.sendEvent(e.ctx, samplePayloadFor(e)))

	is.Equal(rec.calls, 3)
	for _, w := range ws {
		is.Equal(len(e.deliveries(t, w.ID)), 1)
	}
}

// BUG (suspect a): sending an event returned on the first SendWebhook error, so the
// webhooks after the failing one are never delivered. Every subscribed webhook
// must be attempted, and the failure must still be reported.
func TestSendEventFailingWebhookDoesNotBlockOthers(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")

	bad := e.newWebhook(t, srv.URL+"/bad", "", ContentTypeJSON, EventPush)
	// Stored with an unknown content type: SendWebhook fails for it.
	is.NoErr(e.ds.UpdateWebhookByID(e.ctx, e.dbx, models.Webhook{ID: bad.ID, RepoID: e.repo.ID, URL: bad.URL, ContentType: 99, Active: true}))
	good := e.newWebhook(t, srv.URL+"/good", "", ContentTypeJSON, EventPush)

	err := e.s.sendEvent(e.ctx, samplePayloadFor(e))
	is.True(err != nil)
	is.Equal(len(e.deliveries(t, good.ID)), 1)
	is.Equal(rec.calls, 1)
}

func TestSendEventWebhookLookupErrorReturned(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	is.NoErr(e.dbx.Close())

	is.True(e.s.sendEvent(e.ctx, samplePayloadFor(e)) != nil)
}

func TestSendWebhookRecordErrorReturned(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, _ := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	is.NoErr(e.dbx.Close())

	is.True(e.s.sendWebhook(e.ctx, w, EventPush, samplePayload()) != nil)
}

// samplePayloadFor is an event for the test repository, with the owner set.
func samplePayloadFor(e *testEnv) EventPayload {
	return PushEvent{
		Ref:    "refs/heads/main",
		Before: "0000000000000000000000000000000000000000",
		After:  "1111111111111111111111111111111111111111",
		Common: Common{
			EventType:  EventPush,
			Repository: Repository{ID: e.repo.ID, Name: e.repo.Name},
			Sender:     User{ID: e.owner.ID, Username: e.owner.Username},
		},
	}
}

// Every request and response header must be recorded, not only the last one.
func TestSendWebhookRecordsEveryHeader(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Receiver-A", "alpha")
		w.Header().Set("X-Receiver-B", "beta")
		w.WriteHeader(http.StatusAccepted)
	}))
	w := e.newWebhook(t, srv.URL, "secret", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	for _, want := range []string{
		"Content-Type: application/json",
		"User-Agent: SoftServe/",
		"X-Softserve-Event: branch_tag_create",
		"X-Softserve-Delivery: ",
		"X-Softserve-Signature: sha256=",
	} {
		is.True(strings.Contains(ds[0].RequestHeaders, want))
	}
	is.True(strings.Contains(ds[0].ResponseHeaders, "X-Receiver-A: alpha"))
	is.True(strings.Contains(ds[0].ResponseHeaders, "X-Receiver-B: beta"))
	is.Equal(ds[0].ResponseStatus, http.StatusAccepted)
}

// A stored header block is sorted by key, so the same headers are always
// stored the same way, and keeps every value of a repeated header.
func TestFormatHeadersKeepsEveryValue(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	h := http.Header{}
	h.Add("X-B", "1")
	h.Add("Set-Cookie", "a=1")
	h.Add("Set-Cookie", "b=2")
	h.Add("Content-Type", "text/plain")
	h.Add("X-A", "0")

	want := "Content-Type: text/plain\n" +
		"Set-Cookie: a=1\n" +
		"Set-Cookie: b=2\n" +
		"X-A: 0\n" +
		"X-B: 1\n"
	for range 10 { // map order varies between runs
		is.Equal(formatHeaders(h), want)
	}
	is.Equal(formatHeaders(nil), "")
}

// hangingServer accepts requests and never answers them until the test ends.
func hangingServer(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	srv := newTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })
	return srv.URL
}

// Each webhook gets its own send timeout: a receiver that never answers uses
// up its own time only, and the webhooks after it are still sent and recorded.
//
// The send timeout is shortened so the slow receiver runs out of time
// quickly. It is still 2 seconds because the fast webhook has to be delivered
// within it too, which an in-memory request does with a wide margin even on a
// loaded machine. A deadline shared between the webhooks would leave the
// fast one no time at all, so the test still catches that.
func TestDeliverSlowWebhookDoesNotStarveOthers(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	e.s.sendTimeout = 2 * time.Second
	srv, rec := newReceiver(t, http.StatusOK, "")
	slow := e.newWebhook(t, hangingServer(t), "", ContentTypeJSON, EventPush)
	fast := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)

	is.NoErr(e.s.Deliver(e.ctx, []models.Webhook{slow, fast}, samplePayloadFor(e)))

	slowDs := e.deliveries(t, slow.ID)
	is.Equal(len(slowDs), 1)
	is.True(strings.Contains(slowDs[0].RequestError.String, "deadline exceeded"))
	fastDs := e.deliveries(t, fast.ID)
	is.Equal(len(fastDs), 1)
	is.Equal(fastDs[0].ResponseStatus, http.StatusOK)
	is.Equal(rec.calls, 1)
}

// A delivery whose context is already done sends nothing, but it is still
// recorded: the re-read before the send fails on the done context, so the
// event is recorded as undelivered, with a context of its own.
func TestDeliverRecordsUndeliveredWhenContextDoneBeforeSend(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()

	is.NoErr(e.s.Deliver(ctx, []models.Webhook{w}, samplePayloadFor(e)))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestError.String, errRereadFailed.Error())
	is.Equal(rec.calls, 0)
}

// A delivery whose context is cancelled while the request is in flight is
// still recorded, with the cancellation as its error: the row is written with
// a context of its own.
func TestDeliverRecordsAttemptWhenContextCancelledDuringSend(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	release := make(chan struct{})
	srv := newTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		cancel()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)

	is.NoErr(e.s.Deliver(ctx, []models.Webhook{w}, samplePayloadFor(e)))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(strings.Contains(ds[0].RequestError.String, "context canceled"))
	is.Equal(ds[0].ResponseStatus, 0)
}

// Unrecorded deliveries have no row to show a failed status, so a non-2xx
// response is returned as an error for the caller to log.
func TestDeliverUnrecordedReportsNonSuccessStatus(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	bad, _ := newReceiver(t, http.StatusInternalServerError, "boom")
	good, _ := newReceiver(t, http.StatusNoContent, "")
	badHook := e.newWebhook(t, bad.URL, "", ContentTypeJSON, EventPush)
	goodHook := e.newWebhook(t, good.URL, "", ContentTypeJSON, EventPush)

	err := e.s.DeliverUnrecorded(e.ctx, []models.Webhook{badHook}, samplePayloadFor(e))
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "status 500"))
	is.NoErr(e.s.DeliverUnrecorded(e.ctx, []models.Webhook{goodHook}, samplePayloadFor(e)))

	is.Equal(len(e.deliveries(t, badHook.ID)), 0)
	is.Equal(len(e.deliveries(t, goodHook.ID)), 0)
}

// RecordUndelivered stores the request it would have sent, with the reason as
// its error, and sends nothing.
func TestRecordUndeliveredStoresRequestWithoutSending(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "s3cret", ContentTypeJSON, EventPush)
	payload := samplePayloadFor(e)

	is.NoErr(e.s.RecordUndelivered(e.ctx, []models.Webhook{w}, payload, errors.New("queue full")))

	is.Equal(rec.calls, 0)
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestError.String, "queue full")
	is.Equal(ds[0].ResponseStatus, 0)
	is.Equal(ds[0].Event, int(EventPush))
	want, err := json.Marshal(payload)
	is.NoErr(err)
	is.Equal(strings.TrimSpace(ds[0].RequestBody), string(want))
	is.True(strings.Contains(ds[0].RequestHeaders, "X-Softserve-Signature: sha256="))
}

// Undelivered events cost no network time, so they are the quickest way to
// grow the deliveries table. Every record prunes, so a webhook never holds
// more than the limit, and pruning one webhook leaves the others' deliveries
// alone.
func TestRecordedDeliveriesAreBoundedPerWebhook(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.s.maxDeliveries = 5
	a := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)
	b := e.newWebhook(t, "https://93.184.216.34/b", "", ContentTypeJSON, EventPush)
	payload := samplePayloadFor(e)

	var recorded []uuid.UUID
	for i := range 12 {
		is.NoErr(e.s.RecordUndelivered(e.ctx, []models.Webhook{a}, payload, errors.New("queue full")))
		ds := e.deliveries(t, a.ID)
		is.Equal(len(ds), min(i+1, 5))
		// The delivery just recorded is always kept.
		recorded = append(recorded, ds[len(ds)-1].ID)
	}
	for range 3 {
		is.NoErr(e.s.RecordUndelivered(e.ctx, []models.Webhook{b}, payload, errors.New("queue full")))
	}

	// The five kept are the five most recent, oldest first.
	var kept []uuid.UUID
	for _, d := range e.deliveries(t, a.ID) {
		kept = append(kept, d.ID)
	}
	is.Equal(kept, recorded[len(recorded)-5:])
	is.Equal(len(e.deliveries(t, b.ID)), 3)
}

// Production delivery IDs are time-based (version 1) UUIDs, whose text does
// not sort by time, so retention must not depend on them. Deliveries with
// IDs in descending order are still pruned oldest first.
func TestRecordedDeliveriesPruneIgnoresIDOrder(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.s.maxDeliveries = 2
	w := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)
	ids := []uuid.UUID{
		uuid.MustParse("ffffffff-0000-1000-8000-000000000000"),
		uuid.MustParse("88888888-0000-1000-8000-000000000000"),
		uuid.MustParse("00000000-0000-1000-8000-000000000000"),
	}
	for _, id := range ids {
		req, err := e.newRequest(w, EventPush, samplePayloadFor(e))
		is.NoErr(err)
		req.id = id
		is.NoErr(e.s.recordDelivery(e.ctx, w, EventPush, req, nil, 200, "", ""))
	}
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 2)
	is.Equal(ds[0].ID, ids[1])
	is.Equal(ds[1].ID, ids[2])
}

// countingPruneStore counts the prunes it is asked for.
type countingPruneStore struct {
	store.Store
	prunes *atomic.Int32
}

func (s countingPruneStore) PruneWebhookDeliveries(ctx context.Context, h db.Handler, id int64, keep int) error {
	s.prunes.Add(1)
	return s.Store.PruneWebhookDeliveries(ctx, h, id, keep)
}

// A send that was in flight when its webhook was deleted records nothing and
// returns no error: the insert fails on its foreign key, which the store
// maps to db.ErrForeignKey, and there is nothing left to prune. A trigger
// deletes the webhook as the delivery is inserted, on either driver.
func TestRecordDeliveryWebhookDeletedMidSend(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnvOnEitherDriver(t)
	w := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)

	triggers := []string{fmt.Sprintf(`CREATE TRIGGER vanish BEFORE INSERT ON webhook_deliveries
		WHEN NEW.webhook_id = %d
		BEGIN DELETE FROM webhooks WHERE id = NEW.webhook_id; END;`, w.ID)}
	if e.dbx.DriverName() == "postgres" {
		triggers = []string{
			`CREATE FUNCTION vanish() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN DELETE FROM webhooks WHERE id = NEW.webhook_id; RETURN NEW; END $$;`,
			fmt.Sprintf(`CREATE TRIGGER vanish BEFORE INSERT ON webhook_deliveries
			FOR EACH ROW WHEN (NEW.webhook_id = %d) EXECUTE FUNCTION vanish();`, w.ID),
		}
	}
	for _, q := range triggers {
		_, err := e.dbx.ExecContext(e.ctx, q)
		is.NoErr(err)
	}
	var prunes atomic.Int32
	e.s.store = countingPruneStore{e.ds, &prunes}

	req, err := e.newRequest(w, EventPush, samplePayloadFor(e))
	is.NoErr(err)
	is.NoErr(e.s.recordDelivery(e.ctx, w, EventPush, req, nil, 200, "", ""))
	is.Equal(len(e.deliveries(t, w.ID)), 0) // nothing recorded
	is.Equal(prunes.Load(), int32(0))       // and nothing pruned
	// The failed insert rolls back the trigger's delete with it, so the
	// webhook is still there; only the foreign key failure was seen.
}

// failingPruneStore is a store whose prune always fails.
type failingPruneStore struct{ store.Store }

func (failingPruneStore) PruneWebhookDeliveries(context.Context, db.Handler, int64, int) error {
	return errors.New("prune failed")
}

// A failed prune must not lose the delivery, which is recorded first, and is
// logged through the Sender's logger.
func TestRecordDeliveryKeepsDeliveryWhenPruneFails(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	w := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)
	var logs bytes.Buffer
	e.s.store = failingPruneStore{e.ds}
	e.s.logger = log.New(&logs)

	req, err := e.newRequest(w, EventPush, samplePayloadFor(e))
	is.NoErr(err)
	is.NoErr(e.s.recordDelivery(e.ctx, w, EventPush, req, nil, 200, "", ""))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ID, req.id)
	is.True(strings.Contains(logs.String(), "pruning webhook deliveries"))
	is.True(strings.Contains(logs.String(), "prune failed"))
}

// hangingPruneStore is a store whose prune fails with the record timeout's
// error, and which counts the delivery rows it is asked to create.
type hangingPruneStore struct {
	store.Store
	creates *atomic.Int32
}

func (s hangingPruneStore) CreateWebhookDelivery(ctx context.Context, h db.Handler, d models.WebhookDelivery) error {
	s.creates.Add(1)
	return s.Store.CreateWebhookDelivery(ctx, h, d)
}

// PruneWebhookDeliveries fails as a prune that ran out of the record
// timeout does. It fails at once rather than wait for a short timeout to
// pass: the insert before it shares that timeout and, on a loaded machine,
// can use all of it.
func (hangingPruneStore) PruneWebhookDeliveries(context.Context, db.Handler, int64, int) error {
	return fmt.Errorf("prune: %w", context.DeadlineExceeded)
}

// A prune that runs out of recordTimeout keeps the delivery, which was
// written before it, inserted once: the insert and the prune are separate
// statements. The prune's failure is logged, not returned.
func TestRecordDeliveryKeepsDeliveryWhenPruneTimesOut(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	w := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)
	var creates atomic.Int32
	var logs bytes.Buffer
	e.s.store = hangingPruneStore{e.ds, &creates}
	e.s.logger = log.New(&logs)

	req, err := e.newRequest(w, EventPush, samplePayloadFor(e))
	is.NoErr(err)
	is.NoErr(e.s.recordDelivery(e.ctx, w, EventPush, req, nil, 200, "", ""))
	is.Equal(creates.Load(), int32(1))
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ID, req.id)
	is.True(strings.Contains(logs.String(), "pruning webhook deliveries"))
	is.True(strings.Contains(logs.String(), context.DeadlineExceeded.Error()))
}

// Deliver re-reads each webhook before sending, so an event that waited while
// the webhook was changed goes to its current URL, signed with its current
// secret, and not to the old receiver.
func TestDeliverUsesCurrentWebhook(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	oldSrv, oldRec := newReceiver(t, http.StatusOK, "")
	newSrv, newRec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, oldSrv.URL, "old-secret", ContentTypeJSON, EventPush)
	is.NoErr(e.ds.UpdateWebhookByID(e.ctx, e.dbx, models.Webhook{ID: w.ID, RepoID: e.repo.ID, URL: newSrv.URL, Secret: "new-secret", ContentType: int(ContentTypeJSON), Active: true}))

	is.NoErr(e.s.Deliver(e.ctx, []models.Webhook{w}, samplePayloadFor(e)))

	is.Equal(oldRec.calls, 0)
	is.Equal(newRec.calls, 1)
	mac := hmac.New(sha256.New, []byte("new-secret"))
	mac.Write([]byte(newRec.body))
	is.Equal(newRec.header.Get("X-SoftServe-Signature"), "sha256="+hex.EncodeToString(mac.Sum(nil)))
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestURL, newSrv.URL)
}

// A webhook deleted, or no longer subscribed to the event, after the event was
// read is not sent to.
func TestDeliverSkipsRemovedOrUnsubscribedWebhook(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	deleted := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	unsubscribed := e.newWebhook(t, srv.URL+"/?other", "", ContentTypeJSON, EventPush)
	is.NoErr(e.ds.DeleteWebhookForRepoByID(e.ctx, e.dbx, e.repo.ID, deleted.ID))
	evs, err := e.ds.GetWebhookEventsByWebhookID(e.ctx, e.dbx, unsubscribed.ID)
	is.NoErr(err)
	ids := make([]int64, 0, len(evs))
	for _, ev := range evs {
		ids = append(ids, ev.ID)
	}
	is.NoErr(e.ds.DeleteWebhookEventsByID(e.ctx, e.dbx, unsubscribed.ID, ids))
	is.NoErr(e.ds.CreateWebhookEvents(e.ctx, e.dbx, unsubscribed.ID, []int{int(EventBranchTagCreate)}))

	is.NoErr(e.s.Deliver(e.ctx, []models.Webhook{deleted, unsubscribed}, samplePayloadFor(e)))

	is.Equal(rec.calls, 0)
	is.Equal(len(e.deliveries(t, unsubscribed.ID)), 0)
}

// countingLookupStore counts the subscriber lookups made through it.
type countingLookupStore struct {
	store.Store
	listCalls, hookCalls *atomic.Int32
}

func (s countingLookupStore) GetWebhooksByRepoIDWhereEvent(ctx context.Context, h db.Handler, repoID int64, events []int) ([]models.Webhook, error) {
	s.listCalls.Add(1)
	return s.Store.GetWebhooksByRepoIDWhereEvent(ctx, h, repoID, events)
}

func (s countingLookupStore) GetActiveWebhookByIDWhereEvent(ctx context.Context, h db.Handler, repoID, id int64, event int) (models.Webhook, error) {
	s.hookCalls.Add(1)
	return s.Store.GetActiveWebhookByIDWhereEvent(ctx, h, repoID, id, event)
}

// Re-reading a webhook before its send reads only that webhook, not every
// subscriber of the repository once per webhook.
func TestDeliverRereadsEachWebhookAlone(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	var hooks []models.Webhook
	for i := range 3 {
		hooks = append(hooks, e.newWebhook(t, fmt.Sprintf("%s/?%d", srv.URL, i), "", ContentTypeJSON, EventPush))
	}
	cs := countingLookupStore{Store: e.ds, listCalls: new(atomic.Int32), hookCalls: new(atomic.Int32)}
	e.s.store = cs
	ctx := e.ctx

	is.NoErr(e.s.Deliver(ctx, hooks, samplePayloadFor(e)))

	is.Equal(rec.calls, 3)
	is.Equal(cs.listCalls.Load(), int32(0))
	is.Equal(cs.hookCalls.Load(), int32(3))
}

// useRereadTiming sets the re-read timeout and retry delay of the test's
// Sender. A test whose re-read must succeed keeps the production timeout
// (pass defaultRereadTimeout): a real database read on a loaded machine can
// be slow, and only a re-read that is meant to run out of time gets a short
// one.
func (e *testEnv) useRereadTiming(timeout, delay time.Duration) {
	e.s.rereadTimeout, e.s.rereadRetryDelay = timeout, delay
}

// failingLookupStore is a store whose event lookup fails, as during a database
// error, while everything else works. Only the first failures lookups fail,
// or every lookup when failures is zero.
type failingLookupStore struct {
	store.Store
	failures int32
	calls    *atomic.Int32
}

func (s failingLookupStore) GetActiveWebhookByIDWhereEvent(ctx context.Context, h db.Handler, repoID, id int64, event int) (models.Webhook, error) {
	n := s.calls.Add(1)
	if s.failures == 0 || n <= s.failures {
		return models.Webhook{}, errors.New("lookup failed: dial tcp 10.0.0.5:5432")
	}
	return s.Store.GetActiveWebhookByIDWhereEvent(ctx, h, repoID, id, event)
}

// A webhook that cannot be re-read before its send, even after a retry, may
// have been deactivated, so it is not sent to. The event is recorded as
// undelivered with a generic reason, so it can be redelivered; the database
// error is logged with the webhook, repository and event, and is not stored,
// since repository admins can read the stored reason.
func TestDeliverRecordsUndeliveredWhenRereadFails(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	e.useRereadTiming(defaultRereadTimeout, time.Millisecond)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	calls := new(atomic.Int32)
	var logs strings.Builder
	e.s.store = failingLookupStore{Store: e.ds, calls: calls}
	e.s.logger = log.New(&logs)
	ctx := e.ctx

	is.NoErr(e.s.Deliver(ctx, []models.Webhook{w}, samplePayloadFor(e)))

	is.Equal(rec.calls, 0)
	is.Equal(calls.Load(), int32(2))
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestError.String, errRereadFailed.Error())
	is.Equal(ds[0].ResponseStatus, 0)
	is.Equal(ds[0].Event, int(EventPush))
	out := logs.String()
	is.True(strings.Contains(out, "WARN"))
	is.True(strings.Contains(out, "lookup failed: dial tcp 10.0.0.5:5432"))
	is.True(strings.Contains(out, fmt.Sprintf("webhook_id=%d", w.ID)))
	is.True(strings.Contains(out, fmt.Sprintf("repo_id=%d", e.repo.ID)))
	is.True(strings.Contains(out, "event=push"))
	is.True(!strings.Contains(out, srv.URL))
}

// A re-read that fails once is retried, and the webhook is sent to when the
// retry works.
func TestDeliverRetriesFailedReread(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	e.useRereadTiming(defaultRereadTimeout, time.Millisecond)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	calls := new(atomic.Int32)
	e.s.store = failingLookupStore{Store: e.ds, failures: 1, calls: calls}
	ctx := e.ctx

	is.NoErr(e.s.Deliver(ctx, []models.Webhook{w}, samplePayloadFor(e)))

	is.Equal(calls.Load(), int32(2))
	is.Equal(rec.calls, 1)
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestError.String, "")
	is.Equal(ds[0].ResponseStatus, http.StatusOK)
}

// hangingLookupStore is a store whose event lookup does not answer until its
// context is done, as with a database that hangs.
type hangingLookupStore struct {
	store.Store
	calls *atomic.Int32
}

func (s hangingLookupStore) GetActiveWebhookByIDWhereEvent(ctx context.Context, _ db.Handler, _, _ int64, _ int) (models.Webhook, error) {
	s.calls.Add(1)
	<-ctx.Done()
	return models.Webhook{}, ctx.Err()
}

// A background delivery runs with a context that is never cancelled, so the
// re-read is bounded on its own: a database that hangs does not hold the
// sender, and the event is recorded as undelivered.
func TestDeliverRereadIsBounded(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	e.useRereadTiming(50*time.Millisecond, time.Millisecond)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	calls := new(atomic.Int32)
	e.s.store = hangingLookupStore{Store: e.ds, calls: calls}
	ctx := context.WithoutCancel(e.ctx)

	done := make(chan error, 1)
	go func() { done <- e.s.Deliver(ctx, []models.Webhook{w}, samplePayloadFor(e)) }()
	// Deliver returns after both re-reads run out of time and the event is
	// recorded, which may itself take up to recordTimeout; the bound only
	// tells a re-read that is not bounded from a slow machine.
	select {
	case err := <-done:
		is.NoErr(err)
	case <-time.After(time.Minute):
		t.Fatal("Deliver did not return while the re-read hung")
	}

	is.Equal(calls.Load(), int32(2))
	is.Equal(rec.calls, 0)
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestError.String, errRereadFailed.Error())
}

// endlessServer answers with a 200 and streams a body that never ends, until
// the client goes away.
func endlessServer(t *testing.T) string {
	t.Helper()
	chunk := []byte(strings.Repeat("x", 4096))
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	return srv.URL
}

// A receiver that streams without end does not hold the sender until its
// timeout: only maxResponseBody bytes are read and stored, marked as cut off,
// and the delivery itself counts as a success.
func TestSendWebhookResponseBodyIsCapped(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	w := e.newWebhook(t, endlessServer(t), "", ContentTypeJSON, EventPush)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventPush, samplePayloadFor(e)))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestError.String, "")
	is.Equal(ds[0].ResponseStatus, http.StatusOK)
	is.Equal(ds[0].ResponseBody, strings.Repeat("x", maxResponseBody)+truncatedMarker)
}

// A body of exactly maxResponseBody bytes is stored whole and not marked.
func TestSendWebhookResponseBodyAtLimitIsKept(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	body := strings.Repeat("y", maxResponseBody)
	srv, _ := newReceiver(t, http.StatusOK, body)
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventPush, samplePayloadFor(e)))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseBody, body)
}

// A read that fails before the limit keeps what was read and reports the
// error. A failure after the limit is never reached.
func TestReadResponseBodyKeepsPartialReadOnError(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	boom := errors.New("boom")

	got, err := readResponseBody(io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(boom)))
	is.True(errors.Is(err, boom))
	is.Equal(got, "partial")

	long := strings.Repeat("z", maxResponseBody+10)
	got, err = readResponseBody(io.MultiReader(strings.NewReader(long), iotest.ErrReader(boom)))
	is.NoErr(err)
	is.Equal(got, long[:maxResponseBody]+truncatedMarker)
}

// formatHeaders lists headers sorted by key with every value, skips
// keys without a value, and stops at maxStoredHeaders with a marker.
func TestFormatHeaders(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	is.Equal(formatHeaders(http.Header{
		"X-B":     {"two", "three"},
		"X-A":     {"one"},
		"X-Empty": {},
		"X-C":     {""},
	}), "X-A: one\nX-B: two\nX-B: three\nX-C: \n")
	is.Equal(formatHeaders(nil), "")

	big := http.Header{}
	value := strings.Repeat("v", 1000)
	for i := range 200 {
		big.Set(fmt.Sprintf("X-Pad-%03d", i), value)
	}
	got := formatHeaders(big)
	is.True(strings.HasSuffix(got, truncatedHeadersMarker))
	is.True(len(got) <= maxStoredHeaders+len(truncatedHeadersMarker))
	is.True(strings.HasPrefix(got, "X-Pad-000: "+value+"\n"))

	// Invalid bytes grow to U+FFFD (3 bytes) when made storable; the cap
	// holds for the stored text, not the raw bytes.
	invalid := http.Header{}
	bad := strings.Repeat("\xffa", 500)
	for i := range 200 {
		invalid.Set(fmt.Sprintf("X-Bad-%03d", i), bad)
	}
	got = formatHeaders(invalid)
	is.True(strings.HasSuffix(got, truncatedHeadersMarker))
	is.True(len(got) <= maxStoredHeaders+len(truncatedHeadersMarker))
	is.Equal(storableText(got), got)
}

// A receiver's headers must not be stored past maxStoredHeaders. The client
// refuses a response whose headers pass ssrf.MaxResponseHeaderBytes, the same
// size, but headers can still grow once stored: an invalid UTF-8 byte is
// stored as U+FFFD, three bytes (a run of them becomes one U+FFFD, so each
// is followed by a valid byte here). The receiver sends headers the client
// accepts, about 40 KB, which come to about 80 KB as stored text, so the
// stored copy must be cut off.
func TestSendWebhookCapsStoredResponseHeaders(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	e.useHTTPClient(t)
	value := strings.Repeat("\xffa", 500)
	const headers = 40
	is.True(headers*len("X-Pad-000: \r\n"+value) < ssrf.MaxResponseHeaderBytes-1024)
	is.True(headers*len(storableText(value)) > maxStoredHeaders)
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := range headers {
			w.Header().Set(fmt.Sprintf("X-Pad-%03d", i), value)
		}
		w.WriteHeader(http.StatusOK)
	}))
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseStatus, http.StatusOK)
	is.True(len(ds[0].ResponseHeaders) <= maxStoredHeaders+len(truncatedHeadersMarker))
	is.True(strings.HasSuffix(ds[0].ResponseHeaders, truncatedHeadersMarker))
}
