package store

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/google/uuid"
)

// WebhookStore is an interface for managing webhooks.
type WebhookStore interface {
	// GetWebhookByID returns a webhook by its ID.
	GetWebhookByID(ctx context.Context, h db.Handler, repoID int64, id int64) (models.Webhook, error)
	// GetWebhooksByRepoID returns all webhooks for a repository.
	GetWebhooksByRepoID(ctx context.Context, h db.Handler, repoID int64) ([]models.Webhook, error)
	// LockAndCountWebhooks returns how many webhooks a repository has. On
	// Postgres it first locks the repository's row until the transaction
	// ends, so that concurrent callers that insert and then count in one
	// transaction take turns. On SQLite it only counts: a write transaction
	// holds the database's write lock from BEGIN IMMEDIATE (see db.Open), or
	// from its first write, so callers that insert and then count already
	// take turns.
	LockAndCountWebhooks(ctx context.Context, h db.Handler, repoID int64) (int, error)
	// GetWebhooksByRepoIDWhereEvent returns the active webhooks for a repository where event is in the events.
	// Inactive webhooks are left out, since events are not sent to them.
	GetWebhooksByRepoIDWhereEvent(ctx context.Context, h db.Handler, repoID int64, events []int) ([]models.Webhook, error)
	// GetActiveWebhookByIDWhereEvent returns the webhook id of a repository if
	// it is active and subscribes to event. It returns db.ErrRecordNotFound when the
	// webhook does not exist, is inactive, or does not subscribe to event.
	GetActiveWebhookByIDWhereEvent(ctx context.Context, h db.Handler, repoID int64, id int64, event int) (models.Webhook, error)
	// CreateWebhook creates a webhook from w's RepoID, URL, Secret,
	// ContentType and Active, and returns its ID. The other fields are
	// ignored.
	CreateWebhook(ctx context.Context, h db.Handler, w models.Webhook) (int64, error)
	// UpdateWebhookByID sets the URL, Secret, ContentType and Active of the
	// webhook w.ID of repository w.RepoID. It returns db.ErrRecordNotFound
	// when that repository has no such webhook.
	UpdateWebhookByID(ctx context.Context, h db.Handler, w models.Webhook) error
	// DeleteWebhookForRepoByID deletes a webhook for a repository by its ID,
	// with its events and deliveries.
	DeleteWebhookForRepoByID(ctx context.Context, h db.Handler, repoID int64, id int64) error

	// GetWebhookEventsByWebhookID returns all webhook events for a webhook.
	GetWebhookEventsByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookEvent, error)
	// CreateWebhookEvents creates webhook events for a webhook.
	CreateWebhookEvents(ctx context.Context, h db.Handler, webhookID int64, events []int) error
	// DeleteWebhookEventsByID deletes the webhook events with the given IDs
	// that belong to webhookID. IDs of other webhooks' events are ignored.
	DeleteWebhookEventsByID(ctx context.Context, h db.Handler, webhookID int64, ids []int64) error

	// GetWebhookDeliveryByID returns a webhook delivery by its ID.
	GetWebhookDeliveryByID(ctx context.Context, h db.Handler, webhookID int64, id uuid.UUID) (models.WebhookDelivery, error)
	// ListWebhookDeliveriesByWebhookID returns the most recent webhook
	// deliveries for a webhook, at most 100, oldest first. This only returns the delivery ID, response
	// status, event, creation time, and sequence number.
	ListWebhookDeliveriesByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookDelivery, error)
	// CreateWebhookDelivery creates a webhook delivery from d; d's CreatedAt
	// and Seq are ignored and set by the database. If the webhook no longer
	// exists it writes nothing and returns an error matching db.ErrForeignKey.
	CreateWebhookDelivery(ctx context.Context, h db.Handler, d models.WebhookDelivery) error
	// PruneWebhookDeliveries deletes all but the keep most recent deliveries
	// of a webhook, in the order they were recorded (by seq).
	PruneWebhookDeliveries(ctx context.Context, h db.Handler, webhookID int64, keep int) error
}
