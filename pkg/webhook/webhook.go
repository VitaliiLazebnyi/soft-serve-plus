// Package webhook builds, signs, sends and records the server's webhook
// events.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	neturl "net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/google/go-querystring/query"
	"github.com/google/uuid"
)

// Hook is a repository webhook, with its content type and events.
type Hook struct {
	ID          int64
	RepoID      int64
	URL         string
	Secret      string
	Active      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ContentType ContentType
	Events      []Event
}

// NewHook returns the Hook of the stored webhook w, subscribed to events.
func NewHook(w models.Webhook, events []Event) Hook {
	return Hook{
		ID:          w.ID,
		RepoID:      w.RepoID,
		URL:         w.URL,
		Secret:      w.Secret,
		Active:      w.Active,
		CreatedAt:   w.CreatedAt,
		UpdatedAt:   w.UpdatedAt,
		ContentType: ContentType(w.ContentType), //nolint:gosec // the column holds a ContentType the server wrote (int8 range)
		Events:      events,
	}
}

// Delivery is a webhook delivery.
type Delivery struct {
	models.WebhookDelivery
	Event Event
}

// Version is the server version that webhook requests name in their
// User-Agent, "SoftServe/<Version>". The soft command sets it at start.
var Version = ""

// ErrWebhookInactive is returned when a delivery is redelivered to a webhook
// that is not active. Activate the webhook first to send to it again.
var ErrWebhookInactive = errors.New("webhook is not active")

// Default limits of a Sender.
const (
	// defaultSendTimeout bounds the request to one webhook, so a slow
	// receiver cannot use up the time of the other webhooks an event goes
	// to. It matches the cap of the SSRF-protected client.
	defaultSendTimeout = ssrf.ClientTimeout

	// defaultRecordTimeout bounds writing one delivery row and pruning the
	// older ones. On SQLite it covers the wait for this process's turn to
	// write (see db.DB.TransactionContext) as well as the write itself, so
	// it must be well above the time one write takes: a row that runs out
	// of time is lost from the delivery history, and with it the chance to
	// redeliver it. It was 5 seconds, and on a loaded Windows runner the
	// turn plus the write ran past that. It stays short enough that one
	// webhook's worst case, its re-read (rereadTimeout, rereadRetryDelay,
	// rereadTimeout: 10.2 seconds), a send to its client's cap
	// (ssrf.ClientTimeout, 30 seconds) and its record, fits in the shutdown
	// wait (backend.ShutdownDeliveryTimeout, a minute).
	defaultRecordTimeout = 15 * time.Second

	// defaultRereadTimeout bounds each try at re-reading a webhook before
	// its send, so a database that does not answer cannot hold a background
	// sender. A read does not wait for a turn to write.
	defaultRereadTimeout = 5 * time.Second

	// defaultRereadRetryDelay is the wait before the one retry of a failed
	// re-read.
	defaultRereadRetryDelay = 200 * time.Millisecond

	// defaultMaxDeliveries is how many deliveries are kept per webhook.
	// Every recorded delivery deletes the older ones, so events, including
	// ones recorded without sending when the queue is full, cannot grow the
	// deliveries table without limit. Migration 5
	// (pkg/db/migrate/0005_webhook_delivery_retention_*.up.sql) trims to a
	// literal 100 and does not follow this constant.
	defaultMaxDeliveries = 100
)

// Store is the part of store.Store a Sender uses: it reads the webhooks an
// event goes to and the users and collaborators payloads name, and records
// deliveries.
type Store interface {
	GetWebhooksByRepoIDWhereEvent(ctx context.Context, h db.Handler, repoID int64, events []int) ([]models.Webhook, error)
	GetActiveWebhookByIDWhereEvent(ctx context.Context, h db.Handler, repoID int64, id int64, event int) (models.Webhook, error)
	CreateWebhookDelivery(ctx context.Context, h db.Handler, d models.WebhookDelivery) error
	PruneWebhookDeliveries(ctx context.Context, h db.Handler, webhookID int64, keep int) error
	GetUserByID(ctx context.Context, h db.Handler, id int64) (models.User, error)
	GetCollabByUsernameAndRepo(ctx context.Context, h db.Handler, username string, repo string) (models.Collab, error)
}

// Sender builds webhook events and sends, records and redelivers them. The
// backend builds one at start; its fields do not change afterwards.
type Sender struct {
	db     *db.DB
	store  Store
	logger *log.Logger
	// client sends every request: SSRF-protected, letting through the
	// private and internal destinations webhook.allowed_hosts permits. The
	// list is read once, when the Sender is built, as the rest of the
	// config is; a webhook saved under an older list is held to this one.
	client *http.Client

	// The public URLs repository payloads name.
	httpURL, sshURL, gitURL string

	sendTimeout      time.Duration
	recordTimeout    time.Duration
	rereadTimeout    time.Duration
	rereadRetryDelay time.Duration
	maxDeliveries    int
	// now returns the current time, which signs requests.
	now func() time.Time
}

// NewSender returns a Sender that records deliveries in dbx through st, logs
// to logger (the default logger when nil) and takes webhook.allowed_hosts and
// the public URLs from cfg, which may be nil for none.
func NewSender(cfg *config.Config, dbx *db.DB, st Store, logger *log.Logger) *Sender {
	if logger == nil {
		logger = log.Default()
	}
	s := &Sender{
		db:               dbx,
		store:            st,
		logger:           logger.WithPrefix("webhook"),
		sendTimeout:      defaultSendTimeout,
		recordTimeout:    defaultRecordTimeout,
		rereadTimeout:    defaultRereadTimeout,
		rereadRetryDelay: defaultRereadRetryDelay,
		maxDeliveries:    testMaxDeliveries(defaultMaxDeliveries),
		now:              time.Now,
	}
	var allowed ssrf.AllowedHosts
	if cfg != nil {
		allowed = cfg.Webhook.ParsedAllowedHosts()
		s.httpURL, s.sshURL, s.gitURL = cfg.HTTP.PublicURL, cfg.SSH.PublicURL, cfg.Git.PublicURL
	}
	if len(allowed) == 0 {
		s.client = ssrf.NewSecureClient()
	} else {
		s.client = ssrf.NewSecureClientAllowing(allowed)
	}
	s.client = testClient(s.client)
	return s
}

// do sends a webhook request, a POST.
// Caller must close the returned body.
// A returned error never contains the webhook URL's path, query, fragment or
// user info, which can hold secrets (see redactURL).
func (s *Sender) do(ctx context.Context, url string, headers http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, ssrf.RedactURLError(err, redactURL)
	}

	req.Header = headers
	res, err := s.client.Do(req)
	if err != nil {
		return nil, ssrf.RedactURLError(err, redactURL)
	}

	return res, nil
}

// redactedURLPart replaces the parts of a webhook URL that are left out of
// errors.
const redactedURLPart = "[redacted]"

// redactURL returns rawURL reduced to its scheme and host. Many receivers
// (Slack, Discord, Teams and the like) put a token in the path or query, so
// those parts and the fragment are replaced with redactedURLPart, and any
// user info is dropped. A URL that cannot be parsed is replaced entirely.
//
// The host is kept on purpose, so an operator can tell which receiver failed.
// A receiver that puts a token in the host name (a per-hook subdomain such as
// https://<id>.m.pipedream.net) therefore still has that token in the error.
func redactURL(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return redactedURLPart
	}
	redacted := u.Scheme + "://" + u.Host
	if u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		redacted += "/" + redactedURLPart
	}
	return redacted
}

// Redeliver sends a recorded delivery again and records the attempt as a new
// delivery. body is the stored request body, sent as it is, and ct the
// content type it was sent with. The request carries guid, the
// X-SoftServe-Delivery value of the original delivery, so a receiver can tell
// that it is the same event. The timestamp and signatures are computed
// afresh.
func (s *Sender) Redeliver(ctx context.Context, w models.Webhook, event Event, ct ContentType, body []byte, guid uuid.UUID) error {
	return s.send(ctx, w, event, ct, body, guid, true)
}

// sendPayload encodes payload in w's content type and sends it, recording
// the attempt when record is set.
func (s *Sender) sendPayload(ctx context.Context, w models.Webhook, event Event, payload EventPayload, record bool) error {
	ct := ContentType(w.ContentType) //nolint:gosec // the column holds a ContentType the server wrote (int8 range)
	body, err := encode(ct, payload)
	if err != nil {
		return err
	}
	return s.send(ctx, w, event, ct, body, uuid.Nil, record)
}

// errRereadFailed is the reason stored on a delivery that was not sent because
// its webhook could not be re-read. The database error itself is only logged:
// the stored reason is shown to repository admins, who need not see database
// details.
var errRereadFailed = errors.New("not sent: could not re-read webhook (database error)")

// request is a webhook request ready to send or record.
type request struct {
	// id identifies the delivery record. Every attempt gets its own.
	id      uuid.UUID
	headers http.Header
	body    string
}

// Request headers that identify and sign a delivery.
const (
	// HeaderDelivery is the delivery GUID. A redelivery keeps the GUID of
	// the delivery it repeats, so a receiver can drop an event it has
	// already handled.
	HeaderDelivery = "X-SoftServe-Delivery"
	// HeaderTimestamp is when the request was signed, in seconds since the
	// Unix epoch.
	HeaderTimestamp = "X-SoftServe-Timestamp"
	// HeaderSignature is "sha256=" and the hex HMAC-SHA256 of the body,
	// keyed with the webhook secret. It does not cover the timestamp, so a
	// captured request can be replayed; check HeaderSignatureV2 instead.
	HeaderSignature = "X-SoftServe-Signature"
	// HeaderSignatureV2 is "sha256=" and the hex HMAC-SHA256, keyed with the
	// webhook secret, of HeaderTimestamp, a ".", HeaderDelivery, a "." and
	// the body. A receiver that checks it, rejects timestamps too far from
	// its own clock and remembers the GUIDs it has seen recently can refuse
	// replayed requests.
	HeaderSignatureV2 = "X-SoftServe-Signature-V2"
)

// encode encodes the payload of a new event in content type ct: JSON, or a
// form built from the payload's url tags.
func encode(ct ContentType, payload EventPayload) ([]byte, error) {
	switch ct {
	case ContentTypeJSON:
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(payload); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case ContentTypeForm:
		v, err := query.Values(payload)
		if err != nil {
			return nil, err
		}
		return []byte(v.Encode()), nil
	default:
		return nil, ErrInvalidContentType
	}
}

// newRequest builds the headers of a request that sends body, already
// encoded in ct, including the signatures when secret is set. The delivery
// GUID is guid, or the new delivery's id when guid is uuid.Nil.
func (s *Sender) newRequest(secret string, ct ContentType, event Event, body []byte, guid uuid.UUID) (request, error) {
	if ct.String() == "" {
		return request{}, ErrInvalidContentType
	}

	headers := http.Header{}
	headers.Add("Content-Type", ct.String())
	headers.Add("User-Agent", "SoftServe/"+Version)
	headers.Add("X-SoftServe-Event", event.String())

	// A random (version 4) ID: version 1 would embed the server's MAC address.
	id, err := uuid.NewRandom()
	if err != nil {
		return request{}, err
	}

	if guid == uuid.Nil {
		guid = id
	}
	timestamp := strconv.FormatInt(s.now().Unix(), 10)
	headers.Add(HeaderDelivery, guid.String())
	headers.Add(HeaderTimestamp, timestamp)

	b := string(body)
	if secret != "" {
		headers.Add(HeaderSignature, sign(secret, b))
		headers.Add(HeaderSignatureV2, sign(secret, timestamp+"."+guid.String()+"."+b))
	}

	return request{id: id, headers: headers, body: b}, nil
}

// sign returns "sha256=" and the hex HMAC-SHA256 of content keyed with secret.
func sign(secret, content string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(content))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// send sends body, encoded in ct, to w. The request gets its own
// sendTimeout. When record is false the attempt is not written to the
// deliveries table, and a transport error or a non-2xx response is returned
// so the caller can log it.
func (s *Sender) send(ctx context.Context, w models.Webhook, event Event, ct ContentType, body []byte, guid uuid.UUID, record bool) error {
	req, err := s.newRequest(w.Secret, ct, event, body, guid)
	if err != nil {
		return err
	}

	sctx, cancel := context.WithTimeout(ctx, s.sendTimeout)
	defer cancel()
	res, reqErr := s.do(sctx, w.URL, req.headers, strings.NewReader(req.body))

	resStatus := 0
	resHeaders := ""
	resBody := ""

	if res != nil {
		resStatus = res.StatusCode
		resHeaders = formatHeaders(res.Header)

		if res.Body != nil {
			defer res.Body.Close() //nolint: errcheck
			// A failed read still leaves a delivery record: keep what was
			// read and report the read error with the request error.
			var err error
			resBody, err = readResponseBody(res.Body)
			if err != nil {
				reqErr = errors.Join(reqErr, err)
			}
		}
	}

	if !record {
		if reqErr == nil && (resStatus < 200 || resStatus > 299) {
			return fmt.Errorf("webhook %d: receiver responded with status %d", w.ID, resStatus)
		}
		if reqErr != nil {
			// The URL in reqErr is redacted, so name the webhook: several
			// hooks on one host would otherwise log the same line.
			return fmt.Errorf("webhook %d: %w", w.ID, reqErr)
		}
		return nil
	}

	return s.recordDelivery(ctx, w, event, req, reqErr, resStatus, resHeaders, resBody)
}

// maxResponseBody is how much of a receiver's response body is read and
// stored. The rest is not read: the body is closed, so a receiver that streams
// without end cannot hold the sender for its whole timeout or fill the
// deliveries table.
const maxResponseBody = 64 << 10

// truncatedMarker is appended to a stored response body that was cut off at
// maxResponseBody.
const truncatedMarker = "\n[response body truncated by Soft Serve]"

// readResponseBody reads at most maxResponseBody bytes of body. A longer body
// is cut off and marked with truncatedMarker; the cut is moved back to the
// start of a character it would split, so valid UTF-8 stays valid. On a read
// error it returns what was read so far with the error.
func readResponseBody(body io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxResponseBody+1))
	if len(b) > maxResponseBody {
		cut := maxResponseBody
		for i := cut; i > 0 && i > maxResponseBody-utf8.UTFMax; i-- {
			if utf8.RuneStart(b[i]) {
				cut = i
				break
			}
		}
		return string(b[:cut]) + truncatedMarker, err
	}
	return string(b), err
}

// maxStoredHeaders is how much of a request's or response's headers is
// stored with a delivery. The secure client already refuses responses whose
// headers pass ssrf.MaxResponseHeaderBytes; this bounds the stored text
// whatever client sent the request.
const maxStoredHeaders = 64 << 10

// truncatedHeadersMarker is appended to stored headers that were cut off at
// maxStoredHeaders.
const truncatedHeadersMarker = "[headers truncated by Soft Serve]\n"

// formatHeaders renders h as "Key: value" lines, one per value, sorted by key
// so a delivery reads the same on every view. Every value of a repeated
// header is kept, in the order it was sent. It stops before the line that
// would take it past maxStoredHeaders and marks the cut with
// truncatedHeadersMarker. Each key and value goes through storableText before
// it is measured, so the cap holds for the text that is stored even when the
// receiver sent invalid UTF-8.
func formatHeaders(h http.Header) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(h)) {
		key := storableText(k)
		for _, v := range h[k] {
			value := storableText(v)
			if b.Len()+len(key)+len(": ")+len(value)+len("\n") > maxStoredHeaders {
				b.WriteString(truncatedHeadersMarker)
				return b.String()
			}
			b.WriteString(key)
			b.WriteString(": ")
			b.WriteString(value)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// storableText is db.StorableText: Postgres refusing a receiver's text
// would lose the whole delivery row. Use it for text that comes from the
// receiver: its headers and body, and errors that may quote them.
func storableText(s string) string {
	return db.StorableText(s)
}

// recordDelivery writes one delivery row, then prunes the webhook's older
// deliveries. It uses a fresh, short context that keeps ctx's values but not
// its cancellation or deadline, so an attempt that ran out of time or was
// cancelled is still recorded.
func (s *Sender) recordDelivery(ctx context.Context, w models.Webhook, event Event, req request, reqErr error, resStatus int, resHeaders, resBody string) error {
	// formatHeaders already makes every key and value storable.
	reqHeaders := formatHeaders(req.headers)

	// The response and the request error can carry any bytes the receiver
	// sent. The request body is JSON or form-encoded, so always valid UTF-8
	// without NULs, and is stored as sent: a redelivery signs and sends the
	// same bytes. resHeaders normally comes from formatHeaders too; cleaning
	// it again is a no-op then and covers a caller that builds it otherwise.
	d := models.WebhookDelivery{
		ID:              req.id,
		WebhookID:       w.ID,
		Event:           int(event),
		RequestURL:      w.URL,
		RequestMethod:   http.MethodPost,
		RequestHeaders:  reqHeaders,
		RequestBody:     req.body,
		ResponseStatus:  resStatus,
		ResponseHeaders: storableText(resHeaders),
		ResponseBody:    storableText(resBody),
	}
	// A successful delivery stores NULL, not an empty string.
	if reqErr != nil {
		d.RequestError = sql.NullString{String: storableText(reqErr.Error()), Valid: true}
	}

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.recordTimeout)
	defer cancel()

	// The insert and the prune are two transactions of one statement each:
	// they need not be atomic. Each takes this process's turn to write on
	// SQLite (see db.DB.TransactionContext), so a record waits for other
	// writers rather than fail with SQLITE_BUSY. A failed prune only leaves
	// extra rows for the next delivery's prune, so it is logged and the
	// delivery is kept. On SQLite this costs a second write, and sync, per
	// delivery.
	//
	// A send that was in flight when its webhook was deleted fails the
	// insert on its foreign key. Nothing is written then, and no error is
	// returned: there is no webhook left whose history it would belong to.
	//
	// On Postgres seq is taken from its sequence when a row is inserted, not
	// when it commits. Deliveries of one webhook can be recorded at once (a
	// background delivery and a redelivery, say), so a delivery whose insert
	// commits late can already be older, by seq, than the newest
	// maxDeliveries rows and be pruned by the next record. That needs
	// maxDeliveries deliveries of the same webhook recorded while its insert
	// is in flight; the cost is one delivery missing from the history, which
	// pruning by seq accepts rather than ordering commits.
	err := s.db.TransactionContext(rctx, func(tx *db.Tx) error {
		return s.store.CreateWebhookDelivery(rctx, tx, d)
	})
	switch {
	case errors.Is(err, db.ErrForeignKey):
		return nil
	case err != nil:
		return err
	}
	if err := s.db.TransactionContext(rctx, func(tx *db.Tx) error {
		return s.store.PruneWebhookDeliveries(rctx, tx, w.ID, s.maxDeliveries)
	}); err != nil {
		s.logger.Warn("pruning webhook deliveries", "webhook", w.ID, "err", err)
	}
	return nil
}

// MaxWebhooksPerRepo is how many webhooks a repository can have. Each event
// is sent to, recorded for and pruned for every subscribed webhook, and each
// webhook keeps maxDeliveries deliveries, so this bounds both the work one
// event causes and the deliveries a repository stores.
const MaxWebhooksPerRepo = 20

// ErrTooManyWebhooks is returned when creating a webhook for a repository
// that already has MaxWebhooksPerRepo webhooks.
var ErrTooManyWebhooks = fmt.Errorf("a repository can have at most %d webhooks", MaxWebhooksPerRepo)

// RecordUndelivered records a delivery of payload to each webhook without
// sending it, with reason as the request error. Use it when an event cannot
// be sent at all; the recorded request can be redelivered later.
func (s *Sender) RecordUndelivered(ctx context.Context, hooks []models.Webhook, payload EventPayload, reason error) error {
	var errs []error
	for _, w := range hooks {
		ct := ContentType(w.ContentType) //nolint:gosec // the column holds a ContentType the server wrote (int8 range)
		body, err := encode(ct, payload)
		var req request
		if err == nil {
			req, err = s.newRequest(w.Secret, ct, payload.Event(), body, uuid.Nil)
		}
		if err == nil {
			err = s.recordDelivery(ctx, w, payload.Event(), req, reason, 0, "", "")
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Subscribers returns the active webhooks of the payload's repository that
// subscribe to its event. Inactive webhooks are not sent events. Call it
// before a change that removes those webhooks, since they cannot be found
// afterwards.
func (s *Sender) Subscribers(ctx context.Context, payload EventPayload) ([]models.Webhook, error) {
	webhooks, err := s.store.GetWebhooksByRepoIDWhereEvent(ctx, s.db, payload.RepositoryID(), []int{int(payload.Event())})
	if err != nil {
		return nil, err
	}
	return webhooks, nil
}

// Deliver sends the payload to each webhook and records every attempt. Each
// webhook is re-read just before its send: one deleted or unsubscribed since
// hooks was read is skipped, as is one deactivated since, and a changed URL or
// secret is used. A webhook that cannot be re-read is not sent to: the event
// is recorded for it as undelivered, so it can be redelivered later, and a
// warning with the error is logged.
func (s *Sender) Deliver(ctx context.Context, hooks []models.Webhook, payload EventPayload) error {
	return s.deliver(ctx, hooks, payload, true)
}

// DeliverUnrecorded sends the payload to each webhook without recording the
// attempts. Use it when the webhooks are being removed, so no delivery row is
// written that refers to a webhook that no longer exists.
func (s *Sender) DeliverUnrecorded(ctx context.Context, hooks []models.Webhook, payload EventPayload) error {
	return s.deliver(ctx, hooks, payload, false)
}

func (s *Sender) deliver(ctx context.Context, hooks []models.Webhook, payload EventPayload, record bool) error {
	// Every webhook is attempted even if an earlier one fails to send or
	// record, so one bad webhook cannot stop the others from receiving the
	// event. Each send has its own timeout, so a slow receiver does not use up
	// the time of the webhooks after it.
	var errs []error
	for _, w := range hooks {
		if record {
			cur, ok, err := s.currentHook(ctx, w, payload.Event())
			if err != nil {
				// Whether w may still be sent to is unknown, so it is not
				// sent to. The event is recorded as undelivered instead, so
				// it can be redelivered once the webhook can be read. The
				// error is logged here, since a recorded delivery is not
				// returned as an error; the stored reason leaves out the
				// database details. Only a failure to record is returned.
				s.logger.Warn("webhook not sent: re-reading it failed; recorded as undelivered",
					"webhook_id", w.ID, "repo_id", w.RepoID, "event", payload.Event(), "err", err)
				if err := s.RecordUndelivered(ctx, []models.Webhook{w}, payload, errRereadFailed); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			if !ok {
				continue
			}
			w = cur
		}
		if err := s.sendPayload(ctx, w, payload.Event(), payload, record); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// currentHook re-reads w right before it is sent, since a delivery can wait in
// the background queue while its webhook is changed. It returns the webhook as
// stored now, so a rotated secret or changed URL is used, and false when the
// webhook was deleted, deactivated, or no longer subscribes to event. It
// returns an error when the webhook cannot be read; the caller then does not
// send, since the webhook may have been deactivated.
//
// Each try is bounded by rereadTimeout, and a failed try is retried once after
// rereadRetryDelay unless ctx is done, so a short database hiccup does not
// leave the event undelivered.
//
// Only w itself is read, so delivering to n webhooks costs n single-row
// queries rather than n reads of every subscriber.
func (s *Sender) currentHook(ctx context.Context, w models.Webhook, event Event) (models.Webhook, bool, error) {
	cur, ok, err := s.lookupHook(ctx, w, event)
	if err != nil && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(s.rereadRetryDelay):
			cur, ok, err = s.lookupHook(ctx, w, event)
		}
	}
	if err != nil {
		return models.Webhook{}, false, err
	}
	return cur, ok, nil
}

// lookupHook reads w as stored now if it is still active and subscribes to
// event, giving up after rereadTimeout. It returns false when it is not.
func (s *Sender) lookupHook(ctx context.Context, w models.Webhook, event Event) (models.Webhook, bool, error) {
	lctx, cancel := context.WithTimeout(ctx, s.rereadTimeout)
	defer cancel()
	cur, err := s.store.GetActiveWebhookByIDWhereEvent(lctx, s.db, w.RepoID, w.ID, int(event))
	if errors.Is(err, db.ErrRecordNotFound) {
		return models.Webhook{}, false, nil
	}
	if err != nil {
		return models.Webhook{}, false, err
	}
	return cur, true, nil
}

func repoURL(publicURL string, repo string) string {
	return fmt.Sprintf("%s/%s.git", publicURL, proto.SanitizeRepo(repo))
}

// defaultBranch returns repo's default branch for an event payload. An error
// is logged and leaves the branch empty: the event is still worth sending
// without it.
func (s *Sender) defaultBranch(ctx context.Context, repo proto.Repository) string {
	branch, err := getDefaultBranch(ctx, repo)
	if err != nil {
		s.logger.Warn("cannot read the default branch for the webhook payload",
			"repo", repo.Name(), "err", err)
	}
	return branch
}

func getDefaultBranch(ctx context.Context, repo proto.Repository) (string, error) {
	branch, err := proto.RepositoryDefaultBranch(ctx, repo)
	// An empty repository, as before its first push, has no default branch
	// yet: that is "" and no error.
	if err != nil && !errors.Is(err, git.ErrReferenceNotExist) {
		return "", err
	}

	return branch, nil
}
