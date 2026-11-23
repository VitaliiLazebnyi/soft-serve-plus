package backend

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/google/uuid"
)

// Unknown webhook and delivery IDs are reported with their own not-found
// errors, not the driver's "sql: no rows in result set".
func TestWebhookNotFoundSentinels(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	const missing = 424242

	_, err := e.be.Webhook(e.ctx, e.repo, missing)
	if !errors.Is(err, proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrWebhookNotFound)")
	}
	if !errors.Is(e.be.UpdateWebhook(e.ctx, e.repo, missing, WebhookInput{URL: whURL, ContentType: 1, Active: true}), proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(e.be.UpdateWebhook(e.ctx, e.repo, missing, WebhookInput{URL: whURL, ContentType: 1, Active: true}), proto.ErrWebhookNotFound)")
	}
	if !errors.Is(e.be.DeleteWebhook(e.ctx, e.repo, missing), proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(e.be.DeleteWebhook(e.ctx, e.repo, missing), proto.ErrWebhookNotFound)")
	}
	_, err = e.be.ListWebhookDeliveries(e.ctx, e.repo, missing)
	if !errors.Is(err, proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrWebhookNotFound)")
	}
	_, err = e.be.WebhookDelivery(e.ctx, e.repo, missing, uuid.New())
	if !errors.Is(err, proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrWebhookNotFound)")
	}
	if !errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, missing, uuid.New()), proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, missing, uuid.New()), proto.ErrWebhookNotFound)")
	}

	id := e.seedHook(t)
	_, err = e.be.WebhookDelivery(e.ctx, e.repo, id, uuid.New())
	if !errors.Is(err, proto.ErrDeliveryNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrDeliveryNotFound)")
	}
	if !errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, uuid.New()), proto.ErrDeliveryNotFound) {
		t.Fatal("not true: errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, uuid.New()), proto.ErrDeliveryNotFound)")
	}

	// A mistyped ID is the caller's mistake, not a server error.
	if strings.Contains(e.logs.String(), "error getting webhook") {
		t.Fatal("not true: !strings.Contains(e.logs.String(), \"error getting webhook\")")
	}
}

// Renaming onto a name whose row exists without a directory says the name
// is taken, as creating it does, not the driver's constraint error.
func TestRenameOntoRowWithoutDirectory(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	admin := defaultAdmin(t, be)
	for _, name := range []string{"a", "taken"} {
		_, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(be.repoPath("taken")); err != nil {
		t.Fatal(err)
	}

	if !errors.Is(be.RenameRepository(ctx, "a", "taken"), proto.ErrRepoExist) {
		t.Fatal("not true: errors.Is(be.RenameRepository(ctx, \"a\", \"taken\"), proto.ErrRepoExist)")
	}
	r, err := be.Repository(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	} // the directory moved back with the claim given up
}

// failingCreateStore fails every repository insert and repository count
// with err, as a lost database connection would.
type failingCreateStore struct {
	store.Store
	err error
}

func (s failingCreateStore) CreateRepo(context.Context, db.Handler, models.Repo) (models.Repo, error) {
	return models.Repo{}, s.err
}

func (s failingCreateStore) CountUserRepos(context.Context, db.Handler, int64) (int64, error) {
	return 0, s.err
}

// A create, import or rename that fails for a reason other than a refusal
// tells the client only that the server could not do it; the cause is
// logged. The driver's error used to reach the SSH client unchanged.
func TestRepositoryChangesHideServerFaults(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	cfg.MaxReposPerUser = 5
	ctx := backendTestContext(be, cfg)
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("pq: connection to 10.0.0.7:5432 reset")
	be.store = failingCreateStore{Store: be.store, err: cause}

	_, err = be.CreateRepository(ctx, "new", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrServerFault) {
		t.Fatal("not true: errors.Is(err, proto.ErrServerFault)")
	}
	_, err = be.ImportRepository(ctx, "imported", alice, "https://1.1.1.1/x.git", proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrServerFault) {
		t.Fatal("not true: errors.Is(err, proto.ErrServerFault)")
	}
	if !strings.Contains(logs.String(), "10.0.0.7") {
		t.Fatal("not true: strings.Contains(logs.String(), \"10.0.0.7\")")
	} // the cause is logged

	// Refusals are returned as they are, with their own text.
	_, err = be.CreateRepository(ctx, "-bad", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrInvalidRepo) {
		t.Fatal("not true: errors.Is(err, proto.ErrInvalidRepo)")
	}
	if got := err.Error(); got != "repo path segments cannot start with a hyphen" {
		t.Fatalf("err.Error() = %v, want %v", got, "repo path segments cannot start with a hyphen")
	}
	if !errors.Is(be.RenameRepository(asAdmin(t, be, ctx), "missing", "other"), proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(be.RenameRepository(asAdmin(t, be, ctx), \"missing\", \"other\"), proto.ErrRepoNotFound)")
	}
}

// cancellingCreateStore fails the repository insert with err after ending
// the caller's context, as a statement the client's disconnect cancelled
// does.
type cancellingCreateStore struct {
	store.Store
	cancel context.CancelFunc
	err    error
}

func (s cancellingCreateStore) CreateRepo(context.Context, db.Handler, models.Repo) (models.Repo, error) {
	s.cancel()
	return models.Repo{}, s.err
}

// A fault that came with the caller's context ending is the client going
// away, not a server fault: it is logged at Debug, not Error.
func TestRepositoryChangesCancelledNotLoggedAsFault(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx, cancel := context.WithCancel(backendTestContext(be, cfg))
	defer cancel()
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	be.store = cancellingCreateStore{Store: be.store, cancel: cancel, err: errors.New("pq: canceling statement due to user request")}

	_, err = be.CreateRepository(ctx, "new", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrServerFault) {
		t.Fatal("not true: errors.Is(err, proto.ErrServerFault)")
	}
	if strings.Contains(logs.String(), "failed to create repository") {
		t.Fatal("not true: !strings.Contains(logs.String(), \"failed to create repository\")")
	}
}

// The cause of a failed create is logged once.
func TestCreateRepositoryFaultLoggedOnce(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	be.logger.SetLevel(log.DebugLevel)
	ctx := backendTestContext(be, cfg)
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	be.store = failingCreateStore{Store: be.store, err: errors.New("pq: connection to 10.0.0.7:5432 reset")}

	_, err = be.CreateRepository(ctx, "new", alice, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrServerFault) {
		t.Fatal("not true: errors.Is(err, proto.ErrServerFault)")
	}
	if got := strings.Count(logs.String(), "10.0.0.7"); got != 1 {
		t.Fatalf("strings.Count(logs.String(), \"10.0.0.7\") = %v, want %v", got, 1)
	}
}
