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
	"github.com/matryer/is"
)

// Unknown webhook and delivery IDs are reported with their own not-found
// errors, not the driver's "sql: no rows in result set".
func TestWebhookNotFoundSentinels(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newWebhookEnv(t)
	const missing = 424242

	_, err := e.be.Webhook(e.ctx, e.repo, missing)
	is.True(errors.Is(err, proto.ErrWebhookNotFound))
	is.True(errors.Is(e.be.UpdateWebhook(e.ctx, e.repo, missing, WebhookInput{URL: whURL, ContentType: 1, Active: true}), proto.ErrWebhookNotFound))
	is.True(errors.Is(e.be.DeleteWebhook(e.ctx, e.repo, missing), proto.ErrWebhookNotFound))
	_, err = e.be.ListWebhookDeliveries(e.ctx, e.repo, missing)
	is.True(errors.Is(err, proto.ErrWebhookNotFound))
	_, err = e.be.WebhookDelivery(e.ctx, e.repo, missing, uuid.New())
	is.True(errors.Is(err, proto.ErrWebhookNotFound))
	is.True(errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, missing, uuid.New()), proto.ErrWebhookNotFound))

	id := e.seedHook(t)
	_, err = e.be.WebhookDelivery(e.ctx, e.repo, id, uuid.New())
	is.True(errors.Is(err, proto.ErrDeliveryNotFound))
	is.True(errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, uuid.New()), proto.ErrDeliveryNotFound))

	// A mistyped ID is the caller's mistake, not a server error.
	is.True(!strings.Contains(e.logs.String(), "error getting webhook"))
}

// Renaming onto a name whose row exists without a directory says the name
// is taken, as creating it does, not the driver's constraint error.
func TestRenameOntoRowWithoutDirectory(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	admin := defaultAdmin(t, be)
	for _, name := range []string{"a", "taken"} {
		_, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{})
		is.NoErr(err)
	}
	is.NoErr(os.RemoveAll(be.repoPath("taken")))

	is.True(errors.Is(be.RenameRepository(ctx, "a", "taken"), proto.ErrRepoExist))
	r, err := be.Repository(ctx, "a")
	is.NoErr(err)
	_, err = r.Open(t.Context())
	is.NoErr(err) // the directory moved back with the claim given up
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
	is := is.New(t)
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	cfg.MaxReposPerUser = 5
	ctx := backendTestContext(be, cfg)
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	cause := errors.New("pq: connection to 10.0.0.7:5432 reset")
	be.store = failingCreateStore{Store: be.store, err: cause}

	_, err = be.CreateRepository(ctx, "new", alice, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrServerFault))
	_, err = be.ImportRepository(ctx, "imported", alice, "https://1.1.1.1/x.git", proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrServerFault))
	is.True(strings.Contains(logs.String(), "10.0.0.7")) // the cause is logged

	// Refusals are returned as they are, with their own text.
	_, err = be.CreateRepository(ctx, "-bad", alice, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrInvalidRepo))
	is.Equal(err.Error(), "repo path segments cannot start with a hyphen")
	is.True(errors.Is(be.RenameRepository(asAdmin(t, be, ctx), "missing", "other"), proto.ErrRepoNotFound))
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
	is := is.New(t)
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx, cancel := context.WithCancel(backendTestContext(be, cfg))
	defer cancel()
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	be.store = cancellingCreateStore{Store: be.store, cancel: cancel, err: errors.New("pq: canceling statement due to user request")}

	_, err = be.CreateRepository(ctx, "new", alice, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrServerFault))
	is.True(!strings.Contains(logs.String(), "failed to create repository"))
}

// The cause of a failed create is logged once.
func TestCreateRepositoryFaultLoggedOnce(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	be.logger.SetLevel(log.DebugLevel)
	ctx := backendTestContext(be, cfg)
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	be.store = failingCreateStore{Store: be.store, err: errors.New("pq: connection to 10.0.0.7:5432 reset")}

	_, err = be.CreateRepository(ctx, "new", alice, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrServerFault))
	is.Equal(strings.Count(logs.String(), "10.0.0.7"), 1)
}
