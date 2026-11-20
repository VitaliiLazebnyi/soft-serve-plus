package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// afterPush runs the server's work after a push to e.repo by user that
// updated refs, and waits until the deliveries it queued have run.
func (e whEnv) afterPush(t *testing.T, user proto.User, refs []hooks.HookArg) {
	t.Helper()
	e.be.AfterPush(e.ctx, e.repo, user, refs)
	e.drainDeliveries(t)
}

// drainDeliveries waits until no delivery is queued or running.
func (e whEnv) drainDeliveries(t *testing.T) {
	t.Helper()
	_, idle := e.be.deliveries.pending()
	select {
	case <-idle:
	case <-time.After(time.Minute):
		t.Fatal("timed out waiting for the webhook deliveries")
	}
}

// pushPayload is the part of a push event payload the tests read.
type pushPayload struct {
	Sender webhook.User `json:"sender"`
	Pusher webhook.User `json:"pusher"`
	Ref    string       `json:"ref"`
}

// deliveredPushes returns the payloads of the push deliveries recorded for
// a webhook, oldest first.
func (e whEnv) deliveredPushes(t *testing.T, hookID int64) []pushPayload {
	t.Helper()
	is := is.New(t)
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, hookID)
	is.NoErr(err)
	var out []pushPayload
	for _, d := range list {
		if d.Event != webhook.EventPush {
			continue
		}
		// The list leaves the bodies out.
		full, err := e.be.WebhookDelivery(e.ctx, e.repo, hookID, d.ID)
		is.NoErr(err)
		var p pushPayload
		is.NoErr(json.Unmarshal([]byte(full.RequestBody), &p))
		out = append(out, p)
	}
	return out
}

// The events of a push name the account the transport authenticated, by
// its ID, even when that account was renamed and another took its name
// before the events were built. Regression (KNOWN_ISSUES 326(b)): the hook
// process looked the pusher up by the name in SOFT_SERVE_USERNAME, and
// named the other account.
func TestAfterPushSenderIsThePushingAccount(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	alice, err := e.be.CreateUser(e.ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(e.be.SetUsername(e.ctx, "alice", "alice-old"))
	other, err := e.be.CreateUser(e.ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	id := e.seedHook(t, webhook.EventPush)
	commit := makeCommit(t, e.be.repoPath("repo"))

	e.afterPush(t, alice, []hooks.HookArg{{OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main"}})

	got := e.deliveredPushes(t, id)
	is.Equal(len(got), 1)
	is.Equal(got[0].Sender.ID, alice.ID())
	is.Equal(got[0].Pusher.ID, alice.ID())
	is.True(got[0].Sender.ID != other.ID())
}

// A push with no account, such as an anonymous one or a key with no user
// row, sends its events with an empty sender.
func TestAfterPushWithoutAccountHasEmptySender(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventPush)
	commit := makeCommit(t, e.be.repoPath("repo"))

	e.afterPush(t, nil, []hooks.HookArg{{OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main"}})

	got := e.deliveredPushes(t, id)
	is.Equal(len(got), 1)
	is.Equal(got[0].Sender, webhook.User{})
	is.Equal(got[0].Pusher, webhook.User{})
}

// The push's events go through the delivery queue, behind the repository's
// earlier events, so AfterPush does not wait on receivers (R53): with the
// repository's one sender busy, AfterPush returns before any of them is
// sent, and they are sent once the sender is free.
func TestAfterPushQueuesEventsBehindTheRepositorysOthers(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventPush)
	commit := makeCommit(t, e.be.repoPath("repo"))

	release := make(chan struct{})
	is.Equal(e.be.deliveries.enqueue(e.repo.ID(), func() { <-release }), enqueued)

	e.be.AfterPush(e.ctx, e.repo, e.admin, []hooks.HookArg{{OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main"}})
	// The blocking job holds the repository's only sender, so the push's
	// job cannot have run yet.
	is.Equal(len(e.deliveredEvents(t, id)), 0)
	pending, _ := e.be.deliveries.pending()
	is.Equal(pending, 2)

	close(release)
	e.drainDeliveries(t)
	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventBranchTagCreate, webhook.EventPush})
}

// A push queued before shutdown starts is still sent: WaitForDeliveries runs
// the queued jobs to the end of its wait, and a push job is one of them.
// Regression (review round 1): the job stopped as soon as the queue was
// closed, so the push lost every event, recorded nowhere.
func TestAfterPushQueuedBeforeShutdownIsSent(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventPush)
	commit := makeCommit(t, e.be.repoPath("repo"))

	release := make(chan struct{})
	is.Equal(e.be.deliveries.enqueue(e.repo.ID(), func() { <-release }), enqueued)
	e.be.AfterPush(e.ctx, e.repo, e.admin, []hooks.HookArg{{OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main"}})

	// Shutdown starts while the push job waits behind the blocker.
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		e.be.WaitForDeliveries(ctx)
	}()
	waitFor(t, "the queue to close", e.be.deliveries.closedForTest)
	close(release)
	select {
	case <-waited:
	case <-time.After(time.Minute):
		t.Fatal("timed out waiting for shutdown")
	}

	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventBranchTagCreate, webhook.EventPush})
}

// A push to a repository without webhooks for its events builds no payload
// and queues nothing (R55): the owner row is broken here, so building one
// would log an error.
func TestAfterPushWithoutSubscribersBuildsNothing(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventRepository)
	commit := makeCommit(t, e.be.repoPath("repo"))
	corruptOwner(t, e)

	e.be.AfterPush(e.ctx, e.repo, e.admin, []hooks.HookArg{
		{OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main"},
		{OldSha: zeroSha, NewSha: commit, RefName: "refs/tags/v1"},
	})

	pending, _ := e.be.deliveries.pending()
	is.Equal(pending, 0)
	is.True(!strings.Contains(e.logs.String(), "webhook"))
}

// A payload that cannot be built is logged and skipped; the push's other
// events are still sent.
func TestAfterPushSkipsAnEventItCannotBuild(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventPush)
	commit := makeCommit(t, e.be.repoPath("repo"))

	e.afterPush(t, e.admin, []hooks.HookArg{
		// No such commit, so its log cannot be read.
		{OldSha: someSha, NewSha: strings.Repeat("2", 40), RefName: "refs/heads/broken"},
		{OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main"},
	})

	is.True(strings.Contains(e.logs.String(), "error creating push webhook"))
	got := e.deliveredPushes(t, id)
	is.Equal(len(got), 1)
	is.Equal(got[0].Ref, "refs/heads/main")
}

// When the subscribers cannot be read, the failure is logged and nothing is
// sent; the push itself has already happened.
func TestAfterPushSurvivesSubscriberLookupFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventPush)
	commit := makeCommit(t, e.be.repoPath("repo"))
	_, err := e.be.db.ExecContext(e.ctx, "DROP TABLE webhooks")
	is.NoErr(err)

	e.afterPush(t, e.admin, []hooks.HookArg{{OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main"}})

	is.True(strings.Contains(e.logs.String(), "push webhook events not sent: reading the subscribers failed"))
}

// A repository deleted before its push's events are sent sends none.
func TestAfterPushRepositoryGoneSendsNothing(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventPush)
	commit := makeCommit(t, e.be.repoPath("repo"))

	release := make(chan struct{})
	is.Equal(e.be.deliveries.enqueue(e.repo.ID(), func() { <-release }), enqueued)
	e.be.AfterPush(e.ctx, e.repo, e.admin, []hooks.HookArg{{OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main"}})
	_, err := e.be.db.ExecContext(e.ctx, e.be.db.Rebind("DELETE FROM repos WHERE id = ?"), e.repo.ID())
	is.NoErr(err)
	close(release)
	e.drainDeliveries(t)

	is.True(!strings.Contains(e.logs.String(), "webhook event not delivered"))
}

// The post-update work: a push that updated refs writes info/last-modified;
// one that updated none leaves it alone.
func TestAfterPushWritesLastModified(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	commit := makeCommit(t, e.be.repoPath("repo"))
	lm := filepath.Join(e.be.repoPath("repo"), "info", "last-modified")

	e.afterPush(t, e.admin, nil)
	_, err := os.Stat(lm)
	is.True(os.IsNotExist(err))

	e.afterPush(t, e.admin, []hooks.HookArg{{OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main"}})
	b, err := os.ReadFile(lm)
	is.NoErr(err)
	_, err = time.Parse(time.RFC3339, string(b))
	is.NoErr(err)
}
