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
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf8"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/google/uuid"
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
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, `{"ok":true}`)
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}

	if got := rec.method; got != http.MethodPost {
		t.Fatalf("rec.method = %v, want %v", got, http.MethodPost)
	}
	if got, want := rec.header.Get("Content-Type"), ContentTypeJSON.String(); got != want {
		t.Fatalf("rec.header.Get(\"Content-Type\") = %v, want %v", got, want)
	}
	if got, want := rec.header.Get("X-SoftServe-Event"), EventBranchTagCreate.String(); got != want {
		t.Fatalf("rec.header.Get(\"X-SoftServe-Event\") = %v, want %v", got, want)
	}
	if got, want := rec.header.Get("User-Agent"), "SoftServe/"+testVersion; got != want {
		t.Fatalf("rec.header.Get(\"User-Agent\") = %v, want %v", got, want)
	}
	// The delivery ID is a random (version 4) UUID: a version 1 UUID would
	// embed the server's MAC address and clock in every request.
	delID, err := uuid.Parse(rec.header.Get("X-SoftServe-Delivery"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := delID.Version(), uuid.Version(4); got != want {
		t.Fatalf("delID.Version() = %v, want %v", got, want)
	}
	if rec.header.Get("X-SoftServe-Signature") != "" {
		t.Fatal("not true: rec.header.Get(\"X-SoftServe-Signature\") == \"\"")
	}

	var got BranchTagEvent
	if err := json.Unmarshal([]byte(rec.body), &got); err != nil {
		t.Fatal(err)
	}
	if got := got.Ref; got != "refs/heads/main" {
		t.Fatalf("got.Ref = %v, want %v", got, "refs/heads/main")
	}
	if got := got.Sender.Username; got != "alice" {
		t.Fatalf("got.Sender.Username = %v, want %v", got, "alice")
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].ResponseStatus; got != http.StatusOK {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, http.StatusOK)
	}
	if got := ds[0].ResponseBody; got != `{"ok":true}` {
		t.Fatalf("ds[0].ResponseBody = %v, want %v", got, `{"ok":true}`)
	}
	if got := ds[0].RequestURL; got != srv.URL {
		t.Fatalf("ds[0].RequestURL = %v, want %v", got, srv.URL)
	}
	if got := ds[0].RequestMethod; got != http.MethodPost {
		t.Fatalf("ds[0].RequestMethod = %v, want %v", got, http.MethodPost)
	}
	if got := ds[0].RequestError.String; got != "" {
		t.Fatalf("ds[0].RequestError.String = %v, want %v", got, "")
	}
	if got, want := ds[0].Event, int(EventBranchTagCreate); got != want {
		t.Fatalf("ds[0].Event = %v, want %v", got, want)
	}
	if !strings.Contains(ds[0].RequestHeaders, "X-Softserve-Event: branch_tag_create") {
		t.Fatal("not true: strings.Contains(ds[0].RequestHeaders, \"X-Softserve-Event: branch_tag_create\")")
	}
	if !strings.Contains(ds[0].ResponseHeaders, "Content-Type") {
		t.Fatal("not true: strings.Contains(ds[0].ResponseHeaders, \"Content-Type\")")
	}
}

func TestSendWebhookFormDelivered(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusNoContent, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeForm, EventBranchTagCreate)

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}

	if got, want := rec.header.Get("Content-Type"), ContentTypeForm.String(); got != want {
		t.Fatalf("rec.header.Get(\"Content-Type\") = %v, want %v", got, want)
	}
	form, err := url.ParseQuery(rec.body)
	if err != nil {
		t.Fatal(err)
	}
	if got := form.Get("ref"); got != "refs/heads/main" {
		t.Fatalf("form.Get(\"ref\") = %v, want %v", got, "refs/heads/main")
	}
	if got := form.Get("created"); got != "true" {
		t.Fatalf("form.Get(\"created\") = %v, want %v", got, "true")
	}
	if got := form.Get("sender[username]"); got != "alice" {
		t.Fatalf("form.Get(\"sender[username]\") = %v, want %v", got, "alice")
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].ResponseStatus; got != http.StatusNoContent {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, http.StatusNoContent)
	}
}

// A form-encoded push event gives each commit field its own key, in the same
// bracket notation as the other nested fields, rather than a Go struct dump.
func TestSendWebhookFormPushCommits(t *testing.T) {
	t.Parallel()
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
	if err := e.s.sendWebhook(e.ctx, w, EventPush, payload); err != nil {
		t.Fatal(err)
	}

	form, err := url.ParseQuery(rec.body)
	if err != nil {
		t.Fatal(err)
	}
	if got := form["commits"]; got != nil {
		t.Fatalf("form[\"commits\"] = %v, want nil", got)
	} // no struct dump under the bare key
	if got := form.Get("ref"); got != "refs/heads/main" {
		t.Fatalf("form.Get(\"ref\") = %v, want %v", got, "refs/heads/main")
	}
	if got := form.Get("sender[username]"); got != "alice" {
		t.Fatalf("form.Get(\"sender[username]\") = %v, want %v", got, "alice")
	}
	if got := form.Get("commits[0][id]"); got != "1111111111111111111111111111111111111111" {
		t.Fatalf("form.Get(\"commits[0][id]\") = %v, want %v", got, "1111111111111111111111111111111111111111")
	}
	if got := form.Get("commits[0][message]"); got != "first&x=1\n" {
		t.Fatalf("form.Get(\"commits[0][message]\") = %v, want %v", got, "first&x=1\n")
	}
	if got := form.Get("commits[0][title]"); got != "first&x=1" {
		t.Fatalf("form.Get(\"commits[0][title]\") = %v, want %v", got, "first&x=1")
	}
	if got := form.Get("commits[0][author][name]"); got != "Alice" {
		t.Fatalf("form.Get(\"commits[0][author][name]\") = %v, want %v", got, "Alice")
	}
	if got := form.Get("commits[0][author][email]"); got != "alice@example.com" {
		t.Fatalf("form.Get(\"commits[0][author][email]\") = %v, want %v", got, "alice@example.com")
	}
	if got := form.Get("commits[0][author][date]"); got != "2026-01-02T03:04:05Z" {
		t.Fatalf("form.Get(\"commits[0][author][date]\") = %v, want %v", got, "2026-01-02T03:04:05Z")
	}
	if got := form.Get("commits[0][committer][name]"); got != "Bob" {
		t.Fatalf("form.Get(\"commits[0][committer][name]\") = %v, want %v", got, "Bob")
	}
	if got := form.Get("commits[0][timestamp]"); got != "2026-01-02T03:04:05Z" {
		t.Fatalf("form.Get(\"commits[0][timestamp]\") = %v, want %v", got, "2026-01-02T03:04:05Z")
	}
	if got := form.Get("commits[1][id]"); got != "2222222222222222222222222222222222222222" {
		t.Fatalf("form.Get(\"commits[1][id]\") = %v, want %v", got, "2222222222222222222222222222222222222222")
	}
	if got := form.Get("commits[1][author][name]"); got != "Carol" {
		t.Fatalf("form.Get(\"commits[1][author][name]\") = %v, want %v", got, "Carol")
	}
}

func TestSendWebhookInvalidContentTypeRejected(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)
	w.ContentType = 99

	err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload())
	if !errors.Is(err, ErrInvalidContentType) {
		t.Fatal("not true: errors.Is(err, ErrInvalidContentType)")
	}
	if got := rec.calls; got != 0 {
		t.Fatalf("rec.calls = %v, want %v", got, 0)
	}
	if got := len(e.deliveries(t, w.ID)); got != 0 {
		t.Fatalf("len(e.deliveries(t, w.ID)) = %v, want %v", got, 0)
	}
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
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	// Channels cannot be encoded as JSON.
	err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, chanPayload(make(chan int)))
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if got := rec.calls; got != 0 {
		t.Fatalf("rec.calls = %v, want %v", got, 0)
	}
	if got := len(e.deliveries(t, w.ID)); got != 0 {
		t.Fatalf("len(e.deliveries(t, w.ID)) = %v, want %v", got, 0)
	}
}

func TestSendWebhookFormEncodeErrorReturned(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeForm, EventBranchTagCreate)

	// query.Values only accepts structs (or maps), not a bare int.
	err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, intPayload(5))
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if got := rec.calls; got != 0 {
		t.Fatalf("rec.calls = %v, want %v", got, 0)
	}
	if got := len(e.deliveries(t, w.ID)); got != 0 {
		t.Fatalf("len(e.deliveries(t, w.ID)) = %v, want %v", got, 0)
	}
}

func TestSendWebhookSignatureMatchesBody(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "s3cret", ContentTypeJSON, EventBranchTagCreate)

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}

	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(rec.body))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := rec.header.Get("X-SoftServe-Signature"); got != want {
		t.Fatalf("rec.header.Get(\"X-SoftServe-Signature\") = %v, want %v", got, want)
	}
}

// The V2 signature covers the timestamp and delivery GUID as well as the
// body, so a receiver can refuse a replayed request.
func TestSendWebhookSignatureV2CoversTimestampAndDelivery(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	fixed := time.Unix(1731000000, 0)
	e.s.now = func() time.Time { return fixed }
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "s3cret", ContentTypeJSON, EventBranchTagCreate)

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}

	ts := rec.header.Get(HeaderTimestamp)
	if got := ts; got != "1731000000" {
		t.Fatalf("ts = %v, want %v", got, "1731000000")
	}
	guid := rec.header.Get(HeaderDelivery)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(ts + "." + guid + "." + rec.body))
	if got, want := rec.header.Get(HeaderSignatureV2), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("rec.header.Get(HeaderSignatureV2) = %v, want %v", got, want)
	}

	// The delivery GUID of a first delivery is the id of its record.
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].ID.String(); got != guid {
		t.Fatalf("ds[0].ID.String() = %v, want %v", got, guid)
	}
}

// Without a secret there is nothing to sign, but the timestamp is still sent.
func TestSendWebhookTimestampWithoutSecret(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}

	if rec.header.Get(HeaderTimestamp) == "" {
		t.Fatal("not true: rec.header.Get(HeaderTimestamp) != \"\"")
	}
	if got := rec.header.Get(HeaderSignatureV2); got != "" {
		t.Fatalf("rec.header.Get(HeaderSignatureV2) = %v, want %v", got, "")
	}
}

// A redelivery sends the GUID it is given, under a record of its own, and
// signs it with a fresh timestamp.
func TestRedeliverWebhookKeepsGUID(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "s3cret", ContentTypeJSON, EventBranchTagCreate)
	guid := uuid.New()
	fixed := time.Unix(1731000500, 0)
	e.s.now = func() time.Time { return fixed }

	body, err := encode(ContentTypeJSON, samplePayload())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.Redeliver(e.ctx, w, EventBranchTagCreate, ContentTypeJSON, body, guid); err != nil {
		t.Fatal(err)
	}

	if got, want := rec.header.Get(HeaderDelivery), guid.String(); got != want {
		t.Fatalf("rec.header.Get(HeaderDelivery) = %v, want %v", got, want)
	}
	if got := rec.header.Get(HeaderTimestamp); got != "1731000500" {
		t.Fatalf("rec.header.Get(HeaderTimestamp) = %v, want %v", got, "1731000500")
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte("1731000500." + guid.String() + "." + rec.body))
	if got, want := rec.header.Get(HeaderSignatureV2), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("rec.header.Get(HeaderSignatureV2) = %v, want %v", got, want)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if ds[0].ID == guid {
		t.Fatal("not true: ds[0].ID != guid")
	}
	if !strings.Contains(ds[0].RequestHeaders, "X-Softserve-Delivery: "+guid.String()) {
		t.Fatal("not true: strings.Contains(ds[0].RequestHeaders, \"X-Softserve-Delivery: \"+guid.String())")
	}
}

func TestSendWebhookSignatureDependsOnSecret(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	a := e.newWebhook(t, srv.URL, "one", ContentTypeJSON, EventBranchTagCreate)
	if err := e.s.sendWebhook(e.ctx, a, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}
	sigOne := rec.header.Get("X-SoftServe-Signature")

	b := e.newWebhook(t, srv.URL+"/two", "two", ContentTypeJSON, EventBranchTagCreate)
	if err := e.s.sendWebhook(e.ctx, b, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}
	sigTwo := rec.header.Get("X-SoftServe-Signature")

	if sigOne == "" {
		t.Fatal("not true: sigOne != \"\"")
	}
	if sigOne == sigTwo {
		t.Fatal("not true: sigOne != sigTwo")
	}
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
	e := newTestEnv(t)
	e.useHTTPClient(t)
	w := e.newWebhook(t, closingServer(t), "", ContentTypeJSON, EventBranchTagCreate)

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if ds[0].RequestError.String == "" {
		t.Fatal("not true: ds[0].RequestError.String != \"\"")
	}
	if got := ds[0].ResponseStatus; got != 0 {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, 0)
	}
	if got := ds[0].ResponseBody; got != "" {
		t.Fatalf("ds[0].ResponseBody = %v, want %v", got, "")
	}
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
// turn while a delivery is recorded, for writeTurnHold. The record waits for
// its turn, so the delivery is still in the history: a record that gives up
// sooner loses the delivery on a loaded machine.
//
// The record's own timeout is raised for the test, so that no pause of the
// process can make the hold outlast it; the default is checked to be longer
// than the hold.
//
// It stays serial: waitingForWriteTurn looks at every goroutine of the
// process, and would see another test's record.
func TestSendWebhookRecordWaitsForWriteTurn(t *testing.T) {
	// writeTurnHold is longer than SQLite's busy timeout, 5 seconds.
	const writeTurnHold = 6 * time.Second
	e := newTestEnv(t)
	if e.dbx.DriverName() != "sqlite" {
		t.Skip("only SQLite databases have a write turn")
	}
	if e.s.recordTimeout <= writeTurnHold {
		t.Fatalf("the default record timeout is %v, want more than %v", e.s.recordTimeout, writeTurnHold)
	}
	e.s.recordTimeout = time.Hour
	e.useHTTPClient(t)
	srv, _ := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- e.dbx.TransactionContext(context.Background(), func(*db.Tx) error {
			close(held)
			// Hold the turn from when the record waits for it. Load can
			// only make the hold longer.
			deadline := time.Now().Add(time.Minute)
			for !waitingForWriteTurn() {
				if time.Now().After(deadline) {
					return errors.New("the record never waited for the write turn")
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(writeTurnHold)
			return nil
		})
	}()
	<-held

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := len(e.deliveries(t, w.ID)); got != 1 {
		t.Fatalf("len(e.deliveries(t, w.ID)) = %v, want %v", got, 1)
	}
}

func TestSendWebhookNonSuccessStatusRecorded(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, _ := newReceiver(t, http.StatusInternalServerError, "boom")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].RequestError.String; got != "" {
		t.Fatalf("ds[0].RequestError.String = %v, want %v", got, "")
	} // the request itself succeeded
	if got := ds[0].ResponseStatus; got != http.StatusInternalServerError {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, http.StatusInternalServerError)
	}
	if got := ds[0].ResponseBody; got != "boom" {
		t.Fatalf("ds[0].ResponseBody = %v, want %v", got, "boom")
	}
}

func TestSendWebhookInvalidURLRecorded(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	w := e.newWebhook(t, "http://[::1", "", ContentTypeJSON, EventBranchTagCreate)

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if ds[0].RequestError.String == "" {
		t.Fatal("not true: ds[0].RequestError.String != \"\"")
	}
	if got := ds[0].ResponseStatus; got != 0 {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, 0)
	}
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

// A body-read error does not return before the delivery is recorded: the
// delivery must be recorded and the read error kept.
func TestSendWebhookBodyReadErrorStillRecordsDelivery(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	w := e.newWebhook(t, truncatingServer(t), "", ContentTypeJSON, EventBranchTagCreate)

	err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload())
	if err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].ResponseStatus; got != http.StatusOK {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, http.StatusOK)
	}
	if !strings.Contains(ds[0].RequestError.String, "unexpected EOF") {
		t.Fatal("not true: strings.Contains(ds[0].RequestError.String, \"unexpected EOF\")")
	}
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
	e := newTestEnv(t)
	e.useHTTPClient(t)

	if err := e.s.sendEvent(e.ctx, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}
}

func TestSendEventDeliversToSubscribedWebhooksOnly(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")

	push := e.newWebhook(t, srv.URL+"/push", "", ContentTypeJSON, EventPush)
	other := e.newWebhook(t, srv.URL+"/collab", "", ContentTypeJSON, EventCollaborator)

	if err := e.s.sendEvent(e.ctx, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	if got := rec.calls; got != 1 {
		t.Fatalf("rec.calls = %v, want %v", got, 1)
	}
	if got := len(e.deliveries(t, push.ID)); got != 1 {
		t.Fatalf("len(e.deliveries(t, push.ID)) = %v, want %v", got, 1)
	}
	if got := len(e.deliveries(t, other.ID)); got != 0 {
		t.Fatalf("len(e.deliveries(t, other.ID)) = %v, want %v", got, 0)
	}
}

func TestSendEventDeliversToEveryWebhook(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")

	var ws []models.Webhook
	for i := 0; i < 3; i++ {
		ws = append(ws, e.newWebhook(t, fmt.Sprintf("%s/%d", srv.URL, i), "", ContentTypeJSON, EventPush))
	}

	if err := e.s.sendEvent(e.ctx, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	if got := rec.calls; got != 3 {
		t.Fatalf("rec.calls = %v, want %v", got, 3)
	}
	for _, w := range ws {
		if got := len(e.deliveries(t, w.ID)); got != 1 {
			t.Fatalf("len(e.deliveries(t, w.ID)) = %v, want %v", got, 1)
		}
	}
}

// Sending an event does not stop at the first failed webhook: every
// subscribed webhook must be attempted, and the failure must still be
// reported.
func TestSendEventFailingWebhookDoesNotBlockOthers(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")

	bad := e.newWebhook(t, srv.URL+"/bad", "", ContentTypeJSON, EventPush)
	// Stored with an unknown content type: SendWebhook fails for it.
	if err := e.ds.UpdateWebhookByID(e.ctx, e.dbx, models.Webhook{ID: bad.ID, RepoID: e.repo.ID, URL: bad.URL, ContentType: 99, Active: true}); err != nil {
		t.Fatal(err)
	}
	good := e.newWebhook(t, srv.URL+"/good", "", ContentTypeJSON, EventPush)

	err := e.s.sendEvent(e.ctx, samplePayloadFor(e))
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if got := len(e.deliveries(t, good.ID)); got != 1 {
		t.Fatalf("len(e.deliveries(t, good.ID)) = %v, want %v", got, 1)
	}
	if got := rec.calls; got != 1 {
		t.Fatalf("rec.calls = %v, want %v", got, 1)
	}
}

func TestSendEventWebhookLookupErrorReturned(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	if err := e.dbx.Close(); err != nil {
		t.Fatal(err)
	}

	if e.s.sendEvent(e.ctx, samplePayloadFor(e)) == nil {
		t.Fatal("not true: e.s.sendEvent(e.ctx, samplePayloadFor(e)) != nil")
	}
}

func TestSendWebhookRecordErrorReturned(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, _ := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	if err := e.dbx.Close(); err != nil {
		t.Fatal(err)
	}

	if e.s.sendWebhook(e.ctx, w, EventPush, samplePayload()) == nil {
		t.Fatal("not true: e.s.sendWebhook(e.ctx, w, EventPush, samplePayload()) != nil")
	}
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
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Receiver-A", "alpha")
		w.Header().Set("X-Receiver-B", "beta")
		w.WriteHeader(http.StatusAccepted)
	}))
	w := e.newWebhook(t, srv.URL, "secret", ContentTypeJSON, EventBranchTagCreate)

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	for _, want := range []string{
		"Content-Type: application/json",
		"User-Agent: SoftServe/",
		"X-Softserve-Event: branch_tag_create",
		"X-Softserve-Delivery: ",
		"X-Softserve-Signature: sha256=",
	} {
		if !strings.Contains(ds[0].RequestHeaders, want) {
			t.Fatal("not true: strings.Contains(ds[0].RequestHeaders, want)")
		}
	}
	if !strings.Contains(ds[0].ResponseHeaders, "X-Receiver-A: alpha") {
		t.Fatal("not true: strings.Contains(ds[0].ResponseHeaders, \"X-Receiver-A: alpha\")")
	}
	if !strings.Contains(ds[0].ResponseHeaders, "X-Receiver-B: beta") {
		t.Fatal("not true: strings.Contains(ds[0].ResponseHeaders, \"X-Receiver-B: beta\")")
	}
	if got := ds[0].ResponseStatus; got != http.StatusAccepted {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, http.StatusAccepted)
	}
}

// A stored header block is sorted by key, so the same headers are always
// stored the same way, and keeps every value of a repeated header.
func TestFormatHeadersKeepsEveryValue(t *testing.T) {
	t.Parallel()
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
		if got := formatHeaders(h); got != want {
			t.Fatalf("formatHeaders(h) = %v, want %v", got, want)
		}
	}
	if got := formatHeaders(nil); got != "" {
		t.Fatalf("formatHeaders(nil) = %v, want %v", got, "")
	}
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
	e := newTestEnv(t)
	e.useHTTPClient(t)
	e.s.sendTimeout = 2 * time.Second
	srv, rec := newReceiver(t, http.StatusOK, "")
	slow := e.newWebhook(t, hangingServer(t), "", ContentTypeJSON, EventPush)
	fast := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)

	if err := e.s.Deliver(e.ctx, []models.Webhook{slow, fast}, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	slowDs := e.deliveries(t, slow.ID)
	if got := len(slowDs); got != 1 {
		t.Fatalf("len(slowDs) = %v, want %v", got, 1)
	}
	if !strings.Contains(slowDs[0].RequestError.String, "deadline exceeded") {
		t.Fatal("not true: strings.Contains(slowDs[0].RequestError.String, \"deadline exceeded\")")
	}
	fastDs := e.deliveries(t, fast.ID)
	if got := len(fastDs); got != 1 {
		t.Fatalf("len(fastDs) = %v, want %v", got, 1)
	}
	if got := fastDs[0].ResponseStatus; got != http.StatusOK {
		t.Fatalf("fastDs[0].ResponseStatus = %v, want %v", got, http.StatusOK)
	}
	if got := rec.calls; got != 1 {
		t.Fatalf("rec.calls = %v, want %v", got, 1)
	}
}

// A delivery whose context is already done sends nothing, but it is still
// recorded: the re-read before the send fails on the done context, so the
// event is recorded as undelivered, with a context of its own.
func TestDeliverRecordsUndeliveredWhenContextDoneBeforeSend(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()

	if err := e.s.Deliver(ctx, []models.Webhook{w}, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got, want := ds[0].RequestError.String, errRereadFailed.Error(); got != want {
		t.Fatalf("ds[0].RequestError.String = %v, want %v", got, want)
	}
	if got := rec.calls; got != 0 {
		t.Fatalf("rec.calls = %v, want %v", got, 0)
	}
}

// A delivery whose context is cancelled while the request is in flight is
// still recorded, with the cancellation as its error: the row is written with
// a context of its own.
func TestDeliverRecordsAttemptWhenContextCancelledDuringSend(t *testing.T) {
	t.Parallel()
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

	if err := e.s.Deliver(ctx, []models.Webhook{w}, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if !strings.Contains(ds[0].RequestError.String, "context canceled") {
		t.Fatal("not true: strings.Contains(ds[0].RequestError.String, \"context canceled\")")
	}
	if got := ds[0].ResponseStatus; got != 0 {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, 0)
	}
}

// Unrecorded deliveries have no row to show a failed status, so a non-2xx
// response is returned as an error for the caller to log.
func TestDeliverUnrecordedReportsNonSuccessStatus(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	bad, _ := newReceiver(t, http.StatusInternalServerError, "boom")
	good, _ := newReceiver(t, http.StatusNoContent, "")
	badHook := e.newWebhook(t, bad.URL, "", ContentTypeJSON, EventPush)
	goodHook := e.newWebhook(t, good.URL, "", ContentTypeJSON, EventPush)

	err := e.s.DeliverUnrecorded(e.ctx, []models.Webhook{badHook}, samplePayloadFor(e))
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Fatal("not true: strings.Contains(err.Error(), \"status 500\")")
	}
	if err := e.s.DeliverUnrecorded(e.ctx, []models.Webhook{goodHook}, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	if got := len(e.deliveries(t, badHook.ID)); got != 0 {
		t.Fatalf("len(e.deliveries(t, badHook.ID)) = %v, want %v", got, 0)
	}
	if got := len(e.deliveries(t, goodHook.ID)); got != 0 {
		t.Fatalf("len(e.deliveries(t, goodHook.ID)) = %v, want %v", got, 0)
	}
}

// RecordUndelivered stores the request it would have sent, with the reason as
// its error, and sends nothing.
func TestRecordUndeliveredStoresRequestWithoutSending(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "s3cret", ContentTypeJSON, EventPush)
	payload := samplePayloadFor(e)

	if err := e.s.RecordUndelivered(e.ctx, []models.Webhook{w}, payload, errors.New("queue full")); err != nil {
		t.Fatal(err)
	}

	if got := rec.calls; got != 0 {
		t.Fatalf("rec.calls = %v, want %v", got, 0)
	}
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].RequestError.String; got != "queue full" {
		t.Fatalf("ds[0].RequestError.String = %v, want %v", got, "queue full")
	}
	if got := ds[0].ResponseStatus; got != 0 {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, 0)
	}
	if got, want := ds[0].Event, int(EventPush); got != want {
		t.Fatalf("ds[0].Event = %v, want %v", got, want)
	}
	want, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(ds[0].RequestBody), string(want); got != want {
		t.Fatalf("strings.TrimSpace(ds[0].RequestBody) = %v, want %v", got, want)
	}
	if !strings.Contains(ds[0].RequestHeaders, "X-Softserve-Signature: sha256=") {
		t.Fatal("not true: strings.Contains(ds[0].RequestHeaders, \"X-Softserve-Signature: sha256=\")")
	}
}

// Undelivered events cost no network time, so they are the quickest way to
// grow the deliveries table. Every record prunes, so a webhook never holds
// more than the limit, and pruning one webhook leaves the others' deliveries
// alone.
func TestRecordedDeliveriesAreBoundedPerWebhook(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.s.maxDeliveries = 5
	a := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)
	b := e.newWebhook(t, "https://93.184.216.34/b", "", ContentTypeJSON, EventPush)
	payload := samplePayloadFor(e)

	var recorded []uuid.UUID
	for i := range 12 {
		if err := e.s.RecordUndelivered(e.ctx, []models.Webhook{a}, payload, errors.New("queue full")); err != nil {
			t.Fatal(err)
		}
		ds := e.deliveries(t, a.ID)
		if got, want := len(ds), min(i+1, 5); got != want {
			t.Fatalf("len(ds) = %v, want %v", got, want)
		}
		// The delivery just recorded is always kept.
		recorded = append(recorded, ds[len(ds)-1].ID)
	}
	for range 3 {
		if err := e.s.RecordUndelivered(e.ctx, []models.Webhook{b}, payload, errors.New("queue full")); err != nil {
			t.Fatal(err)
		}
	}

	// The five kept are the five most recent, oldest first.
	var kept []uuid.UUID
	for _, d := range e.deliveries(t, a.ID) {
		kept = append(kept, d.ID)
	}
	if got, want := kept, recorded[len(recorded)-5:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("kept = %v, want %v", got, want)
	}
	if got := len(e.deliveries(t, b.ID)); got != 3 {
		t.Fatalf("len(e.deliveries(t, b.ID)) = %v, want %v", got, 3)
	}
}

// Production delivery IDs are time-based (version 1) UUIDs, whose text does
// not sort by time, so retention must not depend on them. Deliveries with
// IDs in descending order are still pruned oldest first.
func TestRecordedDeliveriesPruneIgnoresIDOrder(t *testing.T) {
	t.Parallel()
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
		if err != nil {
			t.Fatal(err)
		}
		req.id = id
		if err := e.s.recordDelivery(e.ctx, w, EventPush, req, nil, 200, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 2 {
		t.Fatalf("len(ds) = %v, want %v", got, 2)
	}
	if got, want := ds[0].ID, ids[1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("ds[0].ID = %v, want %v", got, want)
	}
	if got, want := ds[1].ID, ids[2]; !reflect.DeepEqual(got, want) {
		t.Fatalf("ds[1].ID = %v, want %v", got, want)
	}
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
	e := newTestEnv(t)
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
		if err != nil {
			t.Fatal(err)
		}
	}
	var prunes atomic.Int32
	e.s.store = countingPruneStore{e.ds, &prunes}

	req, err := e.newRequest(w, EventPush, samplePayloadFor(e))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.recordDelivery(e.ctx, w, EventPush, req, nil, 200, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := len(e.deliveries(t, w.ID)); got != 0 {
		t.Fatalf("len(e.deliveries(t, w.ID)) = %v, want %v", got, 0)
	} // nothing recorded
	if got, want := prunes.Load(), int32(0); got != want {
		t.Fatalf("prunes.Load() = %v, want %v", got, want)
	} // and nothing pruned
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
	e := newTestEnv(t)
	w := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)
	var logs bytes.Buffer
	e.s.store = failingPruneStore{e.ds}
	e.s.logger = log.New(&logs)

	req, err := e.newRequest(w, EventPush, samplePayloadFor(e))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.recordDelivery(e.ctx, w, EventPush, req, nil, 200, "", ""); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got, want := ds[0].ID, req.id; !reflect.DeepEqual(got, want) {
		t.Fatalf("ds[0].ID = %v, want %v", got, want)
	}
	if !strings.Contains(logs.String(), "pruning webhook deliveries") {
		t.Fatal("not true: strings.Contains(logs.String(), \"pruning webhook deliveries\")")
	}
	if !strings.Contains(logs.String(), "prune failed") {
		t.Fatal("not true: strings.Contains(logs.String(), \"prune failed\")")
	}
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
	e := newTestEnv(t)
	w := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)
	var creates atomic.Int32
	var logs bytes.Buffer
	e.s.store = hangingPruneStore{e.ds, &creates}
	e.s.logger = log.New(&logs)

	req, err := e.newRequest(w, EventPush, samplePayloadFor(e))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.recordDelivery(e.ctx, w, EventPush, req, nil, 200, "", ""); err != nil {
		t.Fatal(err)
	}
	if got, want := creates.Load(), int32(1); got != want {
		t.Fatalf("creates.Load() = %v, want %v", got, want)
	}
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got, want := ds[0].ID, req.id; !reflect.DeepEqual(got, want) {
		t.Fatalf("ds[0].ID = %v, want %v", got, want)
	}
	if !strings.Contains(logs.String(), "pruning webhook deliveries") {
		t.Fatal("not true: strings.Contains(logs.String(), \"pruning webhook deliveries\")")
	}
	if !strings.Contains(logs.String(), context.DeadlineExceeded.Error()) {
		t.Fatal("not true: strings.Contains(logs.String(), context.DeadlineExceeded.Error())")
	}
}

// Deliver re-reads each webhook before sending, so an event that waited while
// the webhook was changed goes to its current URL, signed with its current
// secret, and not to the old receiver.
func TestDeliverUsesCurrentWebhook(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	oldSrv, oldRec := newReceiver(t, http.StatusOK, "")
	newSrv, newRec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, oldSrv.URL, "old-secret", ContentTypeJSON, EventPush)
	if err := e.ds.UpdateWebhookByID(e.ctx, e.dbx, models.Webhook{ID: w.ID, RepoID: e.repo.ID, URL: newSrv.URL, Secret: "new-secret", ContentType: int(ContentTypeJSON), Active: true}); err != nil {
		t.Fatal(err)
	}

	if err := e.s.Deliver(e.ctx, []models.Webhook{w}, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	if got := oldRec.calls; got != 0 {
		t.Fatalf("oldRec.calls = %v, want %v", got, 0)
	}
	if got := newRec.calls; got != 1 {
		t.Fatalf("newRec.calls = %v, want %v", got, 1)
	}
	mac := hmac.New(sha256.New, []byte("new-secret"))
	mac.Write([]byte(newRec.body))
	if got, want := newRec.header.Get("X-SoftServe-Signature"), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("newRec.header.Get(\"X-SoftServe-Signature\") = %v, want %v", got, want)
	}
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].RequestURL; got != newSrv.URL {
		t.Fatalf("ds[0].RequestURL = %v, want %v", got, newSrv.URL)
	}
}

// A webhook deleted, or no longer subscribed to the event, after the event was
// read is not sent to.
func TestDeliverSkipsRemovedOrUnsubscribedWebhook(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	deleted := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	unsubscribed := e.newWebhook(t, srv.URL+"/?other", "", ContentTypeJSON, EventPush)
	if err := e.ds.DeleteWebhookForRepoByID(e.ctx, e.dbx, e.repo.ID, deleted.ID); err != nil {
		t.Fatal(err)
	}
	evs, err := e.ds.GetWebhookEventsByWebhookID(e.ctx, e.dbx, unsubscribed.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(evs))
	for _, ev := range evs {
		ids = append(ids, ev.ID)
	}
	if err := e.ds.DeleteWebhookEventsByID(e.ctx, e.dbx, unsubscribed.ID, ids); err != nil {
		t.Fatal(err)
	}
	if err := e.ds.CreateWebhookEvents(e.ctx, e.dbx, unsubscribed.ID, []int{int(EventBranchTagCreate)}); err != nil {
		t.Fatal(err)
	}

	if err := e.s.Deliver(e.ctx, []models.Webhook{deleted, unsubscribed}, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	if got := rec.calls; got != 0 {
		t.Fatalf("rec.calls = %v, want %v", got, 0)
	}
	if got := len(e.deliveries(t, unsubscribed.ID)); got != 0 {
		t.Fatalf("len(e.deliveries(t, unsubscribed.ID)) = %v, want %v", got, 0)
	}
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

	if err := e.s.Deliver(ctx, hooks, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	if got := rec.calls; got != 3 {
		t.Fatalf("rec.calls = %v, want %v", got, 3)
	}
	if got, want := cs.listCalls.Load(), int32(0); got != want {
		t.Fatalf("cs.listCalls.Load() = %v, want %v", got, want)
	}
	if got, want := cs.hookCalls.Load(), int32(3); got != want {
		t.Fatalf("cs.hookCalls.Load() = %v, want %v", got, want)
	}
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

	if err := e.s.Deliver(ctx, []models.Webhook{w}, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	if got := rec.calls; got != 0 {
		t.Fatalf("rec.calls = %v, want %v", got, 0)
	}
	if got, want := calls.Load(), int32(2); got != want {
		t.Fatalf("calls.Load() = %v, want %v", got, want)
	}
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got, want := ds[0].RequestError.String, errRereadFailed.Error(); got != want {
		t.Fatalf("ds[0].RequestError.String = %v, want %v", got, want)
	}
	if got := ds[0].ResponseStatus; got != 0 {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, 0)
	}
	if got, want := ds[0].Event, int(EventPush); got != want {
		t.Fatalf("ds[0].Event = %v, want %v", got, want)
	}
	out := logs.String()
	if !strings.Contains(out, "WARN") {
		t.Fatal("not true: strings.Contains(out, \"WARN\")")
	}
	if !strings.Contains(out, "lookup failed: dial tcp 10.0.0.5:5432") {
		t.Fatal("not true: strings.Contains(out, \"lookup failed: dial tcp 10.0.0.5:5432\")")
	}
	if !strings.Contains(out, fmt.Sprintf("webhook_id=%d", w.ID)) {
		t.Fatalf("not true: strings.Contains(out, fmt.Sprintf(\"webhook_id=%%d\", w.ID))")
	}
	if !strings.Contains(out, fmt.Sprintf("repo_id=%d", e.repo.ID)) {
		t.Fatalf("not true: strings.Contains(out, fmt.Sprintf(\"repo_id=%%d\", e.repo.ID))")
	}
	if !strings.Contains(out, "event=push") {
		t.Fatal("not true: strings.Contains(out, \"event=push\")")
	}
	if strings.Contains(out, srv.URL) {
		t.Fatal("not true: !strings.Contains(out, srv.URL)")
	}
}

// A re-read that fails once is retried, and the webhook is sent to when the
// retry works.
func TestDeliverRetriesFailedReread(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	e.useRereadTiming(defaultRereadTimeout, time.Millisecond)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	calls := new(atomic.Int32)
	e.s.store = failingLookupStore{Store: e.ds, failures: 1, calls: calls}
	ctx := e.ctx

	if err := e.s.Deliver(ctx, []models.Webhook{w}, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	if got, want := calls.Load(), int32(2); got != want {
		t.Fatalf("calls.Load() = %v, want %v", got, want)
	}
	if got := rec.calls; got != 1 {
		t.Fatalf("rec.calls = %v, want %v", got, 1)
	}
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].RequestError.String; got != "" {
		t.Fatalf("ds[0].RequestError.String = %v, want %v", got, "")
	}
	if got := ds[0].ResponseStatus; got != http.StatusOK {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, http.StatusOK)
	}
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
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("Deliver did not return while the re-read hung")
	}

	if got, want := calls.Load(), int32(2); got != want {
		t.Fatalf("calls.Load() = %v, want %v", got, want)
	}
	if got := rec.calls; got != 0 {
		t.Fatalf("rec.calls = %v, want %v", got, 0)
	}
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got, want := ds[0].RequestError.String, errRereadFailed.Error(); got != want {
		t.Fatalf("ds[0].RequestError.String = %v, want %v", got, want)
	}
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
	e := newTestEnv(t)
	e.useHTTPClient(t)
	w := e.newWebhook(t, endlessServer(t), "", ContentTypeJSON, EventPush)

	if err := e.s.sendWebhook(e.ctx, w, EventPush, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].RequestError.String; got != "" {
		t.Fatalf("ds[0].RequestError.String = %v, want %v", got, "")
	}
	if got := ds[0].ResponseStatus; got != http.StatusOK {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, http.StatusOK)
	}
	if got, want := ds[0].ResponseBody, strings.Repeat("x", maxResponseBody)+truncatedMarker; got != want {
		t.Fatalf("ds[0].ResponseBody = %v, want %v", got, want)
	}
}

// A body of exactly maxResponseBody bytes is stored whole and not marked.
func TestSendWebhookResponseBodyAtLimitIsKept(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	body := strings.Repeat("y", maxResponseBody)
	srv, _ := newReceiver(t, http.StatusOK, body)
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)

	if err := e.s.sendWebhook(e.ctx, w, EventPush, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].ResponseBody; got != body {
		t.Fatalf("ds[0].ResponseBody = %v, want %v", got, body)
	}
}

// A read that fails before the limit keeps what was read and reports the
// error. A failure after the limit is never reached.
func TestReadResponseBodyKeepsPartialReadOnError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")

	got, err := readResponseBody(io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(boom)))
	if !errors.Is(err, boom) {
		t.Fatal("not true: errors.Is(err, boom)")
	}
	if got := got; got != "partial" {
		t.Fatalf("got = %v, want %v", got, "partial")
	}

	long := strings.Repeat("z", maxResponseBody+10)
	got, err = readResponseBody(io.MultiReader(strings.NewReader(long), iotest.ErrReader(boom)))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := got, long[:maxResponseBody]+truncatedMarker; got != want {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

// formatHeaders lists headers sorted by key with every value, skips
// keys without a value, and stops at maxStoredHeaders with a marker.
func TestFormatHeaders(t *testing.T) {
	t.Parallel()

	if got := formatHeaders(http.Header{
		"X-B":     {"two", "three"},
		"X-A":     {"one"},
		"X-Empty": {},
		"X-C":     {""},
	}); got != "X-A: one\nX-B: two\nX-B: three\nX-C: \n" {
		t.Fatalf("formatHeaders of four headers = %q, want %q", got, "X-A: one\nX-B: two\nX-B: three\nX-C: \n")
	}
	if got := formatHeaders(nil); got != "" {
		t.Fatalf("formatHeaders(nil) = %v, want %v", got, "")
	}

	big := http.Header{}
	value := strings.Repeat("v", 1000)
	for i := range 200 {
		big.Set(fmt.Sprintf("X-Pad-%03d", i), value)
	}
	got := formatHeaders(big)
	if !strings.HasSuffix(got, truncatedHeadersMarker) {
		t.Fatal("not true: strings.HasSuffix(got, truncatedHeadersMarker)")
	}
	if len(got) > maxStoredHeaders+len(truncatedHeadersMarker) {
		t.Fatal("not true: len(got) <= maxStoredHeaders+len(truncatedHeadersMarker)")
	}
	if !strings.HasPrefix(got, "X-Pad-000: "+value+"\n") {
		t.Fatal("not true: strings.HasPrefix(got, \"X-Pad-000: \"+value+\"\\n\")")
	}

	// Invalid bytes grow to U+FFFD (3 bytes) when made storable; the cap
	// holds for the stored text, not the raw bytes.
	invalid := http.Header{}
	bad := strings.Repeat("\xffa", 500)
	for i := range 200 {
		invalid.Set(fmt.Sprintf("X-Bad-%03d", i), bad)
	}
	got = formatHeaders(invalid)
	if !strings.HasSuffix(got, truncatedHeadersMarker) {
		t.Fatal("not true: strings.HasSuffix(got, truncatedHeadersMarker)")
	}
	if len(got) > maxStoredHeaders+len(truncatedHeadersMarker) {
		t.Fatal("not true: len(got) <= maxStoredHeaders+len(truncatedHeadersMarker)")
	}
	if got, want := storableText(got), got; got != want {
		t.Fatalf("storableText(got) = %v, want %v", got, want)
	}
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
	e := newTestEnv(t)
	e.useHTTPClient(t)
	value := strings.Repeat("\xffa", 500)
	const headers = 40
	if headers*len("X-Pad-000: \r\n"+value) >= ssrf.MaxResponseHeaderBytes-1024 {
		t.Fatal("not true: headers*len(\"X-Pad-000: \\r\\n\"+value) < ssrf.MaxResponseHeaderBytes-1024")
	}
	if headers*len(storableText(value)) <= maxStoredHeaders {
		t.Fatal("not true: headers*len(storableText(value)) > maxStoredHeaders")
	}
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := range headers {
			w.Header().Set(fmt.Sprintf("X-Pad-%03d", i), value)
		}
		w.WriteHeader(http.StatusOK)
	}))
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].ResponseStatus; got != http.StatusOK {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, http.StatusOK)
	}
	if len(ds[0].ResponseHeaders) > maxStoredHeaders+len(truncatedHeadersMarker) {
		t.Fatal("not true: len(ds[0].ResponseHeaders) <= maxStoredHeaders+len(truncatedHeadersMarker)")
	}
	if !strings.HasSuffix(ds[0].ResponseHeaders, truncatedHeadersMarker) {
		t.Fatal("not true: strings.HasSuffix(ds[0].ResponseHeaders, truncatedHeadersMarker)")
	}
}

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
			t.Parallel()
			if got := redactURL(c.in); got != c.want {
				t.Fatalf("redactURL(c.in) = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRedactURLErrorKeepsChain(t *testing.T) {
	t.Parallel()
	in := &url.Error{Op: "Post", URL: "https://hooks.slack.com/services/T000/B000/XXXX", Err: context.DeadlineExceeded}

	err := ssrf.RedactURLError(in, redactURL)
	if strings.Contains(err.Error(), "XXXX") {
		t.Fatal("not true: !strings.Contains(err.Error(), \"XXXX\")")
	}
	if strings.Contains(err.Error(), "services") {
		t.Fatal("not true: !strings.Contains(err.Error(), \"services\")")
	}
	if !strings.Contains(err.Error(), "hooks.slack.com") {
		t.Fatal("not true: strings.Contains(err.Error(), \"hooks.slack.com\")")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("not true: errors.Is(err, context.DeadlineExceeded)")
	}
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		t.Fatal("not true: errors.As(err, &uerr)")
	}
	if !uerr.Timeout() {
		t.Fatal("not true: uerr.Timeout()")
	}
	if got := in.URL; got != "https://hooks.slack.com/services/T000/B000/XXXX" {
		t.Fatalf("in.URL = %v, want %v", got, "https://hooks.slack.com/services/T000/B000/XXXX")
	} // the original is not changed

	// A parse error's own message can quote part of the URL.
	_, perr := url.Parse("https://example.com:SECRETPORT/x") //nolint:staticcheck // invalid on purpose
	if perr == nil {
		t.Fatal("not true: perr != nil")
	}
	if !strings.Contains(perr.Error(), "SECRETPORT") {
		t.Fatal("not true: strings.Contains(perr.Error(), \"SECRETPORT\")")
	}
	rerr := ssrf.RedactURLError(perr, redactURL)
	if strings.Contains(rerr.Error(), "SECRETPORT") {
		t.Fatal("not true: !strings.Contains(rerr.Error(), \"SECRETPORT\")")
	}
	if !errors.Is(rerr, ssrf.ErrInvalidURL) {
		t.Fatal("not true: errors.Is(rerr, ssrf.ErrInvalidURL)")
	}
	if !errors.Is(rerr, ErrInvalidURL) {
		t.Fatal("not true: errors.Is(rerr, ErrInvalidURL)")
	} // the same error, through ssrf
	_, perr = url.Parse("https://example.com/%zzSECRET") //nolint:staticcheck // invalid on purpose
	if perr == nil {
		t.Fatal("not true: perr != nil")
	}
	if strings.Contains(ssrf.RedactURLError(perr, redactURL).Error(), "zz") {
		t.Fatal("not true: !strings.Contains(ssrf.RedactURLError(perr, redactURL).Error(), \"zz\")")
	}

	other := errors.New("other")
	if got, want := ssrf.RedactURLError(other, redactURL), other; !reflect.DeepEqual(got, want) {
		t.Fatalf("ssrf.RedactURLError(other, redactURL) = %v, want %v", got, want)
	}
	if got := ssrf.RedactURLError(nil, redactURL); got != nil {
		t.Fatalf("ssrf.RedactURLError(nil, redactURL) = %v, want nil", got)
	}
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
	e := newTestEnv(t)
	e.useHTTPClient(t)
	base := strings.Replace(closingServer(t), "http://", "http://USERSECRET@", 1)
	w := e.newWebhook(t, base+secretPath+secretQuery, "", ContentTypeJSON, EventPush)

	err := e.s.DeliverUnrecorded(e.ctx, []models.Webhook{w}, samplePayloadFor(e))
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	assertNoSecret(t, err.Error())
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatal("not true: strings.Contains(err.Error(), \"[redacted]\")")
	}
	// With the URL redacted, the error names the webhook instead.
	if !strings.Contains(err.Error(), fmt.Sprintf("webhook %d: ", w.ID)) {
		t.Fatalf("not true: strings.Contains(err.Error(), fmt.Sprintf(\"webhook %%d: \", w.ID))")
	}
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		t.Fatal("not true: errors.As(err, &uerr)")
	}
}

// A send that runs out of time still reports a deadline error through the
// redacted error, and the recorded error text is redacted too.
func TestDeliverTimeoutRedactsURLAndKeepsDeadline(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	e.s.sendTimeout = 200 * time.Millisecond
	w := e.newWebhook(t, hangingServer(t)+secretPath+secretQuery, "", ContentTypeJSON, EventPush)

	err := e.s.DeliverUnrecorded(e.ctx, []models.Webhook{w}, samplePayloadFor(e))
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	assertNoSecret(t, err.Error())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("not true: errors.Is(err, context.DeadlineExceeded)")
	}
	var nerr net.Error
	if !errors.As(err, &nerr) {
		t.Fatal("not true: errors.As(err, &nerr)")
	}
	if !nerr.Timeout() {
		t.Fatal("not true: nerr.Timeout()")
	}

	if err := e.s.Deliver(e.ctx, []models.Webhook{w}, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if !strings.Contains(ds[0].RequestError.String, "deadline exceeded") {
		t.Fatal("not true: strings.Contains(ds[0].RequestError.String, \"deadline exceeded\")")
	}
	assertNoSecret(t, ds[0].RequestError.String)
}

// A cancelled send keeps context.Canceled in the chain.
func TestDeliverUnrecordedCancelKeepsCanceled(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv, _ := newReceiver(t, 200, "")
	w := e.newWebhook(t, srv.URL+secretPath+secretQuery, "", ContentTypeJSON, EventPush)
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()

	err := e.s.DeliverUnrecorded(ctx, []models.Webhook{w}, samplePayloadFor(e))
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	assertNoSecret(t, err.Error())
	if !errors.Is(err, context.Canceled) {
		t.Fatal("not true: errors.Is(err, context.Canceled)")
	}
}

// A stored webhook URL that cannot be parsed fails in http.NewRequest. The
// parse error quotes part of the URL (here the port), so both the returned and
// the recorded error must say only "invalid URL" and carry no part of it.
func TestDeliverUnparsableURLRedactsURL(t *testing.T) {
	t.Parallel()
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
		if !strings.Contains(s, "invalid URL") {
			t.Fatal("not true: strings.Contains(s, \"invalid URL\")")
		}
	}

	err := e.s.DeliverUnrecorded(e.ctx, []models.Webhook{w}, samplePayloadFor(e))
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if !errors.Is(err, ssrf.ErrInvalidURL) {
		t.Fatal("not true: errors.Is(err, ssrf.ErrInvalidURL)")
	}
	if got, want := err.Error(), fmt.Sprintf("webhook %d: parse \"[redacted]\": invalid URL", w.ID); got != want {
		t.Fatalf("err.Error() = %v, want %v", got, want)
	}
	assertRedacted(err.Error())

	if err := e.s.Deliver(e.ctx, []models.Webhook{w}, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].ResponseStatus; got != 0 {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, 0)
	}
	assertRedacted(ds[0].RequestError.String)
}

// A body cut off at maxResponseBody is cut before a character the limit
// falls inside, so a UTF-8 body stays valid UTF-8.
func TestReadResponseBodyCutsOnRuneBoundary(t *testing.T) {
	t.Parallel()
	// "€" is three bytes and maxResponseBody is not a multiple of three, so
	// the limit falls inside one.
	if maxResponseBody%3 == 0 {
		t.Fatalf("not true: maxResponseBody%%3 != 0")
	}
	body := strings.Repeat("€", maxResponseBody/3+10)

	got, err := readResponseBody(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(got) {
		t.Fatal("not true: utf8.ValidString(got)")
	}
	if got, want := got, strings.Repeat("€", maxResponseBody/3)+truncatedMarker; got != want {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestStorableText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"plain", "plain"},
		{"caf\xe9", "caf�"},
		{"a\x00b", "ab"},
		{"\xe2\x82", "�"},
		{"ok €", "ok €"},
	} {
		got := storableText(tc.in)
		if got != tc.want {
			t.Errorf("storableText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A response with a Latin-1 header and a body holding a NUL and invalid UTF-8
// is still recorded, as text Postgres accepts.
func TestSendWebhookStoresInvalidUTF8ResponseAsText(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.useHTTPClient(t)
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Name", "caf\xe9")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("a\x00b\xffc"))
	}))
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)

	if err := e.s.sendWebhook(e.ctx, w, EventPush, samplePayloadFor(e)); err != nil {
		t.Fatal(err)
	}

	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	d := ds[0]
	if got := d.ResponseStatus; got != http.StatusOK {
		t.Fatalf("d.ResponseStatus = %v, want %v", got, http.StatusOK)
	}
	if got := d.ResponseBody; got != "ab�c" {
		t.Fatalf("d.ResponseBody = %v, want %v", got, "ab�c")
	}
	if !strings.Contains(d.ResponseHeaders, "X-Name: caf�\n") {
		t.Fatal("not true: strings.Contains(d.ResponseHeaders, \"X-Name: caf�\\n\")")
	}
	for _, s := range []string{d.RequestHeaders, d.RequestBody, d.ResponseHeaders, d.ResponseBody, d.RequestError.String} {
		if !utf8.ValidString(s) {
			t.Fatal("not true: utf8.ValidString(s)")
		}
		if strings.Contains(s, "\x00") {
			t.Fatal("not true: !strings.Contains(s, \"\\x00\")")
		}
	}
}

// TestSSRFProtection is an integration test verifying the webhook send path
// blocks private IPs end-to-end (models.Webhook -> secureHTTPClient -> ssrf).
// Each row must fail with ssrf.ErrPrivateIP, which only the SSRF check
// produces: a dial to these addresses fails anyway on most machines, so an
// error alone would pass without the protection. The loopback row targets a
// listener that must not be reached. Public destinations are covered in
// pkg/ssrf, where the connection attempt can be stubbed; nothing here leaves
// the machine.
func TestSSRFProtection(t *testing.T) {
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

// setAllowedHosts sets webhook.allowed_hosts on the test config and parses it
// the way the server does, and builds the test's Sender again with it.
func (e *testEnv) setAllowedHosts(t *testing.T, entries ...string) {
	t.Helper()
	e.cfg.Webhook.AllowedHosts = entries
	if err := e.cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// The list is read when a Sender is built, as at start-up.
	e.s = NewSender(e.cfg, e.dbx, e.ds, nil)
}

// TestSendWebhookAllowedHosts sends to a loopback receiver through the real
// SSRF-protected client: it is reached only while webhook.allowed_hosts
// covers it. The Sender reads the list when it is built.
func TestSendWebhookAllowedHosts(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	// The SSRF checks run on the socket, so this receiver is on loopback
	// TCP, not the in-memory network newReceiver uses.
	rec := &recorder{status: http.StatusOK}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	w := e.newWebhook(t, srv.URL+"/hook", "", ContentTypeJSON, EventBranchTagCreate)

	// Not listed: refused at send time, recorded with the reason.
	if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
		t.Fatal(err)
	}
	if got := rec.calls; got != 0 {
		t.Fatalf("rec.calls = %v, want %v", got, 0)
	}
	ds := e.deliveries(t, w.ID)
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if !ds[0].RequestError.Valid {
		t.Fatal("not true: ds[0].RequestError.Valid")
	}
	if !strings.Contains(ds[0].RequestError.String, ssrf.ErrPrivateIP.Error()) {
		t.Fatal("not true: strings.Contains(ds[0].RequestError.String, ssrf.ErrPrivateIP.Error())")
	}

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
		if err := e.s.sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()); err != nil {
			t.Fatal(err)
		}
		ds = e.deliveries(t, w.ID)
		last := ds[len(ds)-1]
		if calls() > 0 {
			if got := last.ResponseStatus; got != http.StatusOK {
				t.Fatalf("last.ResponseStatus = %v, want %v", got, http.StatusOK)
			}
			if last.RequestError.Valid {
				t.Fatal("not true: !last.RequestError.Valid")
			}
			break
		}
		if strings.Contains(last.RequestError.String, ssrf.ErrPrivateIP.Error()) || time.Now().After(deadline) {
			t.Fatalf("listed send not delivered: status %d, error %q", last.ResponseStatus, last.RequestError.String)
		}
		// A failure that comes back at once would otherwise spin and
		// record a delivery row per try.
		time.Sleep(100 * time.Millisecond)
	}
	if got := calls(); got != 1 {
		t.Fatalf("calls() = %v, want %v", got, 1)
	}

	// Removed from the list again: the stored webhook is refused, since
	// the current config applies to every delivery.
	e.setAllowedHosts(t, "10.0.0.0/8")
	err := e.s.sendPayload(e.ctx, w, EventBranchTagCreate, samplePayload(), false)
	if !errors.Is(err, ssrf.ErrPrivateIP) {
		t.Fatal("not true: errors.Is(err, ssrf.ErrPrivateIP)")
	}
	if got := calls(); got != 1 {
		t.Fatalf("calls() = %v, want %v", got, 1)
	}
}

// TestSendWebhookAllowedHostsKeepsMetadataBlocked checks that a broad entry
// does not open the cloud metadata address.
func TestSendWebhookAllowedHostsKeepsMetadataBlocked(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.setAllowedHosts(t, "0.0.0.0/0")
	w := e.newWebhook(t, "http://169.254.169.254/latest/meta-data/", "", ContentTypeJSON, EventBranchTagCreate)
	err := e.s.sendPayload(e.ctx, w, EventBranchTagCreate, samplePayload(), false)
	if !errors.Is(err, ssrf.ErrPrivateIP) {
		t.Fatal("not true: errors.Is(err, ssrf.ErrPrivateIP)")
	}
}

func TestValidateWebhookURLAllowing(t *testing.T) {
	t.Parallel()
	allowed, err := ssrf.ParseAllowedHosts([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWebhookURLAllowing(context.Background(), "http://127.0.0.1:8080/hook", allowed); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ValidateWebhookURLAllowing(context.Background(), "http://127.0.0.1:8080/hook", nil), ErrPrivateIP) {
		t.Fatal("not true: errors.Is(ValidateWebhookURLAllowing(context.Background(), \"http://127.0.0.1:8080/hook\", nil), ErrPrivateIP)")
	}
	if !errors.Is(ValidateWebhookURLAllowing(context.Background(), "http://169.254.169.254/", allowed), ErrPrivateIP) {
		t.Fatal("not true: errors.Is(ValidateWebhookURLAllowing(context.Background(), \"http://169.254.169.254/\", allowed), ErrPrivateIP)")
	}
}
