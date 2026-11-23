package webhook

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/google/uuid"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

// testEnv is a fresh, migrated SQLite database, a Sender on it built the way
// the backend builds one, and the rows the tests need. Each test has its
// own, so the tests can run in parallel.
type testEnv struct {
	ctx   context.Context
	dbx   *db.DB
	ds    store.Store
	cfg   *config.Config
	s     *Sender
	owner models.User
	repo  models.Repo
}

// newRequest encodes payload in w's content type and builds the request
// that sends it, as a send does.
func (e *testEnv) newRequest(w models.Webhook, event Event, payload EventPayload) (request, error) {
	ct := ContentType(w.ContentType)
	body, err := encode(ct, payload)
	if err != nil {
		return request{}, err
	}
	return e.s.newRequest(w.Secret, ct, event, body, uuid.Nil)
}

// sendWebhook encodes payload in w's content type, sends it and records the
// attempt.
func (s *Sender) sendWebhook(ctx context.Context, w models.Webhook, event Event, payload EventPayload) error {
	return s.sendPayload(ctx, w, event, payload, true)
}

// testVersion is the server version of newTestEnv's Sender.
const testVersion = "1.2.3-test"

// newTestEnv builds a test environment. The owner user and a repository named
// "repo" owned by that user are created.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	is := is.New(t)

	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = filepath.Join(dp, "test.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	cfg.HTTP.PublicURL = "http://127.0.0.1:23232"
	cfg.SSH.PublicURL = "ssh://127.0.0.1:23231"
	cfg.Git.PublicURL = "git://127.0.0.1:9418"
	cfg.Version = testVersion

	ctx := context.Background()
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource, db.Options{})
	is.NoErr(err)
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	return newTestEnvOn(t, cfg, dbx)
}

// newTestEnvOnEitherDriver is newTestEnv on a database from dbtest.Open:
// Postgres when SOFT_SERVE_DB_DRIVER is "postgres", SQLite otherwise.
func newTestEnvOnEitherDriver(t *testing.T) *testEnv {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.HTTP.PublicURL = "http://127.0.0.1:23232"
	cfg.SSH.PublicURL = "ssh://127.0.0.1:23231"
	cfg.Git.PublicURL = "git://127.0.0.1:9418"
	dbx, err := dbtest.Open(context.Background(), t)
	if err != nil {
		t.Fatal(err)
	}
	return newTestEnvOn(t, cfg, dbx)
}

// newTestEnvOn migrates dbx and fills it with the rows of a testEnv.
func newTestEnvOn(t *testing.T, cfg *config.Config, dbx *db.DB) *testEnv {
	t.Helper()
	is := is.New(t)
	ctx := context.Background()

	is.NoErr(migrate.Migrate(ctx, dbx, cfg, nil))
	ds := database.New(nil)

	is.NoErr(ds.CreateUser(ctx, dbx, "alice", false, nil))
	owner, err := ds.FindUserByUsername(ctx, dbx, "alice")
	is.NoErr(err)

	if _, err := ds.CreateRepo(ctx, dbx, models.Repo{
		Name:        "repo",
		UserID:      owner.ID,
		ProjectName: "Project",
		Description: "a description",
	}); err != nil {
		t.Fatal(err)
	}
	repo, err := ds.GetRepoByName(ctx, dbx, "repo")
	is.NoErr(err)

	return &testEnv{ctx: ctx, dbx: dbx, ds: ds, cfg: cfg, s: NewSender(cfg, dbx, ds, nil), owner: owner, repo: repo}
}

// newWebhook stores an active webhook on the test repository that receives the
// given events, and returns it as the models.Webhook the sender uses.
func (e *testEnv) newWebhook(t *testing.T, url, secret string, contentType ContentType, events ...Event) models.Webhook {
	t.Helper()
	is := is.New(t)
	ids := make([]int, len(events))
	for i, ev := range events {
		ids[i] = int(ev)
	}
	id, err := e.ds.CreateWebhook(e.ctx, e.dbx, models.Webhook{RepoID: e.repo.ID, URL: url, Secret: secret, ContentType: int(contentType), Active: true})
	is.NoErr(err)
	is.NoErr(e.ds.CreateWebhookEvents(e.ctx, e.dbx, id, ids))
	w, err := e.ds.GetWebhookByID(e.ctx, e.dbx, e.repo.ID, id)
	is.NoErr(err)
	return w
}

// deliveries returns every recorded delivery for a webhook.
func (e *testEnv) deliveries(t *testing.T, webhookID int64) []models.WebhookDelivery {
	t.Helper()
	is := is.New(t)
	var ds []models.WebhookDelivery
	is.NoErr(e.dbx.SelectContext(e.ctx, &ds, e.dbx.Rebind("SELECT * FROM webhook_deliveries WHERE webhook_id = ? ORDER BY seq"), webhookID))
	return ds
}

// testClientTimeout caps each request made by the client useHTTPClient
// installs. It is above sendTimeout's default (ssrf.ClientTimeout), so a send
// is bounded by sendTimeout as in production, never by a tighter test value.
const testClientTimeout = time.Minute

// useHTTPClient gives the test's Sender, in place of the SSRF-protected
// client, one that reaches the in-memory receivers newTestServer starts (see
// memnet_test.go), and nothing else. Production always uses the protected
// client. Apart from the dialer and the whole-request cap, it is configured
// like ssrf.NewSecureClient: the same response header limits, and redirects
// are not followed. Each test gets its own transport, so no connection
// outlives it.
func (e *testEnv) useHTTPClient(t *testing.T) {
	t.Helper()
	tr := &http.Transport{
		DialContext:            dialMem,
		MaxIdleConns:           100,
		IdleConnTimeout:        90 * time.Second,
		MaxResponseHeaderBytes: ssrf.MaxResponseHeaderBytes,
		ResponseHeaderTimeout:  ssrf.ResponseHeaderTimeout,
	}
	e.s.client = &http.Client{
		Timeout:   testClientTimeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
}

// fakeUser implements proto.User.
type fakeUser struct {
	id   int64
	name string
}

func (u fakeUser) ID() int64                   { return u.id }
func (u fakeUser) Username() string            { return u.name }
func (u fakeUser) IsAdmin() bool               { return false }
func (u fakeUser) PublicKeys() []ssh.PublicKey { return nil }

// fakeRepo implements proto.Repository. Open returns a real git repository at
// path, or openErr when set, so error paths can be driven.
type fakeRepo struct {
	id      int64
	userID  int64
	name    string
	path    string
	openErr error
}

func (r fakeRepo) ID() int64            { return r.id }
func (r fakeRepo) Name() string         { return r.name }
func (r fakeRepo) ProjectName() string  { return "Project" }
func (r fakeRepo) Description() string  { return "a description" }
func (r fakeRepo) IsPrivate() bool      { return false }
func (r fakeRepo) IsMirror() bool       { return false }
func (r fakeRepo) IsHidden() bool       { return false }
func (r fakeRepo) UserID() int64        { return r.userID }
func (r fakeRepo) CreatedAt() time.Time { return time.Unix(1700000000, 0) }
func (r fakeRepo) UpdatedAt() time.Time { return time.Unix(1700000100, 0) }

func (r fakeRepo) Open(ctx context.Context) (*git.Repository, error) {
	if r.openErr != nil {
		return nil, r.openErr
	}
	return git.Open(ctx, r.path)
}

var (
	_ proto.User       = fakeUser{}
	_ proto.Repository = fakeRepo{}
)

// gitRepo creates a bare git repository with the given files committed to
// main, and returns its path and the commit hash of the tip.
func gitRepo(t *testing.T) (string, string) {
	t.Helper()
	is := is.New(t)
	dir := filepath.Join(t.TempDir(), "repo.git")
	_, err := git.Init(t.Context(), dir, true)
	is.NoErr(err)
	work := t.TempDir()
	runGit(t, work, "init", "-q", "-b", "main")
	runGit(t, work, "-c", "user.name=Alice", "-c", "user.email=alice@example.com",
		"commit", "-q", "--allow-empty", "-m", "first commit\n\nbody of first")
	runGit(t, work, "-c", "user.name=Alice", "-c", "user.email=alice@example.com",
		"commit", "-q", "--allow-empty", "-m", "second commit")
	runGit(t, work, "push", "-q", dir, "main")
	runGit(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	head := gitOutput(t, work, "rev-parse", "HEAD")
	return dir, head
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = gitOutput(t, dir, args...)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	is := is.New(t)
	cmd := exec.Command("git", args...) //nolint:noctx
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	is.NoErr(err)
	return strings.TrimSpace(string(out))
}
