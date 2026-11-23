package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"
)

// TestWebhookDeliveriesAreScopedToRepository verifies that webhook deliveries
// cannot be read across repositories.
//
// The `repo webhook deliveries list|get` commands authorize the caller against
// the REPOSITORY argument, so the delivery lookup must be keyed on it as well
// as on the numeric webhook ID. Since any user who owns a repository has admin
// access to it, a user could otherwise pass their own repository name together
// with another repository's webhook ID and read that webhook's stored
// deliveries.
//
// Deliveries store the full request URL, headers (including the HMAC
// signature), request body, and response body, so this is a cross-repository
// disclosure of private repository event payloads.
func TestWebhookDeliveriesAreScopedToRepository(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)

	// The victim owns a private repository with a webhook, and that webhook
	// has a recorded delivery containing sensitive request/response data.
	victimCtx := withUser(t, ctx, be, "victim", false)
	victim := proto.UserFromContext(victimCtx)
	victimRepo, err := be.CreateRepository(victimCtx, "victim-repo", victim, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.CreateWebhook(victimCtx, victimRepo, backend.WebhookInput{URL: "http://93.184.216.34/hook", ContentType: webhook.ContentTypeJSON, Secret: "s3cret", Events: []webhook.Event{webhook.EventPush}, Active: true}); err != nil {
		t.Fatal(err)
	}

	victimHooks, err := be.ListWebhooks(victimCtx, victimRepo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(victimHooks); got != 1 {
		t.Fatalf("len(victimHooks) = %v, want %v", got, 1)
	}
	victimHookID := victimHooks[0].ID

	const (
		secretBody      = "private-repo-payload"
		secretSignature = "X-SoftServe-Signature: sha256=leaked-signature\n"
	)
	deliveryID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	if err := cmdsOf(ctx).store.CreateWebhookDelivery(ctx, cmdsOf(ctx).db, models.WebhookDelivery{
		ID:              deliveryID,
		WebhookID:       victimHookID,
		Event:           int(webhook.EventPush),
		RequestURL:      "http://93.184.216.34/hook",
		RequestMethod:   "POST",
		RequestHeaders:  secretSignature,
		RequestBody:     secretBody,
		ResponseStatus:  200,
		ResponseHeaders: "Content-Type: text/plain\n",
		ResponseBody:    "victim response body",
	}); err != nil {
		t.Fatal(err)
	}

	// The attacker owns their own repository, so they hold admin access to
	// it, but they have no access at all to the victim's repository.
	attackerCtx := withUser(t, ctx, be, "attacker", false)
	attacker := proto.UserFromContext(attackerCtx)
	_, err = be.CreateRepository(attackerCtx, "attacker-repo", attacker, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	hookID := strconv.FormatInt(victimHookID, 10)

	// Listing deliveries by naming their own repository must not enumerate
	// the victim webhook's delivery IDs.
	stdout, _, err := runRepoOutput(t, attackerCtx, "webhook", "deliveries", "list", "attacker-repo", hookID)
	if err == nil {
		t.Error("expected error listing another repository's webhook deliveries")
	}
	if strings.Contains(stdout, deliveryID.String()) {
		t.Errorf("leaked delivery ID in output: %q", stdout)
	}

	// Reading a specific delivery must not disclose its stored request or
	// response data either.
	stdout, _, err = runRepoOutput(t, attackerCtx, "webhook", "deliveries", "get", "attacker-repo", hookID, deliveryID.String())
	if err == nil {
		t.Error("expected error getting another repository's webhook delivery")
	}
	for _, secret := range []string{secretBody, "leaked-signature", "victim response body"} {
		if strings.Contains(stdout, secret) {
			t.Errorf("leaked %q in output: %q", secret, stdout)
		}
	}

	// The owner can still read their own webhook's deliveries.
	stdout, _, err = runRepoOutput(t, victimCtx, "webhook", "deliveries", "list", "victim-repo", hookID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, deliveryID.String()) {
		t.Errorf("owner could not list own deliveries, got: %q", stdout)
	}

	stdout, _, err = runRepoOutput(t, victimCtx, "webhook", "deliveries", "get", "victim-repo", hookID, deliveryID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, secretBody) {
		t.Errorf("owner could not read own delivery body, got: %q", stdout)
	}
}

// Webhook URLs are literal public addresses: validation accepts them without a
// DNS lookup. Tests that send to a webhook store a loopback URL for it
// directly (see whPointAtLoopback), which the SSRF-protected client refuses
// before any packet is sent.
const (
	whURL      = "https://93.184.216.34/hook"
	whURLOther = "https://93.184.216.35"
)

// whRepo creates the repository "hooks", owned by a fresh non-admin user, and
// returns that user's context.
func whRepo(t *testing.T, ctx context.Context, be *backend.Backend) context.Context {
	t.Helper()
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "hooks", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ownerCtx
}

// whHooks returns the webhooks of repository "hooks" as the backend sees them.
func whHooks(t *testing.T, ctx context.Context, be *backend.Backend, repo string) []webhook.Hook {
	t.Helper()
	r, err := be.Repository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := be.ListWebhooks(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	return hooks
}

func eventNames(evs []webhook.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.String())
	}
	sort.Strings(out)
	return out
}

// whSnapshot captures every stored field of the hooks of "hooks", so tests can
// assert that a failed command changed nothing.
func whSnapshot(t *testing.T, ctx context.Context, be *backend.Backend) string {
	t.Helper()
	var b strings.Builder
	for _, h := range whHooks(t, ctx, be, "hooks") {
		b.WriteString(strconv.FormatInt(h.ID, 10) + "|" + h.URL + "|" + h.Secret + "|" +
			strconv.FormatBool(h.Active) + "|" + h.ContentType.String() + "|" +
			strings.Join(eventNames(h.Events), ",") + "\n")
	}
	return b.String()
}

// whSeq numbers the webhooks created by whCreate, so their URLs differ. It is
// atomic so the helper stays safe if tests are ever run in parallel.
var whSeq atomic.Int64

// whCreate creates a webhook on "hooks" with a URL unique to this call, since
// a repository cannot hold two webhooks with the same URL, and returns its ID.
func whCreate(t *testing.T, ctx context.Context, be *backend.Backend, extra ...string) int64 {
	t.Helper()
	url := fmt.Sprintf("%s?n=%d", whURL, whSeq.Add(1))
	args := append([]string{"webhook", "create", "hooks", url}, extra...)
	_, _, err := runRepoOutput(t, ctx, args...)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range whHooks(t, ctx, be, "hooks") {
		if h.URL == url {
			return h.ID
		}
	}
	t.Fatalf("webhook %s not found", url)
	return 0
}

// whPointAtLoopback stores a loopback URL for the webhook without validation.
// Sends to it then fail in the SSRF-protected client before anything goes on
// the network, and are recorded as failed deliveries.
func whPointAtLoopback(t *testing.T, ctx context.Context, repo string, hookID int64) {
	t.Helper()
	be := cmdsOf(ctx).be
	r, err := be.Repository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	h, err := be.Webhook(ctx, r, hookID)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdsOf(ctx).store.UpdateWebhookByID(ctx, cmdsOf(ctx).db, models.Webhook{ID: hookID, RepoID: r.ID(), URL: "http://127.0.0.1:1/", Secret: h.Secret, ContentType: int(h.ContentType), Active: h.Active}); err != nil {
		t.Fatal(err)
	}
}

// whAwaitDeliveries waits until the webhook has at least n recorded
// deliveries. Unlike Backend.WaitForDeliveries it leaves the delivery queue
// open, so a test can wait for one change's delivery before making the next.
// A read that fails while a record commits is retried.
func whAwaitDeliveries(t *testing.T, ctx context.Context, be *backend.Backend, repo string, hookID int64, n int) {
	t.Helper()
	r, err := be.Repository(ctx, repo)
	if err != nil {
		t.Fatalf("repository %s: %v", repo, err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		ds, err := be.ListWebhookDeliveries(ctx, r, hookID)
		if err == nil && len(ds) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhook %d: want %d deliveries, have %d (last error: %v)", hookID, n, len(ds), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func whInsertDelivery(t *testing.T, ctx context.Context, hookID int64, id uuid.UUID, status int) {
	t.Helper()
	if err := cmdsOf(ctx).store.CreateWebhookDelivery(ctx, cmdsOf(ctx).db, models.WebhookDelivery{
		ID:              id,
		WebhookID:       hookID,
		Event:           int(webhook.EventPush),
		RequestURL:      whURL,
		RequestMethod:   "POST",
		RequestHeaders:  "Content-Type: application/json\nX-SoftServe-Event: push\n",
		RequestBody:     `{"ref":"main"}`,
		ResponseStatus:  status,
		ResponseHeaders: "X-Reply: yes\n",
		ResponseBody:    "reply body",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookCreatePersistsEveryFlag(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	stdout, stderr, err := runRepoOutput(t, ownerCtx, "webhook", "create", "hooks", whURL,
		"-e", "push", "-e", "repository", "-s", "topsecret", "-c", "form", "-a=false")
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout; got != "" {
		t.Fatalf("stdout = %v, want %v", got, "")
	}
	if got := stderr; got != "" {
		t.Fatalf("stderr = %v, want %v", got, "")
	}

	hooks := whHooks(t, ownerCtx, be, "hooks")
	if got := len(hooks); got != 1 {
		t.Fatalf("len(hooks) = %v, want %v", got, 1)
	}
	h := hooks[0]
	if got := h.URL; got != whURL {
		t.Fatalf("h.URL = %v, want %v", got, whURL)
	}
	if got := h.Secret; got != "topsecret" {
		t.Fatalf("h.Secret = %v, want %v", got, "topsecret")
	}
	if got := h.ContentType; got != webhook.ContentTypeForm {
		t.Fatalf("h.ContentType = %v, want %v", got, webhook.ContentTypeForm)
	}
	if got := h.Active; got != false {
		t.Fatalf("h.Active = %v, want %v", got, false)
	}
	if got, want := eventNames(h.Events), ([]string{"push", "repository"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("eventNames(h.Events) = %v, want %v", got, want)
	}
}

func TestWebhookCreateDefaults(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	if err := runRepo(t, ownerCtx, "webhook", "create", "hooks", whURL); err != nil {
		t.Fatal(err)
	}

	h := whHooks(t, ownerCtx, be, "hooks")[0]
	if got := h.ContentType; got != webhook.ContentTypeJSON {
		t.Fatalf("h.ContentType = %v, want %v", got, webhook.ContentTypeJSON)
	}
	if got := h.Active; got != true {
		t.Fatalf("h.Active = %v, want %v", got, true)
	}
	if got := h.Secret; got != "" {
		t.Fatalf("h.Secret = %v, want %v", got, "")
	}
	if got := len(h.Events); got != 0 {
		t.Fatalf("len(h.Events) = %v, want %v", got, 0)
	}
}

func TestWebhookCreateContentTypeIsCaseInsensitiveAndTrimmed(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	whCreate(t, ownerCtx, be, "-c", " JSON ")
	whCreate(t, ownerCtx, be, "-c", "Form")

	hooks := whHooks(t, ownerCtx, be, "hooks")
	if got := len(hooks); got != 2 {
		t.Fatalf("len(hooks) = %v, want %v", got, 2)
	}
	if got := hooks[0].ContentType; got != webhook.ContentTypeJSON {
		t.Fatalf("hooks[0].ContentType = %v, want %v", got, webhook.ContentTypeJSON)
	}
	if got := hooks[1].ContentType; got != webhook.ContentTypeForm {
		t.Fatalf("hooks[1].ContentType = %v, want %v", got, webhook.ContentTypeForm)
	}
}

func TestWebhookCreateTrimsURL(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	if err := runRepo(t, ownerCtx, "webhook", "create", "hooks", "  "+whURL+"  "); err != nil {
		t.Fatal(err)
	}
	if got := whHooks(t, ownerCtx, be, "hooks")[0].URL; got != whURL {
		t.Fatalf("whHooks(t, ownerCtx, be, \"hooks\")[0].URL = %v, want %v", got, whURL)
	}
}

// Every invalid create input is refused for its own reason and leaves nothing
// behind. Each case names the error text it must fail with.
func TestWebhookCreateRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args []string
		want string
	}{
		"unknown event":       {[]string{"webhook", "create", "hooks", whURL, "-e", "bogus"}, webhook.ErrInvalidEvent.Error()},
		"unknown content":     {[]string{"webhook", "create", "hooks", whURL, "-c", "xml"}, webhook.ErrInvalidContentType.Error()},
		"private target":      {[]string{"webhook", "create", "hooks", "http://127.0.0.1/x"}, webhook.ErrPrivateIP.Error()},
		"loopback hostname":   {[]string{"webhook", "create", "hooks", "http://localhost/x"}, webhook.ErrPrivateIP.Error()},
		"not a url":           {[]string{"webhook", "create", "hooks", "::not a url"}, "cannot parse URL"},
		"missing url":         {[]string{"webhook", "create", "hooks"}, "accepts 2 arg(s), received 1"},
		"missing repository":  {[]string{"webhook", "create"}, "accepts 2 arg(s), received 0"},
		"unsupported scheme":  {[]string{"webhook", "create", "hooks", "ftp://93.184.216.34/x"}, webhook.ErrInvalidScheme.Error()},
		"unknown repository":  {[]string{"webhook", "create", "nope", whURL}, proto.ErrRepoNotFound.Error()},
		"empty content type":  {[]string{"webhook", "create", "hooks", whURL, "-c", ""}, webhook.ErrInvalidContentType.Error()},
		"space-only content":  {[]string{"webhook", "create", "hooks", whURL, "-c", "   "}, webhook.ErrInvalidContentType.Error()},
		"bad event in middle": {[]string{"webhook", "create", "hooks", whURL, "-e", "push", "-e", "nope"}, webhook.ErrInvalidEvent.Error()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			before := whSnapshot(t, ownerCtx, be)

			_, _, err := runRepoOutput(t, ownerCtx, tc.args...)
			if err == nil {
				t.Fatal("not true: err != nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			if got := whSnapshot(t, ownerCtx, be); got != before {
				t.Fatalf("whSnapshot(t, ownerCtx, be) = %v, want %v", got, before)
			}
		})
	}
}

func TestWebhookCreateReportsUnknownEventAndContentType(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	err := runRepo(t, ownerCtx, "webhook", "create", "hooks", whURL, "-e", "bogus")
	if !errors.Is(err, webhook.ErrInvalidEvent) {
		t.Fatal("not true: errors.Is(err, webhook.ErrInvalidEvent)")
	}

	err = runRepo(t, ownerCtx, "webhook", "create", "hooks", whURL, "-c", "xml")
	if !errors.Is(err, webhook.ErrInvalidContentType) {
		t.Fatal("not true: errors.Is(err, webhook.ErrInvalidContentType)")
	}
}

func TestWebhookListOutputsRowsWithoutSecrets(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	whCreate(t, ownerCtx, be, "-s", "hidden-secret", "-e", "push", "-a=false")
	whCreate(t, ownerCtx, be, "-e", "repository")

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "list", "hooks")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "ID") {
		t.Fatal("not true: strings.Contains(stdout, \"ID\")")
	}
	if !strings.Contains(stdout, "Events") {
		t.Fatal("not true: strings.Contains(stdout, \"Events\")")
	}
	if !strings.Contains(stdout, "push") {
		t.Fatal("not true: strings.Contains(stdout, \"push\")")
	}
	if !strings.Contains(stdout, "repository") {
		t.Fatal("not true: strings.Contains(stdout, \"repository\")")
	}
	if !strings.Contains(stdout, "false") {
		t.Fatal("not true: strings.Contains(stdout, \"false\")")
	}
	if !strings.Contains(stdout, "true") {
		t.Fatal("not true: strings.Contains(stdout, \"true\")")
	}
	if !strings.Contains(stdout, whURL) {
		t.Fatal("not true: strings.Contains(stdout, whURL)")
	}
	if strings.Contains(stdout, "hidden-secret") {
		t.Fatal("not true: !strings.Contains(stdout, \"hidden-secret\")")
	}
}

func TestWebhookListEmptyRepository(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "list", "hooks")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "ID") {
		t.Fatal("not true: strings.Contains(stdout, \"ID\")")
	}
	if strings.Contains(stdout, whURL) {
		t.Fatal("not true: !strings.Contains(stdout, whURL)")
	}
}

func TestWebhookDeleteRemovesOnlyThatWebhook(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	first := whCreate(t, ownerCtx, be)
	second := whCreate(t, ownerCtx, be)

	if err := runRepo(t, ownerCtx, "webhook", "delete", "hooks", strconv.FormatInt(first, 10)); err != nil {
		t.Fatal(err)
	}
	hooks := whHooks(t, ownerCtx, be, "hooks")
	if got := len(hooks); got != 1 {
		t.Fatalf("len(hooks) = %v, want %v", got, 1)
	}
	if got := hooks[0].ID; got != second {
		t.Fatalf("hooks[0].ID = %v, want %v", got, second)
	}
}

func TestWebhookDeleteRejectsBadIDs(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	whCreate(t, ownerCtx, be)
	before := whSnapshot(t, ownerCtx, be)

	err := runRepo(t, ownerCtx, "webhook", "delete", "hooks", "abc")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if !strings.Contains(err.Error(), "invalid webhook ID") {
		t.Fatal("not true: strings.Contains(err.Error(), \"invalid webhook ID\")")
	}

	if runRepo(t, ownerCtx, "webhook", "delete", "hooks", "999999") == nil {
		t.Fatal("not true: runRepo(t, ownerCtx, \"webhook\", \"delete\", \"hooks\", \"999999\") != nil")
	}
	if got := whSnapshot(t, ownerCtx, be); got != before {
		t.Fatalf("whSnapshot(t, ownerCtx, be) = %v, want %v", got, before)
	}
}

// A webhook ID from another repository must not be deletable through this
// repository.
func TestWebhookDeleteIsScopedToRepository(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	_, err := be.CreateRepository(ownerCtx, "other", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	otherHook := whCreate(t, ownerCtx, be)
	if err := runRepo(t, ownerCtx, "webhook", "create", "other", whURL); err != nil {
		t.Fatal(err)
	}
	otherHooks := whHooks(t, ownerCtx, be, "other")
	if got := len(otherHooks); got != 1 {
		t.Fatalf("len(otherHooks) = %v, want %v", got, 1)
	}
	id := otherHooks[0].ID
	if id == otherHook {
		t.Fatal("not true: id != otherHook")
	}

	if runRepo(t, ownerCtx, "webhook", "delete", "hooks", strconv.FormatInt(id, 10)) == nil {
		t.Fatal("not true: runRepo(t, ownerCtx, \"webhook\", \"delete\", \"hooks\", strconv.FormatInt(id, 10)) != nil")
	}
	if got := len(whHooks(t, ownerCtx, be, "other")); got != 1 {
		t.Fatalf("len(whHooks(t, ownerCtx, be, \"other\")) = %v, want %v", got, 1)
	}
}

func TestWebhookUpdateChangesOnlyRequestedFields(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be, "-s", "old", "-e", "push", "-c", "json", "-a=true")
	ids := strconv.FormatInt(id, 10)

	originalURL := whHooks(t, ownerCtx, be, "hooks")[0].URL

	if err := runRepo(t, ownerCtx, "webhook", "update", "hooks", ids, "-a=false"); err != nil {
		t.Fatal(err)
	}
	h := whHooks(t, ownerCtx, be, "hooks")[0]
	if got := h.Active; got != false {
		t.Fatalf("h.Active = %v, want %v", got, false)
	}
	if got := h.URL; got != originalURL {
		t.Fatalf("h.URL = %v, want %v", got, originalURL)
	}
	if got := h.Secret; got != "old" {
		t.Fatalf("h.Secret = %v, want %v", got, "old")
	}
	if got := h.ContentType; got != webhook.ContentTypeJSON {
		t.Fatalf("h.ContentType = %v, want %v", got, webhook.ContentTypeJSON)
	}
	if got, want := eventNames(h.Events), ([]string{"push"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("eventNames(h.Events) = %v, want %v", got, want)
	}
}

func TestWebhookUpdateChangesEveryFlag(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be, "-s", "old", "-e", "push", "-c", "json")
	ids := strconv.FormatInt(id, 10)

	if err := runRepo(t, ownerCtx, "webhook", "update", "hooks", ids,
		"-u", whURLOther+"/new", "-s", "new", "-c", "FORM", "-e", "repository", "-e", "collaborator", "-a", "false"); err != nil {
		t.Fatal(err)
	}

	h := whHooks(t, ownerCtx, be, "hooks")[0]
	if got, want := h.URL, whURLOther+"/new"; got != want {
		t.Fatalf("h.URL = %v, want %v", got, want)
	}
	if got := h.Secret; got != "new" {
		t.Fatalf("h.Secret = %v, want %v", got, "new")
	}
	if got := h.ContentType; got != webhook.ContentTypeForm {
		t.Fatalf("h.ContentType = %v, want %v", got, webhook.ContentTypeForm)
	}
	if got, want := eventNames(h.Events), ([]string{"collaborator", "repository"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("eventNames(h.Events) = %v, want %v", got, want)
	}
	if got := h.Active; got != false {
		t.Fatalf("h.Active = %v, want %v", got, false)
	}
}

func TestWebhookUpdateRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"bad active":       {"-a", "maybe"},
		"bad content type": {"-c", "xml"},
		"bad event":        {"-e", "bogus"},
		"private url":      {"-u", "http://10.0.0.1/x"},
		"loopback url":     {"-u", "http://127.0.0.1/x"},
		"bad url scheme":   {"-u", "gopher://93.184.216.34/x"},
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			id := whCreate(t, ownerCtx, be, "-e", "push")
			before := whSnapshot(t, ownerCtx, be)

			args := append([]string{"webhook", "update", "hooks", strconv.FormatInt(id, 10)}, flags...)
			_, _, err := runRepoOutput(t, ownerCtx, args...)
			if err == nil {
				t.Fatal("not true: err != nil")
			}
			if got := whSnapshot(t, ownerCtx, be); got != before {
				t.Fatalf("whSnapshot(t, ownerCtx, be) = %v, want %v", got, before)
			}
		})
	}
}

// --active must refuse anything that is not a boolean, with an error naming
// the flag, and leave the webhook as it was rather than switching it off.
func TestWebhookUpdateRejectsNonBooleanActive(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be, "-e", "push", "-a=true")
	ids := strconv.FormatInt(id, 10)
	before := whSnapshot(t, ownerCtx, be)

	for _, v := range []string{"yes", "no", "off", "maybe", "True "} {
		err := runRepo(t, ownerCtx, "webhook", "update", "hooks", ids, "--active", v)
		if err == nil || !strings.Contains(err.Error(), "--active: invalid boolean value") {
			t.Errorf("--active %q: err = %v, want the shared invalid boolean value error, naming the flag", v, err)
		}
		if got := whSnapshot(t, ownerCtx, be); got != before {
			t.Fatalf("whSnapshot(t, ownerCtx, be) = %v, want %v", got, before)
		}
	}
}

func TestWebhookUpdateRejectsBadIDs(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	whCreate(t, ownerCtx, be)

	err := runRepo(t, ownerCtx, "webhook", "update", "hooks", "abc", "-a=false")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if !strings.Contains(err.Error(), "invalid webhook ID") {
		t.Fatal("not true: strings.Contains(err.Error(), \"invalid webhook ID\")")
	}

	if runRepo(t, ownerCtx, "webhook", "update", "hooks", "999999", "-a=false") == nil {
		t.Fatal("not true: runRepo(t, ownerCtx, \"webhook\", \"update\", \"hooks\", \"999999\", \"-a=false\") != nil")
	}
}

func TestWebhookDeliveriesListShowsStatusRows(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)

	okID := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	badID := uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	noneID := uuid.MustParse("00000000-0000-0000-0000-00000000000c")
	whInsertDelivery(t, ctx, id, okID, 204)
	whInsertDelivery(t, ctx, id, badID, 500)
	whInsertDelivery(t, ctx, id, noneID, 0)

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "list", "hooks", strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{okID.String(), badID.String(), noneID.String(), "push"} {
		if !strings.Contains(stdout, want) {
			t.Fatal("not true: strings.Contains(stdout, want)")
		}
	}
	okRow := rowContaining(stdout, okID.String())
	badRow := rowContaining(stdout, badID.String())
	noneRow := rowContaining(stdout, noneID.String())
	if !strings.Contains(okRow, "✅") {
		t.Fatal("not true: strings.Contains(okRow, \"✅\")")
	}
	if !strings.Contains(badRow, "❌") {
		t.Fatal("not true: strings.Contains(badRow, \"❌\")")
	}
	if !strings.Contains(noneRow, "❌") {
		t.Fatal("not true: strings.Contains(noneRow, \"❌\")")
	}
}

// rowContaining returns the rendered table line that holds needle.
func rowContaining(out, needle string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func TestWebhookDeliveriesListWithoutDeliveries(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "list", "hooks", strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Status") {
		t.Fatal("not true: strings.Contains(stdout, \"Status\")")
	}
}

func TestWebhookDeliveriesRejectBadIDs(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be), 10)
	delID := "00000000-0000-0000-0000-000000000001"

	for _, args := range [][]string{
		{"webhook", "deliveries", "list", "hooks", "abc"},
		{"webhook", "deliveries", "get", "hooks", "abc", delID},
		{"webhook", "deliveries", "get", "hooks", id, "not-a-uuid"},
		{"webhook", "deliveries", "redeliver", "hooks", "abc", delID},
		{"webhook", "deliveries", "redeliver", "hooks", id, "not-a-uuid"},
	} {
		err := runRepo(t, ownerCtx, args...)
		if err == nil {
			t.Fatal("not true: err != nil")
		}
	}
}

func TestWebhookDeliveriesGetPrintsEverySection(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)
	delID := uuid.MustParse("00000000-0000-0000-0000-0000000000d1")
	whInsertDelivery(t, ctx, id, delID, 200)

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "get", "hooks", strconv.FormatInt(id, 10), delID.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ID: " + delID.String(),
		"Event: push",
		"Request URL: " + whURL,
		"Request Method: POST",
		"Request Headers:",
		"  X-SoftServe-Event: push",
		"Request Body:",
		`  {"ref":"main"}`,
		"Response Status: 200",
		"Response Headers:",
		"  X-Reply: yes",
		"Response Body:",
		"  reply body",
	} {
		// Whole-line match: "Event: push" is a substring of the header line
		// "X-SoftServe-Event: push", so substring matching cannot tell them apart.
		if !hasLine(stdout, want) {
			t.Fatal("not true: hasLine(stdout, want)")
		}
	}
}

// hasLine reports whether out contains a line equal to want.
func hasLine(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// Each list row must describe its own webhook: the events are comma-joined and
// the active flag matches that webhook.
func TestWebhookListRowsDescribeEachWebhook(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	const inactiveURL = whURL + "?n=inactive"
	const activeURL = whURL + "?n=active"
	if err := runRepo(t, ownerCtx, "webhook", "create", "hooks", inactiveURL, "-e", "push", "-e", "repository", "-a=false"); err != nil {
		t.Fatal(err)
	}
	if err := runRepo(t, ownerCtx, "webhook", "create", "hooks", activeURL, "-e", "collaborator"); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "list", "hooks")
	if err != nil {
		t.Fatal(err)
	}
	inactiveRow := rowContaining(stdout, inactiveURL)
	activeRow := rowContaining(stdout, activeURL)
	if !strings.Contains(inactiveRow, "push,repository") {
		t.Fatal("not true: strings.Contains(inactiveRow, \"push,repository\")")
	}
	if !strings.Contains(inactiveRow, "false") {
		t.Fatal("not true: strings.Contains(inactiveRow, \"false\")")
	}
	if strings.Contains(inactiveRow, "true") {
		t.Fatal("not true: !strings.Contains(inactiveRow, \"true\")")
	}
	if !strings.Contains(activeRow, "collaborator") {
		t.Fatal("not true: strings.Contains(activeRow, \"collaborator\")")
	}
	if !strings.Contains(activeRow, "true") {
		t.Fatal("not true: strings.Contains(activeRow, \"true\")")
	}
	if strings.Contains(activeRow, "false") {
		t.Fatal("not true: !strings.Contains(activeRow, \"false\")")
	}
}

// Updating only the URL or secret must keep an inactive webhook inactive.
func TestWebhookUpdateKeepsInactiveState(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be, "-a=false"), 10)

	if err := runRepo(t, ownerCtx, "webhook", "update", "hooks", id, "-s", "rotated"); err != nil {
		t.Fatal(err)
	}
	h := whHooks(t, ownerCtx, be, "hooks")[0]
	if got := h.Active; got != false {
		t.Fatalf("h.Active = %v, want %v", got, false)
	}
	if got := h.Secret; got != "rotated" {
		t.Fatalf("h.Secret = %v, want %v", got, "rotated")
	}
}

// Delivery rows are successful only for 2xx statuses.
func TestWebhookDeliveriesStatusBoundaries(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)

	cases := map[int]string{199: "❌", 200: "✅", 299: "✅", 300: "❌"}
	ids := map[int]uuid.UUID{}
	n := 0
	for status := range cases {
		n++
		ids[status] = uuid.MustParse("00000000-0000-0000-0000-0000000001" + strconv.Itoa(10+n))
		whInsertDelivery(t, ctx, id, ids[status], status)
	}

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "list", "hooks", strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatal(err)
	}
	for status, mark := range cases {
		row := rowContaining(stdout, ids[status].String())
		if !strings.Contains(row, mark) {
			t.Fatal("not true: strings.Contains(row, mark)")
		}
	}
}

func TestWebhookDeliveriesGetUnknownDelivery(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be), 10)

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "get", "hooks", id, "00000000-0000-0000-0000-0000000000ff")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if got := stdout; got != "" {
		t.Fatalf("stdout = %v, want %v", got, "")
	}
}

// A delivery belonging to one webhook must not be readable or redeliverable
// through another webhook of the same repository.
func TestWebhookDeliveriesAreScopedToWebhook(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	a := whCreate(t, ownerCtx, be)
	b := whCreate(t, ownerCtx, be)
	delID := uuid.MustParse("00000000-0000-0000-0000-0000000000e1")
	whInsertDelivery(t, ctx, a, delID, 200)

	bs := strconv.FormatInt(b, 10)
	_, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "get", "hooks", bs, delID.String())
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if runRepo(t, ownerCtx, "webhook", "deliveries", "redeliver", "hooks", bs, delID.String()) == nil {
		t.Fatal("not true: runRepo(t, ownerCtx, \"webhook\", \"deliveries\", \"redeliver\", \"hooks\", bs, delID.String()) != nil")
	}
}

func TestWebhookRedeliverRecordsNewDelivery(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)
	ids := strconv.FormatInt(id, 10)
	whPointAtLoopback(t, ownerCtx, "hooks", id)
	delID := uuid.MustParse("00000000-0000-0000-0000-0000000000f1")
	whInsertDelivery(t, ctx, id, delID, 200)

	if err := runRepo(t, ownerCtx, "webhook", "deliveries", "redeliver", "hooks", ids, delID.String()); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "list", "hooks", ids)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(stdout, "push"); got != 2 {
		t.Fatalf("strings.Count(stdout, \"push\") = %v, want %v", got, 2)
	}
}

func TestWebhookCommandsRequireAdminAccess(t *testing.T) {
	ctxBase, beBase := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctxBase, beBase)
	id := whCreate(t, ownerCtx, beBase)
	ids := strconv.FormatInt(id, 10)
	delID := "00000000-0000-0000-0000-0000000000a1"
	whInsertDelivery(t, ownerCtx, id, uuid.MustParse(delID), 200)

	reader := func(t *testing.T) context.Context {
		rc := withUser(t, ctxBase, beBase, "reader", false)
		if err := beBase.AddCollaborator(ownerCtx, "hooks", "reader", access.ReadOnlyAccess); err != nil {
			t.Fatal(err)
		}
		return rc
	}
	stranger := func(t *testing.T) context.Context {
		return withUser(t, ctxBase, beBase, "stranger", false)
	}

	for name, who := range map[string]func(*testing.T) context.Context{"read-only collaborator": reader, "stranger": stranger} {
		t.Run(name, func(t *testing.T) {
			ctx := who(t)
			before := whSnapshot(t, ownerCtx, beBase)

			for _, args := range [][]string{
				{"webhook", "list", "hooks"},
				{"webhook", "create", "hooks", whURL},
				{"webhook", "update", "hooks", ids, "-a=false"},
				{"webhook", "delete", "hooks", ids},
				{"webhook", "deliveries", "list", "hooks", ids},
				{"webhook", "deliveries", "get", "hooks", ids, delID},
				{"webhook", "deliveries", "redeliver", "hooks", ids, delID},
			} {
				err := runRepo(t, ctx, args...)
				if err == nil {
					t.Fatal("not true: err != nil")
				}
			}
			if got := whSnapshot(t, ownerCtx, beBase); got != before {
				t.Fatalf("whSnapshot(t, ownerCtx, beBase) = %v, want %v", got, before)
			}
		})
	}
}

func TestWebhookServerAdminCanManageAnyRepository(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	rootCtx := withUser(t, ctx, be, "root", true)

	if err := runRepo(t, rootCtx, "webhook", "create", "hooks", whURL, "-e", "push"); err != nil {
		t.Fatal(err)
	}
	id := whHooks(t, ownerCtx, be, "hooks")[0].ID
	ids := strconv.FormatInt(id, 10)

	if err := runRepo(t, rootCtx, "webhook", "update", "hooks", ids, "-a=false"); err != nil {
		t.Fatal(err)
	}
	if got := whHooks(t, ownerCtx, be, "hooks")[0].Active; got != false {
		t.Fatalf("whHooks(t, ownerCtx, be, \"hooks\")[0].Active = %v, want %v", got, false)
	}
	if err := runRepo(t, rootCtx, "webhook", "delete", "hooks", ids); err != nil {
		t.Fatal(err)
	}
	if got := len(whHooks(t, ownerCtx, be, "hooks")); got != 0 {
		t.Fatalf("len(whHooks(t, ownerCtx, be, \"hooks\")) = %v, want %v", got, 0)
	}
}

func TestWebhookCommandsOnMissingRepository(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	rootCtx := withUser(t, ctx, be, "root", true)

	for _, c := range []context.Context{ownerCtx, rootCtx} {
		if runRepo(t, c, "webhook", "list", "ghost") == nil {
			t.Fatal("not true: runRepo(t, c, \"webhook\", \"list\", \"ghost\") != nil")
		}
		if runRepo(t, c, "webhook", "create", "ghost", whURL) == nil {
			t.Fatal("not true: runRepo(t, c, \"webhook\", \"create\", \"ghost\", whURL) != nil")
		}
		if runRepo(t, c, "webhook", "delete", "ghost", "1") == nil {
			t.Fatal("not true: runRepo(t, c, \"webhook\", \"delete\", \"ghost\", \"1\") != nil")
		}
	}
}

func TestWebhookSecretIsNeverPrinted(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	const secret = "never-print-me"

	stdout, stderr, err := runRepoOutput(t, ownerCtx, "webhook", "create", "hooks", whURL, "-s", secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
		t.Fatal("not true: !strings.Contains(stdout, secret) && !strings.Contains(stderr, secret)")
	}
	id := strconv.FormatInt(whHooks(t, ownerCtx, be, "hooks")[0].ID, 10)

	stdout, stderr, err = runRepoOutput(t, ownerCtx, "webhook", "update", "hooks", id, "-s", secret+"2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
		t.Fatal("not true: !strings.Contains(stdout, secret) && !strings.Contains(stderr, secret)")
	}

	stdout, _, err = runRepoOutput(t, ownerCtx, "webhook", "list", "hooks")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, secret) {
		t.Fatal("not true: !strings.Contains(stdout, secret)")
	}
	if strings.Contains(stdout, secret+"2") {
		t.Fatal("not true: !strings.Contains(stdout, secret+\"2\")")
	}
}

// create trims surrounding whitespace from the URL, so update should accept
// the same padded URL and store it trimmed.
func TestWebhookUpdateTrimsURLLikeCreate(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be), 10)

	if err := runRepo(t, ownerCtx, "webhook", "update", "hooks", id, "-u", "  "+whURLOther+"/padded  "); err != nil {
		t.Fatal(err)
	}
	if got, want := whHooks(t, ownerCtx, be, "hooks")[0].URL, whURLOther+"/padded"; got != want {
		t.Fatalf("whHooks(t, ownerCtx, be, \"hooks\")[0].URL = %v, want %v", got, want)
	}
}

// requireRepo normally resolves the repository before RunE runs. Each leaf
// command must still refuse to run when no repository was resolved.
func TestWebhookRunEFailsWithoutResolvedRepository(t *testing.T) {
	t.Parallel()
	ctx, _ := newAuthTestContext(t)

	var leaves []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if len(c.Commands()) == 0 && c.RunE != nil {
			leaves = append(leaves, c)
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(cmdsOf(ctx).webhookCommand())
	// list, create, delete, update, deliveries list, redeliver, get.
	if got := len(leaves); got != 7 {
		t.Fatalf("len(leaves) = %v, want %v", got, 7)
	}

	for _, c := range leaves {
		c.SetContext(ctx)
		err := c.RunE(c, []string{"hooks", "1", "00000000-0000-0000-0000-000000000001"})
		if err == nil {
			t.Fatal("not true: err != nil")
		}
	}
}

func TestWebhookUpdateContentTypeJSON(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be, "-c", "form"), 10)

	if err := runRepo(t, ownerCtx, "webhook", "update", "hooks", id, "-c", " Json "); err != nil {
		t.Fatal(err)
	}
	if got := whHooks(t, ownerCtx, be, "hooks")[0].ContentType; got != webhook.ContentTypeJSON {
		t.Fatalf("whHooks(t, ownerCtx, be, \"hooks\")[0].ContentType = %v, want %v", got, webhook.ContentTypeJSON)
	}
}

// failingWebhookStore fails the webhook listing query and delegates everything
// else, so the repository check still passes and the list command runs.
type failingWebhookStore struct {
	store.Store
}

func (failingWebhookStore) GetWebhooksByRepoID(context.Context, db.Handler, int64) ([]models.Webhook, error) {
	return nil, errors.New("store unavailable")
}

func TestWebhookListReportsStoreFailure(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	// The backend lists webhooks through its own store, not the context's,
	// so the failing store goes into a backend over the same database.
	failStore := failingWebhookStore{Store: cmdsOf(ownerCtx).store}
	failBe := backend.New(cmdsOf(ownerCtx).cfg, cmdsOf(ownerCtx).db, failStore, cmdsOf(ownerCtx).logger)
	failCtx := withTestBackend(withTestStore(ownerCtx, failStore), failBe)

	stdout, _, err := runRepoOutput(t, failCtx, "webhook", "list", "hooks")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if !strings.Contains(err.Error(), "store unavailable") {
		t.Fatal("not true: strings.Contains(err.Error(), \"store unavailable\")")
	}
	if got := stdout; got != "" {
		t.Fatalf("stdout = %v, want %v", got, "")
	}
}

// whPushRefs pushes one commit to the "hooks" repository as the branches main,
// feature and other, and the tag v1. main is the default branch.
func whPushRefs(t *testing.T, ctx context.Context) {
	t.Helper()
	bare := filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "hooks.git")
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...) //nolint:noctx
		cmd.Dir = work
		// Ignore the user's git config, which may sign or annotate tags.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "first")
	git("tag", "v1")
	git("push", "-q", bare, "main", "main:feature", "main:other", "v1")
	if err := exec.Command("git", "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main").Run(); err != nil { //nolint:noctx
		t.Fatal(err)
	}
}

// Branch delete, tag delete and default branch change send their events
// through the backend's background deliveries: the session ending does not
// cancel them, and once shutdown has closed the queue no new one starts.
func TestBranchAndTagEventsUseBackgroundDeliveries(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	whPushRefs(t, ownerCtx)
	id := whCreate(t, ownerCtx, be, "-e", "branch_tag_delete", "-e", "repository")
	whPointAtLoopback(t, ownerCtx, "hooks", id)

	// Each command runs in a session context that is cancelled right after it.
	run := func(args ...string) {
		t.Helper()
		sess, cancel := context.WithCancel(ownerCtx)
		if err := runRepo(t, sess, args...); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	// Each change waits for the one before it to be recorded: under a
	// DELETE journal_mode data source a command's repository read waits out
	// a record's commit, and one longer than busy_timeout fails the command
	// with errLookupFailed.
	run("branch", "delete", "hooks", "feature")
	whAwaitDeliveries(t, ownerCtx, be, "hooks", id, 1)
	run("tag", "delete", "hooks", "v1")
	whAwaitDeliveries(t, ownerCtx, be, "hooks", id, 2)
	run("branch", "default", "hooks", "other")

	be.WaitForDeliveries(context.Background())
	r, err := be.Repository(ownerCtx, "hooks")
	if err != nil {
		t.Fatal(err)
	}
	events := func() []string {
		ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(ds))
		for _, d := range ds {
			out = append(out, d.Event.String())
		}
		sort.Strings(out)
		return out
	}
	if got, want := events(), ([]string{"branch_tag_delete", "branch_tag_delete", "repository"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("events() = %v, want %v", got, want)
	}

	// The queue is closed now, as during shutdown: the change still happens,
	// but its event is skipped.
	run("branch", "delete", "hooks", "main")
	if got, want := events(), ([]string{"branch_tag_delete", "branch_tag_delete", "repository"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("events() = %v, want %v", got, want)
	}
}

// An empty --secret or --events given to update clears the stored value;
// leaving the flag out keeps it.
func TestWebhookUpdateClearsSecretAndEvents(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be, "-s", "old", "-e", "push", "-e", "repository"), 10)

	if err := runRepo(t, ownerCtx, "webhook", "update", "hooks", id, "--secret="); err != nil {
		t.Fatal(err)
	}
	h := whHooks(t, ownerCtx, be, "hooks")[0]
	if got := h.Secret; got != "" {
		t.Fatalf("h.Secret = %v, want %v", got, "")
	}
	if got, want := eventNames(h.Events), ([]string{"push", "repository"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("eventNames(h.Events) = %v, want %v", got, want)
	}

	if err := runRepo(t, ownerCtx, "webhook", "update", "hooks", id, "-s", "new", "--events="); err != nil {
		t.Fatal(err)
	}
	h = whHooks(t, ownerCtx, be, "hooks")[0]
	if got := h.Secret; got != "new" {
		t.Fatalf("h.Secret = %v, want %v", got, "new")
	}
	if got := len(h.Events); got != 0 {
		t.Fatalf("len(h.Events) = %v, want %v", got, 0)
	}

	if err := runRepo(t, ownerCtx, "webhook", "update", "hooks", id, "-e", "push", "-s", ""); err != nil {
		t.Fatal(err)
	}
	h = whHooks(t, ownerCtx, be, "hooks")[0]
	if got := h.Secret; got != "" {
		t.Fatalf("h.Secret = %v, want %v", got, "")
	}
	if got, want := eventNames(h.Events), ([]string{"push"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("eventNames(h.Events) = %v, want %v", got, want)
	}
}

// An empty value for a flag that cannot be empty is refused, not silently
// treated as if the flag were left out.
func TestWebhookUpdateRejectsEmptyRequiredFlags(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		flag string
		want string
	}{
		"url":          {"--url=", "invalid"},
		"content type": {"--content-type=", webhook.ErrInvalidContentType.Error()},
		"active":       {"--active=", "--active: invalid boolean value"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			id := whCreate(t, ownerCtx, be, "-s", "keep", "-e", "push")
			before := whSnapshot(t, ownerCtx, be)

			_, _, err := runRepoOutput(t, ownerCtx, "webhook", "update", "hooks", strconv.FormatInt(id, 10), tc.flag)
			if err == nil {
				t.Fatal("not true: err != nil")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			if got := whSnapshot(t, ownerCtx, be); got != before {
				t.Fatalf("whSnapshot(t, ownerCtx, be) = %v, want %v", got, before)
			}
		})
	}
}

func TestParseWebhookContentType(t *testing.T) {
	t.Parallel()
	cases := map[string]webhook.ContentType{
		"json": webhook.ContentTypeJSON, " JSON ": webhook.ContentTypeJSON,
		"form": webhook.ContentTypeForm, "Form": webhook.ContentTypeForm,
	}
	for in, want := range cases {
		got, err := parseWebhookContentType(in)
		if err != nil || got != want {
			t.Errorf("parseWebhookContentType(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", " ", "xml", "application/json", "jsonx"} {
		if _, err := parseWebhookContentType(in); !errors.Is(err, webhook.ErrInvalidContentType) {
			t.Errorf("parseWebhookContentType(%q) error = %v; want ErrInvalidContentType", in, err)
		}
	}
}

// A default branch change says which branch was the default before.
func TestDefaultBranchEventNamesPreviousBranch(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	whPushRefs(t, ownerCtx)
	id := whCreate(t, ownerCtx, be, "-e", "repository")
	whPointAtLoopback(t, ownerCtx, "hooks", id)

	if err := runRepo(t, ownerCtx, "branch", "default", "hooks", "feature"); err != nil {
		t.Fatal(err)
	}
	be.WaitForDeliveries(context.Background())

	r, err := be.Repository(ownerCtx, "hooks")
	if err != nil {
		t.Fatal(err)
	}
	ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	d, err := be.WebhookDelivery(ownerCtx, r, id, ds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.RequestBody, `"action":"default_branch_change"`) {
		t.Fatal("not true: strings.Contains(d.RequestBody, `\"action\":\"default_branch_change\"`)")
	}
	if !strings.Contains(d.RequestBody, `"default_branch":"feature"`) {
		t.Fatal("not true: strings.Contains(d.RequestBody, `\"default_branch\":\"feature\"`)")
	}
	if !strings.Contains(d.RequestBody, `"changes":{"default_branch":{"from":"main"}}`) {
		t.Fatal("not true: strings.Contains(d.RequestBody, `\"changes\":{\"default_branch\":{\"from\":\"main\"}}`)")
	}
}

// Deleting an annotated tag sends the tag object as "before", the same id a
// push that deletes the tag sends, not the commit it points at.
func TestTagDeleteEventNamesTagObject(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	whPushRefs(t, ownerCtx)
	bare := filepath.Join(cmdsOf(ownerCtx).cfg.DataPath, "repos", "hooks.git")
	gitOut := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"--git-dir", bare, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...) //nolint:noctx
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitOut("tag", "-a", "-m", "annotated", "v2", "main")
	tagObj := gitOut("rev-parse", "refs/tags/v2")
	commit := gitOut("rev-parse", "refs/tags/v2^{commit}")
	if tagObj == commit {
		t.Fatal("not true: tagObj != commit")
	}

	id := whCreate(t, ownerCtx, be, "-e", "branch_tag_delete")
	whPointAtLoopback(t, ownerCtx, "hooks", id)
	if err := runRepo(t, ownerCtx, "tag", "delete", "hooks", "v2"); err != nil {
		t.Fatal(err)
	}
	be.WaitForDeliveries(context.Background())

	r, err := be.Repository(ownerCtx, "hooks")
	if err != nil {
		t.Fatal(err)
	}
	ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	d, err := be.WebhookDelivery(ownerCtx, r, id, ds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.RequestBody, `"before":"`+tagObj+`"`) {
		t.Fatal("not true: strings.Contains(d.RequestBody, `\"before\":\"`+tagObj+`\"`)")
	}
}

// cancelAfterRefChangeStore cancels the session context on the first user
// lookup made once changed reports that the command has changed the ref. It
// stands for an SSH client that disconnects right after the ref change, while
// the command builds the webhook event.
type cancelAfterRefChangeStore struct {
	store.Store

	changed func() bool

	mu     sync.Mutex
	cancel context.CancelFunc
}

func (s *cancelAfterRefChangeStore) GetUserByID(ctx context.Context, h db.Handler, id int64) (models.User, error) {
	s.mu.Lock()
	if s.cancel != nil && s.changed() {
		s.cancel()
		s.cancel = nil
	}
	s.mu.Unlock()
	return s.Store.GetUserByID(ctx, h, id)
}

// Branch delete, tag delete and default branch change still send their event
// when the session ends after the ref has changed but before the event is
// built.
func TestBranchAndTagEventsSentWhenSessionEndsAfterRefChange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		args    []string
		event   string
		changed func(t *testing.T, bare string) bool
	}{
		{
			name:  "branch delete",
			args:  []string{"branch", "delete", "hooks", "feature"},
			event: "branch_tag_delete",
			changed: func(t *testing.T, bare string) bool {
				t.Helper()
				return !gitRefExists(t, bare, "refs/heads/feature")
			},
		},
		{
			name:  "tag delete",
			args:  []string{"tag", "delete", "hooks", "v1"},
			event: "branch_tag_delete",
			changed: func(t *testing.T, bare string) bool {
				t.Helper()
				return !gitRefExists(t, bare, "refs/tags/v1")
			},
		},
		{
			name:  "branch default",
			args:  []string{"branch", "default", "hooks", "other"},
			event: "repository",
			changed: func(t *testing.T, bare string) bool {
				t.Helper()
				out, err := exec.CommandContext(t.Context(), "git", "--git-dir", bare, "symbolic-ref", "HEAD").Output()
				return err == nil && strings.TrimSpace(string(out)) == "refs/heads/other"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			whPushRefs(t, ownerCtx)
			id := whCreate(t, ownerCtx, be, "-e", "branch_tag_delete", "-e", "repository")
			whPointAtLoopback(t, ownerCtx, "hooks", id)
			bare := filepath.Join(cmdsOf(ownerCtx).cfg.DataPath, "repos", "hooks.git")

			sess, cancel := context.WithCancel(ownerCtx)
			defer cancel()
			st := &cancelAfterRefChangeStore{
				Store:   cmdsOf(ownerCtx).store,
				changed: func() bool { return tc.changed(t, bare) },
				cancel:  cancel,
			}
			// The event is built by the backend, so the session runs on one
			// whose store is st.
			stBe := backend.New(cmdsOf(ownerCtx).cfg, cmdsOf(ownerCtx).db, st, nil)
			sess = withTestBackend(withTestStore(sess, st), stBe)

			if err := runRepo(t, sess, tc.args...); err != nil {
				t.Fatal(err)
			}
			if sess.Err() == nil {
				t.Fatal("not true: sess.Err() != nil")
			} // the session ended after the ref change

			stBe.WaitForDeliveries(context.Background())
			r, err := be.Repository(ownerCtx, "hooks")
			if err != nil {
				t.Fatal(err)
			}
			ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
			if err != nil {
				t.Fatal(err)
			}
			events := make([]string, 0, len(ds))
			for _, d := range ds {
				events = append(events, d.Event.String())
			}
			if got, want := events, ([]string{tc.event}); !reflect.DeepEqual(got, want) {
				t.Fatalf("events = %v, want %v", got, want)
			}
		})
	}
}

// gitRefExists reports whether ref exists in the bare repository.
func gitRefExists(t *testing.T, bare, ref string) bool {
	t.Helper()
	return exec.CommandContext(t.Context(), "git", "--git-dir", bare, "rev-parse", "--verify", "-q", ref).Run() == nil
}
