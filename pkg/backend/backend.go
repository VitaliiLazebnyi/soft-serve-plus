package backend

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

// Backend is the Soft Serve backend that handles users, repositories, and
// server settings management and operations.
type Backend struct {
	cfg    *config.Config
	db     *db.DB
	store  store.Store
	logger *log.Logger

	// importsDisabled returns why imports and mirror syncs are off, or ""
	// when they are allowed. It checks git's version once, on first use:
	// the server asks at start-up.
	importsDisabled func() string

	// deliveries runs the webhook deliveries in the background, bounded, and
	// tracks them so shutdown can wait for them.
	deliveries *deliveryQueue

	// webhooks builds, sends and records the webhook events.
	webhooks *webhook.Sender

	// imports counts the imports in flight and cancels them at shutdown.
	imports *importTracker

	// afterRegister, when a test sets it, runs once registerRepository has
	// committed a new repository.
	afterRegister func()

	// pushGates orders pushes against the removal of repositories created
	// by a push; see BeginPush and RemovePushCreatedRepository.
	pushGates pushGates

	// pushCreatedRetry is how RemovePushCreatedRepository retries removing
	// a directory left behind; see removePushCreatedDir.
	pushCreatedRetry retryPolicy

	// renameTxDone, when a test sets it, replaces the result of a rename's
	// transaction, as a COMMIT whose answer was lost would.
	renameTxDone func(error) error

	// The fields below are set by New and replaced only by tests of this
	// package, each on its own Backend.

	// binVersion returns the installed git's version, for the import gate.
	binVersion func() (git.Version, error)
	// validateGitRemote validates an import or mirror remote.
	validateGitRemote func(ctx context.Context, remote string, policy ssrf.GitRemotePolicy) (ssrf.ValidatedGitRemote, error)
	// searchPointerBlobs scans a repository for LFS pointers.
	searchPointerBlobs func(ctx context.Context, r *git.Repository, include, exclude []string, pointerChan chan<- lfs.PointerBlob, errChan chan<- error)
	// importTimeout bounds a whole import: the clone, and the LFS fetch
	// when asked for. The low-speed limits in ssrf.GitEnv catch a stalled
	// http remote sooner; this catches one that trickles just fast enough,
	// and the ssh and git transports, which have no such limit.
	importTimeout time.Duration
	// beforePushCreatedDelete, when set, runs in
	// RemovePushCreatedRepository after the check for refs and before the
	// transaction that checks the rest and deletes the repository, with the
	// repository's gate held.
	beforePushCreatedDelete func()
	// afterRefSnapshot, when set, runs in pushCreatedRefSnapshot with the
	// repository's directory rp, right after the snapshot of its refs, so
	// it follows the snapshot wherever RemovePushCreatedRepository takes it.
	afterRefSnapshot func(rp string)
}

// noReplaceObjectsEnv is the variable that makes git read objects as they
// are stored, ignoring refs/replace (see New).
const noReplaceObjectsEnv = "GIT_NO_REPLACE_OBJECTS"

// New returns a new Soft Serve backend.
//
// New sets GIT_NO_REPLACE_OBJECTS=1 in the environment of the whole
// process, a side effect beyond the backend it returns, so every git
// process the server and its hooks start reads objects as they are stored. A
// refs/replace ref, which anyone who may push can push and a --mirror
// import fetches, would otherwise change what the server shows of a
// repository (trees, blobs, logs, READMEs, webhook payloads), while
// upload-pack ignores it and clones get the original objects.
//
// It logs to logger, the default logger when nil.
func New(cfg *config.Config, db *db.DB, st store.Store, logger *log.Logger) *Backend {
	_ = os.Setenv(noReplaceObjectsEnv, "1") // only fails for an invalid name
	if logger == nil {
		logger = log.Default()
	}
	webhooks := webhook.NewSender(cfg, db, st, logger)
	logger = logger.WithPrefix("backend")
	b := &Backend{
		cfg:    cfg,
		db:     db,
		store:  st,
		logger: logger,

		deliveries:       newDeliveryQueue(maxBackgroundDeliveries, maxQueuedDeliveries, logger),
		webhooks:         webhooks,
		imports:          newImportTracker(),
		pushCreatedRetry: pushCreatedDirRetry,

		binVersion:         git.BinVersion,
		validateGitRemote:  ssrf.ValidateGitRemoteContext,
		searchPointerBlobs: lfs.SearchPointerBlobsBetween,
		importTimeout:      time.Hour,
	}
	// The version is read through the field when first needed, so a test
	// can replace binVersion after New.
	b.importsDisabled = sync.OnceValue(func() string { return checkGitVersion(b.binVersion) })
	return b
}

// ImportsDisabledReason returns why repository imports and mirror syncs are
// off, or "" when they are allowed. git's version is checked once per
// process; the server does so at start-up, and logs the reason. The reason
// is for the server log; users get proto.ErrImportsDisabled.
func (d *Backend) ImportsDisabledReason() string {
	return d.importsDisabled()
}

// minImportGitVersion is the oldest git that honours the settings the import
// and mirror guards rely on: http.curloptResolve (2.37) for pinning the
// validated address and http.followRedirects via GIT_CONFIG_* (2.31). Older
// git silently ignores unknown keys, so on 2.31 to 2.36 the pin would be
// dropped and the guard open to DNS rebinding.
var minImportGitVersion = git.Version{Major: 2, Minor: 37}

// checkGitVersion returns why imports must stay off, or "" if the git whose
// version binVersion returns is new enough. Running a clone under a git that
// ignores the guards would be worse than refusing it, so an unreadable
// version also closes the gate, until the server restarts.
func checkGitVersion(binVersion func() (git.Version, error)) string {
	v, err := binVersion()
	if err != nil {
		return fmt.Sprintf("cannot determine git version: %v", err)
	}
	if v.Less(minImportGitVersion) {
		return fmt.Sprintf("git %s is older than %s, which imports and mirror syncs need to be safe", v, minImportGitVersion)
	}
	return ""
}
