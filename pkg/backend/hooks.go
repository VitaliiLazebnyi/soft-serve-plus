package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

// AfterPush does, in the server, the work the post-receive and post-update
// hooks used to do in a hook process, for a push to repo by user that
// updated refs. The transport calls it once receive-pack has exited, with
// the refs git gave post-receive (see hooks.PushRefs): only the refs it
// actually updated, so a ref that git or a hook rejected sends nothing.
//
// It writes the repository's info/last-modified and queues the push and
// branch/tag webhook events on the server's bounded delivery queue, behind
// the repository's other events. The events name user, the account the
// transport authenticated, as their sender and pusher: nil, for an
// anonymous or keyless push or a key with no account, leaves them empty.
//
// The push has happened, so a failure is logged rather than returned, and a
// caller that has gone away does not stop it (see AfterChangeContext).
func (d *Backend) AfterPush(ctx context.Context, repo proto.Repository, user proto.User, refs []hooks.HookArg) {
	if repo == nil || len(refs) == 0 {
		return
	}
	ctx, cancel := AfterChangeContext(ctx)
	defer cancel()

	if err := d.populateLastModified(ctx, repo.Name()); err != nil {
		d.logger.Error("error populating last-modified", "repo", repo.Name(), "err", err)
	}
	d.queuePushEvents(d.webhookContext(ctx), repo, user, refs)
}

// pushSubscribers are the active webhooks of a repository that subscribe to
// each event a push sends.
type pushSubscribers map[webhook.Event][]models.Webhook

// pushEvents are the events a push sends, in the order it sends them for
// each ref.
var pushEvents = []webhook.Event{webhook.EventBranchTagCreate, webhook.EventBranchTagDelete, webhook.EventPush}

// queuePushEvents queues the webhook events of a push to repo on the
// delivery queue, as one job that sends them in order. The subscribers are
// read once, and a push to a repository without any sends nothing and
// builds no payload.
func (d *Backend) queuePushEvents(ctx context.Context, repo proto.Repository, user proto.User, refs []hooks.HookArg) {
	subs := pushSubscribers{}
	for _, ev := range pushEvents {
		whs, err := d.store.GetWebhooksByRepoIDWhereEvent(ctx, d.db, repo.ID(), []int{int(ev)})
		if err != nil {
			d.logger.Error("push webhook events not sent: reading the subscribers failed", "repo", repo.Name(), "event", ev, "err", err)
			return
		}
		if len(whs) > 0 {
			subs[ev] = whs
		}
	}
	if len(subs) == 0 {
		return
	}

	bg := context.WithoutCancel(ctx)
	job := func() {
		d.sendPushEvents(bg, repo.ID(), user, refs, subs)
	}
	var reason error
	switch d.deliveries.enqueue(repo.ID(), job) {
	case enqueueClosed:
		d.logger.Warn("push webhook events skipped: server is shutting down", "repo_id", repo.ID(), "refs", len(refs))
	case enqueueFull:
		d.logger.Error("push webhook events dropped: delivery queue is full", "repo_id", repo.ID(), "refs", len(refs), "queued", maxQueuedDeliveries)
		reason = errDeliveryQueueFull
	case enqueueRepoFull:
		d.logger.Error("push webhook events dropped: repository has too many pending deliveries", "repo_id", repo.ID(), "refs", len(refs), "pending", maxRepoDeliveries)
		reason = errRepoDeliveriesFull
	}
	if reason != nil {
		// As for other events, a dropped event is recorded so it can be
		// redelivered. ctx bounds the time spent building them, in the
		// request path; the refs left when it runs out are logged.
		done := d.forEachPushEvent(ctx, repo, user, refs, subs, func(whs []models.Webhook, ev webhook.EventPayload) bool {
			if ctx.Err() != nil {
				return false
			}
			if err := webhook.RecordUndelivered(bg, whs, ev, reason); err != nil {
				d.logger.Error("dropped webhook event not recorded", "event", ev.Event(), "repo_id", ev.RepositoryID(), "err", err)
			}
			return true
		})
		if left := len(refs) - done; left > 0 {
			d.logger.Error("dropped push webhook events not recorded: out of time", "repo_id", repo.ID(), "refs", len(refs), "refs_not_recorded", left, "err", ctx.Err())
		}
	}
}

// sendPushEvents builds and sends the events of a push to the repository
// with ID id. It runs on the delivery queue. The repository is read again,
// so the payloads describe it as it is now, under its current name. Each
// payload is built just before it is sent, so a push of many refs holds one
// at a time. A job queued before shutdown runs like any other; shutdown's
// wait for the queue (WaitForDeliveries) bounds it.
func (d *Backend) sendPushEvents(ctx context.Context, id int64, user proto.User, refs []hooks.HookArg, subs pushSubscribers) {
	m, err := d.store.GetRepoByID(ctx, d.db, id)
	if errors.Is(err, db.ErrRecordNotFound) {
		d.logger.Debug("push webhook events not sent: the repository is gone", "repo_id", id)
		return
	}
	if err != nil {
		d.logger.Error("push webhook events not sent: reading the repository failed", "repo_id", id, "err", err)
		return
	}
	repo := d.repoFromRow(m.Name, m)
	d.forEachPushEvent(ctx, repo, user, refs, subs, func(whs []models.Webhook, ev webhook.EventPayload) bool {
		if err := webhook.Deliver(ctx, whs, ev); err != nil {
			d.logEventNotDelivered(ev, err)
		}
		return true
	})
}

// forEachPushEvent builds, for each ref in order, the branch/tag event of a
// created or deleted ref and then the push event, each only when it has
// subscribers, and calls fn with them. It stops when fn returns false or
// ctx is done, and returns how many refs it went through to the end. A
// payload that cannot be built is logged and skipped.
func (d *Backend) forEachPushEvent(ctx context.Context, repo proto.Repository, user proto.User, refs []hooks.HookArg, subs pushSubscribers, fn func([]models.Webhook, webhook.EventPayload) bool) int {
	for i, ref := range refs {
		if ctx.Err() != nil {
			return i
		}
		var ev webhook.Event
		switch {
		case git.IsZeroHash(ref.OldSha):
			ev = webhook.EventBranchTagCreate
		case git.IsZeroHash(ref.NewSha):
			ev = webhook.EventBranchTagDelete
		}
		if whs := subs[ev]; ev != 0 && len(whs) > 0 {
			if payload, err := d.buildPushEvent(ctx, ev, repo, user, ref); err != nil {
				d.logger.Error("error creating branch_tag webhook", "repo", repo.Name(), "ref", ref.RefName, "err", err)
			} else if !fn(whs, payload) {
				return i
			}
		}
		if whs := subs[webhook.EventPush]; len(whs) > 0 {
			if payload, err := d.buildPushEvent(ctx, webhook.EventPush, repo, user, ref); err != nil {
				d.logger.Error("error creating push webhook", "repo", repo.Name(), "ref", ref.RefName, "err", err)
			} else if !fn(whs, payload) {
				return i
			}
		}
	}
	return len(refs)
}

// buildPushEvent builds the payload of event ev for ref, giving it at most
// afterChangeTimeout.
func (d *Backend) buildPushEvent(ctx context.Context, ev webhook.Event, repo proto.Repository, user proto.User, ref hooks.HookArg) (webhook.EventPayload, error) {
	ctx, cancel := context.WithTimeout(ctx, afterChangeTimeout)
	defer cancel()
	switch ev {
	case webhook.EventPush:
		return webhook.NewPushEvent(ctx, user, repo, ref.RefName, ref.OldSha, ref.NewSha)
	case webhook.EventBranchTagCreate, webhook.EventBranchTagDelete:
		return webhook.NewBranchTagEvent(ctx, user, repo, ref.RefName, ref.OldSha, ref.NewSha)
	}
	return nil, fmt.Errorf("not a push event: %v", ev)
}
